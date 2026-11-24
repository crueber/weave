package cards

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// One card on several boards: the write ops (card.place, card.unplace, card.placements) and the changed semantics
// of move, archive/restore/trash/delete, labels, members, claims, card.next, duplicate, recurrence and the board,
// project and column deletions (docs/DESIGN.md "Placements"). testutil.CheckPlacements runs after every test.

// mb extends the cards fixture with board b3 in a second project p2 (cross-project placements) and a second column
// on b2.
type mb struct {
	*fx
	b3Todo, b3Doing, b3Closed, b3Arch, b3Trash string
	b2b                                        string
}

func seedMB(t *testing.T) *mb {
	t.Helper()
	f := seed(t)
	m := &mb{fx: f}
	now := db.Now()
	mustExec(t, f.e, `INSERT INTO projects(id,name,created_at) VALUES('p2','Second',?)`, now)
	mustExec(t, f.e, `INSERT INTO boards(id,project_id,position,name,created_at) VALUES('b3','p2',1,'b3',?)`, now)
	mustExec(t, f.e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('m-ed-b3','b3',?,'editor',?)`, f.ed, now)
	mk := func(id, board, typ string, pos float64) string {
		mustExec(t, f.e, `INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES(?,?,?,?,?,?)`, id, board, typ, pos, id, now)
		return id
	}
	m.b3Todo, m.b3Doing, m.b3Closed = mk("l3-todo", "b3", "active", 1), mk("l3-doing", "b3", "active", 2), mk("l3-done", "b3", "closed", 3)
	m.b3Arch, m.b3Trash = mk("l3-arch", "b3", "archive", 4), mk("l3-trash", "b3", "trash", 5)
	m.b2b = mk("l-b2b", "b2", "active", 2)
	return m
}

// user adds a user with a role on some boards and returns its Env.
func (m *mb) user(t *testing.T, id string, roles map[string]string) *ops.Env {
	t.Helper()
	now := db.Now()
	mustExec(t, m.e, `INSERT INTO users(id,email,name,created_at) VALUES(?,?,?,?)`, id, id+"@x.io", id, now)
	for b, r := range roles {
		cc := any(nil)
		if r == "commenter" {
			r, cc = "viewer", 1
		}
		mustExec(t, m.e, `INSERT INTO board_memberships(id,board_id,user_id,role,can_comment,created_at) VALUES(?,?,?,?,?,?)`, "m-"+id+"-"+b, b, id, r, cc, now)
	}
	return testutil.As(m.e, &ops.Principal{UserID: id, Role: "boardUser", Name: id})
}

func errCode(err error) string {
	var oe *ops.Error
	if errors.As(err, &oe) {
		return oe.Code
	}
	if err == nil {
		return "ok"
	}
	return err.Error()
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if got := errCode(err); got != code {
		t.Fatalf("got %s (%v), want %s", got, err, code)
	}
}

func (m *mb) place(t *testing.T, e *ops.Env, cardID, listID string) Card {
	t.Helper()
	return call[Card](t, e, "card.place", PlaceIn{ID: cardID, ListID: listID})
}

func placementListsOf(t *testing.T, m *mb, cardID string) map[string]string {
	t.Helper()
	out := map[string]string{}
	rows, err := m.e.DB.Query(`SELECT board_id,list_id FROM card_placements WHERE card_id=?`, cardID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var b, l string
		_ = rows.Scan(&b, &l)
		out[b] = l
	}
	return out
}

func homeOf(t *testing.T, m *mb, cardID string) (board, list string) {
	t.Helper()
	if err := m.e.DB.QueryRow(`SELECT board_id,list_id FROM cards WHERE id=?`, cardID).Scan(&board, &list); err != nil {
		t.Fatal(err)
	}
	return
}

func TestPlaceUnplaceCrossProject(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "shared")
	got := m.place(t, m.e, c.ID, m.b3Doing)
	if got.BoardID != "b3" || got.ListID != m.b3Doing || got.HomeBoardID != "b1" || len(got.Placements) != 2 || got.AlsoOnBoards != 1 {
		t.Fatalf("place returns the card on the new board: %+v", got)
	}
	pl := call[PlacementsOut](t, m.e, "card.placements", PlacementsIn{ID: c.ID})
	if len(pl.Items) != 2 || !pl.Items[0].IsHome || pl.Items[0].BoardID != "b1" || pl.Items[1].BoardID != "b3" || pl.Items[1].ProjectName != "Second" || !pl.Items[1].CanEdit {
		t.Fatalf("placements %+v", pl)
	}
	for _, b := range []string{"b1", "b3"} {
		if l := call[ListOut](t, m.e, "card.list", ListCardsIn{BoardID: b}); len(l.Items) != 1 || l.Items[0].ID != c.ID {
			t.Fatalf("card.list %s: %+v", b, l.Items)
		}
	}
	// the same board twice is a conflict
	_, err := ops.Typed[Card](m.e, "card.place", PlaceIn{ID: c.ID, ListID: m.b3Todo})
	wantCode(t, err, "conflict")
	// the viewer of b1 sees only b1 in the placements
	pv := call[PlacementsOut](t, m.viewer, "card.placements", PlacementsIn{ID: c.ID})
	if len(pv.Items) != 1 || pv.Items[0].BoardID != "b1" || pv.Items[0].CanEdit {
		t.Fatalf("viewer placements %+v", pv)
	}
	// remove from b3; the last board cannot be removed
	out := call[UnplaceOut](t, m.e, "card.unplace", UnplaceIn{ID: c.ID, BoardID: "b3"})
	if out.NewHomeBoardID != "" || len(placementListsOf(t, m, c.ID)) != 1 {
		t.Fatalf("unplace %+v %v", out, placementListsOf(t, m, c.ID))
	}
	_, err = ops.Typed[UnplaceOut](m.e, "card.unplace", UnplaceIn{ID: c.ID, BoardID: "b1"})
	wantCode(t, err, "validation")
	_, err = ops.Typed[UnplaceOut](m.e, "card.unplace", UnplaceIn{ID: c.ID, BoardID: "b3"})
	wantCode(t, err, "not_found")
	var acts int
	_ = m.e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE card_id=? AND type IN ('cardPlace','cardUnplace')`, c.ID).Scan(&acts)
	if acts != 2 {
		t.Fatalf("cardPlace + cardUnplace activity rows = %d, want 2", acts)
	}
}

func TestPlaceRefusals(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "c")
	for _, tc := range []struct {
		name string
		in   PlaceIn
	}{
		{"archive column", PlaceIn{ID: c.ID, ListID: m.b3Arch}},
		{"trash column", PlaceIn{ID: c.ID, ListID: m.b3Trash}},
		{"boardId does not match the column", PlaceIn{ID: c.ID, ListID: m.b3Todo, BoardID: "b2"}},
	} {
		_, err := ops.Typed[Card](m.e, "card.place", tc.in)
		if errCode(err) != "validation" {
			t.Errorf("%s: %v, want validation", tc.name, err)
		}
	}
	tpl := call[Card](t, m.e, "card.template.save", TemplateSaveIn{ID: c.ID})
	_, err := ops.Typed[Card](m.e, "card.place", PlaceIn{ID: tpl.ID, ListID: m.b3Todo})
	wantCode(t, err, "validation")
	mustExec(t, m.e, `UPDATE boards SET has_inbox=1 WHERE id='b1'`)
	d := call[Card](t, m.e, "card.draft.create", DraftCreateIn{BoardID: "b1", Name: "draft"})
	_, err = ops.Typed[Card](m.e, "card.place", PlaceIn{ID: d.ID, ListID: m.b3Todo})
	wantCode(t, err, "validation")
	mustExec(t, m.e, `UPDATE boards SET is_template=1 WHERE id='b3'`)
	_, err = ops.Typed[Card](m.e, "card.place", PlaceIn{ID: c.ID, ListID: m.b3Todo})
	wantCode(t, err, "validation")
	if n := len(placementListsOf(t, m, c.ID)); n != 1 {
		t.Fatalf("refused placements wrote %d placements", n)
	}
}

// Placing needs Edit on the card (any placement) AND Edit on the target; removing needs Edit on that board.
// Someone who cannot see the object gets not_found, never forbidden.
func TestPlaceUnplaceAuthzMatrix(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "c")
	personas := map[string]*ops.Env{
		"viewer":          m.user(t, "u-v", map[string]string{"b1": "viewer", "b3": "editor"}),
		"commenter":       m.user(t, "u-c", map[string]string{"b1": "commenter", "b3": "editor"}),
		"outsiderTarget":  m.user(t, "u-o", map[string]string{"b1": "editor"}),
		"viewerTarget":    m.user(t, "u-vt", map[string]string{"b1": "editor", "b3": "viewer"}),
		"targetOnly":      m.user(t, "u-t", map[string]string{"b3": "editor"}),
		"editorBoth":      m.user(t, "u-e", map[string]string{"b1": "editor", "b3": "editor"}),
		"adminNoMember":   testutil.As(m.e, &ops.Principal{UserID: "u-root", Role: "admin", Name: "Root"}),
		"strangerEnabled": m.stranger,
	}
	mustExec(t, m.e, `INSERT INTO users(id,email,name,role,created_at) VALUES('u-root','root@x.io','Root','admin',?)`, db.Now())
	want := map[string]string{"viewer": "forbidden", "commenter": "forbidden", "outsiderTarget": "not_found", "viewerTarget": "forbidden",
		"targetOnly": "not_found", "editorBoth": "ok", "adminNoMember": "not_found", "strangerEnabled": "not_found"}
	for _, name := range []string{"viewer", "commenter", "outsiderTarget", "viewerTarget", "targetOnly", "adminNoMember", "strangerEnabled", "editorBoth"} {
		_, err := ops.Typed[Card](personas[name], "card.place", PlaceIn{ID: c.ID, ListID: m.b3Todo})
		if got := errCode(err); got != want[name] {
			t.Errorf("place as %s: %s (%v), want %s", name, got, err, want[name])
		}
	}
	// now on b1 and b3: remove from b3
	wantUn := map[string]string{"viewer": "ok", "commenter": "ok", "outsiderTarget": "not_found", "viewerTarget": "forbidden",
		"targetOnly": "ok", "adminNoMember": "not_found", "strangerEnabled": "not_found"}
	for _, name := range []string{"outsiderTarget", "viewerTarget", "adminNoMember", "strangerEnabled", "targetOnly"} {
		_, err := ops.Typed[UnplaceOut](personas[name], "card.unplace", UnplaceIn{ID: c.ID, BoardID: "b3"})
		if got := errCode(err); got != wantUn[name] {
			t.Errorf("unplace as %s: %s (%v), want %s", name, got, err, wantUn[name])
		}
	}
	// the viewer of b1 cannot remove the card from b1
	m.place(t, m.e, c.ID, m.b3Todo)
	_, err := ops.Typed[UnplaceOut](personas["viewer"], "card.unplace", UnplaceIn{ID: c.ID, BoardID: "b1"})
	wantCode(t, err, "forbidden")
	// card.placements: not_found for whoever sees no placement
	_, err = ops.Typed[PlacementsOut](personas["adminNoMember"], "card.placements", PlacementsIn{ID: c.ID})
	wantCode(t, err, "not_found")
	if p := call[PlacementsOut](t, personas["targetOnly"], "card.placements", PlacementsIn{ID: c.ID}); len(p.Items) != 1 || p.Items[0].BoardID != "b3" {
		t.Fatalf("a b3-only member sees only b3: %+v", p)
	}
}

// Closed is per card: a closing column on one board closes the card everywhere; an ordinary column re-opens it.
func TestClosingColumnOnOneBoardClosesEverywhere(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "c")
	m.place(t, m.e, c.ID, m.b3Todo)
	moved := call[Card](t, m.e, "card.move", MoveIn{ID: c.ID, ListID: m.b3Closed})
	if moved.BoardID != "b3" || !moved.IsClosed {
		t.Fatalf("move on b3 %+v", moved)
	}
	if got := call[Card](t, m.e, "card.get", GetIn{ID: c.ID, BoardID: "b1"}); !got.IsClosed || got.ListID != m.todo {
		t.Fatalf("seen on b1 the card is closed and still in todo: %+v", got)
	}
	call[Card](t, m.e, "card.move", MoveIn{ID: c.ID, ListID: m.doing}) // a column change on b1 re-opens it
	if got := call[Card](t, m.e, "card.get", GetIn{ID: c.ID, BoardID: "b3"}); got.IsClosed || got.ListID != m.b3Closed {
		t.Fatalf("seen on b3 after re-opening on b1: %+v", got)
	}
	// placing into a closing column closes it too
	d := m.card(t, m.todo, "d")
	m.place(t, m.e, d.ID, m.b3Closed)
	if got := call[Card](t, m.e, "card.get", GetIn{ID: d.ID}); !got.IsClosed {
		t.Fatal("placing into a closing column closes the card")
	}
}

// card.move picks one placement: the one on listId's board, else the only visible one, else boardId is required.
func TestMoveResolvesOnePlacement(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "c")
	m.place(t, m.e, c.ID, m.list2)
	call[Card](t, m.e, "card.move", MoveIn{ID: c.ID, ListID: m.doing})
	call[Card](t, m.e, "card.move", MoveIn{ID: c.ID, ListID: m.b2b})
	if p := placementListsOf(t, m, c.ID); p["b1"] != m.doing || p["b2"] != m.b2b {
		t.Fatalf("each board's placement moved on its board: %v", p)
	}
	_, err := ops.Typed[Card](m.e, "card.move", MoveIn{ID: c.ID, ListID: m.b3Todo})
	wantCode(t, err, "validation") // several boards: which one goes to b3?
	got := call[Card](t, m.e, "card.move", MoveIn{ID: c.ID, ListID: m.b3Todo, BoardID: "b2"})
	if got.BoardID != "b3" {
		t.Fatalf("relocated b2 -> b3: %+v", got)
	}
	if p := placementListsOf(t, m, c.ID); p["b3"] != m.b3Todo || p["b2"] != "" || p["b1"] != m.doing {
		t.Fatalf("placements after relocation: %v", p)
	}
	_, err = ops.Typed[Card](m.e, "card.move", MoveIn{ID: c.ID, ListID: m.b3Doing, BoardID: "b1"})
	wantCode(t, err, "conflict") // b3 already holds the card
	// a b1 viewer who edits b3 may move b3's placement but not b1's
	ve := m.user(t, "u-v", map[string]string{"b1": "viewer", "b3": "editor"})
	call[Card](t, ve, "card.move", MoveIn{ID: c.ID, ListID: m.b3Doing})
	_, err = ops.Typed[Card](ve, "card.move", MoveIn{ID: c.ID, ListID: m.todo})
	wantCode(t, err, "forbidden")
	if b, _ := homeOf(t, m, c.ID); b != "b1" {
		t.Fatalf("home stays b1, got %s", b)
	}
}

// Archive and trash are card actions: every placement goes to its own board's archive (trash) list; restore brings
// each back; permanent delete needs every placement in the trash.
func TestArchiveRestoreTrashDeleteFanOut(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.doing, "c")
	m.place(t, m.e, c.ID, m.list2) // b2 has no archive list: created on demand
	m.place(t, m.e, c.ID, m.b3Doing)
	call[Card](t, m.e, "card.archive", ArchiveIn{ID: c.ID})
	p := placementListsOf(t, m, c.ID)
	b2arch, _ := boards.SystemList(m.e.Ctx, m.e.DB, "b2", "archive")
	if p["b1"] != m.arch || p["b2"] != b2arch || p["b3"] != m.b3Arch {
		t.Fatalf("archived on every board: %v (b2 archive %s)", p, b2arch)
	}
	_, err := ops.Typed[Deleted](m.e, "card.delete", IDIn{ID: c.ID})
	wantCode(t, err, "validation")
	got := call[Card](t, m.e, "card.restore", IDIn{ID: c.ID})
	if got.BoardID != "b1" || got.ListID != m.doing {
		t.Fatalf("restore returns the card on its context board: %+v", got)
	}
	if p := placementListsOf(t, m, c.ID); p["b1"] != m.doing || p["b2"] != m.list2 || p["b3"] != m.b3Doing {
		t.Fatalf("restored on every board to its previous column: %v", p)
	}
	call[Card](t, m.e, "card.archive", ArchiveIn{ID: c.ID, Trash: true})
	if p := placementListsOf(t, m, c.ID); p["b1"] != m.trash || p["b3"] != m.b3Trash {
		t.Fatalf("trashed on every board: %v", p)
	}
	call[Deleted](t, m.e, "card.delete", IDIn{ID: c.ID})
	var n int
	_ = m.e.DB.QueryRow(`SELECT COUNT(*) FROM card_placements WHERE card_id=?`, c.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d placements left after delete", n)
	}
	// a placement archived on one board only (a column operation) does not count as trashed
	d := m.card(t, m.todo, "d")
	m.place(t, m.e, d.ID, m.b3Todo)
	call[Card](t, m.e, "card.move", MoveIn{ID: d.ID, ListID: m.b3Trash})
	_, err = ops.Typed[Deleted](m.e, "card.delete", IDIn{ID: d.ID})
	wantCode(t, err, "validation")
	// restore brings back only the archived placement
	call[Card](t, m.e, "card.restore", IDIn{ID: d.ID})
	if p := placementListsOf(t, m, d.ID); p["b1"] != m.todo || p["b3"] != m.b3Todo {
		t.Fatalf("restore of one archived placement: %v", p)
	}
}

// Removing the home board's placement re-homes the card on its oldest other placement; labels and members only the
// removed board allowed are dropped.
func TestUnplaceHomeRehomes(t *testing.T) {
	m := seedMB(t)
	only1 := m.user(t, "u-only1", map[string]string{"b1": "editor"})
	_ = only1
	m.user(t, "u-both", map[string]string{"b1": "editor", "b2": "editor"})
	c := m.card(t, m.todo, "c")
	m.place(t, m.e, c.ID, m.list2)
	m.place(t, m.e, c.ID, m.b3Todo)
	mustExec(t, m.e, `UPDATE card_placements SET placed_at='2000-01-01T00:00:00Z' WHERE card_id=? AND board_id='b2'`, c.ID)
	mustExec(t, m.e, `UPDATE card_placements SET placed_at='2001-01-01T00:00:00Z' WHERE card_id=? AND board_id='b3'`, c.ID)
	call[Card](t, m.e, "card.label.add", LabelIn{ID: c.ID, LabelID: m.labelA, BoardID: "b1"})
	call[Card](t, m.e, "card.label.add", LabelIn{ID: c.ID, LabelID: m.labelOther, BoardID: "b2"})
	for _, u := range []string{"u-only1", "u-both", m.ed} {
		call[Card](t, m.e, "card.member.add", MemberIn{ID: c.ID, UserID: u})
	}
	out := call[UnplaceOut](t, m.e, "card.unplace", UnplaceIn{ID: c.ID, BoardID: "b1"})
	if out.NewHomeBoardID != "b2" {
		t.Fatalf("new home %+v, want b2 (the oldest other placement)", out)
	}
	if b, l := homeOf(t, m, c.ID); b != "b2" || l != m.list2 {
		t.Fatalf("cards mirror follows the new home: %s %s", b, l)
	}
	got := call[Card](t, m.e, "card.get", GetIn{ID: c.ID})
	if got.BoardID != "b2" || got.HomeBoardID != "b2" || len(got.LabelIDs) != 1 || got.LabelIDs[0] != m.labelOther {
		t.Fatalf("after re-home: %+v", got)
	}
	var labels int
	_ = m.e.DB.QueryRow(`SELECT COUNT(*) FROM card_labels WHERE card_id=?`, c.ID).Scan(&labels)
	if labels != 1 {
		t.Fatalf("b1's label must be dropped: %d labels left", labels)
	}
	members := map[string]bool{}
	for _, u := range got.MemberIDs {
		members[u] = true
	}
	if members["u-only1"] || !members["u-both"] || !members[m.ed] {
		t.Fatalf("members after leaving b1: %v", got.MemberIDs)
	}
	// unplacing a non-home board that the caller cannot see the home of never names the hidden home
	b3only := m.user(t, "u-b3", map[string]string{"b3": "editor"})
	m.place(t, m.e, c.ID, m.doing)
	if out := call[UnplaceOut](t, b3only, "card.unplace", UnplaceIn{ID: c.ID, BoardID: "b3"}); out.NewHomeBoardID != "" {
		t.Fatalf("%+v", out)
	}
}

// Deleting a board (or project) that is the home of a card placed elsewhere re-homes the card; a card on the doomed
// boards only goes with them.
func TestDeleteHomeBoardAndProjectRehome(t *testing.T) {
	m := seedMB(t)
	shared := m.card(t, m.todo, "shared")
	m.place(t, m.e, shared.ID, m.b3Todo)
	alone := m.card(t, m.todo, "alone")
	guest := m.card(t, m.list2, "guest") // home b2, placed on b1 (b1's deletion removes only that placement)
	m.place(t, m.e, guest.ID, m.doing)
	if _, err := ops.CallMap(m.admin, "board.delete", map[string]any{"id": "b1"}); err != nil {
		t.Fatal(err)
	}
	if b, l := homeOf(t, m, shared.ID); b != "b3" || l != m.b3Todo {
		t.Fatalf("shared card re-homed on b3: %s %s", b, l)
	}
	var n int
	_ = m.e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE id=?`, alone.ID).Scan(&n)
	if n != 0 {
		t.Fatal("a card on the deleted board only is deleted")
	}
	if p := placementListsOf(t, m, guest.ID); len(p) != 1 || p["b2"] != m.list2 {
		t.Fatalf("guest card keeps its home only: %v", p)
	}
	// project p1 (b2) deleted: the guest card (home b2) placed on b3 survives there
	m.place(t, m.e, guest.ID, m.b3Doing)
	if _, err := ops.CallMap(m.admin, "project.delete", map[string]any{"id": "p1"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := homeOf(t, m, guest.ID); b != "b3" {
		t.Fatalf("guest card re-homed on b3 (other project), got %s", b)
	}
	var cards int
	_ = m.e.DB.QueryRow(`SELECT COUNT(*) FROM cards`).Scan(&cards)
	if cards != 2 {
		t.Fatalf("%d cards left, want the two on b3", cards)
	}
}

// Emptying a trash deletes cards that live only there and removes from the board the ones that are on other boards.
func TestListClearUnplacesSharedCards(t *testing.T) {
	m := seedMB(t)
	x := m.card(t, m.todo, "x")
	y := m.card(t, m.todo, "y")
	m.place(t, m.e, y.ID, m.b3Todo)
	call[Card](t, m.e, "card.archive", ArchiveIn{ID: x.ID, Trash: true})
	call[Card](t, m.e, "card.archive", ArchiveIn{ID: y.ID, Trash: true})
	if _, err := ops.CallMap(m.e, "list.clear", map[string]any{"id": m.trash}); err != nil {
		t.Fatal(err)
	}
	var n int
	_ = m.e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE id=?`, x.ID).Scan(&n)
	if n != 0 {
		t.Fatal("x lived only in b1's trash: deleted")
	}
	if p := placementListsOf(t, m, y.ID); len(p) != 1 || p["b3"] != m.b3Trash {
		t.Fatalf("y is removed from b1 only: %v", p)
	}
	if b, _ := homeOf(t, m, y.ID); b != "b3" {
		t.Fatalf("y re-homed on b3, got %s", b)
	}
	call[Deleted](t, m.e, "card.delete", IDIn{ID: y.ID}) // now in the trash on its only board
}

// Auto-archive is a column rule of one board: it archives that board's placement only.
func TestAutoArchiveArchivesOnlyThatBoardsPlacement(t *testing.T) {
	m := seedMB(t)
	mustExec(t, m.e, `UPDATE lists SET archive_after=1 WHERE id=?`, m.b3Todo)
	a := m.card(t, m.todo, "a")
	m.place(t, m.e, a.ID, m.b3Todo)
	b := m.card(t, m.doing, "b")
	m.place(t, m.e, b.ID, m.b3Todo) // b lands: a (the oldest) leaves b3's column
	if p := placementListsOf(t, m, a.ID); p["b3"] != m.b3Arch || p["b1"] != m.todo {
		t.Fatalf("a archived on b3 only: %v", p)
	}
	if p := placementListsOf(t, m, b.ID); p["b3"] != m.b3Todo {
		t.Fatalf("b stays: %v", p)
	}
}

// Two agents listening on columns of two boards that hold the same card: it is handed out once.
func TestCardNextTwoBoardsOneWinner(t *testing.T) {
	for round := 0; round < 5; round++ {
		m := seedMB(t)
		c := m.card(t, m.todo, "contested")
		m.place(t, m.e, c.ID, m.list2)
		ags := m.agents(t, 8)
		for i := range ags {
			mustExec(t, m.e, `INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,'b2',?,'editor',?)`, fmt.Sprintf("m2-%d", i), ags[i].User.UserID, db.Now())
		}
		var wg sync.WaitGroup
		var mu sync.Mutex
		got := 0
		start := make(chan struct{})
		for i, a := range ags {
			list := m.todo
			if i%2 == 1 {
				list = m.list2
			}
			wg.Add(1)
			go func(a *ops.Env, list string) {
				defer wg.Done()
				<-start
				s, err := ops.Typed[*events.CardSnap](a, "card.next", NextIn{ListID: list, Claim: true})
				if err != nil {
					t.Errorf("next: %v", err)
					return
				}
				if s != nil {
					mu.Lock()
					got++
					mu.Unlock()
				}
			}(a, list)
		}
		close(start)
		wg.Wait()
		if got != 1 {
			t.Fatalf("round %d: the card was handed out %d times, want 1", round, got)
		}
	}
}

// A claim's move acts on the placement on the target list's board; a list of a board the card is not on is refused
// before anything is claimed when the card is on several boards.
func TestClaimMovesThatPlacement(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "c")
	m.place(t, m.e, c.ID, m.list2)
	_, err := ops.Typed[Card](m.e, "card.claim", ClaimIn{ID: c.ID, ListID: m.b3Todo})
	wantCode(t, err, "validation")
	if got := call[Card](t, m.e, "card.get", GetIn{ID: c.ID}); got.ClaimedBy != nil {
		t.Fatal("a refused move must not leave a claim")
	}
	got := call[Card](t, m.e, "card.claim", ClaimIn{ID: c.ID, ListID: m.b2b})
	if got.BoardID != "b2" || got.ListID != m.b2b || got.ClaimedBy == nil {
		t.Fatalf("claim moved b2's placement: %+v", got)
	}
	if p := placementListsOf(t, m, c.ID); p["b1"] != m.todo || p["b2"] != m.b2b {
		t.Fatalf("placements %v", p)
	}
	rel := call[Card](t, m.e, "card.release", ReleaseIn{ID: c.ID, ListID: m.doing})
	if rel.ListID != m.doing || placementListsOf(t, m, c.ID)["b2"] != m.b2b {
		t.Fatalf("release moved b1's placement only: %+v %v", rel, placementListsOf(t, m, c.ID))
	}
	// card.next on b2's column hands out the card as seen on b2
	s := call[*events.CardSnap](t, m.e, "card.next", NextIn{ListID: m.b2b})
	if s == nil || s.BoardID != "b2" || s.ListID != m.b2b {
		t.Fatalf("next on b2: %+v", s)
	}
}

func TestRecurrenceCopiesPlacements(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "weekly")
	m.place(t, m.e, c.ID, m.b3Doing)
	m.place(t, m.e, c.ID, m.list2)
	call[Card](t, m.e, "card.move", MoveIn{ID: c.ID, ListID: m.b3Closed}) // a closing column on b3: the next one goes to b3's first open column
	call[Card](t, m.e, "card.label.add", LabelIn{ID: c.ID, LabelID: m.labelOther, BoardID: "b2"})
	call[Card](t, m.e, "card.due.set", DueIn{ID: c.ID, DueDate: "2020-01-01T09:00:00Z"})
	call[Card](t, m.e, "card.recurrence.set", RecurrenceIn{ID: c.ID, Recurrence: `{"freq":"daily"}`})
	next, err := spawnNextAt(m.e, c.ID, time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil || next == "" {
		t.Fatalf("spawn %q %v", next, err)
	}
	p := placementListsOf(t, m, next)
	if len(p) != 3 || p["b1"] != m.todo || p["b2"] != m.list2 || p["b3"] != m.b3Todo {
		t.Fatalf("next occurrence placements %v", p)
	}
	if b, _ := homeOf(t, m, next); b != "b1" {
		t.Fatalf("home b1, got %s", b)
	}
	// every board of the series hears the new occurrence arrive in its own column (one card.created per board)
	rows, err := m.e.DB.Query(`SELECT board_id,list_id FROM actions WHERE card_id=? AND type='cardCreate' ORDER BY board_id`, next)
	if err != nil {
		t.Fatal(err)
	}
	created := map[string]string{}
	for rows.Next() {
		var b, l string
		_ = rows.Scan(&b, &l)
		created[b] = l
	}
	rows.Close()
	if len(created) != 3 || created["b1"] != m.todo || created["b2"] != m.list2 || created["b3"] != m.b3Todo {
		t.Fatalf("card.created rows of the next occurrence: %v", created)
	}
	got := call[Card](t, m.e, "card.get", GetIn{ID: next, BoardID: "b2"})
	if len(got.LabelIDs) != 1 || got.LabelIDs[0] != m.labelOther || got.IsClosed {
		t.Fatalf("next occurrence on b2: %+v", got)
	}
}

func TestDuplicateMakesOnePlacement(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "c")
	m.place(t, m.e, c.ID, m.list2)
	d := call[Card](t, m.e, "card.duplicate", DuplicateIn{ID: c.ID, BoardID: "b2"})
	if p := placementListsOf(t, m, d.ID); len(p) != 1 || p["b2"] != m.list2 || d.BoardID != "b2" {
		t.Fatalf("the copy is on b2 only: %v %+v", p, d)
	}
	// someone who sees only b2 duplicates on b2 (never on the hidden home)
	b2only := m.user(t, "u-b2", map[string]string{"b2": "editor"})
	d2 := call[Card](t, b2only, "card.duplicate", DuplicateIn{ID: c.ID})
	if d2.BoardID != "b2" {
		t.Fatalf("copy by a b2-only member: %+v", d2)
	}
	_, err := ops.Typed[Card](b2only, "card.duplicate", DuplicateIn{ID: c.ID, BoardID: "b1"})
	wantCode(t, err, "not_found")
}

func TestLabelsArePerBoard(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "c")
	call[Card](t, m.e, "card.label.add", LabelIn{ID: c.ID, LabelID: m.labelA}) // one board: no boardId needed
	m.place(t, m.e, c.ID, m.list2)
	_, err := ops.Typed[Card](m.e, "card.label.add", LabelIn{ID: c.ID, LabelID: m.labelB})
	wantCode(t, err, "validation") // several boards: which board's label?
	_, err = ops.Typed[Card](m.e, "card.label.add", LabelIn{ID: c.ID, LabelID: m.labelOther, BoardID: "b1"})
	wantCode(t, err, "not_found")
	got := call[Card](t, m.e, "card.label.add", LabelIn{ID: c.ID, LabelID: m.labelOther, BoardID: "b2"})
	if got.BoardID != "b2" || len(got.LabelIDs) != 1 || got.LabelIDs[0] != m.labelOther {
		t.Fatalf("label on b2: %+v", got)
	}
	_, err = ops.Typed[Card](m.e, "card.label.remove", LabelRemoveIn{ID: c.ID, LabelID: m.labelA, BoardID: "b2"})
	wantCode(t, err, "not_found") // b1's label is not b2's to remove
	call[Card](t, m.e, "card.label.remove", LabelRemoveIn{ID: c.ID, LabelID: m.labelA, BoardID: "b1"})
	// moving away from b2 drops b2's label; placing back does not restore it
	call[UnplaceOut](t, m.e, "card.unplace", UnplaceIn{ID: c.ID, BoardID: "b2"})
	var n int
	_ = m.e.DB.QueryRow(`SELECT COUNT(*) FROM card_labels WHERE card_id=?`, c.ID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d labels left after leaving b2", n)
	}
}

// Leaving a board drops a user's card memberships only for cards that are not on another board they belong to.
func TestBoardMemberRemovalKeepsSharedCardMembership(t *testing.T) {
	m := seedMB(t)
	m.user(t, "u-m", map[string]string{"b1": "editor", "b2": "editor"})
	shared := m.card(t, m.todo, "shared")
	m.place(t, m.e, shared.ID, m.list2)
	solo := m.card(t, m.todo, "solo")
	for _, id := range []string{shared.ID, solo.ID} {
		call[Card](t, m.e, "card.member.add", MemberIn{ID: id, UserID: "u-m"})
	}
	if _, err := ops.CallMap(m.admin, "board.member.remove", map[string]any{"memberId": "m-u-m-b1"}); err != nil {
		t.Fatal(err)
	}
	isMember := func(card string) bool {
		var n int
		_ = m.e.DB.QueryRow(`SELECT COUNT(*) FROM card_memberships WHERE card_id=? AND user_id='u-m'`, card).Scan(&n)
		return n > 0
	}
	if !isMember(shared.ID) || isMember(solo.ID) {
		t.Fatalf("shared kept=%v solo kept=%v, want true/false", isMember(shared.ID), isMember(solo.ID))
	}
	// a member of b2 only can be added to the shared card (a member of a board it is on)
	m.user(t, "u-b2m", map[string]string{"b2": "editor"})
	call[Card](t, m.e, "card.member.add", MemberIn{ID: shared.ID, UserID: "u-b2m"})
	_, err := ops.Typed[Card](m.e, "card.member.add", MemberIn{ID: solo.ID, UserID: "u-b2m"})
	wantCode(t, err, "validation")
}

// Applying a template to a board moves the cards placed there from other boards too (never cascade-deletes them
// with the old columns).
func TestTemplateApplyKeepsPlacedCards(t *testing.T) {
	m := seedMB(t)
	guest := m.card(t, m.list2, "guest") // home b2, placed on b1
	m.place(t, m.e, guest.ID, m.doing)
	own := m.card(t, m.todo, "own")
	out := call[boards.TemplateApplyOut](t, m.admin, "board.template.apply", boards.TemplateApplyIn{ID: "b1", TemplateID: "builtin:kanban-basics", Confirm: true})
	if out.CardsMoved != 2 {
		t.Fatalf("cards moved %d, want 2 (own + guest)", out.CardsMoved)
	}
	p := placementListsOf(t, m, guest.ID)
	if p["b2"] != m.list2 || p["b1"] != out.TargetColumn.ID {
		t.Fatalf("guest placements after apply: %v (target %s)", p, out.TargetColumn.ID)
	}
	if p := placementListsOf(t, m, own.ID); p["b1"] != out.TargetColumn.ID {
		t.Fatalf("own %v", p)
	}
}

// A person who sees only a non-home placement works on the card there; nothing they get back names the home.
func TestHiddenHomeOutputsStayOnVisibleBoard(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "c")
	m.place(t, m.e, c.ID, m.list2)
	b2only := m.user(t, "u-b2", map[string]string{"b2": "editor"})
	if _, err := boards.SystemList(m.e.Ctx, m.e.DB, "b2", "archive"); err != nil {
		t.Fatal(err)
	}
	n := "renamed"
	for _, step := range []struct {
		name string
		fn   func() (Card, error)
	}{
		{"get", func() (Card, error) { return ops.Typed[Card](b2only, "card.get", GetIn{ID: c.ID}) }},
		{"update", func() (Card, error) { return ops.Typed[Card](b2only, "card.update", UpdateIn{ID: c.ID, Name: &n}) }},
		{"move", func() (Card, error) { return ops.Typed[Card](b2only, "card.move", MoveIn{ID: c.ID, ListID: m.b2b}) }},
		{"close", func() (Card, error) { return ops.Typed[Card](b2only, "card.close", CloseIn{ID: c.ID}) }},
		{"archive", func() (Card, error) { return ops.Typed[Card](b2only, "card.archive", ArchiveIn{ID: c.ID}) }},
		{"restore", func() (Card, error) { return ops.Typed[Card](b2only, "card.restore", IDIn{ID: c.ID}) }},
		{"claim", func() (Card, error) { return ops.Typed[Card](b2only, "card.claim", ClaimIn{ID: c.ID}) }},
		{"duplicate", func() (Card, error) { return ops.Typed[Card](b2only, "card.duplicate", DuplicateIn{ID: c.ID}) }},
	} {
		got, err := step.fn()
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if got.BoardID != "b2" || got.HomeBoardID != "" {
			t.Fatalf("%s returns %+v", step.name, got)
		}
	}
	// the archive by the b2-only member archived the hidden b1 placement too, and restore brought it back
	if p := placementListsOf(t, m, c.ID); p["b1"] != m.todo {
		t.Fatalf("b1 placement after archive+restore by a b2 member: %v", p)
	}
}

// card.next with moveTo on a board the picked card is not on relocates the placement of the column it was picked
// from (no move_failed release for a card on several boards); a card already on moveTo's board moves its
// placement there instead.
func TestCardNextMoveToAnotherBoard(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "relocated")
	m.place(t, m.e, c.ID, m.list2) // b1 (home) and b2
	s := call[*events.CardSnap](t, m.e, "card.next", NextIn{ListID: m.todo, Claim: true, MoveTo: m.b3Doing})
	if s == nil || s.ID != c.ID || s.BoardID != "b3" || s.ListID != m.b3Doing {
		t.Fatalf("next with moveTo on b3: %+v", s)
	}
	p := placementListsOf(t, m, c.ID)
	if p["b3"] != m.b3Doing || p["b2"] != m.list2 || p["b1"] != "" {
		t.Fatalf("placements %v: b1's placement should have moved to b3", p)
	}
	if got := call[Card](t, m.e, "card.get", GetIn{ID: c.ID}); got.ClaimedBy == nil {
		t.Fatal("the claim was released")
	}
	// a card on b1 and b3: moveTo on b3 moves b3's placement, b1's stays
	d := m.card(t, m.todo, "already there")
	m.place(t, m.e, d.ID, m.b3Todo)
	s = call[*events.CardSnap](t, m.e, "card.next", NextIn{ListID: m.todo, Claim: true, MoveTo: m.b3Doing})
	if s == nil || s.ID != d.ID {
		t.Fatalf("second next: %+v", s)
	}
	if p := placementListsOf(t, m, d.ID); p["b1"] != m.todo || p["b3"] != m.b3Doing {
		t.Fatalf("placements %v", p)
	}
}

// The recurrence runner picks a due card that is live on ANY of its boards: its home placement archived alone (an
// auto-archive archives one board's placement only) does not stop the series while another board still shows it.
func TestRecurrenceRunsWhileLiveOnAnotherBoard(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.b3Todo, "weekly sync") // home b3
	m.place(t, m.e, c.ID, m.todo)
	mustExec(t, m.e, `UPDATE cards SET due_date='2020-01-06T09:00:00Z', recurrence='{"freq":"weekly","interval":1,"trigger":"due"}' WHERE id=?`, c.ID)
	if _, err := ops.Typed[Card](m.e, "card.move", MoveIn{ID: c.ID, ListID: m.b3Arch, BoardID: "b3"}); err != nil {
		t.Fatal(err)
	}
	n, err := RunRecurrence(m.e.Ctx, m.e.DB, m.e.Hub, time.Now())
	if err != nil || n != 1 {
		t.Fatalf("RunRecurrence spawned %d (%v), want 1", n, err)
	}
	// archived on every board: no longer due
	d := m.card(t, m.b3Todo, "archived series")
	m.place(t, m.e, d.ID, m.todo)
	mustExec(t, m.e, `UPDATE cards SET due_date='2020-01-06T09:00:00Z', recurrence='{"freq":"weekly","interval":1,"trigger":"due"}' WHERE id=?`, d.ID)
	if _, err := ops.Typed[Card](m.e, "card.archive", ArchiveIn{ID: d.ID}); err != nil {
		t.Fatal(err)
	}
	if n, _ := RunRecurrence(m.e.Ctx, m.e.DB, m.e.Hub, time.Now()); n != 0 {
		t.Fatalf("an archived series spawned %d", n)
	}
}
