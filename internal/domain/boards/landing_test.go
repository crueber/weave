package boards_test

import (
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
)

// The one rule for is_closed on landing (docs/DESIGN.md "Closed cards").
func TestClosedAfterLandingRule(t *testing.T) {
	for _, c := range []struct {
		typ       string
		was, want bool
	}{
		{"closed", false, true}, {"closed", true, true},
		{"active", false, false}, {"active", true, false},
		{"archive", false, false}, {"archive", true, true},
		{"trash", false, false}, {"trash", true, true},
	} {
		if got := boards.ClosedAfterLanding(c.typ, c.was); got != c.want {
			t.Errorf("%s (was %v): got %v want %v", c.typ, c.was, got, c.want)
		}
	}
}

func isClosed(t *testing.T, e *ops.Env, id string) bool {
	t.Helper()
	var v bool
	if err := e.DB.QueryRow(`SELECT is_closed FROM cards WHERE id=?`, id).Scan(&v); err != nil {
		t.Fatalf("card %s: %v", id, err)
	}
	return v
}

func setClosed(t *testing.T, e *ops.Env, id string, v bool) {
	t.Helper()
	if _, err := e.DB.Exec(`UPDATE cards SET is_closed=? WHERE id=?`, v, id); err != nil {
		t.Fatal(err)
	}
}

func listOfType(t *testing.T, e *ops.Env, boardID, typ string) string {
	t.Helper()
	id, err := boards.SystemList(e.Ctx, e.DB, boardID, typ)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func closedList(t *testing.T, e *ops.Env, boardID, name string) boards.List {
	t.Helper()
	return call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: boardID, Name: name, Type: "closed"})
}

// Landed itself: pairs across several columns, only differing rows are written and reported, drafts/templates and
// archive/trash are never touched.
func TestLandedHelper(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "L")
	todo, done := addList(t, e, b.ID, "Todo"), closedList(t, e, b.ID, "Done")
	arch := listOfType(t, e, b.ID, "archive")
	addCard(t, e, b.ID, todo.ID, "a", "A", 1, "") // open in active: unchanged
	addCard(t, e, b.ID, todo.ID, "b", "B", 2, "") // closed in active: re-opened
	addCard(t, e, b.ID, done.ID, "c", "C", 1, "") // open in closed: closed
	addCard(t, e, b.ID, done.ID, "d", "D", 2, "") // draft in closed: untouched
	addCard(t, e, b.ID, arch, "x", "X", 1, "")    // closed in archive: kept
	addCard(t, e, b.ID, arch, "y", "Y", 2, "")    // open in archive: kept
	setClosed(t, e, "b", true)
	setClosed(t, e, "x", true)
	if _, err := e.DB.Exec(`UPDATE cards SET is_draft=1 WHERE id='d'`); err != nil {
		t.Fatal(err)
	}
	var res boards.LandResult
	err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
		var err error
		res, err = boards.Landed(e.Ctx, tx, boards.Landing{CardID: "a", ListID: todo.ID}, boards.Landing{CardID: "b", ListID: todo.ID},
			boards.Landing{CardID: "c", ListID: done.ID}, boards.Landing{CardID: "d", ListID: done.ID},
			boards.Landing{CardID: "x", ListID: arch}, boards.Landing{CardID: "y", ListID: arch}, boards.Landing{CardID: "", ListID: todo.ID})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Closed, ",") != "c" || strings.Join(res.Reopened, ",") != "b" {
		t.Fatalf("result %+v", res)
	}
	for id, want := range map[string]bool{"a": false, "b": false, "c": true, "d": false, "x": true, "y": false} {
		if got := isClosed(t, e, id); got != want {
			t.Errorf("%s: closed=%v want %v", id, got, want)
		}
	}
}

// Bulk column moves (list.moveCards, archiving or deleting a column) and a column type switch go through the rule.
func TestLandingBulkListOps(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Bulk")
	todo, done := addList(t, e, b.ID, "Todo"), closedList(t, e, b.ID, "Done")
	addCard(t, e, b.ID, todo.ID, "t1", "T1", 1, "")
	addCard(t, e, b.ID, todo.ID, "t2", "T2", 2, "")
	// list.moveCards into a closed column closes every card
	call[boards.ListMoveCardsOut](t, e, "list.moveCards", boards.ListMoveCardsIn{ID: todo.ID, ListID: done.ID})
	if !isClosed(t, e, "t1") || !isClosed(t, e, "t2") {
		t.Fatal("moveCards into a closed column must close the cards")
	}
	// ...and out of it re-opens them
	call[boards.ListMoveCardsOut](t, e, "list.moveCards", boards.ListMoveCardsIn{ID: done.ID, ListID: todo.ID})
	if isClosed(t, e, "t1") || isClosed(t, e, "t2") {
		t.Fatal("moveCards into an active column must re-open the cards")
	}
	// archiving a closed column keeps its cards closed (they go to the archive with their check mark)
	call[boards.ListMoveCardsOut](t, e, "list.moveCards", boards.ListMoveCardsIn{ID: todo.ID, ListID: done.ID})
	call[boards.Deleted](t, e, "list.archive", boards.ListRefIn{ID: done.ID})
	arch := listOfType(t, e, b.ID, "archive")
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE list_id=? AND is_closed=1`, arch).Scan(&n)
	if n != 2 {
		t.Fatalf("archived closed cards: %d of 2 kept the closed state", n)
	}
	// moveCards out of the archive (a restore) applies the destination's rule
	call[boards.ListMoveCardsOut](t, e, "list.moveCards", boards.ListMoveCardsIn{ID: arch, ListID: todo.ID})
	if isClosed(t, e, "t1") {
		t.Fatal("restored into an active column: open")
	}
	// deleting a column trashes its cards and keeps their state
	done2 := closedList(t, e, b.ID, "Shipped")
	call[boards.ListMoveCardsOut](t, e, "list.moveCards", boards.ListMoveCardsIn{ID: todo.ID, ListID: done2.ID})
	call[boards.Deleted](t, e, "list.delete", boards.ListRefIn{ID: done2.ID})
	if !isClosed(t, e, "t1") || !isClosed(t, e, "t2") {
		t.Fatal("trashed closed cards keep the closed state")
	}
}

// Switching a column's type re-lands its cards: closed closes them, active re-opens them; quiet card events
// (changed isClosed, quiet: the planner sends no notifications) plus counts on the list event.
func TestListTypeSwitchRelands(t *testing.T) {
	e := fixture(t)
	var evs []ops.Event
	prev := ops.EmitHook
	ops.EmitHook = func(_ *ops.Env, ev ops.Event) { evs = append(evs, ev) }
	t.Cleanup(func() { ops.EmitHook = prev })
	b := newBoard(t, e, "Switch")
	l := addList(t, e, b.ID, "Review")
	addCard(t, e, b.ID, l.ID, "r1", "R1", 1, "")
	addCard(t, e, b.ID, l.ID, "r2", "R2", 2, "")
	addCard(t, e, b.ID, l.ID, "rd", "Draft", 3, "")
	if _, err := e.DB.Exec(`UPDATE cards SET is_draft=1 WHERE id='rd'`); err != nil {
		t.Fatal(err)
	}
	setClosed(t, e, "r2", true) // closed in place already: stays closed, no event
	evs = nil
	closed := "closed"
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Type: &closed})
	if !isClosed(t, e, "r1") || !isClosed(t, e, "r2") || isClosed(t, e, "rd") {
		t.Fatal("switching to closed closes the column's cards (not drafts)")
	}
	var cardEvents []string
	var listData map[string]any
	for _, ev := range evs {
		switch ev.Type {
		case "cardUpdate":
			cardEvents = append(cardEvents, ev.CardID)
			ch, _ := ev.Data["changed"].([]string)
			if len(ch) != 1 || ch[0] != "isClosed" || ev.Data["isClosed"] != true || ev.Data["auto"] != "listType" || ev.Data["quiet"] != true {
				t.Fatalf("card event must say isClosed changed and be quiet: %+v", ev.Data)
			}
		case "listUpdate":
			listData = ev.Data
		}
	}
	if strings.Join(cardEvents, ",") != "r1" || listData == nil || listData["closed"] != 1 {
		t.Fatalf("events: cards %v list %v", cardEvents, listData)
	}
	// a rename does not re-land anything
	evs = nil
	nm := "Reviewed"
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Name: &nm})
	for _, ev := range evs {
		if ev.Type == "cardUpdate" {
			t.Fatal("a rename must not touch cards")
		}
	}
	active := "active"
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Type: &active})
	if isClosed(t, e, "r1") || isClosed(t, e, "r2") {
		t.Fatal("switching back to active re-opens the cards")
	}
}

// Export carries isClosed; import honours it and closes everything that lands in a closed column; files without the
// field fall back to the column's rule; published templates drop the progress (but closed columns still close).
func TestClosedExportImport(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Src")
	todo, done := addList(t, e, b.ID, "Todo"), closedList(t, e, b.ID, "Done")
	arch := listOfType(t, e, b.ID, "archive")
	addCard(t, e, b.ID, todo.ID, "open", "Open", 1, "")
	addCard(t, e, b.ID, todo.ID, "inplace", "Closed in place", 2, "")
	addCard(t, e, b.ID, done.ID, "done", "Done one", 1, "")
	addCard(t, e, b.ID, arch, "archived", "Archived closed", 1, "")
	addCard(t, e, b.ID, arch, "archopen", "Archived open", 2, "")
	for _, id := range []string{"inplace", "done", "archived"} {
		setClosed(t, e, id, true)
	}
	st := call[*ops.Stream](t, e, "board.export", boards.ExportIn{ID: b.ID})
	var doc struct {
		Cards []map[string]any `json:"cards"`
	}
	if err := json.Unmarshal(st.Data, &doc); err != nil {
		t.Fatal(err)
	}
	flags := map[string]bool{}
	for _, c := range doc.Cards {
		v, _ := c["isClosed"].(bool)
		flags[c["name"].(string)] = v
	}
	if !flags["Closed in place"] || !flags["Done one"] || !flags["Archived closed"] || flags["Open"] || flags["Archived open"] {
		t.Fatalf("export isClosed: %v", flags)
	}
	byName := func(boardID string) map[string]bool {
		rows, err := e.DB.Query(`SELECT name,is_closed FROM cards WHERE board_id=?`, boardID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var n string
			var c bool
			_ = rows.Scan(&n, &c)
			out[n] = c
		}
		return out
	}
	imp := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: string(st.Data), Name: "Copy"})
	got := byName(imp.ID)
	for n, want := range flags {
		if got[n] != want {
			t.Errorf("round trip %q: closed=%v want %v", n, got[n], want)
		}
	}
	// an old file (no isClosed): cards in a closed column arrive closed, others open
	old := strings.ReplaceAll(string(st.Data), `"isClosed": true,`, "")
	if strings.Contains(old, "isClosed") {
		t.Fatal("test file still mentions isClosed")
	}
	got = byName(call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: old, Name: "Old"}).ID)
	if !got["Done one"] || got["Closed in place"] || got["Archived closed"] || got["Open"] {
		t.Fatalf("old file: %v", got)
	}
	// a published template: progress does not travel, but a sample card in a closed column is closed
	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: b.ID, IsTemplate: true})
	inst := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p2", TemplateID: b.ID, Name: "From template"})
	got = byName(inst.ID)
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if !got["Done one"] || got["Closed in place"] {
		t.Fatalf("instantiated: %v (%v)", got, names)
	}
}

// The type switch is quiet against the real notification planner: a board watcher gets nothing for the bulk flip
// (while a bulk move into a closed column still notifies, as before), and every flip is in the activity log.
func TestListTypeSwitchQuietForWatchers(t *testing.T) {
	e := fixture(t)
	if ops.EmitHook == nil {
		t.Skip("notify domain not linked")
	}
	b := newBoard(t, e, "Watched")
	l, other := addList(t, e, b.ID, "Review"), addList(t, e, b.ID, "Todo")
	addCard(t, e, b.ID, l.ID, "w1", "W1", 1, "")
	addCard(t, e, b.ID, l.ID, "w2", "W2", 2, "")
	addCard(t, e, b.ID, other.ID, "w3", "W3", 1, "")
	if _, err := e.DB.Exec(`INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('m-w',?,?,'editor','2024-01-01T00:00:00Z')`, b.ID, editor.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO board_subscriptions(board_id,user_id) VALUES(?,?)`, b.ID, editor.UserID); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...any) int {
		var n int
		if err := e.DB.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	closed := "closed"
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Type: &closed})
	if n := count(`SELECT COUNT(*) FROM notifications WHERE user_id=?`, editor.UserID); n != 0 {
		t.Fatalf("the bulk type switch notified the board watcher %d times", n)
	}
	if n := count(`SELECT COUNT(*) FROM actions WHERE card_id IN ('w1','w2')`); n < 2 {
		t.Fatalf("each flipped card must still be in the activity log (got %d rows)", n)
	}
	// control: the watch works (moving a card into the closed column notifies, unchanged behaviour)
	call[boards.ListMoveCardsOut](t, e, "list.moveCards", boards.ListMoveCardsIn{ID: other.ID, ListID: l.ID})
	if n := count(`SELECT COUNT(*) FROM notifications WHERE user_id=? AND type='closeCard'`, editor.UserID); n != 1 {
		t.Fatalf("control: closeCard notifications for a move = %d, want 1", n)
	}
}
