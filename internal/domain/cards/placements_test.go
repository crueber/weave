package cards

import (
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/testutil"
)

// Positions live on card_placements (migration 0404); the cards columns mirror the home placement.

func columnOrder(t *testing.T, f *fx, list string) string {
	t.Helper()
	rows, err := f.e.DB.Query(`SELECT c.name FROM card_placements p JOIN cards c ON c.id=p.card_id WHERE p.list_id=? AND c.is_draft=0 ORDER BY p.position, p.card_id`, list)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		out = append(out, n)
	}
	return strings.Join(out, ",")
}

// A drop between two cards whose positions are closer than 1 rebalances the column's placements and lands the card
// between them (the generic boards.PlaceBetween used to rebalance cards rows directly, which the guard now refuses).
func TestDropIntoCollapsedGapRebalancesPlacements(t *testing.T) {
	f := seed(t)
	a, b := f.card(t, f.doing, "a"), f.card(t, f.doing, "b")
	x := f.card(t, f.todo, "x")
	mustExec(t, f.e, `UPDATE card_placements SET position=1 WHERE card_id=? AND is_home=1`, a.ID)
	mustExec(t, f.e, `UPDATE card_placements SET position=1.5 WHERE card_id=? AND is_home=1`, b.ID)
	w := do(mount(f, f.e), "POST", "/ui/boards/"+f.board+"/cards/drop", url.Values{"id": {x.ID}, "container": {f.doing}, "afterId": {a.ID}, "beforeId": {b.ID}})
	if w.Code >= 400 {
		t.Fatalf("drop %d %s", w.Code, w.Body.String())
	}
	if got := columnOrder(t, f, f.doing); got != "a,x,b" {
		t.Fatalf("order after the drop = %s, want a,x,b", got)
	}
	// the same through card.move with a sibling (positionFor's rebalance)
	y := f.card(t, f.todo, "y")
	mustExec(t, f.e, `UPDATE card_placements SET position=1 WHERE card_id=? AND is_home=1`, a.ID)
	mustExec(t, f.e, `UPDATE card_placements SET position=1.25 WHERE card_id=? AND is_home=1`, x.ID)
	call[Card](t, f.e, "card.move", MoveIn{ID: y.ID, ListID: f.doing, AfterID: a.ID})
	if got := columnOrder(t, f, f.doing); got != "a,y,x,b" {
		t.Fatalf("order after card.move = %s, want a,y,x,b", got)
	}
	// quick add at the top of a column whose first gap collapsed (boards' quick-add route, real card.create)
	mustExec(t, f.e, `UPDATE card_placements SET position=0.5 WHERE card_id=? AND is_home=1`, a.ID)
	w = do(mount(f, f.e), "POST", "/ui/lists/"+f.doing+"/cards/quick", url.Values{"name": {"t1\nt2"}, "top": {"1"}})
	if w.Code >= 400 {
		t.Fatalf("quick add %d %s", w.Code, w.Body.String())
	}
	if got := columnOrder(t, f, f.doing); got != "t1,t2,a,y,x,b" {
		t.Fatalf("order after quick add at the top = %s, want t1,t2,a,y,x,b", got)
	}
	testutil.CheckPlacements(t, f.e.DB)
}

// Reorders, column moves on one board, list.sort, list.moveCards and rebalances write placements only: the search
// index is never rebuilt for them (notify_card_au fires on UPDATE OF board_id, which the mirror never names on one
// board). A move to another board reindexes the card once.
func TestPlacementWritesDoNotChurnSearch(t *testing.T) {
	f := seed(t)
	a, b, c := f.card(t, f.todo, "Charlie"), f.card(t, f.todo, "Alpha"), f.card(t, f.todo, "Bravo")
	mustExec(t, f.e, `CREATE TABLE fts_churn(ref_id TEXT)`)
	mustExec(t, f.e, `CREATE TRIGGER fts_churn_log AFTER DELETE ON search_map BEGIN INSERT INTO fts_churn VALUES(OLD.ref_id); END`)
	rids := func() string {
		rows, err := f.e.DB.Query(`SELECT ref_id||':'||rid FROM search_map ORDER BY ref_id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			out = append(out, s)
		}
		return strings.Join(out, ",")
	}
	before := rids()
	call[Card](t, f.e, "card.move", MoveIn{ID: a.ID, ListID: f.todo, BeforeID: b.ID}) // reorder
	call[Card](t, f.e, "card.move", MoveIn{ID: b.ID, ListID: f.doing})                // column change
	call[Card](t, f.e, "card.move", MoveIn{ID: b.ID, ListID: f.todo, AfterID: c.ID})
	call[boards.List](t, f.e, "list.sort", boards.ListSortIn{ID: f.todo, Field: "name", Order: "asc"})
	if got := columnOrder(t, f, f.todo); got != "Alpha,Bravo,Charlie" {
		t.Fatalf("list.sort order = %s", got)
	}
	call[boards.ListMoveCardsOut](t, f.e, "list.moveCards", boards.ListMoveCardsIn{ID: f.todo, ListID: f.doing})
	mustExec(t, f.e, `UPDATE card_placements SET position=1 WHERE card_id=?`, a.ID)
	mustExec(t, f.e, `UPDATE card_placements SET position=1.5 WHERE card_id=?`, b.ID)
	call[Card](t, f.e, "card.move", MoveIn{ID: c.ID, ListID: f.doing, AfterID: a.ID, BeforeID: b.ID}) // rebalance
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: c.ID})
	call[Card](t, f.e, "card.restore", IDIn{ID: c.ID})
	var n int
	_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM fts_churn`).Scan(&n)
	if n != 0 || rids() != before {
		t.Fatalf("same-board placement writes reindexed the search index %d times (rowids %s -> %s)", n, before, rids())
	}
	call[Card](t, f.e, "card.move", MoveIn{ID: c.ID, ListID: f.list2})
	_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM fts_churn WHERE ref_id=?`, c.ID).Scan(&n)
	var board string
	_ = f.e.DB.QueryRow(`SELECT i.board_id FROM search_idx i JOIN search_map m ON m.rid=i.rowid WHERE m.kind='card' AND m.ref_id=?`, c.ID).Scan(&board)
	if n != 1 || board != f.board2 {
		t.Fatalf("a cross-board move should reindex the card once under its new board (%d, %q)", n, board)
	}
}
