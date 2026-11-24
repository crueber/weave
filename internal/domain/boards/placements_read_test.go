package boards_test

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/domain/cardextras"
	"github.com/crueber/weave/internal/domain/cards"
	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// Phase 2 of "one card on several boards" (docs/DESIGN.md "Placements"): every board reader shows the cards PLACED
// on the board (each exactly once, with that board's column and position) and only that board's labels; a viewer of
// one board never learns the names or the number of the card's other, hidden placements.
//
// placedWorld: projects PA (project-wide labels on) with boards Alpha and AlphaTwo, and PB with board Beta. Card
// "c1" lives on Alpha (home, column "Alpha todo") and is placed (by SQL: no op places cards yet) on Beta ("Beta
// review") and AlphaTwo ("AlphaTwo col"): three placements. "c2" lives on Alpha only, "cb" on Beta only.
// Users: "both" edits Alpha, AlphaTwo and Beta; "bonly" views Beta only; "aonly" views Alpha only.
func placedWorld(t *testing.T) *ops.Env {
	t.Helper()
	e := testutil.Env(t, nil)
	placedWorldOn(t, e)
	return e
}

func placedWorldOn(t *testing.T, e *ops.Env) {
	t.Helper()
	x := func(q string, args ...any) {
		t.Helper()
		if _, err := e.DB.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	now := "2026-01-01T00:00:00Z"
	for _, u := range []string{"both", "bonly", "aonly", "outsider"} {
		x(`INSERT INTO users(id,email,name,role,created_at) VALUES(?,?,?,'boardUser',?)`, u, u+"@example.com", "User "+u, now)
	}
	x(`INSERT INTO projects(id,name,use_project_labels,created_at) VALUES('PA','Alpha project',1,?),('PB','Beta project',0,?)`, now, now)
	x(`INSERT INTO boards(id,project_id,position,name,created_at) VALUES('A','PA',1,'Alpha',?),('A2','PA',2,'AlphaTwo',?),('B','PB',1,'Beta',?)`, now, now, now)
	x(`INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES
		('la1','A','active',1,'Alpha todo',?),('la2','A','closed',2,'Alpha done',?),('la2a','A2','active',1,'AlphaTwo col',?),
		('lb1','B','active',1,'Beta first',?),('lb2','B','active',2,'Beta review',?)`, now, now, now, now, now)
	for i, m := range []struct{ user, board, role string }{
		{"both", "A", "editor"}, {"both", "A2", "editor"}, {"both", "B", "editor"}, {"bonly", "B", "viewer"}, {"aonly", "A", "viewer"},
	} {
		x(`INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,?,?,?)`, "bm"+string(rune('a'+i)), m.board, m.user, m.role, now)
	}
	x(`INSERT INTO cards(id,board_id,list_id,creator_user_id,position,name,description,due_date,lat,lng,location_name,list_changed_at,created_at) VALUES
		('c1','A','la1','both',1,'Shared card','zebra description','2026-03-10T00:00:00Z',52.5,13.4,'Somewhere',?,?),
		('c2','A','la1','both',2,'Solo card',NULL,NULL,NULL,NULL,NULL,?,?),
		('cb','B','lb1','both',1,'Native card',NULL,NULL,NULL,NULL,NULL,?,?)`, now, now, now, now, now, now)
	testutil.Place(t, e.DB, "c1", "B", "lb2", 5)
	testutil.Place(t, e.DB, "c1", "A2", "la2a", 1)
	x(`INSERT INTO labels(id,board_id,project_id,name,color,position,created_at) VALUES
		('labA','A',NULL,'alpha-label','berry-red',1,?),('labA2','A2',NULL,'alphatwo-label','pumpkin-orange',1,?),
		('labB','B',NULL,'beta-label','lagoon-blue',1,?),('labP',NULL,'PA','project-label','midnight-blue',2,?)`, now, now, now, now)
	x(`INSERT INTO card_labels(card_id,label_id) VALUES('c1','labA'),('c1','labA2'),('c1','labB'),('c1','labP')`)
	x(`INSERT INTO card_memberships(card_id,user_id) VALUES('c1','both'),('c1','bonly')`)
	x(`INSERT INTO card_subscriptions(card_id,user_id,is_permanent) VALUES('c1','bonly',1),('c1','aonly',1)`)
	x(`INSERT INTO comments(id,card_id,user_id,text,created_at) VALUES('m1','c1','both','zebra comment',?)`, now)
	x(`INSERT INTO attachments(id,card_id,creator_user_id,type,name,url,created_at) VALUES('at1','c1','both','link','spec link','https://example.com/spec',?)`, now)
	x(`INSERT INTO custom_field_groups(id,board_id,card_id,name,position,created_at) VALUES
		('gA','A',NULL,'Alpha fields',1,?),('gB','B',NULL,'Beta fields',1,?),('gc',NULL,'c1','Card fields',2,?)`, now, now, now)
}

func asUser(e *ops.Env, id string) *ops.Env {
	return testutil.As(e, &ops.Principal{UserID: id, Role: "boardUser", Name: "User " + id})
}

// callJSON runs an op by name and returns its output as JSON (what REST and the CLI see).
func callJSON(t *testing.T, e *ops.Env, op string, args map[string]any) string {
	t.Helper()
	v, err := ops.CallMap(e, op, args)
	if err != nil {
		t.Fatalf("%s %v: %v", op, args, err)
	}
	if s, ok := v.(*ops.Stream); ok {
		return string(s.Data)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// noAlpha fails when a Beta-only viewer's output names anything of the hidden Alpha boards or labels.
func noAlpha(t *testing.T, what, out string) {
	t.Helper()
	for _, s := range []string{"Alpha", "alpha", `"A"`, `"A2"`, `"la1"`, `"la2a"`, "labA", "labP", "project-label"} {
		if strings.Contains(out, s) {
			t.Errorf("%s leaks %q to a Beta-only viewer: %s", what, s, out)
		}
	}
}

func treeCards(t *testing.T, e *ops.Env, board string) map[string][]events.CardSnap {
	t.Helper()
	tr := call[boards.TreeOut](t, e, "board.tree", boards.TreeIn{ID: board, IncludeClosed: true})
	out := map[string][]events.CardSnap{}
	seen := map[string]int{}
	for _, l := range tr.Lists {
		for _, c := range l.Cards {
			seen[c.ID]++
			if c.BoardID != board || c.ListID != l.ID {
				t.Errorf("tree %s: card %s reports board %s list %s, in list %s", board, c.ID, c.BoardID, c.ListID, l.ID)
			}
			out[l.ID] = append(out[l.ID], c)
		}
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("tree %s: card %s appears %d times", board, id, n)
		}
	}
	return out
}

func labelNames(ls []events.Ref) []string {
	out := []string{}
	for _, l := range ls {
		out = append(out, l.Name)
	}
	return out
}

func TestPlacedCardOnEveryBoardTree(t *testing.T) {
	e := placedWorld(t)
	both := asUser(e, "both")
	for _, tc := range []struct {
		board, list string
		labels      []string
		cards       map[string]int // list -> number of cards
	}{
		{"A", "la1", []string{"alpha-label", "project-label"}, map[string]int{"la1": 2}},
		{"A2", "la2a", []string{"alphatwo-label", "project-label"}, map[string]int{"la2a": 1}},
		{"B", "lb2", []string{"beta-label"}, map[string]int{"lb1": 1, "lb2": 1}},
	} {
		got := treeCards(t, both, tc.board)
		for l, n := range tc.cards {
			if len(got[l]) != n {
				t.Errorf("tree %s list %s: %d cards, want %d", tc.board, l, len(got[l]), n)
			}
		}
		var c1 *events.CardSnap
		for i, c := range got[tc.list] {
			if c.ID == "c1" {
				c1 = &got[tc.list][i]
			}
		}
		if c1 == nil {
			t.Fatalf("tree %s: c1 not in %s", tc.board, tc.list)
		}
		if !reflect.DeepEqual(labelNames(c1.Labels), tc.labels) {
			t.Errorf("tree %s: c1 labels %v, want %v (board labels only)", tc.board, labelNames(c1.Labels), tc.labels)
		}
		if c1.AlsoOnBoards != 2 {
			t.Errorf("tree %s: c1 alsoOnBoards = %d, want 2 for a viewer of all three boards", tc.board, c1.AlsoOnBoards)
		}
	}
	// a Beta-only viewer: the card is there, on Beta's column, with Beta's labels, and no hidden board is counted
	bonly := asUser(e, "bonly")
	got := treeCards(t, bonly, "B")
	if len(got["lb2"]) != 1 || got["lb2"][0].AlsoOnBoards != 0 || !reflect.DeepEqual(labelNames(got["lb2"][0].Labels), []string{"beta-label"}) {
		t.Errorf("Beta-only tree: %+v", got["lb2"])
	}
	noAlpha(t, "board.tree", callJSON(t, bonly, "board.tree", map[string]any{"id": "B"}))
	// closed is per card: closing c1 hides it from every board's default tree
	if _, err := e.DB.Exec(`UPDATE cards SET is_closed=1 WHERE id='c1'`); err != nil {
		t.Fatal(err)
	}
	tr := call[boards.TreeOut](t, both, "board.tree", boards.TreeIn{ID: "B"})
	for _, l := range tr.Lists {
		for _, c := range l.Cards {
			if c.ID == "c1" {
				t.Errorf("closed c1 still in Beta's default tree")
			}
		}
	}
}

func TestPlacedCardInCardListCalendarMapStats(t *testing.T) {
	e := placedWorld(t)
	bonly := asUser(e, "bonly")
	// card.list: each card once, with Beta's placement and labels
	cl := call[cards.ListOut](t, bonly, "card.list", cards.ListCardsIn{BoardID: "B"})
	if len(cl.Items) != 2 || cl.Items[0].ID != "cb" || cl.Items[1].ID != "c1" {
		t.Fatalf("card.list B: %+v", cl.Items)
	}
	c1 := cl.Items[1]
	if c1.BoardID != "B" || c1.ListID != "lb2" || *c1.Position != 5 || !reflect.DeepEqual(c1.LabelIDs, []string{"labB"}) || c1.AlsoOnBoards != 0 {
		t.Errorf("card.list B c1 = board %s list %s pos %v labels %v also %d", c1.BoardID, c1.ListID, *c1.Position, c1.LabelIDs, c1.AlsoOnBoards)
	}
	noAlpha(t, "card.list", callJSON(t, bonly, "card.list", map[string]any{"boardId": "B"}))
	if got := call[cards.ListOut](t, bonly, "card.list", cards.ListCardsIn{BoardID: "B", ListID: "lb2"}); len(got.Items) != 1 || got.Items[0].ID != "c1" {
		t.Errorf("card.list B listId=lb2: %+v", got.Items)
	}
	// a label filter matches only labels shown on the board
	if got := call[cards.ListOut](t, bonly, "card.list", cards.ListCardsIn{BoardID: "B", LabelID: "labA"}); len(got.Items) != 0 {
		t.Errorf("card.list B with Alpha's label matched %d cards", len(got.Items))
	}
	if got := call[cards.ListOut](t, bonly, "card.list", cards.ListCardsIn{BoardID: "B", LabelID: "labB"}); len(got.Items) != 1 {
		t.Errorf("card.list B with Beta's label matched %d cards", len(got.Items))
	}
	both := asUser(e, "both")
	if got := call[cards.ListOut](t, both, "card.list", cards.ListCardsIn{BoardID: "A2"}); len(got.Items) != 1 || got.Items[0].ListID != "la2a" || got.Items[0].AlsoOnBoards != 2 ||
		!reflect.DeepEqual(got.Items[0].LabelIDs, []string{"labA2", "labP"}) {
		t.Errorf("card.list A2 as both: %+v", got.Items)
	}
	// calendar and map: Beta's column, Beta's label colours
	cal := call[cards.CalendarOut](t, bonly, "card.calendar", cards.CalendarIn{BoardID: "B", Month: "2026-03"})
	if len(cal.Cards) != 1 || cal.Cards[0].ID != "c1" || cal.Cards[0].ListID != "lb2" || !reflect.DeepEqual(cal.Cards[0].Colors, []string{"lagoon-blue"}) {
		t.Errorf("calendar B: %+v", cal.Cards)
	}
	if len(cal.Unscheduled) != 1 || cal.Unscheduled[0].ID != "cb" {
		t.Errorf("calendar B unscheduled: %+v", cal.Unscheduled)
	}
	mp := call[cards.MapOut](t, bonly, "card.map", cards.InboxIn{BoardID: "B"})
	if len(mp.Points) != 1 || mp.Points[0].ID != "c1" || mp.Points[0].ListID != "lb2" {
		t.Errorf("map B: %+v", mp.Points)
	}
	// statistics: the placed card counts on every board it is on, in that board's column
	st, err := boards.Stats(both, []string{"A", "A2", "B"})
	if err != nil {
		t.Fatal(err)
	}
	for b, want := range map[string]map[string]int{"A": {"la1": 2, "la2": 0}, "A2": {"la2a": 1}, "B": {"lb1": 1, "lb2": 1}} {
		got := map[string]int{}
		for _, c := range st[b].PerColumn {
			got[c.ListID] = c.Cards
		}
		if !reflect.DeepEqual(got, want) || st[b].Open != st[b].Cards() {
			t.Errorf("stats %s: per column %v (open %d), want %v", b, got, st[b].Open, want)
		}
	}
	// closed is per card: Done counts it on every board
	if _, err := e.DB.Exec(`UPDATE cards SET is_closed=1 WHERE id='c1'`); err != nil {
		t.Fatal(err)
	}
	st, _ = boards.Stats(both, []string{"A2", "B"})
	if st["A2"].Done != 1 || st["B"].Done != 1 || st["B"].Open != 1 {
		t.Errorf("stats after closing c1: A2 done %d, B done %d open %d", st["A2"].Done, st["B"].Done, st["B"].Open)
	}
}

func TestPlacedCardGetOptionsAndFields(t *testing.T) {
	e := placedWorld(t)
	bonly := asUser(e, "bonly")
	c := call[cards.Card](t, bonly, "card.get", cards.GetIn{ID: "c1"})
	if c.BoardID != "B" || c.ListID != "lb2" || c.HomeBoardID != "" || c.AlsoOnBoards != 0 || len(c.Placements) != 1 ||
		c.Placements[0].BoardID != "B" || c.Placements[0].ListName != "Beta review" || c.Placements[0].IsHome ||
		!reflect.DeepEqual(c.LabelIDs, []string{"labB"}) || c.ShareToken != nil {
		t.Errorf("card.get as Beta-only viewer: %+v", c)
	}
	noAlpha(t, "card.get", callJSON(t, bonly, "card.get", map[string]any{"id": "c1", "full": true}))
	// a hint for a hidden board is ignored (the context stays a viewable placement)
	if c := call[cards.Card](t, bonly, "card.get", cards.GetIn{ID: "c1", BoardID: "A"}); c.BoardID != "B" {
		t.Errorf("card.get boardId=A as Beta-only viewer: context %s", c.BoardID)
	}
	both := asUser(e, "both")
	c = call[cards.Card](t, both, "card.get", cards.GetIn{ID: "c1"})
	var pls []string
	for _, p := range c.Placements {
		pls = append(pls, p.BoardID+"/"+p.ListID)
	}
	if c.BoardID != "A" || c.HomeBoardID != "A" || c.AlsoOnBoards != 2 || !reflect.DeepEqual(pls, []string{"A/la1", "B/lb2", "A2/la2a"}) && !reflect.DeepEqual(pls, []string{"A/la1", "A2/la2a", "B/lb2"}) ||
		!c.Placements[0].IsHome || !reflect.DeepEqual(c.LabelIDs, []string{"labA", "labP"}) {
		t.Errorf("card.get as both: board %s home %s also %d placements %v labels %v", c.BoardID, c.HomeBoardID, c.AlsoOnBoards, pls, c.LabelIDs)
	}
	c = call[cards.Card](t, both, "card.get", cards.GetIn{ID: "c1", BoardID: "B"})
	if c.BoardID != "B" || c.ListID != "lb2" || c.HomeBoardID != "A" || !reflect.DeepEqual(c.LabelIDs, []string{"labB"}) {
		t.Errorf("card.get boardId=B as both: %+v", c)
	}
	aonly := asUser(e, "aonly")
	if c := call[cards.Card](t, aonly, "card.get", cards.GetIn{ID: "c1"}); c.BoardID != "A" || len(c.Placements) != 1 || c.AlsoOnBoards != 0 {
		t.Errorf("card.get as Alpha-only viewer: board %s placements %+v", c.BoardID, c.Placements)
	}
	if _, err := ops.CallMap(asUser(e, "outsider"), "card.get", map[string]any{"id": "c1"}); err == nil {
		t.Errorf("an outsider read c1")
	}
	// card.options: the context board's names and labels
	o := call[cards.Options](t, bonly, "card.options", cards.OptionsIn{ID: "c1"})
	if o.BoardName != "Beta" || o.ListName != "Beta review" || o.ProjectName != "Beta project" || len(o.CardLabels) != 1 || o.CardLabels[0].ID != "labB" {
		t.Errorf("card.options as Beta-only viewer: %+v", o)
	}
	noAlpha(t, "card.options", callJSON(t, bonly, "card.options", map[string]any{"id": "c1"}))
	if o := call[cards.Options](t, both, "card.options", cards.OptionsIn{ID: "c1", BoardID: "A2"}); o.BoardName != "AlphaTwo" || o.ListName != "AlphaTwo col" ||
		len(o.CardLabels) != 2 {
		t.Errorf("card.options boardId=A2 as both: %+v", o)
	}
	// custom fields: the context board's board-wide groups plus the card's own groups
	groups := func(e *ops.Env, board string) []string {
		gs := call[[]cardextras.GroupItem](t, e, "customfieldgroup.list", cardextras.GroupListIn{CardID: "c1", BoardID: board})
		var out []string
		for _, g := range gs {
			out = append(out, g.ID)
		}
		return out
	}
	if g := groups(bonly, ""); !reflect.DeepEqual(g, []string{"gB", "gc"}) {
		t.Errorf("field groups as Beta-only viewer: %v", g)
	}
	if g := groups(both, ""); !reflect.DeepEqual(g, []string{"gA", "gc"}) {
		t.Errorf("field groups as both (home context): %v", g)
	}
	if g := groups(both, "A2"); !reflect.DeepEqual(g, []string{"gc"}) {
		t.Errorf("field groups as both on AlphaTwo: %v", g)
	}
	// media view of Beta includes the placed card's attachment
	media := call[[]cardextras.AttachmentItem](t, bonly, "attachment.media", cardextras.MediaIn{BoardID: "B"})
	if len(media) != 1 || media[0].ID != "at1" {
		t.Errorf("Beta media: %+v", media)
	}
}

func TestPlacedCardExportsSearchDashboardWatching(t *testing.T) {
	e := placedWorld(t)
	bonly := asUser(e, "bonly")
	// HTML export of Beta: the placed card once, under Beta's column, with Beta's labels only
	html := callJSON(t, bonly, "export.board", map[string]any{"id": "B"})
	if strings.Count(html, "Shared card") != 1 || !strings.Contains(html, "Beta review") || !strings.Contains(html, "beta-label") {
		t.Errorf("export.board B: %s", html)
	}
	noAlpha(t, "export.board", html)
	if card := callJSON(t, bonly, "export.card", map[string]any{"id": "c1"}); !strings.Contains(card, "Beta") {
		t.Errorf("export.card as Beta-only viewer: %s", card)
	} else {
		noAlpha(t, "export.card", card)
	}
	// JSON board export: the cards on the board, their Beta column, and only Beta's labels
	both := asUser(e, "both")
	doc := callJSON(t, both, "board.export", map[string]any{"id": "B"})
	var ex struct {
		Labels []struct{ ID string } `json:"labels"`
		Cards  []struct {
			ID       string   `json:"id"`
			ListID   string   `json:"listId"`
			LabelIDs []string `json:"labelIds"`
		} `json:"cards"`
	}
	if err := json.Unmarshal([]byte(doc), &ex); err != nil {
		t.Fatalf("board.export: %v %s", err, doc)
	}
	if len(ex.Cards) != 2 || len(ex.Labels) != 1 || ex.Labels[0].ID != "labB" {
		t.Errorf("board.export B: %s", doc)
	}
	for _, c := range ex.Cards {
		if c.ID == "c1" && (c.ListID != "lb2" || !reflect.DeepEqual(c.LabelIDs, []string{"labB"})) {
			t.Errorf("board.export B c1: %+v", c)
		}
	}
	// search: one hit per card or comment, on a board the caller can view; the boardId filter finds placed cards
	hits := func(e *ops.Env, q, board string) []notify.SearchHit {
		return call[notify.SearchOut](t, e, "search.query", notify.SearchIn{Q: q, BoardID: board}).Items
	}
	for _, q := range []string{"Shared", "zebra"} {
		h := hits(bonly, q, "")
		if len(h) == 0 {
			t.Fatalf("search %q as Beta-only viewer: no hit", q)
		}
		for _, x := range h {
			if x.CardID != "c1" || x.BoardID != "B" || x.BoardName != "Beta" {
				t.Errorf("search %q as Beta-only viewer: %+v", q, x)
			}
		}
		if h := hits(both, q, "B"); len(h) == 0 || h[0].BoardID != "B" {
			t.Errorf("search %q boardId=B as both: %+v", q, h)
		}
		if h := hits(both, q, ""); len(h) == 0 || h[0].BoardID != "A" {
			t.Errorf("search %q as both (home context): %+v", q, h)
		}
		// the filter is no oracle: a Beta-only viewer filtering on a hidden board learns nothing
		if h := hits(bonly, q, "A"); len(h) != 0 {
			t.Errorf("search %q boardId=A as Beta-only viewer: %+v", q, h)
		}
		if h := hits(asUser(e, "outsider"), q, ""); len(h) != 0 {
			t.Errorf("search %q as outsider: %+v", q, h)
		}
	}
	noAlpha(t, "search.query", callJSON(t, bonly, "search.query", map[string]any{"q": "zebra"}))
	// dashboard: one row per card, on a viewable board
	d := call[notify.DashboardOut](t, bonly, "dashboard.get", notify.NoInput{})
	if len(d.MyCards) != 1 || d.MyCards[0].ID != "c1" || d.MyCards[0].BoardID != "B" || d.MyCards[0].List != "Beta review" {
		t.Errorf("dashboard as Beta-only viewer: %+v", d.MyCards)
	}
	if d := call[notify.DashboardOut](t, both, "dashboard.get", notify.NoInput{}); len(d.MyCards) != 1 || d.MyCards[0].BoardID != "A" {
		t.Errorf("dashboard as both: %+v", d.MyCards)
	}
	// Settings > Watching: one row per card, on the viewer's board
	w := call[notify.WatchListOut](t, bonly, "subscription.list", notify.WatchListIn{})
	if len(w.Cards) != 1 || w.Cards[0].BoardID != "B" || w.Cards[0].Context != "Beta / Beta review" || w.CardsTotal != 1 {
		t.Errorf("watching as Beta-only viewer: %+v", w.Cards)
	}
	noAlpha(t, "subscription.list", callJSON(t, bonly, "subscription.list", map[string]any{}))
	if w := call[notify.WatchListOut](t, asUser(e, "aonly"), "subscription.list", notify.WatchListIn{}); len(w.Cards) != 1 || w.Cards[0].BoardID != "A" {
		t.Errorf("watching as Alpha-only viewer: %+v", w.Cards)
	}
}

// The kanban body, the WIP counter, the list sheet count and the quick-edit tile: the placed card once per board,
// in that board's column, with that board's labels; a quick edit re-renders the tile of the board it was asked for.
func TestPlacedCardTilesAndCounts(t *testing.T) {
	e := placedWorld(t)
	if _, err := e.DB.Exec(`UPDATE lists SET wip_limit=1 WHERE id='lb2'`); err != nil {
		t.Fatal(err)
	}
	bonly := asUser(e, "bonly")
	code, body := get(t, mux(bonly), "/ui/boards/B/body", true)
	if code != 200 || strings.Count(body, `id="tile-c1"`) != 1 || strings.Count(body, `id="tile-cb"`) != 1 {
		t.Fatalf("Beta body %d: c1 tiles %d, cb tiles %d", code, strings.Count(body, `id="tile-c1"`), strings.Count(body, `id="tile-cb"`))
	}
	// c1's tile sits in Beta's second column, which counts it against its WIP limit
	col := body[strings.Index(body, `data-list="lb2"`):]
	if !strings.Contains(col, `id="tile-c1"`) || !strings.Contains(body, `data-total="1"`) {
		t.Errorf("c1 not in Beta review or not counted: %s", body)
	}
	if !strings.Contains(body, "beta-label") || strings.Contains(body, "alpha-label") || strings.Contains(body, "project-label") {
		t.Errorf("Beta body shows another board's labels: %s", body)
	}
	noAlpha(t, "board body", body)
	if code, sheet := get(t, mux(bonly), "/ui/lists/lb2", true); code != 200 || !strings.Contains(sheet, "1 card") {
		t.Errorf("list sheet lb2 %d: %s", code, sheet)
	}
	// Alpha's body, as a viewer of all three boards: Alpha's labels, Alpha's column
	both := asUser(e, "both")
	_, body = get(t, mux(both), "/ui/boards/A/body", true)
	if strings.Count(body, `id="tile-c1"`) != 1 || !strings.Contains(body, "alpha-label") || !strings.Contains(body, "project-label") || strings.Contains(body, "beta-label") {
		t.Errorf("Alpha body: %s", body)
	}
	// quick edit: the tile of the board named by ?board= (else the home)
	if code, tile := get(t, mux(both), "/ui/cards/c1/tile?board=B", true); code != 200 || !strings.Contains(tile, "beta-label") || strings.Contains(tile, "alpha-label") {
		t.Errorf("tile ?board=B %d: %s", code, tile)
	}
	if _, tile := get(t, mux(both), "/ui/cards/c1/tile", true); !strings.Contains(tile, "alpha-label") {
		t.Errorf("tile (home): %s", tile)
	}
	if _, tile := get(t, mux(bonly), "/ui/cards/c1/tile?board=A", true); !strings.Contains(tile, "beta-label") || strings.Contains(tile, "alpha-label") {
		t.Errorf("tile ?board=A as Beta-only viewer: %s", tile)
	}
}

// The board readers cost a constant number of queries whatever the number of cards and placements: 1 vs 12 cards,
// each on three boards (home Alpha, Beta, AlphaTwo), for a viewer of Beta only and a viewer of all three boards.
func TestPlacedReadsQueryCountIsConstant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testutil.CheckPlacements(t, d) })
	e := testutil.Env(t, nil)
	e.DB = d
	placedWorldOn(t, e)
	n := 0
	addCards := func(k int) {
		for i := 0; i < k; i++ {
			n++
			id := fmt.Sprintf("q%02d", n)
			if _, err := d.Exec(`INSERT INTO cards(id,board_id,list_id,creator_user_id,position,name,due_date,list_changed_at,created_at) VALUES(?,'A','la1','both',?,'quantity card','2026-03-11T00:00:00Z','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`, id, 10+n); err != nil {
				t.Fatal(err)
			}
			testutil.Place(t, d, id, "B", "lb2", float64(10+n))
			testutil.Place(t, d, id, "A2", "la2a", float64(10+n))
			_, _ = d.Exec(`INSERT INTO card_labels(card_id,label_id) VALUES(?,'labA'),(?,'labB'),(?,'labP')`, id, id, id)
			_, _ = d.Exec(`INSERT INTO card_memberships(card_id,user_id) VALUES(?,'both')`, id)
			_, _ = d.Exec(`INSERT INTO card_subscriptions(card_id,user_id,is_permanent) VALUES(?,'bonly',1)`, id)
		}
	}
	cdb, err := sql.Open("sqlite-counting", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	readers := []struct {
		name string
		run  func(e *ops.Env)
	}{
		{"board.tree", func(e *ops.Env) { call[boards.TreeOut](t, e, "board.tree", boards.TreeIn{ID: "B"}) }},
		{"board body (tiles)", func(e *ops.Env) {
			if code, _ := get(t, mux(e), "/ui/boards/B/body", true); code != 200 {
				t.Fatalf("body %d", code)
			}
		}},
		{"board.stats", func(e *ops.Env) {
			if _, err := boards.Stats(e, []string{"A", "A2", "B"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"card.map", func(e *ops.Env) { call[cards.MapOut](t, e, "card.map", cards.InboxIn{BoardID: "B"}) }},
		{"dashboard.get", func(e *ops.Env) { call[notify.DashboardOut](t, e, "dashboard.get", notify.NoInput{}) }},
		{"subscription.list", func(e *ops.Env) { call[notify.WatchListOut](t, e, "subscription.list", notify.WatchListIn{}) }},
		{"search.query", func(e *ops.Env) { call[notify.SearchOut](t, e, "search.query", notify.SearchIn{Q: "quantity"}) }},
		{"export.board", func(e *ops.Env) { callJSON(t, e, "export.board", map[string]any{"id": "B"}) }},
	}
	count := func() map[string]int64 {
		out := map[string]int64{}
		for _, u := range []string{"bonly", "both"} {
			ce := asUser(e, u)
			ce.DB = &db.DB{DB: cdb}
			for _, r := range readers {
				queries.Store(0)
				r.run(ce)
				out[u+" "+r.name] = queries.Load()
			}
		}
		return out
	}
	addCards(1)
	one := count()
	addCards(11)
	many := count()
	for k, v := range one {
		if many[k] != v {
			t.Errorf("%s: %d queries with 1 placed card, %d with 12 (want equal)", k, v, many[k])
		}
	}
	t.Logf("queries: %v", one)
	// and constant in the number of placements: put every card on three more boards the viewer of all can see
	// (card.options is left out: its movable-lists loop asks BoardLevel while its rows are open, which needs a second
	// connection that this single-connection counting pool does not have)
	readers = append(readers,
		struct {
			name string
			run  func(e *ops.Env)
		}{"card.get", func(e *ops.Env) { call[cards.Card](t, e, "card.get", cards.GetIn{ID: "c1"}) }},
		struct {
			name string
			run  func(e *ops.Env)
		}{"card.list", func(e *ops.Env) { call[cards.ListOut](t, e, "card.list", cards.ListCardsIn{BoardID: "B"}) }})
	before := count()
	for i := 0; i < 3; i++ {
		bid, lid := fmt.Sprintf("E%d", i), fmt.Sprintf("le%d", i)
		x := func(q string, args ...any) {
			if _, err := d.Exec(q, args...); err != nil {
				t.Fatalf("%s: %v", q, err)
			}
		}
		x(`INSERT INTO boards(id,project_id,position,name,created_at) VALUES(?,'PA',?,?,'2026-01-01T00:00:00Z')`, bid, 10+i, "Extra "+bid)
		x(`INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES(?,?,'active',1,'Extra col','2026-01-01T00:00:00Z')`, lid, bid)
		x(`INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,'both','editor','2026-01-01T00:00:00Z')`, "bmx"+bid, bid)
		rows, _ := d.Query(`SELECT id FROM cards`)
		var ids []string
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		for j, id := range ids {
			testutil.Place(t, d, id, bid, lid, float64(j+1))
		}
	}
	after := count()
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s: %d queries with 3 placements per card, %d with 6 (want equal)", k, v, after[k])
		}
	}
}

// Card writes answer with the card as seen on the caller's context board, never the hidden home placement.
func TestPlacedCardWriteOutputsStayOnContext(t *testing.T) {
	e := placedWorld(t)
	if _, err := e.DB.Exec(`UPDATE board_memberships SET role='editor' WHERE user_id='bonly'`); err != nil {
		t.Fatal(err)
	}
	bonly := asUser(e, "bonly")
	name, desc := "Shared card", "zebra description"
	for _, w := range []struct {
		op   string
		args map[string]any
	}{
		{"card.update", map[string]any{"id": "c1", "name": name, "description": desc}},
		{"card.label.add", map[string]any{"id": "c1", "labelId": "labB"}},
		{"card.member.add", map[string]any{"id": "c1", "userId": "bonly"}},
		{"card.member.remove", map[string]any{"id": "c1", "userId": "both"}},
		{"card.location.set", map[string]any{"id": "c1", "lat": 1.5, "lng": 2.5}},
		{"card.claim", map[string]any{"id": "c1"}},
		{"card.renew", map[string]any{"id": "c1"}},
		{"card.release", map[string]any{"id": "c1"}},
		{"card.subscribe", map[string]any{"id": "c1"}},
		{"card.unsubscribe", map[string]any{"id": "c1"}},
		{"card.due.set", map[string]any{"id": "c1", "dueDate": "2026-04-01"}},
	} {
		v, err := ops.CallMap(bonly, w.op, w.args)
		if err != nil {
			t.Errorf("%s: %v", w.op, err)
			continue
		}
		b, _ := json.Marshal(v)
		noAlpha(t, w.op, string(b))
	}
}
