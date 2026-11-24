package notify_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/cardextras"
	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
	"github.com/crueber/weave/internal/testutil"
)

// Fan-out of events for one card on several boards (docs/DESIGN.md "Placements", events; phase 3).
//
// placeWorld: project "Apollo" (pA) with board "Alpha" (brdA: colA1 AlphaTodo, colA2 AlphaReview, colA3 AlphaDone
// (closing), archive, trash) and project "Borealis" (pB) with board "Bravo" (brdB: colB1 BravoInbox, colB2
// BravoDoing, archive, trash). The card "Rocket" is created by alice in colA1 (home Alpha) and then placed on Bravo
// colB1 by SQL (testutil.Place: no op creates a second placement yet).
//
//	alice  editor on both (acts)          bob   editor on Bravo only (acts on Bravo)
//	carol  viewer on Alpha, no watch      dan   viewer on both, watches the card
//	erin   viewer on Bravo, watches colB1 fay   viewer on Alpha, watches colA1
//	gus    viewer on Bravo, watches Bravo hal   viewer on Alpha, watches Alpha
//
// Webhooks (by SQL): whA (board Alpha), whB (board Bravo), whPA (project Apollo), whPB (project Borealis), whG (all).
type placeWorld struct {
	root *ops.Env
	as   map[string]*ops.Env
	card string
	mu   sync.Mutex
	hub  []realtime.Event
}

func newPlaceWorld(t *testing.T) *placeWorld {
	t.Helper()
	old := notify.RetryBackoff
	notify.RetryBackoff = nil
	t.Cleanup(func() { notify.Wait(); notify.RetryBackoff = old })
	root := testutil.Env(t, nil)
	w := &placeWorld{root: root, as: map[string]*ops.Env{}}
	now := db.Now()
	for _, u := range []string{"alice", "bob", "carol", "dan", "erin", "fay", "gus", "hal"} {
		name := strings.ToUpper(u[:1]) + u[1:]
		x(t, root, `INSERT INTO users(id,email,name,role,subscribe_own_cards,created_at) VALUES(?,?,?,'user',1,?)`, u, u+"@x.io", name, now)
		w.as[u] = testutil.As(root, &ops.Principal{UserID: u, Role: "user", Name: name})
	}
	x(t, root, `INSERT INTO projects(id,name,created_at) VALUES('pA','Apollo',?),('pB','Borealis',?)`, now, now)
	x(t, root, `INSERT INTO boards(id,project_id,position,name,created_at) VALUES('brdA','pA',1,'Alpha',?),('brdB','pB',1,'Bravo',?)`, now, now)
	x(t, root, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES
		('colA1','brdA','active',1,'AlphaTodo',?),('colA2','brdA','active',2,'AlphaReview',?),('colA3','brdA','closed',3,'AlphaDone',?),
		('colAarch','brdA','archive',100,NULL,?),('colAtrash','brdA','trash',101,NULL,?),
		('colB1','brdB','active',1,'BravoInbox',?),('colB2','brdB','active',2,'BravoDoing',?),
		('colBarch','brdB','archive',100,NULL,?),('colBtrash','brdB','trash',101,NULL,?)`, now, now, now, now, now, now, now, now, now)
	x(t, root, `INSERT INTO labels(id,board_id,name,color,position,created_at) VALUES('lblA','brdA','AlphaLabel','berry-red',1,?),('lblB','brdB','BravoLabel','pink-tulip',1,?)`, now, now)
	member := func(board, user, role string) {
		x(t, root, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,?,?,?)`, db.NewID(), board, user, role, now)
	}
	member("brdA", "alice", "editor")
	member("brdB", "alice", "editor")
	member("brdB", "bob", "editor")
	for _, u := range []string{"carol", "fay", "hal", "dan"} {
		member("brdA", u, "viewer")
	}
	for _, u := range []string{"erin", "gus", "dan"} {
		member("brdB", u, "viewer")
	}
	w.card = w.call(t, "alice", "card.create", map[string]any{"listId": "colA1", "name": "Rocket"})
	testutil.Place(t, root.DB, w.card, "brdB", "colB1", 1)
	w.call(t, "dan", "card.subscribe", map[string]any{"id": w.card})
	w.call(t, "erin", "list.subscribe", map[string]any{"id": "colB1"})
	w.call(t, "fay", "list.subscribe", map[string]any{"id": "colA1"})
	w.call(t, "gus", "board.subscribe", map[string]any{"id": "brdB"})
	w.call(t, "hal", "board.subscribe", map[string]any{"id": "brdA"})
	for _, h := range []struct{ id, project, board string }{{"whA", "", "brdA"}, {"whB", "", "brdB"}, {"whPA", "pA", ""}, {"whPB", "pB", ""}, {"whG", "", ""}} {
		x(t, root, `INSERT INTO webhooks(id,name,url,project_id,board_id,created_at) VALUES(?,?,?,?,?,?)`, h.id, h.id, "http://127.0.0.1:9/"+h.id, nzs(h.project), nzs(h.board), now)
	}
	// Tap is called synchronously by every Publish (a subscriber's buffer could drop events and weaken the checks)
	root.Hub.Tap = func(ev realtime.Event) {
		w.mu.Lock()
		w.hub = append(w.hub, ev)
		w.mu.Unlock()
	}
	w.reset(t)
	return w
}

func nzs(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func (w *placeWorld) call(t *testing.T, who, op string, args map[string]any) string {
	t.Helper()
	out, err := ops.CallMap(w.as[who], op, args)
	if err != nil {
		t.Fatalf("%s %s %v: %v", who, op, args, err)
	}
	b, _ := json.Marshal(out)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	id, _ := m["id"].(string)
	return id
}

// mark is the newest event id before a step: the step's rows are the ones after it.
var markSeq int64

// reset forgets everything recorded so far: hub events, notifications, deliveries; actions rows are skipped by seq.
func (w *placeWorld) reset(t *testing.T) {
	t.Helper()
	notify.Wait()
	w.mu.Lock()
	w.hub = nil
	w.mu.Unlock()
	x(t, w.root, `DELETE FROM notifications`)
	x(t, w.root, `DELETE FROM webhook_deliveries`)
	markSeq = events.LastSeq(w.root.Ctx, w.root.DB)
}

type actRow struct {
	id, board, origin, typ, data, envelope string
}

// step collects what one step produced.
type step struct {
	acts   []actRow
	hub    []realtime.Event
	notifs map[string][]map[string]string // user -> [{type, board, data}]
	hooks  map[string][]string            // webhook id -> payloads (event deliveries)
	nhooks map[string][]string            // webhook id -> notificationCreate payloads
}

func (w *placeWorld) collect(t *testing.T) step {
	t.Helper()
	notify.Wait()
	var s step
	r, err := w.root.DB.Query(`SELECT id,board_id,COALESCE(origin_id,''),type,data,COALESCE(envelope,'') FROM actions WHERE seq > ? ORDER BY seq`, markSeq)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var a actRow
		_ = r.Scan(&a.id, &a.board, &a.origin, &a.typ, &a.data, &a.envelope)
		s.acts = append(s.acts, a)
	}
	r.Close()
	w.mu.Lock()
	s.hub = append(s.hub, w.hub...)
	w.mu.Unlock()
	s.notifs = map[string][]map[string]string{}
	r, err = w.root.DB.Query(`SELECT user_id,type,COALESCE(board_id,''),data FROM notifications ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var u, typ, b, d string
		_ = r.Scan(&u, &typ, &b, &d)
		s.notifs[u] = append(s.notifs[u], map[string]string{"type": typ, "board": b, "data": d})
	}
	r.Close()
	s.hooks, s.nhooks = map[string][]string{}, map[string][]string{}
	r, err = w.root.DB.Query(`SELECT webhook_id,event,COALESCE(payload,'') FROM webhook_deliveries ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for r.Next() {
		var id, ev, p string
		_ = r.Scan(&id, &ev, &p)
		if ev == "notificationCreate" {
			s.nhooks[id] = append(s.nhooks[id], p)
		} else {
			s.hooks[id] = append(s.hooks[id], p)
		}
	}
	r.Close()
	w.reset(t)
	return s
}

func (s step) hubIn(room string) []realtime.Event {
	var out []realtime.Event
	for _, ev := range s.hub {
		if ev.Room == room {
			out = append(out, ev)
		}
	}
	return out
}

func (s step) actsOn(board string) []actRow {
	var out []actRow
	for _, a := range s.acts {
		if a.board == board {
			out = append(out, a)
		}
	}
	return out
}

func (s step) expectNotified(t *testing.T, name string, want map[string]string) {
	t.Helper()
	var got []string
	for u, ns := range s.notifs {
		if len(ns) != 1 {
			t.Errorf("%s: %s got %d notifications (want at most one per event)", name, u, len(ns))
		}
		got = append(got, u)
	}
	sort.Strings(got)
	for u, wb := range want {
		typ, board, _ := strings.Cut(wb, "@")
		ns := s.notifs[u]
		if len(ns) == 0 || ns[0]["type"] != typ || ns[0]["board"] != board {
			t.Errorf("%s: %s got %v, want %s on %s", name, u, ns, typ, board)
		}
	}
	for _, u := range got {
		if _, ok := want[u]; !ok {
			t.Errorf("%s: %s should hear nothing, got %v", name, u, s.notifs[u])
		}
	}
}

func (s step) expectHooks(t *testing.T, name string, want ...string) {
	t.Helper()
	var got []string
	for id, ps := range s.hooks {
		for range ps {
			got = append(got, id)
		}
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s: webhooks %v, want %v (once each)", name, got, want)
	}
}

// A card-level event (a comment) on a card on Alpha and Bravo: one actions row per board (the second names the first
// as its origin), one hub publish per room with that board's own card, the same origin in both envelopes, one
// notification per watcher naming the board they watch (card watchers: the home), one delivery per webhook, each
// board webhook with its own board's view.
func TestFanoutCardLevelEvent(t *testing.T) {
	w := newPlaceWorld(t)
	w.call(t, "alice", "card.label.add", map[string]any{"id": w.card, "labelId": "lblA", "boardId": "brdA"})
	_ = w.collect(t)
	w.call(t, "alice", "comment.create", map[string]any{"cardId": w.card, "text": "liftoff"})
	s := w.collect(t)

	var com []actRow
	for _, a := range s.acts {
		if a.typ == "commentCreate" {
			com = append(com, a)
		}
	}
	if len(com) != 2 || com[0].board != "brdA" || com[1].board != "brdB" || com[0].origin != "" || com[1].origin != com[0].id {
		t.Fatalf("comment rows = %+v, want one on Alpha (origin) then one on Bravo naming it", com)
	}
	origins := map[string]bool{}
	for _, a := range com {
		var env events.Envelope
		if err := json.Unmarshal([]byte(a.envelope), &env); err != nil {
			t.Fatal(err)
		}
		origins[env.Origin] = true
		if env.BoardID != a.board || env.Card == nil || env.Card.BoardID != a.board {
			t.Fatalf("envelope on %s: board %s card %+v", a.board, env.BoardID, env.Card)
		}
		if a.board == "brdB" && (env.ListID != "colB1" || env.Card.ListID != "colB1" || len(env.Card.Labels) != 0) {
			t.Fatalf("Bravo envelope shows another board: list %s card %+v", env.ListID, env.Card)
		}
		if a.board == "brdA" && (env.Card.ListID != "colA1" || len(env.Card.Labels) != 1) {
			t.Fatalf("Alpha envelope: %+v", env.Card)
		}
	}
	if len(origins) != 1 || !origins[com[0].id] {
		t.Fatalf("envelopes carry origins %v, want both %s", origins, com[0].id)
	}
	// the durable stream an agent on both boards reads: two envelopes, one origin
	rows, err := events.Query(w.root.Ctx, w.root.DB, events.Filter{Since: markSeq - 10, Boards: []string{"brdA", "brdB"}, Types: []string{"comment.created"}})
	if err != nil || len(rows) != 2 || rows[0].BoardID == rows[1].BoardID {
		t.Fatalf("stream rows = %v (%v), want one per board", rows, err)
	}
	for room, n := range map[string]int{"board:brdA": 1, "board:brdB": 1} {
		got := 0
		for _, ev := range s.hubIn(room) {
			if ev.Name == "comment.created" {
				got++
			}
		}
		if got != n {
			t.Fatalf("%s heard comment.created %d times, want %d", room, got, n)
		}
	}
	s.expectNotified(t, "comment", map[string]string{
		"dan": "commentCard@brdA", "erin": "commentCard@brdB", "fay": "commentCard@brdA", "gus": "commentCard@brdB", "hal": "commentCard@brdA",
	})
	s.expectHooks(t, "comment", "whA", "whB", "whPA", "whPB", "whG")
	for _, p := range append(s.hooks["whB"], s.hooks["whPB"]...) {
		if strings.Contains(p, "brdA") || strings.Contains(p, "colA") {
			t.Fatalf("a Bravo webhook got Alpha's ids: %s", p)
		}
	}
	// activity: one row per logical event for someone on both boards; an Alpha-only viewer sees Alpha's copy
	for who, board := range map[string]string{"dan": "brdA", "carol": "brdA"} {
		acts, err := ops.Typed[[]cardextras.ActionItem](w.as[who], "activity.card", cardextras.CardActionsIn{CardID: w.card})
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, a := range acts {
			if a.Type == "commentCreate" {
				n++
				if a.BoardID != board {
					t.Fatalf("%s sees the comment as recorded on %s", who, a.BoardID)
				}
			}
		}
		if n != 1 {
			t.Fatalf("%s sees the comment %d times in the card activity", who, n)
		}
	}
	d, err := ops.Typed[notify.DashboardOut](w.as["dan"], "dashboard.get", notify.NoInput{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, a := range d.Activity {
		if a.Type == "commentCreate" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("dashboard lists the comment %d times, want once", n)
	}
}

// A placement event (a move between columns of one board) is recorded on that board only; the card's other board
// gets the unstored card.placement.changed nudge with the card id only. Bravo's column watcher hears Bravo's moves
// and nothing of Alpha's.
func TestFanoutPlacementEvent(t *testing.T) {
	w := newPlaceWorld(t)
	w.call(t, "bob", "card.move", map[string]any{"id": w.card, "listId": "colB2"})
	s := w.collect(t)
	if len(s.acts) != 1 || s.acts[0].board != "brdB" || s.acts[0].typ != "cardMove" {
		t.Fatalf("a move on Bravo wrote %+v, want one Bravo row", s.acts)
	}
	if hb := s.hubIn("board:brdB"); len(hb) != 1 || hb[0].Name != "card.moved" {
		t.Fatalf("Bravo room heard %+v", hb)
	}
	ha := s.hubIn("board:brdA")
	if len(ha) != 1 || ha[0].Name != events.PlacementChanged {
		t.Fatalf("Alpha room heard %+v, want only the placement nudge", ha)
	}
	if b, _ := json.Marshal(ha[0].Data); string(b) != `{"cardId":"`+w.card+`"}` {
		t.Fatalf("nudge data = %s, want the card id only", b)
	}
	s.expectNotified(t, "move on Bravo", map[string]string{"erin": "moveCard@brdB", "gus": "moveCard@brdB", "dan": "moveCard@brdB", "alice": "moveCard@brdB"})
	s.expectHooks(t, "move on Bravo", "whB", "whPB", "whG")
	for _, p := range s.hooks["whG"] {
		if strings.Contains(p, "brdA") || strings.Contains(p, "colA") {
			t.Fatalf("the global webhook got the Alpha view of a Bravo move: %s", p)
		}
	}

	// relocating the home placement onto a board that already holds the card is a conflict, never a key failure
	// (without boardId, a move into a column of a board the card is on moves THAT board's placement)
	if _, err := ops.CallMap(w.as["alice"], "card.move", map[string]any{"id": w.card, "listId": "colB1", "boardId": "brdA"}); err == nil || !strings.Contains(err.Error(), "already on that board") {
		t.Fatalf("move onto a board that holds the card: %v, want a conflict", err)
	}
	w.call(t, "alice", "card.move", map[string]any{"id": w.card, "listId": "colA2"})
	s = w.collect(t)
	if len(s.acts) != 1 || s.acts[0].board != "brdA" {
		t.Fatalf("a move on Alpha wrote %+v", s.acts)
	}
	s.expectNotified(t, "move on Alpha", map[string]string{"fay": "moveCard@brdA", "hal": "moveCard@brdA", "dan": "moveCard@brdA"})
	s.expectHooks(t, "move on Alpha", "whA", "whPA", "whG")
}

// Card-level events on every board: a close caused by a move into Alpha's closing column (closed is per card:
// Bravo's watchers hear it, without Alpha's columns; Bravo's tile learns it from a quiet card.updated), a manual
// archive (card.archive moves EVERY placement: one row per board, origin-linked, one publish per room, one
// notification per watcher, Bravo's copy names only Bravo's archive column and its watcher of the column the card
// left hears it), and a delete from the trash (every placement must be in the trash; the boards are read before the
// delete: card.deleted reaches both rooms, with no snapshot).
func TestFanoutArchiveCloseDelete(t *testing.T) {
	w := newPlaceWorld(t)
	w.call(t, "alice", "card.close", map[string]any{"id": w.card})
	s := w.collect(t)
	var moves, closed []actRow
	for _, a := range s.acts {
		switch a.typ {
		case "cardMove":
			moves = append(moves, a)
		case "cardUpdate":
			closed = append(closed, a)
		default:
			t.Fatalf("unexpected row %+v", a)
		}
	}
	if len(moves) != 1 || moves[0].board != "brdA" {
		t.Fatalf("a close by move is recorded (as a move) on the moved placement's board only, got %+v", moves)
	}
	if len(closed) != 2 || closed[0].board != "brdA" || closed[1].board != "brdB" || closed[1].origin != closed[0].id ||
		!strings.Contains(closed[1].data, `"quiet":true`) || !strings.Contains(closed[1].envelope, `"isClosed":true`) || strings.Contains(closed[1].data+closed[1].envelope, "colA") {
		t.Fatalf("the closed state (per card) reaches Bravo as one quiet card.updated per board: %+v", closed)
	}
	s.expectNotified(t, "close", map[string]string{"dan": "closeCard@brdA", "erin": "closeCard@brdB", "fay": "closeCard@brdA", "gus": "closeCard@brdB", "hal": "closeCard@brdA"})
	for _, u := range []string{"erin", "gus"} {
		if d := s.notifs[u][0]["data"]; strings.Contains(d, "colA") || strings.Contains(d, "Alpha") {
			t.Fatalf("%s (Bravo only) got Alpha's columns: %s", u, d)
		}
	}
	if !strings.Contains(s.notifs["fay"][0]["data"], "AlphaDone") {
		t.Fatalf("fay (Alpha) should see the columns: %s", s.notifs["fay"][0]["data"])
	}

	var bFrom string // Bravo's column before the archive: Bravo's copy names its own move (events.PlacementLists)
	_ = w.root.DB.QueryRow(`SELECT list_id FROM card_placements WHERE card_id=? AND board_id='brdB'`, w.card).Scan(&bFrom)
	w.call(t, "alice", "card.archive", map[string]any{"id": w.card})
	s = w.collect(t)
	a, b := s.actsOn("brdA"), s.actsOn("brdB")
	if len(a) != 1 || len(b) != 1 || len(s.acts) != 2 || a[0].origin != "" || b[0].origin != a[0].id {
		t.Fatalf("archive rows = %+v, want one per board, Bravo's naming Alpha's as its origin", s.acts)
	}
	var env events.Envelope
	_ = json.Unmarshal([]byte(b[0].envelope), &env)
	if env.Type != "card.archived" || bFrom == "" || env.FromListID != bFrom || env.ToListID != "colBarch" || env.ListID != "colBarch" || env.Card == nil || env.Card.ListID != "colBarch" ||
		strings.Contains(b[0].data+b[0].envelope, "colA") || strings.Contains(b[0].data+b[0].envelope, events.CardLevel) || strings.Contains(b[0].data+b[0].envelope, events.PlacementLists) {
		t.Fatalf("Bravo's archive copy: %+v data %s", env, b[0].data)
	}
	_ = json.Unmarshal([]byte(a[0].envelope), &env)
	if env.Type != "card.archived" || env.ListID != "colAarch" || env.FromListID != "colA3" {
		t.Fatalf("Alpha's archive row: %+v", env)
	}
	for _, room := range []string{"board:brdA", "board:brdB"} {
		if h := s.hubIn(room); len(h) != 1 || h[0].Name != "card.moved" {
			t.Fatalf("%s heard %+v, want the archive once", room, h)
		}
	}
	s.expectNotified(t, "archive", map[string]string{"dan": "moveCard@brdA", "erin": "moveCard@brdB", "gus": "moveCard@brdB", "hal": "moveCard@brdA"})
	if !strings.Contains(s.notifs["erin"][0]["data"], `"via":"list"`) || strings.Contains(s.notifs["erin"][0]["data"], "colA") {
		t.Fatalf("erin's archive notification: %v", s.notifs["erin"])
	}
	var pl []string
	for _, brd := range []string{"brdA", "brdB"} {
		var l string
		_ = w.root.DB.QueryRow(`SELECT list_id FROM card_placements WHERE card_id=? AND board_id=?`, w.card, brd).Scan(&l)
		pl = append(pl, l)
	}
	if pl[0] != "colAarch" || pl[1] != "colBarch" {
		t.Fatalf("after card.archive the placements are in %v, want both archives", pl)
	}

	w.call(t, "alice", "card.archive", map[string]any{"id": w.card, "trash": true})
	_ = w.collect(t)
	w.call(t, "alice", "card.delete", map[string]any{"id": w.card})
	s = w.collect(t)
	for _, board := range []string{"brdA", "brdB"} {
		rows := s.actsOn(board)
		if len(rows) != 1 || rows[0].typ != "cardDelete" {
			t.Fatalf("delete rows on %s = %+v", board, rows)
		}
		var env events.Envelope
		_ = json.Unmarshal([]byte(rows[0].envelope), &env)
		if env.Card != nil || (board == "brdB" && env.ListID != "") {
			t.Fatalf("delete envelope on %s: %+v", board, env)
		}
		if h := s.hubIn("board:" + board); len(h) != 1 || h[0].Name != "card.deleted" {
			t.Fatalf("%s heard %+v", board, h)
		}
	}
	s.expectHooks(t, "delete", "whA", "whB", "whPA", "whPB", "whG")
}

// card.archive when the context placement is already archived (Alpha's column was archived with its cards: a
// placement move) still moves and announces the card's other placements: the event is card-level by its routing key,
// not by its kind.
func TestFanoutArchiveWhenContextAlreadyArchived(t *testing.T) {
	w := newPlaceWorld(t)
	w.call(t, "alice", "card.move", map[string]any{"id": w.card, "listId": "colAarch", "boardId": "brdA"})
	s := w.collect(t)
	if len(s.acts) != 1 || s.acts[0].board != "brdA" {
		t.Fatalf("archiving Alpha's placement by a move is board-only, got %+v", s.acts)
	}
	if h := s.hubIn("board:brdB"); len(h) != 1 || h[0].Name != events.PlacementChanged {
		t.Fatalf("Bravo heard %+v, want only the nudge", h)
	}
	if _, ok := s.notifs["erin"]; ok {
		t.Fatalf("erin (Bravo column) heard Alpha's placement archive: %v", s.notifs["erin"])
	}
	w.call(t, "alice", "card.archive", map[string]any{"id": w.card})
	s = w.collect(t)
	b := s.actsOn("brdB")
	if len(b) != 1 || b[0].typ != "cardMove" {
		t.Fatalf("Bravo rows = %+v, want the archive", s.acts)
	}
	var env events.Envelope
	_ = json.Unmarshal([]byte(b[0].envelope), &env)
	if env.ListID != "colBarch" {
		t.Fatalf("Bravo's copy: %+v", env)
	}
	var l string
	_ = w.root.DB.QueryRow(`SELECT list_id FROM card_placements WHERE card_id=? AND board_id='brdB'`, w.card).Scan(&l)
	if l != "colBarch" {
		t.Fatalf("Bravo's placement is in %s", l)
	}
}

// A column's archive-after rule archives that board's placement only: the card.archived (quiet) is recorded on that
// board, the card's other board gets the nudge only and keeps its placement, and nobody is notified.
func TestFanoutAutoArchiveIsBoardOnly(t *testing.T) {
	w := newPlaceWorld(t)
	x(t, w.root, `UPDATE card_placements SET list_id='colB2', list_changed_at='2000-01-01T00:00:00Z' WHERE card_id=? AND board_id='brdB'`, w.card)
	x(t, w.root, `UPDATE lists SET archive_after=1 WHERE id='colB2'`)
	_ = w.collect(t)
	w.call(t, "bob", "card.create", map[string]any{"listId": "colB2", "name": "Newer"})
	s := w.collect(t)
	var auto []actRow
	for _, a := range s.acts {
		if a.typ == "cardMove" && strings.Contains(a.data, `"auto":"archiveAfter"`) {
			auto = append(auto, a)
		}
	}
	if len(auto) != 1 || auto[0].board != "brdB" || !strings.Contains(auto[0].envelope, `"type":"card.archived"`) {
		t.Fatalf("auto-archive rows = %+v, want one on Bravo", auto)
	}
	for _, a := range s.actsOn("brdA") {
		t.Fatalf("Alpha got a row: %+v", a)
	}
	if h := s.hubIn("board:brdA"); len(h) != 1 || h[0].Name != events.PlacementChanged {
		t.Fatalf("Alpha heard %+v, want only the nudge", h)
	}
	for u, ns := range s.notifs {
		for _, n := range ns {
			if strings.Contains(n["data"], w.card) || n["type"] == "moveCard" {
				t.Fatalf("%s was notified of the auto-archive: %v", u, n)
			}
		}
	}
	var la, lb string
	_ = w.root.DB.QueryRow(`SELECT list_id FROM card_placements WHERE card_id=? AND board_id='brdA'`, w.card).Scan(&la)
	_ = w.root.DB.QueryRow(`SELECT list_id FROM card_placements WHERE card_id=? AND board_id='brdB'`, w.card).Scan(&lb)
	if la != "colA1" || lb != "colBarch" {
		t.Fatalf("placements after the auto-archive: Alpha %s Bravo %s", la, lb)
	}
}

// Deleting the home board re-homes a card that is also on another board (it survives): its other board hears one
// card.updated (changed placements, home) and no card.deleted; a card on the deleted board only goes with it.
func TestFanoutHomeBoardDelete(t *testing.T) {
	w := newPlaceWorld(t)
	only := w.call(t, "alice", "card.create", map[string]any{"listId": "colA1", "name": "AlphaOnly"})
	x(t, w.root, `INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES(?,?,?,?)`, db.NewID(), "pA", "alice", db.Now())
	_ = w.collect(t)
	w.call(t, "alice", "board.delete", map[string]any{"id": "brdA"})
	s := w.collect(t)
	var hb []string
	for _, ev := range s.hubIn("board:brdB") {
		hb = append(hb, ev.Name)
	}
	rows := s.actsOn("brdB")
	if strings.Join(hb, ",") != "card.updated" || len(rows) != 1 || rows[0].typ != "cardUpdate" ||
		!strings.Contains(rows[0].data, `"changed":["placements","home"]`) || strings.Contains(rows[0].data+rows[0].envelope, "colA") {
		t.Fatalf("Bravo after deleting Alpha: hub %v rows %+v", hb, rows)
	}
	var home string
	var n int
	_ = w.root.DB.QueryRow(`SELECT board_id FROM cards WHERE id=?`, w.card).Scan(&home)
	_ = w.root.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE id=?`, only).Scan(&n)
	if home != "brdB" || n != 0 {
		t.Fatalf("after deleting Alpha: Rocket's home %q, AlphaOnly rows %d", home, n)
	}
}

// card.place and card.unplace through the ops: a placement event on the new (removed) board only, the card's other
// boards get the nudge; nothing of either board leaks into the other board's rows, rooms or webhooks.
func TestFanoutPlaceUnplace(t *testing.T) {
	w := newPlaceWorld(t)
	c := w.call(t, "alice", "card.create", map[string]any{"listId": "colA2", "name": "Probe"})
	_ = w.collect(t)
	w.call(t, "alice", "card.place", map[string]any{"id": c, "listId": "colB2"})
	s := w.collect(t)
	if len(s.acts) != 1 || s.acts[0].board != "brdB" || s.acts[0].typ != events.TypePlace || !strings.Contains(s.acts[0].envelope, `"type":"`+events.StreamPlaced+`"`) ||
		strings.Contains(s.acts[0].data+s.acts[0].envelope, "colA") {
		t.Fatalf("place rows = %+v", s.acts)
	}
	if h := s.hubIn("board:brdA"); len(h) != 1 || h[0].Name != events.PlacementChanged {
		t.Fatalf("Alpha heard %+v, want only the nudge", h)
	}
	s.expectHooks(t, "place", "whB", "whPB", "whG")
	for _, p := range s.hooks["whG"] {
		if strings.Contains(p, "colA") || strings.Contains(p, "brdA") {
			t.Fatalf("global webhook got Alpha's view of a placement on Bravo: %s", p)
		}
	}
	w.call(t, "alice", "card.unplace", map[string]any{"id": c, "boardId": "brdA"}) // the home leaves: Bravo becomes home
	s = w.collect(t)
	var types []string
	for _, a := range s.acts {
		types = append(types, a.typ+"@"+a.board)
		if a.board == "brdB" && strings.Contains(a.data+a.envelope, "colA") {
			t.Fatalf("Bravo row names Alpha's column: %+v", a)
		}
	}
	if strings.Join(types, ",") != events.TypeUnplace+"@brdA,cardUpdate@brdB" {
		t.Fatalf("unplace rows = %v, want the removal on Alpha and the new home on Bravo", types)
	}
	var home string
	_ = w.root.DB.QueryRow(`SELECT board_id FROM cards WHERE id=?`, c).Scan(&home)
	if home != "brdB" {
		t.Fatalf("home after unplacing it = %s", home)
	}
}

// Privacy: everything that goes to someone who sees only one board (that board's actions rows and envelopes, its
// room, notifications of its viewers, its board and project webhooks) never names the other board, its columns or
// its labels, whichever board the change was made on.
func TestFanoutNeverLeaksAnotherBoard(t *testing.T) {
	type side struct {
		actor, board, room string
		steps              []func(w *placeWorld)
		viewers            []string // see only the other board
		hooks              []string
		otherBoard         string
		secrets            []string
	}
	sides := []side{{
		actor: "alice", otherBoard: "brdB", viewers: []string{"bob", "erin", "gus"}, hooks: []string{"whB", "whPB"},
		secrets: []string{"brdA", "colA1", "colA2", "colA3", "colAarch", "colAtrash", "Alpha", "lblA", "Apollo", `"pA"`},
	}, {
		actor: "bob", otherBoard: "brdA", viewers: []string{"carol", "fay", "hal"}, hooks: []string{"whA", "whPA"},
		secrets: []string{"brdB", "colB1", "colB2", "colBarch", "colBtrash", "Bravo", "lblB", "Borealis", `"pB"`},
	}}
	for _, sd := range sides {
		t.Run(sd.actor, func(t *testing.T) {
			w := newPlaceWorld(t)
			own := map[string]string{"alice": "colA", "bob": "colB"}[sd.actor]
			lbl := map[string]string{"alice": "lblA", "bob": "lblB"}[sd.actor]
			due := "2030-01-02T00:00:00Z"
			// a board-wide custom field of the acting board ("AlphaField"/"BravoField": the name is that board's vocabulary)
			myBoard := map[string]string{"alice": "brdA", "bob": "brdB"}[sd.actor]
			x(t, w.root, `INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES(?,?,?,?)`, db.NewID(), map[string]string{"alice": "pA", "bob": "pB"}[sd.actor], sd.actor, db.Now())
			grp := w.call(t, sd.actor, "customfieldgroup.create", map[string]any{"boardId": myBoard, "name": "Fields"})
			fld := w.call(t, sd.actor, "customfield.create", map[string]any{"groupId": grp, "name": map[string]string{"alice": "AlphaField", "bob": "BravoField"}[sd.actor]})
			_ = w.collect(t)
			do := []struct {
				op   string
				args map[string]any
			}{
				{"card.move", map[string]any{"id": w.card, "listId": own + "2"}},
				{"card.label.add", map[string]any{"id": w.card, "labelId": lbl, "boardId": myBoard}},
				{"card.update", map[string]any{"id": w.card, "name": "Rocket 2", "dueDate": due}},
				{"comment.create", map[string]any{"cardId": w.card, "text": "go"}},
				{"card.label.remove", map[string]any{"id": w.card, "labelId": lbl, "boardId": myBoard}},
				{"card.move", map[string]any{"id": w.card, "listId": own + "1"}},
				{"card.archive", map[string]any{"id": w.card}},
				{"card.restore", map[string]any{"id": w.card}},
				{"card.archive", map[string]any{"id": w.card, "trash": true}},
				{"card.restore", map[string]any{"id": w.card}},
				{"customfieldvalue.set", map[string]any{"cardId": w.card, "groupId": grp, "fieldId": fld, "content": "x"}},
				{"customfieldvalue.clear", map[string]any{"cardId": w.card, "groupId": grp, "fieldId": fld}},
			}
			if sd.actor == "alice" {
				do = append(do, struct {
					op   string
					args map[string]any
				}{"card.close", map[string]any{"id": w.card}})
			}
			for _, d := range do {
				w.call(t, sd.actor, d.op, d.args)
			}
			s := w.collect(t)
			var leak []string
			check := func(what, text string) {
				for _, sec := range sd.secrets {
					if strings.Contains(text, sec) {
						leak = append(leak, fmt.Sprintf("%s contains %s: %s", what, sec, text))
					}
				}
			}
			for _, a := range s.actsOn(sd.otherBoard) {
				check("actions row", a.data+a.envelope)
			}
			for _, ev := range s.hubIn("board:" + sd.otherBoard) {
				b, _ := json.Marshal(ev.Data)
				check("hub "+ev.Name, string(b))
			}
			for _, u := range sd.viewers {
				for _, n := range s.notifs[u] {
					check("notification of "+u, n["board"]+n["data"])
				}
			}
			for _, h := range sd.hooks {
				for _, p := range append(s.hooks[h], s.nhooks[h]...) {
					check("webhook "+h, p)
				}
			}
			// notifications that name the other board (whoever gets them: someone on both boards too) carry none of
			// the acting board's names: they travel under the other board's name to its project webhooks and channels
			for u, ns := range s.notifs {
				for _, n := range ns {
					if n["board"] == sd.otherBoard {
						check("notification on the other board for "+u, n["data"])
					}
				}
			}
			if len(leak) > 0 {
				t.Fatalf("leaks:\n%s", strings.Join(leak, "\n"))
			}
			if len(s.actsOn(sd.otherBoard)) == 0 {
				t.Fatalf("the card-level events never reached the other board")
			}
			// the other board's viewers' card activity never shows the acting board's rows
			for _, u := range sd.viewers[:1] {
				acts, err := ops.Typed[[]cardextras.ActionItem](w.as[u], "activity.card", cardextras.CardActionsIn{CardID: w.card})
				if err != nil {
					t.Fatal(err)
				}
				for _, a := range acts {
					b, _ := json.Marshal(a)
					check("activity.card of "+u, string(b))
				}
				if len(leak) > 0 {
					t.Fatalf("leaks:\n%s", strings.Join(leak, "\n"))
				}
			}
		})
	}
}

// The planner's query count stays constant with three placements: one card+placements query, the source column of
// a move, one UNION over every in-scope column and board, one CardViewers.
func TestPlannerQueryCountWithPlacements(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	defer testutil.CheckPlacements(t, d)
	e := testutil.Env(t, nil)
	e.DB = d
	now := db.Now()
	x(t, e, `INSERT INTO projects(id,name,created_at) VALUES('p1','P',?)`, now)
	for i := 1; i <= 3; i++ {
		b := fmt.Sprintf("b%d", i)
		x(t, e, `INSERT INTO boards(id,project_id,position,name,created_at) VALUES(?,'p1',?,?,?)`, b, i, b, now)
		x(t, e, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES(?,?,'active',1,'A',?),(?,?,'active',2,'Z',?)`, b+"l1", b, now, b+"l2", b, now)
	}
	x(t, e, `INSERT INTO cards(id,board_id,list_id,position,name,list_changed_at,created_at) VALUES('c1','b1','b1l2',1,'C',?,?)`, now, now)
	testutil.Place(t, d, "c1", "b2", "b2l1", 1)
	testutil.Place(t, d, "c1", "b3", "b3l1", 1)
	addWatchers := func(from, to int) {
		for i := from; i < to; i++ {
			u := fmt.Sprintf("u%02d", i)
			b := fmt.Sprintf("b%d", i%3+1)
			x(t, e, `INSERT INTO users(id,email,name,role,created_at) VALUES(?,?,?,'user',?)`, u, u+"@x.io", u, now)
			x(t, e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,?,?,?)`, db.NewID(), b, u, "viewer", now)
			switch i % 3 {
			case 0:
				x(t, e, `INSERT INTO card_subscriptions(card_id,user_id) VALUES('c1',?)`, u)
			case 1:
				x(t, e, `INSERT INTO list_subscriptions(list_id,user_id,created_at) VALUES(?,?,?)`, b+"l1", u, now)
			default:
				x(t, e, `INSERT INTO board_subscriptions(board_id,user_id) VALUES(?,?)`, b, u)
			}
		}
	}
	cdb, err := sql.Open("sqlite-counting-notify", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	ce := testutil.As(e, nil)
	ce.DB = &db.DB{DB: cdb}
	count := func() (int64, int) {
		queries.Store(0)
		n := 0
		for _, ev := range []ops.Event{
			{Type: "cardMove", BoardID: "b1", CardID: "c1", Data: map[string]any{"fromListId": "b1l1", "toListId": "b1l2"}},
			{Type: "commentCreate", BoardID: "b1", CardID: "c1", Data: map[string]any{"commentId": "m", "text": "hi"}},
		} {
			n += len(notify.PlanForTest(ce, ev, "nobody"))
		}
		return queries.Load(), n
	}
	addWatchers(0, 6)
	few, n1 := count()
	addWatchers(6, 60)
	many, n2 := count()
	if n1 == 0 || n2 <= n1 {
		t.Fatalf("planned %d and %d notifications", n1, n2)
	}
	if few != many || many > 10 {
		t.Fatalf("6 watchers = %d queries, 60 watchers = %d (want equal and small)", few, many)
	}
}

// A card's activity keeps the rows of a board it has left for that board's viewers only.
func TestCardActivityAfterLeavingABoard(t *testing.T) {
	w := newPlaceWorld(t)
	c2 := w.call(t, "alice", "card.create", map[string]any{"listId": "colA1", "name": "Probe"})
	w.call(t, "alice", "card.move", map[string]any{"id": c2, "listId": "colB2"})
	_ = w.collect(t)
	boardsSeen := func(who string) map[string]bool {
		acts, err := ops.Typed[[]cardextras.ActionItem](w.as[who], "activity.card", cardextras.CardActionsIn{CardID: c2})
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, a := range acts {
			out[a.BoardID] = true
		}
		return out
	}
	if got := boardsSeen("dan"); !got["brdA"] || !got["brdB"] {
		t.Fatalf("dan (both boards) sees rows on %v, want Alpha's history too", got)
	}
	if got := boardsSeen("bob"); got["brdA"] || !got["brdB"] {
		t.Fatalf("bob (Bravo only) sees rows on %v, want Bravo's only", got)
	}
}

// Anonymous readers of a public board (activity.card is AuthPublic) see that board's rows of a card that is also on a
// private board, and nothing of a card that is only on private boards.
func TestCardActivityAnonymousPublicBoard(t *testing.T) {
	w := newPlaceWorld(t)
	w.call(t, "alice", "comment.create", map[string]any{"cardId": w.card, "text": "hello"})
	w.call(t, "alice", "card.move", map[string]any{"id": w.card, "listId": "colA2"})
	private := w.call(t, "alice", "card.create", map[string]any{"listId": "colA1", "name": "Secret"})
	_ = w.collect(t)
	x(t, w.root, `UPDATE boards SET is_public=1 WHERE id='brdB'`)
	anon := testutil.As(w.root, nil)
	acts, err := ops.Typed[[]cardextras.ActionItem](anon, "activity.card", cardextras.CardActionsIn{CardID: w.card})
	if err != nil {
		t.Fatal(err)
	}
	comments := 0
	for _, a := range acts {
		if a.BoardID != "brdB" {
			t.Fatalf("anonymous reader of the public board sees a row of %s: %+v", a.BoardID, a)
		}
		if a.Type == "commentCreate" {
			comments++
		}
	}
	if comments != 1 {
		t.Fatalf("anonymous reader sees the comment %d times, want once (Bravo's copy)", comments)
	}
	if _, err := ops.Typed[[]cardextras.ActionItem](anon, "activity.card", cardextras.CardActionsIn{CardID: private}); err == nil {
		t.Fatalf("anonymous reader got the activity of a card on a private board only")
	}
}

// list.moveCards into a closing column closes the cards it moves (closed is per card): a card on another board too
// gets one quiet card.updated isClosed on that board (boards.EmitClosedElsewhere) that names none of the moving
// board's columns, and the move itself stays on the moving board.
func TestFanoutMoveCardsClosesElsewhere(t *testing.T) {
	w := newPlaceWorld(t)
	w.call(t, "alice", "list.moveCards", map[string]any{"id": "colA1", "listId": "colA3"})
	s := w.collect(t)
	var bUpd []actRow
	for _, a := range s.actsOn("brdB") {
		switch a.typ {
		case "cardUpdate":
			bUpd = append(bUpd, a)
		case "cardMove":
			t.Fatalf("Alpha's column move recorded on Bravo: %+v", a)
		}
	}
	if len(bUpd) != 1 || !strings.Contains(bUpd[0].data, `"quiet":true`) || !strings.Contains(bUpd[0].envelope, `"isClosed":true`) ||
		strings.Contains(bUpd[0].data+bUpd[0].envelope, "colA") {
		t.Fatalf("Bravo's closed-state rows = %+v, want one quiet card.updated isClosed without Alpha's columns", bUpd)
	}
	var closed bool
	_ = w.root.DB.QueryRow(`SELECT is_closed FROM cards WHERE id=?`, w.card).Scan(&closed)
	if !closed {
		t.Fatal("the card is not closed")
	}
	if h := s.hubIn("board:brdB"); len(h) == 0 {
		t.Fatal("Bravo's room heard nothing")
	}
}
