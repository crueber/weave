package cards

import (
	"encoding/json"
	"net/url"
	"testing"
	"time"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
)

// Every path that lands a card honours a column's "Close cards that land here" setting (switched on with list.update,
// not seeded), through boards.Landed. Where the op returns the card, the returned isClosed (what the CLI and MCP see)
// is checked as well as the stored row.
func TestClosingColumnEveryLandingPath(t *testing.T) {
	f := seed(t)
	closing := f.doing
	on := "closed"
	if l := call[boards.List](t, f.e, "list.update", boards.ListUpdateIn{ID: closing, Type: &on}); l.Type != "closed" {
		t.Fatalf("switch on: %s", l.Type)
	}
	b := func(v bool) *bool { return &v }
	// claims and drafts need a board member who may edit: the seeded editor is one
	call[InboxOut](t, f.admin, "card.inbox.set", InboxSetIn{BoardID: f.board, Enabled: true})
	tpl := call[Card](t, f.e, "card.template.save", TemplateSaveIn{ID: f.card(t, f.todo, "template source").ID})

	type landing func(t *testing.T) (id string, returned *bool)
	paths := []struct {
		name string
		land landing
	}{
		{"card.create", func(t *testing.T) (string, *bool) {
			c := call[Card](t, f.e, "card.create", CreateIn{ListID: closing, Name: "created"})
			return c.ID, b(c.IsClosed)
		}},
		{"card.move", func(t *testing.T) (string, *bool) {
			c := call[Card](t, f.e, "card.move", MoveIn{ID: f.card(t, f.todo, "moved").ID, ListID: closing})
			return c.ID, b(c.IsClosed)
		}},
		{"drag-drop (UI)", func(t *testing.T) (string, *bool) {
			c := f.card(t, f.todo, "dropped")
			if w := do(mount(f, f.e), "POST", "/ui/boards/"+f.board+"/cards/drop", url.Values{"id": {c.ID}, "container": {closing}}); w.Code >= 400 {
				t.Fatalf("drop %d %s", w.Code, w.Body.String())
			}
			return c.ID, nil
		}},
		{"card.claim with listId", func(t *testing.T) (string, *bool) {
			c := call[Card](t, f.e, "card.claim", ClaimIn{ID: f.card(t, f.todo, "claimed").ID, ListID: closing})
			return c.ID, b(c.IsClosed)
		}},
		{"card.release with listId", func(t *testing.T) (string, *bool) {
			c := f.card(t, f.todo, "released")
			call[Card](t, f.e, "card.claim", ClaimIn{ID: c.ID})
			r := call[Card](t, f.e, "card.release", ReleaseIn{ID: c.ID, ListID: closing})
			return r.ID, b(r.IsClosed)
		}},
		{"card.next claim+moveTo", func(t *testing.T) (string, *bool) {
			src := call[boards.List](t, f.e, "list.create", boards.ListCreateIn{BoardID: f.board, Name: "queue"})
			f.card(t, src.ID, "next up")
			s := call[*events.CardSnap](t, f.e, "card.next", NextIn{ListID: src.ID, Claim: true, MoveTo: closing})
			if s == nil {
				t.Fatal("card.next found nothing")
			}
			return s.ID, b(s.IsClosed)
		}},
		{"card.draft.publish", func(t *testing.T) (string, *bool) {
			d := call[Card](t, f.e, "card.draft.create", DraftCreateIn{BoardID: f.board, Name: "idea"})
			c := call[Card](t, f.e, "card.draft.publish", PublishIn{ID: d.ID, ListID: closing})
			return c.ID, b(c.IsClosed)
		}},
		{"card.fromTemplate", func(t *testing.T) (string, *bool) {
			c := call[Card](t, f.e, "card.fromTemplate", FromTemplateIn{ID: tpl.ID, ListID: closing})
			return c.ID, b(c.IsClosed)
		}},
		{"card.duplicate (inside the column)", func(t *testing.T) (string, *bool) {
			src := call[Card](t, f.e, "card.create", CreateIn{ListID: closing, Name: "original"})
			c := call[Card](t, f.e, "card.duplicate", DuplicateIn{ID: src.ID})
			return c.ID, b(c.IsClosed)
		}},
		{"card.restore (from the archive)", func(t *testing.T) (string, *bool) {
			c := call[Card](t, f.e, "card.create", CreateIn{ListID: closing, Name: "archived"})
			mustExec(t, f.e, `UPDATE cards SET is_closed=0 WHERE id=?`, c.ID) // as if it had been kept open
			call[Card](t, f.e, "card.archive", ArchiveIn{ID: c.ID})
			r := call[Card](t, f.e, "card.restore", IDIn{c.ID})
			return r.ID, b(r.IsClosed)
		}},
		{"recurrence spawn", func(t *testing.T) (string, *bool) {
			rc := call[Card](t, f.e, "card.create", CreateIn{ListID: closing, Name: "weekly"})
			call[Card](t, f.e, "card.due.set", DueIn{ID: rc.ID, DueDate: time.Now().UTC().AddDate(0, 0, -2).Format(time.RFC3339)})
			call[Card](t, f.e, "card.recurrence.set", RecurrenceIn{ID: rc.ID, Recurrence: `{"freq":"daily","count":2}`})
			if n, err := RunRecurrence(f.e.Ctx, f.e.DB, f.e.Hub, time.Now()); err != nil || n != 1 {
				t.Fatalf("recurrence spawned %d err %v", n, err)
			}
			var id string
			_ = f.e.DB.QueryRow(`SELECT id FROM cards WHERE name='weekly' AND id<>? AND is_template=0`, rc.ID).Scan(&id)
			return id, nil
		}},
		{"list.moveCards", func(t *testing.T) (string, *bool) {
			src := call[boards.List](t, f.e, "list.create", boards.ListCreateIn{BoardID: f.board, Name: "bulk"})
			c := f.card(t, src.ID, "bulk moved")
			call[boards.ListMoveCardsOut](t, f.e, "list.moveCards", boards.ListMoveCardsIn{ID: src.ID, ListID: closing})
			return c.ID, nil
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			id, ret := p.land(t)
			if id == "" {
				t.Fatal("no card")
			}
			if !closedNow(t, f, id) {
				t.Errorf("stored is_closed=false after landing in a closing column")
			}
			if ret != nil && !*ret {
				t.Errorf("the op returned isClosed=false")
			}
			var list string
			_ = f.e.DB.QueryRow(`SELECT list_id FROM cards WHERE id=?`, id).Scan(&list)
			if list != closing {
				t.Errorf("card is in %s, not the closing column", list)
			}
		})
	}

	// board.import and board.instantiate: the setting travels with the column (type closed), and a card arriving in it
	// is closed even when the file says it was open (a card kept open with existingCards=keep comes back closed)
	kept := call[Card](t, f.e, "card.create", CreateIn{ListID: closing, Name: "kept open"})
	mustExec(t, f.e, `UPDATE cards SET is_closed=0 WHERE id=?`, kept.ID)
	st := call[*ops.Stream](t, f.admin, "board.export", boards.ExportIn{ID: f.board})
	var doc struct {
		Lists []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"lists"`
	}
	if err := json.Unmarshal(st.Data, &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, l := range doc.Lists {
		if l.Name == "l-doing" {
			found = l.Type == "closed"
		}
	}
	if !found {
		t.Fatalf("export lost the closing setting: %+v", doc.Lists)
	}
	closedIn := func(boardID string) bool {
		var c bool
		if err := f.e.DB.QueryRow(`SELECT c.is_closed FROM cards c JOIN lists l ON l.id=c.list_id WHERE c.board_id=? AND c.name='kept open' AND l.type='closed'`, boardID).Scan(&c); err != nil {
			t.Fatalf("kept card on %s: %v", boardID, err)
		}
		return c
	}
	imp := call[boards.Board](t, f.admin, "board.import", &boards.ImportIn{ProjectID: f.proj, Format: "planka", Data: string(st.Data), Name: "Imported"})
	if !closedIn(imp.ID) {
		t.Error("board.import: a card landing in a closing column must be closed")
	}
	call[boards.Board](t, f.admin, "board.template.publish", boards.TemplatePublishIn{ID: f.board, IsTemplate: true})
	inst := call[boards.Board](t, f.admin, "board.instantiate", boards.InstantiateIn{ProjectID: f.proj, TemplateID: f.board, Name: "From template"})
	if !closedIn(inst.ID) {
		t.Error("board.instantiate: a sample card in a closing column must be closed")
	}
}

// existingCards=keep leaves open cards in a closing column (and closed cards in an active one); card.close still works
// on them: it closes (or re-opens) in place instead of moving.
func TestCardCloseOnKeptCards(t *testing.T) {
	f := seed(t)
	keptOpen := f.card(t, f.doing, "kept open")
	keptClosed := f.card(t, f.closed, "kept closed")
	on, off := "closed", "active"
	call[boards.List](t, f.e, "list.update", boards.ListUpdateIn{ID: f.doing, Type: &on, ExistingCards: "keep"})
	call[boards.List](t, f.e, "list.update", boards.ListUpdateIn{ID: f.closed, Type: &off, ExistingCards: "keep"})
	if closedNow(t, f, keptOpen.ID) || !closedNow(t, f, keptClosed.ID) {
		t.Fatal("keep must leave both cards as they were")
	}
	c := call[Card](t, f.e, "card.close", CloseIn{ID: keptOpen.ID})
	if !c.IsClosed || c.ListID != f.doing || !closedNow(t, f, keptOpen.ID) {
		t.Fatalf("closing a kept-open card in a closing column: closed=%v list=%s", c.IsClosed, c.ListID)
	}
	no := false
	c = call[Card](t, f.e, "card.close", CloseIn{ID: keptClosed.ID, Closed: &no})
	if c.IsClosed || c.ListID != f.closed || closedNow(t, f, keptClosed.ID) {
		t.Fatalf("re-opening a kept-closed card in an active column: closed=%v list=%s", c.IsClosed, c.ListID)
	}
}
