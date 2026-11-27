package notify_test

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// watchWorld: board b1 "Launch" with columns Todo (l1), Review (l2) and a closed column Done (l3); board b2 "Other" (m1).
// alice acts; the others watch in different ways.
type watchWorld struct {
	root *ops.Env
	as   map[string]*ops.Env
}

func (w *watchWorld) do(t *testing.T, who, op string, args map[string]any) string {
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

func newWatchWorld(t *testing.T) *watchWorld {
	t.Helper()
	root := testutil.Env(t, nil)
	w := &watchWorld{root: root, as: map[string]*ops.Env{}}
	now := db.Now()
	for _, u := range []struct{ id, role string }{
		{"alice", "user"}, {"bob", "user"}, {"carol", "user"}, {"dave", "user"}, {"erin", "user"}, {"frank", "user"},
		{"gina", "user"}, {"hank", "user"}, {"ivy", "user"}, {"vic", "user"}, {"robo", "user"}, {"olga", "user"},
	} {
		name := strings.ToUpper(u.id[:1]) + u.id[1:]
		x(t, root, `INSERT INTO users(id,email,name,role,subscribe_own_cards,created_at) VALUES(?,?,?,?,1,?)`, u.id, u.id+"@x.io", name, u.role, now)
		w.as[u.id] = testutil.As(root, &ops.Principal{UserID: u.id, Role: u.role, Name: name})
	}
	x(t, root, `UPDATE users SET is_agent=1 WHERE id='robo'`)
	w.as["robo"].User.IsAgent = true
	x(t, root, `INSERT INTO projects(id,name,created_at) VALUES('p1','Apollo',?)`, now)
	x(t, root, `INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b1','p1',1,'Launch',?),('b2','p1',2,'Other',?)`, now, now)
	x(t, root, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('l1','b1','active',1,'Todo',?),('l2','b1','active',2,'Review',?),
		('l3','b1','closed',3,'Done',?),('m1','b2','active',1,'Inbox',?)`, now, now, now, now)
	for _, u := range []string{"alice", "bob", "carol", "dave", "erin", "gina", "hank", "ivy", "robo"} {
		x(t, root, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,?,?,?)`, db.NewID(), "b1", u, "editor", now)
	}
	x(t, root, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,?,?,?)`, db.NewID(), "b1", "vic", "viewer", now)
	for _, u := range []string{"alice", "olga"} {
		x(t, root, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,?,?,?)`, db.NewID(), "b2", u, "editor", now)
	}
	// watches made through the ops (a viewer may watch too)
	w.do(t, "carol", "list.subscribe", map[string]any{"id": "l1"})
	w.do(t, "dave", "board.subscribe", map[string]any{"id": "b1"})
	w.do(t, "erin", "list.subscribe", map[string]any{"id": "l1"})
	w.do(t, "erin", "board.subscribe", map[string]any{"id": "b1"})
	w.do(t, "hank", "list.subscribe", map[string]any{"id": "l2"})
	w.do(t, "ivy", "list.subscribe", map[string]any{"id": "l1"})
	w.do(t, "ivy", "list.subscribe", map[string]any{"id": "l2"})
	w.do(t, "vic", "board.subscribe", map[string]any{"id": "b1"})
	w.do(t, "olga", "board.subscribe", map[string]any{"id": "b2"})
	// stale watches: frank never had access, gina was deactivated afterwards
	x(t, root, `INSERT INTO board_subscriptions(board_id,user_id) VALUES('b1','frank'),('b1','gina')`)
	x(t, root, `UPDATE users SET is_deactivated=1 WHERE id='gina'`)
	return w
}

type got struct {
	types map[string][]string
	data  map[string]map[string]any
}

// take returns the notifications written since the last call (and clears them).
func (w *watchWorld) take(t *testing.T) got {
	t.Helper()
	notify.Wait()
	r, err := w.root.DB.Query(`SELECT user_id,type,data FROM notifications ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	g := got{types: map[string][]string{}, data: map[string]map[string]any{}}
	for r.Next() {
		var u, typ, data string
		_ = r.Scan(&u, &typ, &data)
		g.types[u] = append(g.types[u], typ)
		m := map[string]any{}
		_ = json.Unmarshal([]byte(data), &m)
		g.data[u] = m
	}
	r.Close()
	x(t, w.root, `DELETE FROM notifications`)
	return g
}

// expect checks exactly who heard what (each listed user exactly once) and the reason recorded for them.
func (g got) expect(t *testing.T, step string, want map[string]string) {
	t.Helper()
	var users []string
	for u := range g.types {
		users = append(users, u)
	}
	sort.Strings(users)
	for _, u := range users {
		if len(g.types[u]) != 1 {
			t.Errorf("%s: %s got %d notifications %v (want at most one per event)", step, u, len(g.types[u]), g.types[u])
		}
	}
	for u, w := range want {
		typ, via, _ := strings.Cut(w, "/")
		if len(g.types[u]) == 0 || g.types[u][0] != typ {
			t.Errorf("%s: %s got %v, want %s", step, u, g.types[u], typ)
			continue
		}
		if got := fmt.Sprint(g.data[u]["via"]); via != "" && got != via {
			if l, ok := g.data[u]["viaList"].(map[string]any); !ok || got+":"+fmt.Sprint(l["name"]) != via {
				t.Errorf("%s: %s via %s %v, want %s", step, u, got, g.data[u]["viaList"], via)
			}
		}
	}
	for _, u := range users {
		if _, ok := want[u]; !ok {
			t.Errorf("%s: %s should hear nothing, got %v", step, u, g.types[u])
		}
	}
}

func TestWatchScopesAndDedupe(t *testing.T) {
	w := newWatchWorld(t)

	// created in Todo: column and board watchers, most specific reason wins; alice (actor), frank (no access),
	// gina (deactivated) and olga (other board) hear nothing; vic (viewer) may watch and hears it
	card := w.do(t, "alice", "card.create", map[string]any{"listId": "l1", "name": "Fuel the rocket"})
	w.take(t).expect(t, "create", map[string]string{
		"carol": "createCard/list:Todo", "dave": "createCard/board", "erin": "createCard/list:Todo", "ivy": "createCard/list:Todo", "vic": "createCard/board",
	})
	var perm, muted int
	if err := w.root.DB.QueryRow(`SELECT is_permanent,muted FROM card_subscriptions WHERE card_id=? AND user_id='alice'`, card).Scan(&perm, &muted); err != nil || perm != 0 || muted != 0 {
		t.Fatalf("creating a card auto-watches it (not explicit): perm=%d muted=%d err=%v", perm, muted, err)
	}

	w.do(t, "bob", "card.subscribe", map[string]any{"id": card})
	w.do(t, "erin", "card.subscribe", map[string]any{"id": card})

	// a comment: the mention beats dave's board watch; the author hears nothing; everyone else exactly once
	w.do(t, "alice", "comment.create", map[string]any{"cardId": card, "text": "ready? @[Dave](dave) @[Frank](frank)"})
	w.take(t).expect(t, "comment", map[string]string{
		"dave": "mentionInComment", "bob": "commentCard/card", "carol": "commentCard/list:Todo", "erin": "commentCard/card",
		"ivy": "commentCard/list:Todo", "vic": "commentCard/board",
	})

	// a move: source (carol) and destination (hank) column watchers hear it once; ivy watches both and hears it
	// once, for the destination
	w.do(t, "alice", "card.move", map[string]any{"id": card, "listId": "l2"})
	w.take(t).expect(t, "move", map[string]string{
		"bob": "moveCard/card", "carol": "moveCard/list:Todo", "dave": "moveCard/board", "erin": "moveCard/card",
		"hank": "moveCard/list:Review", "ivy": "moveCard/list:Review", "vic": "moveCard/board",
	})

	// a reorder inside one column is silent
	w.do(t, "alice", "card.move", map[string]any{"id": card, "listId": "l2", "position": 1})
	w.take(t).expect(t, "reorder", map[string]string{})

	// erin mutes the card: nothing reaches her even though she watches its column and the board
	w.do(t, "erin", "card.unsubscribe", map[string]any{"id": card})
	// closing moves the card into the closed column: ONE closeCard, never also a moveCard
	w.do(t, "alice", "card.close", map[string]any{"id": card})
	w.take(t).expect(t, "close", map[string]string{
		"bob": "closeCard/card", "dave": "closeCard/board", "hank": "closeCard/list:Review", "ivy": "closeCard/list:Review", "vic": "closeCard/board",
	})
	w.do(t, "alice", "card.close", map[string]any{"id": card, "closed": false})
	w.take(t).expect(t, "reopen", map[string]string{
		"bob": "reopenCard/card", "dave": "reopenCard/board", "hank": "reopenCard/list:Review", "ivy": "reopenCard/list:Review", "vic": "reopenCard/board",
	})

	// due date set; the same date again and clearing it are silent; a title edit is silent
	w.do(t, "alice", "card.due.set", map[string]any{"id": card, "dueDate": "2030-03-04"})
	g := w.take(t)
	g.expect(t, "due", map[string]string{
		"bob": "setCardDueDate/card", "dave": "setCardDueDate/board", "hank": "setCardDueDate/list:Review", "ivy": "setCardDueDate/list:Review", "vic": "setCardDueDate/board",
	})
	w.do(t, "alice", "card.due.set", map[string]any{"id": card, "dueDate": "2030-03-04"})
	w.do(t, "alice", "card.update", map[string]any{"id": card, "name": "Fuel the big rocket"})
	w.do(t, "alice", "card.due.set", map[string]any{"id": card, "dueDate": ""})
	w.take(t).expect(t, "silent edits", map[string]string{})

	// one op touching two notifying fields still yields one notification per user
	w.do(t, "alice", "card.update", map[string]any{"id": card, "name": "Fuel", "dueDate": "2031-01-01"})
	w.take(t).expect(t, "update name+due", map[string]string{
		"bob": "setCardDueDate", "dave": "setCardDueDate", "hank": "setCardDueDate", "ivy": "setCardDueDate", "vic": "setCardDueDate",
	})

	// auto-watch never undoes a mute: erin comments (preference on by default) and stays muted
	w.do(t, "erin", "comment.create", map[string]any{"cardId": card, "text": "I muted this"})
	w.take(t)
	if err := w.root.DB.QueryRow(`SELECT muted FROM card_subscriptions WHERE card_id=? AND user_id='erin'`, card).Scan(&muted); err != nil || muted != 1 {
		t.Fatalf("commenting must not clear a mute: muted=%d err=%v", muted, err)
	}
	// ... watching again does
	w.do(t, "erin", "card.subscribe", map[string]any{"id": card})
	if err := w.root.DB.QueryRow(`SELECT muted,is_permanent FROM card_subscriptions WHERE card_id=? AND user_id='erin'`, card).Scan(&muted, &perm); err != nil || muted != 0 || perm != 1 {
		t.Fatalf("card.subscribe clears the mute and marks the watch explicit: muted=%d perm=%d", muted, perm)
	}

	// commenting watches the card when the preference is on (hank), not when it is off (carol)
	x(t, w.root, `UPDATE users SET subscribe_to_card_when_commenting=0 WHERE id='carol'`)
	w.do(t, "hank", "comment.create", map[string]any{"cardId": card, "text": "on it"})
	w.do(t, "carol", "comment.create", map[string]any{"cardId": card, "text": "me too"})
	w.take(t)
	var n int
	_ = w.root.DB.QueryRow(`SELECT COUNT(*) FROM card_subscriptions WHERE card_id=? AND user_id='hank' AND muted=0`, card).Scan(&n)
	if n != 1 {
		t.Fatal("hank should watch the card after commenting")
	}
	_ = w.root.DB.QueryRow(`SELECT COUNT(*) FROM card_subscriptions WHERE card_id=? AND user_id='carol'`, card).Scan(&n)
	if n != 0 {
		t.Fatal("carol turned 'Watch when I comment' off")
	}

	// agents never auto-watch, people with "Watch cards I create" off do not either
	ac := w.do(t, "robo", "card.create", map[string]any{"listId": "l1", "name": "Agent work"})
	x(t, w.root, `UPDATE users SET subscribe_own_cards=0 WHERE id='bob'`)
	bc := w.do(t, "bob", "card.create", map[string]any{"listId": "l1", "name": "Bob's card"})
	w.take(t)
	_ = w.root.DB.QueryRow(`SELECT COUNT(*) FROM card_subscriptions WHERE card_id IN (?,?)`, ac, bc).Scan(&n)
	if n != 0 {
		t.Fatalf("agent and opted-out creators must not auto-watch, %d rows", n)
	}

	// a cross-board move: source column watchers without access to the destination board hear nothing,
	// the destination board's watcher does (and never sees the source board's column in fromList)
	w.do(t, "alice", "card.move", map[string]any{"id": card, "listId": "m1"})
	xg := w.take(t)
	xg.expect(t, "cross-board", map[string]string{"olga": "moveCard/board"})
	if xg.data["olga"]["fromList"] != nil {
		t.Fatalf("cross-board move leaked source board column in fromList: %v", xg.data["olga"])
	}
	if xg.data["olga"]["toList"] == nil {
		t.Fatalf("cross-board move missing destination column in toList: %v", xg.data["olga"])
	}
}

func TestWatchReasonInMessages(t *testing.T) {
	w := newWatchWorld(t)
	w.do(t, "alice", "card.create", map[string]any{"listId": "l1", "name": "Fuel <the> *rocket*"})
	w.take(t)
	card := ""
	_ = w.root.DB.QueryRow(`SELECT id FROM cards WHERE board_id='b1'`).Scan(&card)
	w.do(t, "alice", "card.move", map[string]any{"id": card, "listId": "l2"})
	notify.Wait()
	l, err := ops.Typed[notify.ListOut](w.as["carol"], "notification.list", notify.ListIn{})
	if err != nil || len(l.Items) != 1 {
		t.Fatalf("carol: %+v %v", l, err)
	}
	n := l.Items[0]
	if n.Reason != "You get this because you watch the column Todo." || !strings.Contains(n.Summary, "Alice moved") || strings.Contains(n.Summary, "because") {
		t.Fatalf("summary=%q reason=%q", n.Summary, n.Reason)
	}
	_, md := notify.Message(n, "markdown")
	_, h := notify.Message(n, "html")
	_, txt := notify.Message(n, "text")
	if !strings.Contains(md, "_You get this because you watch the column Todo._") || !strings.Contains(md, `\*rocket\*`) {
		t.Errorf("markdown = %q", md)
	}
	if !strings.Contains(h, "<small>You get this because you watch the column Todo.</small>") || strings.Contains(h, "<the>") {
		t.Errorf("html = %q", h)
	}
	if !strings.HasSuffix(txt, "\n\nYou get this because you watch the column Todo.") {
		t.Errorf("text = %q", txt)
	}
	for _, c := range []struct {
		typ  string
		data map[string]any
		want string
	}{
		{"createCard", map[string]any{"list": map[string]any{"name": "Todo"}, "via": "board"}, "Weave created C in Todo on B\n\nYou get this because you watch the board B."},
		{"closeCard", map[string]any{"via": "card"}, "Weave closed C on B\n\nYou get this because you watch this card."},
		{"reopenCard", nil, "Weave re-opened C on B"},
		{"setCardDueDate", map[string]any{"dueDate": "2030-03-04T00:00:00Z"}, "Weave set the due date of C to 4 Mar 2030 on B"},
		{"setCardDueDate", map[string]any{"dueDate": "2030-03-04T15:30:00Z"}, "Weave set the due date of C to 4 Mar 2030 15:30 UTC on B"},
	} {
		// no creator: scheduler work (recurring cards) is attributed to Weave
		if _, b := notify.Message(notify.NotificationOut{Type: c.typ, CardName: "C", BoardName: "B", Data: c.data}, "text"); b != c.want {
			t.Errorf("%s: %q, want %q", c.typ, b, c.want)
		}
	}
}

// The watch state is per viewer: it never travels in event payloads (realtime, activity, webhooks).
func TestWatchStateNotInPayloads(t *testing.T) {
	w := newWatchWorld(t)
	card := w.do(t, "alice", "card.create", map[string]any{"listId": "l1", "name": "Mine"})
	w.do(t, "alice", "card.subscribe", map[string]any{"id": card})
	w.do(t, "alice", "card.update", map[string]any{"id": card, "name": "Still mine"})
	notify.Wait()
	r, err := w.root.DB.Query(`SELECT data||COALESCE(envelope,'') FROM actions`)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	rows := 0
	for r.Next() {
		var s string
		_ = r.Scan(&s)
		rows++
		if strings.Contains(s, `"isSubscribed":true`) || strings.Contains(s, `"watch"`) {
			t.Errorf("per-viewer watch state leaked into an event: %s", s)
		}
	}
	if rows == 0 {
		t.Fatal("no actions written")
	}
	// the caller still sees their own state
	out, _ := ops.CallMap(w.as["alice"], "card.get", map[string]any{"id": card})
	b, _ := json.Marshal(out)
	if !strings.Contains(string(b), `"isSubscribed":true`) {
		t.Fatalf("card.get should report the caller's watch: %s", b)
	}
	// watch toggles write no activity
	var n int
	_ = w.root.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE type LIKE '%ubscri%'`).Scan(&n)
	if n != 0 {
		t.Fatal("watch toggles must not emit events")
	}
}

type countingDriver struct {
	inner driver.Driver
	n     *atomic.Int64
}
type countingConn struct {
	driver.Conn
	n *atomic.Int64
}

func (d countingDriver) Open(name string) (driver.Conn, error) {
	c, err := d.inner.Open(name)
	return countingConn{c, d.n}, err
}
func (c countingConn) Prepare(q string) (driver.Stmt, error) { c.n.Add(1); return c.Conn.Prepare(q) }

var queries atomic.Int64

func init() {
	raw, _ := sql.Open("sqlite", ":memory:")
	sql.Register("sqlite-counting-notify", countingDriver{raw.Driver(), &queries})
	raw.Close()
}

// The planner costs the same number of queries for one watcher and for many.
func TestPlannerQueryCountIsConstant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	defer testutil.CheckPlacements(t, d) // before the Close
	e := testutil.Env(t, nil)
	e.DB = d
	now := db.Now()
	x(t, e, `INSERT INTO projects(id,name,created_at) VALUES('p1','P',?)`, now)
	x(t, e, `INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b1','p1',1,'B',?)`, now)
	x(t, e, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('l1','b1','active',1,'A',?),('l2','b1','active',2,'Z',?)`, now, now)
	x(t, e, `INSERT INTO cards(id,board_id,list_id,position,name,list_changed_at,created_at) VALUES('c1','b1','l2',1,'C',?,?)`, now, now)
	addWatchers := func(from, to int) {
		for i := from; i < to; i++ {
			u := fmt.Sprintf("u%02d", i)
			x(t, e, `INSERT INTO users(id,email,name,role,created_at) VALUES(?,?,?,'user',?)`, u, u+"@x.io", u, now)
			x(t, e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,?,?,?)`, db.NewID(), "b1", u, "viewer", now)
			switch i % 3 {
			case 0:
				x(t, e, `INSERT INTO card_subscriptions(card_id,user_id) VALUES('c1',?)`, u)
			case 1:
				x(t, e, `INSERT INTO list_subscriptions(list_id,user_id,created_at) VALUES('l1',?,?)`, u, now)
			default:
				x(t, e, `INSERT INTO board_subscriptions(board_id,user_id) VALUES('b1',?)`, u)
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
			{Type: "cardMove", BoardID: "b1", CardID: "c1", Data: map[string]any{"fromListId": "l1", "toListId": "l2"}},
			{Type: "commentCreate", BoardID: "b1", CardID: "c1", Data: map[string]any{"commentId": "m", "text": "hi @[U00](u00)"}},
		} {
			n += len(notify.PlanForTest(ce, ev, "nobody"))
		}
		return queries.Load(), n
	}
	addWatchers(0, 3)
	few, n1 := count()
	addWatchers(3, 40)
	many, n2 := count()
	// the move reaches every watcher (l1 is its source column); the comment skips the l1 watchers (the card is in l2)
	if n1 != 5 || n2 != 67 {
		t.Fatalf("planned %d and %d notifications, want 5 and 67", n1, n2)
	}
	if few != many || many > 10 {
		t.Fatalf("3 watchers = %d queries, 40 watchers = %d (want equal and small)", few, many)
	}
}

// Quiet events (Data["quiet"] == true) never notify, whatever rule would match: card, column and board watches, a
// mention, being added to a card. The column closing switch (list.update type, existingCards=flip) uses them for its
// per-card events, so watchers get nothing for the bulk flip while every flip stays in the activity log. An ordinary
// single move into a closing column still notifies as before.
func TestQuietEventsNeverNotify(t *testing.T) {
	w := newWatchWorld(t)
	card := w.do(t, "alice", "card.create", map[string]any{"listId": "l1", "name": "Ship it"})
	other := w.do(t, "alice", "card.create", map[string]any{"listId": "l1", "name": "Ship that"})
	w.do(t, "bob", "card.subscribe", map[string]any{"id": card})
	w.take(t)

	// the real switch: Todo starts closing cards and closes the two in it; carol, erin, ivy (column), dave, erin,
	// vic (board) and bob (card) watch, nobody hears anything
	w.do(t, "alice", "list.update", map[string]any{"id": "l1", "type": "closed"})
	w.take(t).expect(t, "closing switch", map[string]string{})
	var closed, logged int
	_ = w.root.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE id IN (?,?) AND is_closed=1`, card, other).Scan(&closed)
	_ = w.root.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE card_id IN (?,?) AND type='cardUpdate' AND data LIKE '%"quiet":true%' AND data LIKE '%"isClosed"%'`, card, other).Scan(&logged)
	if closed != 2 || logged != 2 {
		t.Fatalf("the switch closed %d cards and logged %d quiet card events, want 2 and 2", closed, logged)
	}
	w.do(t, "alice", "list.update", map[string]any{"id": "l1", "type": "active"})
	w.take(t).expect(t, "re-open switch", map[string]string{})

	// every rule is silenced by the flag, including a mention and being added to a card (synthetic events)
	quiet := []ops.Event{
		{Type: "commentCreate", BoardID: "b1", CardID: card, Data: map[string]any{"text": "look @[Dave](dave)", "commentId": "c-q", "quiet": true}},
		{Type: "cardMembershipCreate", BoardID: "b1", CardID: card, Data: map[string]any{"userId": "dave", "quiet": true}},
		{Type: "cardMove", BoardID: "b1", CardID: card, Data: map[string]any{"fromListId": "l2", "toListId": "l1", "quiet": true}},
		{Type: "cardUpdate", BoardID: "b1", CardID: card, Data: map[string]any{"changed": []string{"isClosed", "dueDate"}, "quiet": true}},
	}
	for _, ev := range quiet {
		if got := notify.PlanForTest(w.as["alice"], ev, "alice"); len(got) != 0 {
			t.Errorf("quiet %s planned %v", ev.Type, got)
		}
		loud := ev
		loud.Data = map[string]any{}
		for k, v := range ev.Data {
			if k != "quiet" {
				loud.Data[k] = v
			}
		}
		if got := notify.PlanForTest(w.as["alice"], loud, "alice"); len(got) == 0 {
			t.Errorf("control: the same %s without quiet notifies someone", ev.Type)
		}
		ops.Emit(w.as["alice"], ev)
	}
	w.take(t).expect(t, "quiet synthetic", map[string]string{})

	// control: a single move into the closing column is not quiet and notifies as before
	w.do(t, "alice", "list.update", map[string]any{"id": "l2", "type": "closed"})
	w.take(t)
	w.do(t, "alice", "card.move", map[string]any{"id": card, "listId": "l2"})
	w.take(t).expect(t, "single move into a closing column", map[string]string{
		"bob": "closeCard/card", "carol": "closeCard/list:Todo", "dave": "closeCard/board", "erin": "closeCard/list:Todo",
		"hank": "closeCard/list:Review", "ivy": "closeCard/list:Review", "vic": "closeCard/board",
	})
}

// Auto-archive (a column's archiveAfter) is quiet: neither the retroactive pass nor a landing that archives the
// column's oldest card notifies anyone about the archived cards (card, column or board watchers), while every archived
// card still gets its card.archived activity row. The card that lands notifies exactly as it would without the rule.
func TestAutoArchiveIsQuiet(t *testing.T) {
	w := newWatchWorld(t)
	a := w.do(t, "alice", "card.create", map[string]any{"listId": "l1", "name": "First"})
	b := w.do(t, "alice", "card.create", map[string]any{"listId": "l1", "name": "Second"})
	x(t, w.root, `UPDATE card_placements SET list_changed_at='2024-01-01T00:00:00Z' WHERE card_id=? AND is_home=1`, a)
	x(t, w.root, `UPDATE card_placements SET list_changed_at='2024-01-02T00:00:00Z' WHERE card_id=? AND is_home=1`, b)
	w.do(t, "bob", "card.subscribe", map[string]any{"id": a})
	w.do(t, "bob", "card.subscribe", map[string]any{"id": b})
	w.take(t)

	// the retroactive pass archives "First": nobody hears anything (bob watches it, carol/erin/ivy the column,
	// dave/erin/vic the board)
	w.do(t, "alice", "list.update", map[string]any{"id": "l1", "archiveAfter": 1, "confirmArchive": true})
	w.take(t).expect(t, "retroactive auto-archive", map[string]string{})
	// a new card lands: the column and board watchers hear about the new card only, bob (who watches the archived
	// "Second") hears nothing
	w.do(t, "alice", "card.create", map[string]any{"listId": "l1", "name": "Third"})
	g := w.take(t)
	if len(g.types["bob"]) != 0 {
		t.Errorf("bob was notified about the auto-archived card: %v", g.types["bob"])
	}
	for u, types := range g.types {
		for _, typ := range types {
			if typ != "createCard" {
				t.Errorf("%s got %s (only the new card may notify)", u, typ)
			}
		}
	}
	if len(g.types["carol"]) != 1 || len(g.types["dave"]) != 1 {
		t.Errorf("column/board watchers must still hear about the new card: %v", g.types)
	}
	var logged int
	_ = w.root.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE card_id IN (?,?) AND etype='card.archived' AND data LIKE '%"quiet":true%'`, a, b).Scan(&logged)
	if logged != 2 {
		t.Fatalf("activity rows for the auto-archived cards: %d, want 2", logged)
	}
}
