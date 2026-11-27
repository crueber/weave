package boards_test

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
)

func TestMoveCardsSkipsDraftsAndTemplatesAndRetireReparks(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Retire")
	l1 := addList(t, e, b.ID, "Col 1")
	l2 := addList(t, e, b.ID, "Col 2")
	addCard(t, e, b.ID, l1.ID, "c1", "Real card", 100, "")
	if _, err := e.DB.Exec(`INSERT INTO cards(id,board_id,list_id,position,name,is_draft,list_changed_at,created_at) VALUES('d1',?,?,200,'Draft',1,'2024-01-01T00:00:00Z','2024-01-01T00:00:00Z')`, b.ID, l1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(`INSERT INTO cards(id,board_id,list_id,position,name,is_template,list_changed_at,created_at) VALUES('t1',?,?,300,'Card Tpl',1,'2024-01-01T00:00:00Z','2024-01-01T00:00:00Z')`, b.ID, l1.ID); err != nil {
		t.Fatal(err)
	}

	out := call[boards.ListMoveCardsOut](t, e, "list.moveCards", boards.ListMoveCardsIn{ID: l1.ID, ListID: l2.ID})
	if out.Moved != 1 {
		t.Fatalf("list.moveCards moved %d, want 1 (must skip draft and template)", out.Moved)
	}
	var c1List, d1List, t1List string
	_ = e.DB.QueryRow(`SELECT list_id FROM card_placements WHERE card_id='c1'`).Scan(&c1List)
	_ = e.DB.QueryRow(`SELECT list_id FROM card_placements WHERE card_id='d1'`).Scan(&d1List)
	_ = e.DB.QueryRow(`SELECT list_id FROM card_placements WHERE card_id='t1'`).Scan(&t1List)
	if c1List != l2.ID || d1List != l1.ID || t1List != l1.ID {
		t.Fatalf("after moveCards: c1=%s d1=%s t1=%s", c1List, d1List, t1List)
	}

	// Add another real card in l1, then delete l1: c2 goes to trash, d1 and t1 are re-parked on l2 (not cascade-deleted).
	addCard(t, e, b.ID, l1.ID, "c2", "Real 2", 400, "")
	call[boards.Deleted](t, e, "list.delete", boards.ListRefIn{ID: l1.ID})

	var c2Type string
	if err := e.DB.QueryRow(`SELECT l.type FROM card_placements p JOIN lists l ON l.id=p.list_id WHERE p.card_id='c2'`).Scan(&c2Type); err != nil || c2Type != "trash" {
		t.Fatalf("c2 list type=%q err=%v, want trash", c2Type, err)
	}
	if err := e.DB.QueryRow(`SELECT list_id FROM card_placements WHERE card_id='d1'`).Scan(&d1List); err != nil || d1List != l2.ID {
		t.Fatalf("draft d1 after list.delete: list=%q err=%v, want %s", d1List, err, l2.ID)
	}
	if err := e.DB.QueryRow(`SELECT list_id FROM card_placements WHERE card_id='t1'`).Scan(&t1List); err != nil || t1List != l2.ID {
		t.Fatalf("template t1 after list.delete: list=%q err=%v, want %s", t1List, err, l2.ID)
	}
}

func TestLabelUpdateAtomicValidationAndGroupNameMaxLen(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Labels")
	lab := call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "orig", Color: "berry-red"})

	// Invalid color must not commit the name change.
	mustFail(t, e, "label.update", boards.LabelUpdateIn{ID: lab.ID, Name: sp("changed"), Color: sp("not-a-color")}, "validation")
	// Missing group must not commit the name or color change.
	mustFail(t, e, "label.update", boards.LabelUpdateIn{ID: lab.ID, Name: sp("changed"), Color: sp("lagoon-blue"), GroupID: sp("no-such-group")}, "not_found")

	var name, color string
	if err := e.DB.QueryRow(`SELECT name,color FROM labels WHERE id=?`, lab.ID).Scan(&name, &color); err != nil {
		t.Fatal(err)
	}
	if name != "orig" || color != "berry-red" {
		t.Fatalf("partial commit occurred: name=%q color=%q", name, color)
	}

	// Label group max name length enforced on create and update.
	tooLong := strings.Repeat("a", ops.MaxNameLen+1)
	mustFail(t, e, "labelgroup.create", boards.GroupCreateIn{BoardID: b.ID, Name: tooLong}, "validation")
	grp := call[boards.LabelGroup](t, e, "labelgroup.create", boards.GroupCreateIn{BoardID: b.ID, Name: "ok"})
	mustFail(t, e, "labelgroup.update", boards.GroupUpdateIn{ID: grp.ID, Name: &tooLong}, "validation")
}

func TestImportValidationAndClosedCappedColumn(t *testing.T) {
	e := fixture(t)
	tooLong := strings.Repeat("b", ops.MaxNameLen+1)
	doc := `{"format":"weave-board","version":1,"board":{"name":"ok"},"lists":[{"id":"l1","name":"Done","type":"closed","archiveAfter":2,"position":1}],"cards":[
		{"id":"c1","listId":"l1","name":"One","position":1},
		{"id":"c2","listId":"l1","name":"Two","position":2},
		{"id":"c3","listId":"l1","name":"Three","position":3}
	]}`
	mustFail(t, e, "board.import", &boards.ImportIn{ProjectID: "p1", Format: "planka", Name: tooLong, Data: doc}, "validation")

	b := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p1", Format: "planka", Data: doc})
	var inDone, inArch, closedCount int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM card_placements p JOIN lists l ON l.id=p.list_id WHERE p.board_id=? AND l.type='closed'`, b.ID).Scan(&inDone)
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM card_placements p JOIN lists l ON l.id=p.list_id WHERE p.board_id=? AND l.type='archive'`, b.ID).Scan(&inArch)
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE board_id=? AND is_closed=1`, b.ID).Scan(&closedCount)
	if inDone != 2 || inArch != 1 || closedCount != 3 {
		t.Fatalf("closed+capped import: inDone=%d inArch=%d closed=%d (want 2, 1, 3)", inDone, inArch, closedCount)
	}
}

func TestApplyTemplateNegativeCardPositionStaysAfterMoved(t *testing.T) {
	e := fixture(t)
	tplBoard := newBoard(t, e, "NegPosTpl")
	tl := addList(t, e, tplBoard.ID, "Inbox")
	addCard(t, e, tplBoard.ID, tl.ID, "tc1", "Sample neg", -65536, "")
	addCard(t, e, tplBoard.ID, tl.ID, "tc2", "Sample zero-ish", -100, "")
	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: tplBoard.ID, IsTemplate: true, Visibility: "public"})

	dst := newBoard(t, e, "Target")
	dl := addList(t, e, dst.ID, "Old")
	addCard(t, e, dst.ID, dl.ID, "ec1", "Existing", 1, "")

	out := call[boards.TemplateApplyOut](t, e, "board.template.apply", boards.TemplateApplyIn{
		ID: dst.ID, TemplateID: tplBoard.ID, Confirm: true, IncludeSampleCards: true,
	})
	names := cardNames(t, e, out.TargetColumn.ID)
	want := "Existing,Sample neg,Sample zero-ish"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("card order after apply = %q, want %q", got, want)
	}
}

func TestDeleteProjectBoardsCallsHookInTx(t *testing.T) {
	e := fixture(t)
	b1 := newBoard(t, e, "B1")
	b2 := newBoard(t, e, "B2")

	var seenBoards []string
	var ranAfter bool
	orig := boards.BeforeBoardsDelete
	t.Cleanup(func() { boards.BeforeBoardsDelete = orig })
	boards.BeforeBoardsDelete = func(_ *ops.Env, _ *sql.Tx, boardIDs []string) (func(), error) {
		seenBoards = append([]string{}, boardIDs...)
		return func() { ranAfter = true }, nil
	}
	after, err := boards.DeleteProjectBoards(e, "p1", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(e.Ctx, `DELETE FROM projects WHERE id='p1'`)
		return err
	})
	if err != nil {
		t.Fatalf("DeleteProjectBoards: %v", err)
	}
	after()
	if !ranAfter {
		t.Fatal("after hook did not run")
	}
	found := map[string]bool{}
	for _, id := range seenBoards {
		found[id] = true
	}
	if !found[b1.ID] || !found[b2.ID] {
		t.Fatalf("hook saw %v, expected %s and %s", seenBoards, b1.ID, b2.ID)
	}
}

func TestGalleryApplyModeHidesPublishedTabWhenOnlySelfPublished(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "OnlySelfTpl")
	addList(t, e, b.ID, "Todo")
	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: b.ID, IsTemplate: true, Visibility: "public"})

	m := mux(e)
	code, html := get(t, m, "/ui/boards/"+b.ID+"/apply-template", true)
	if code != http.StatusOK {
		t.Fatalf("apply-template: %d", code)
	}
	if strings.Contains(html, `data-cat="published"`) || strings.Contains(html, `value="published"`) {
		t.Fatalf("Published tab should be hidden when only self is published:\n%s", html)
	}
}

func TestPlaceBetweenReversedSiblings(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Pos")
	l1 := addList(t, e, b.ID, "First")
	l2 := addList(t, e, b.ID, "Second")
	addCard(t, e, b.ID, l1.ID, "c1", "A", 100, "")
	addCard(t, e, b.ID, l1.ID, "c2", "B", 200, "")

	// Reversed siblings: after=c2 (200), before=c1 (100) -> appends past end without rebalancing c1/c2.
	pos, err := boards.PlaceCardBetween(e.Ctx, e.DB, l1.ID, "c3", "c2", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if pos <= 200 {
		t.Fatalf("reversed PlaceCardBetween = %v, want > 200", pos)
	}
	var p1, p2 float64
	_ = e.DB.QueryRow(`SELECT position FROM card_placements WHERE card_id='c1'`).Scan(&p1)
	_ = e.DB.QueryRow(`SELECT position FROM card_placements WHERE card_id='c2'`).Scan(&p2)
	if p1 != 100 || p2 != 200 {
		t.Fatalf("reversed siblings should not trigger rebalance: p1=%v p2=%v", p1, p2)
	}

	// Same for PlaceBetween on lists.
	lp := boards.PlaceBetween(e.Ctx, e.DB, "lists", "board_id=? AND type IN ('active','closed')", []any{b.ID}, l2.ID, l1.ID)
	if lp <= *l2.Position {
		t.Fatalf("reversed PlaceBetween = %v, want > %v", lp, *l2.Position)
	}
}
