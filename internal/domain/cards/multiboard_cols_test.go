package cards

import (
	"net/url"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
)

// An editor of a non-home board who only views the home board edits the card through the board they edit: the
// card-level actions never trip over the read-only home.
func TestEditorOfNonHomeBoardWithReadOnlyHome(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "c")
	m.place(t, m.e, c.ID, m.b3Todo)
	ve := m.user(t, "u-v", map[string]string{"b1": "viewer", "b3": "editor"})
	closed := call[Card](t, ve, "card.close", CloseIn{ID: c.ID}) // b1 has a closing column, but b1 is read-only for ve
	if p := placementListsOf(t, m, c.ID); !closed.IsClosed || closed.BoardID != "b3" || p["b3"] != m.b3Closed || p["b1"] != m.todo {
		t.Fatalf("close by a b3 editor moves b3's placement: %+v %v", closed, p)
	}
	no := false
	if got := call[Card](t, ve, "card.close", CloseIn{ID: c.ID, Closed: &no}); got.IsClosed || got.ListID != m.b3Todo {
		t.Fatalf("reopen %+v", got)
	}
	d := call[Card](t, ve, "card.duplicate", DuplicateIn{ID: c.ID})
	if p := placementListsOf(t, m, d.ID); len(p) != 1 || p["b3"] == "" {
		t.Fatalf("duplicate lands on b3: %v", p)
	}
	call[Card](t, ve, "card.claim", ClaimIn{ID: c.ID, ListID: m.b3Doing})
	call[Card](t, ve, "card.release", ReleaseIn{ID: c.ID, ListID: m.b3Todo})
	call[Card](t, ve, "card.archive", ArchiveIn{ID: c.ID})
	if p := placementListsOf(t, m, c.ID); p["b1"] != m.arch || p["b3"] != m.b3Arch {
		t.Fatalf("archived everywhere: %v", p)
	}
	call[Card](t, ve, "card.restore", IDIn{ID: c.ID})
	call[Card](t, ve, "card.archive", ArchiveIn{ID: c.ID, Trash: true})
	call[Deleted](t, ve, "card.delete", IDIn{ID: c.ID})
}

// The column operations of one board act on the placements there, a card homed elsewhere included.
func TestColumnOpsWithGuestPlacement(t *testing.T) {
	m := seedMB(t)
	guest := m.card(t, m.list2, "guest") // home b2
	m.place(t, m.e, guest.ID, m.b3Todo)
	tp := call[boards.ListTypePreview](t, m.e, "list.typePreview", boards.ListTypePreviewIn{ID: m.b3Todo, Type: "closed"})
	if tp.Cards != 1 || tp.WillClose != 1 {
		t.Fatalf("type preview %+v", tp)
	}
	ap := call[boards.ListArchivePreview](t, m.e, "list.archivePreview", boards.ListArchivePreviewIn{ID: m.b3Todo, ArchiveAfter: 1})
	if ap.Cards != 1 {
		t.Fatalf("archive preview %+v", ap)
	}
	typ := "closed"
	call[boards.List](t, m.e, "list.update", boards.ListUpdateIn{ID: m.b3Todo, Type: &typ})
	if got := call[Card](t, m.e, "card.get", GetIn{ID: guest.ID, BoardID: "b2"}); !got.IsClosed {
		t.Fatal("flipping b3's column closes the card everywhere")
	}
	typ = "active"
	call[boards.List](t, m.e, "list.update", boards.ListUpdateIn{ID: m.b3Todo, Type: &typ})
	call[boards.ListMoveCardsOut](t, m.e, "list.moveCards", boards.ListMoveCardsIn{ID: m.b3Todo, ListID: m.b3Doing})
	if p := placementListsOf(t, m, guest.ID); p["b3"] != m.b3Doing || p["b2"] != m.list2 {
		t.Fatalf("moveCards %v", p)
	}
	call[boards.Deleted](t, m.e, "list.archive", boards.ListRefIn{ID: m.b3Doing})
	if p := placementListsOf(t, m, guest.ID); p["b3"] != m.b3Arch || p["b2"] != m.list2 {
		t.Fatalf("list.archive %v", p)
	}
	call[Card](t, m.e, "card.restore", IDIn{ID: guest.ID}) // the previous column is gone: b3's first open column
	if p := placementListsOf(t, m, guest.ID); p["b3"] != m.b3Todo || p["b2"] != m.list2 {
		t.Fatalf("restore into the first open column: %v", p)
	}
	call[boards.Deleted](t, m.e, "list.delete", boards.ListRefIn{ID: m.b3Todo})
	if p := placementListsOf(t, m, guest.ID); p["b3"] != m.b3Trash || p["b2"] != m.list2 {
		t.Fatalf("list.delete %v", p)
	}
}

// The tile's quick label toggle names the tile's board, so it works on a card that is on several boards.
func TestQuickLabelOnTwoBoardCard(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "c")
	m.place(t, m.e, c.ID, m.list2)
	w := do(mount(m.fx, m.e), "POST", "/ui/cards/"+c.ID+"/quick/label?board=b2", url.Values{"labelId": {m.labelOther}})
	if w.Code >= 400 {
		t.Fatalf("quick label %d %s", w.Code, w.Body.String())
	}
	if got := call[Card](t, m.e, "card.get", GetIn{ID: c.ID, BoardID: "b2"}); len(got.LabelIDs) != 1 || got.LabelIDs[0] != m.labelOther {
		t.Fatalf("labels on b2: %v", got.LabelIDs)
	}
}
