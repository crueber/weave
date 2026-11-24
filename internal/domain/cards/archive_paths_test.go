package cards

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/events"
	"github.com/crueber/weave/internal/ops"
)

// Every path that lands a card holds a column with auto-archive at N: the column keeps N cards, the oldest other
// card goes to the archive (prev_list_id = the column), the card that landed stays, and one quiet card.archived plus
// one column summary are recorded.
func TestArchiveAfterEveryLandingPath(t *testing.T) {
	f := seed(t)
	lane := f.doing
	const keep = 3
	call[InboxOut](t, f.admin, "card.inbox.set", InboxSetIn{BoardID: f.board, Enabled: true})
	tpl := call[Card](t, f.e, "card.template.save", TemplateSaveIn{ID: f.card(t, f.todo, "template source").ID})
	n := 0
	// fill the lane with keep cards that entered long ago (oldest first), then switch the rule on
	fill := func(t *testing.T) []string {
		t.Helper()
		mustExec(t, f.e, `UPDATE card_placements SET list_id=?, prev_list_id=NULL WHERE list_id=?`, f.trash, lane) // empty the lane
		var ids []string
		for i := 0; i < keep; i++ {
			n++
			c := f.card(t, lane, fmt.Sprintf("old %d", n))
			mustExec(t, f.e, `UPDATE card_placements SET list_changed_at=? WHERE card_id=? AND is_home=1`, fmt.Sprintf("2024-01-01T00:00:%02dZ", i), c.ID)
			ids = append(ids, c.ID)
		}
		return ids
	}
	k := keep
	fill(t)
	call[boards.List](t, f.e, "list.update", boards.ListUpdateIn{ID: lane, ArchiveAfter: &k})

	paths := []struct {
		name string
		land func(t *testing.T) string
	}{
		{"card.create", func(t *testing.T) string {
			return call[Card](t, f.e, "card.create", CreateIn{ListID: lane, Name: "created"}).ID
		}},
		{"card.move", func(t *testing.T) string {
			return call[Card](t, f.e, "card.move", MoveIn{ID: f.card(t, f.todo, "moved").ID, ListID: lane}).ID
		}},
		{"card.move (from another board)", func(t *testing.T) string {
			return call[Card](t, f.e, "card.move", MoveIn{ID: f.card(t, f.list2, "cross").ID, ListID: lane}).ID
		}},
		{"drag-drop (UI)", func(t *testing.T) string {
			c := f.card(t, f.todo, "dropped")
			if w := do(mount(f, f.e), "POST", "/ui/boards/"+f.board+"/cards/drop", url.Values{"id": {c.ID}, "container": {lane}}); w.Code >= 400 {
				t.Fatalf("drop %d %s", w.Code, w.Body.String())
			}
			return c.ID
		}},
		{"card.claim with listId", func(t *testing.T) string {
			return call[Card](t, f.e, "card.claim", ClaimIn{ID: f.card(t, f.todo, "claimed").ID, ListID: lane}).ID
		}},
		{"card.release with listId", func(t *testing.T) string {
			c := f.card(t, f.todo, "released")
			call[Card](t, f.e, "card.claim", ClaimIn{ID: c.ID})
			return call[Card](t, f.e, "card.release", ReleaseIn{ID: c.ID, ListID: lane}).ID
		}},
		{"card.next claim+moveTo", func(t *testing.T) string {
			src := call[boards.List](t, f.e, "list.create", boards.ListCreateIn{BoardID: f.board, Name: "queue"})
			f.card(t, src.ID, "next up")
			s := call[*events.CardSnap](t, f.e, "card.next", NextIn{ListID: src.ID, Claim: true, MoveTo: lane})
			if s == nil {
				t.Fatal("card.next found nothing")
			}
			return s.ID
		}},
		{"card.draft.publish", func(t *testing.T) string {
			d := call[Card](t, f.e, "card.draft.create", DraftCreateIn{BoardID: f.board, Name: "idea"})
			return call[Card](t, f.e, "card.draft.publish", PublishIn{ID: d.ID, ListID: lane}).ID
		}},
		{"card.fromTemplate", func(t *testing.T) string {
			return call[Card](t, f.e, "card.fromTemplate", FromTemplateIn{ID: tpl.ID, ListID: lane}).ID
		}},
		{"card.duplicate (inside the column)", func(t *testing.T) string {
			var src string
			_ = f.e.DB.QueryRow(`SELECT id FROM cards WHERE list_id=? ORDER BY list_changed_at DESC LIMIT 1`, lane).Scan(&src)
			return call[Card](t, f.e, "card.duplicate", DuplicateIn{ID: src}).ID
		}},
		{"card.restore (into the full column)", func(t *testing.T) string {
			c := f.card(t, f.todo, "restored")
			mustExec(t, f.e, `UPDATE card_placements SET list_id=?, prev_list_id=?, list_changed_at='2020-01-01T00:00:00Z' WHERE card_id=? AND is_home=1`, f.arch, lane, c.ID)
			return call[Card](t, f.e, "card.restore", IDIn{c.ID}).ID
		}},
		{"recurrence spawn", func(t *testing.T) string {
			var src string
			_ = f.e.DB.QueryRow(`SELECT id FROM cards WHERE list_id=? ORDER BY list_changed_at DESC LIMIT 1`, lane).Scan(&src)
			call[Card](t, f.e, "card.due.set", DueIn{ID: src, DueDate: time.Now().UTC().AddDate(0, 0, -2).Format(time.RFC3339)})
			call[Card](t, f.e, "card.recurrence.set", RecurrenceIn{ID: src, Recurrence: `{"freq":"daily","count":2}`})
			if n, err := RunRecurrence(f.e.Ctx, f.e.DB, f.e.Hub, time.Now()); err != nil || n != 1 {
				t.Fatalf("recurrence spawned %d err %v", n, err)
			}
			var id string
			_ = f.e.DB.QueryRow(`SELECT id FROM cards WHERE list_id=? AND id<>? ORDER BY created_at DESC, id DESC LIMIT 1`, lane, src).Scan(&id)
			return id
		}},
		{"list.moveCards", func(t *testing.T) string {
			src := call[boards.List](t, f.e, "list.create", boards.ListCreateIn{BoardID: f.board, Name: "bulk"})
			return f.card(t, src.ID, "bulk moved").ID
		}},
	}
	for _, p := range paths {
		t.Run(p.name, func(t *testing.T) {
			old := fill(t)
			var before int
			_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE type='listUpdate' AND data LIKE '%"autoArchived"%'`).Scan(&before)
			id := p.land(t)
			if p.name == "list.moveCards" {
				var src string
				_ = f.e.DB.QueryRow(`SELECT list_id FROM cards WHERE id=?`, id).Scan(&src)
				call[boards.ListMoveCardsOut](t, f.e, "list.moveCards", boards.ListMoveCardsIn{ID: src, ListID: lane})
			}
			if id == "" {
				t.Fatal("no card")
			}
			var cnt int
			_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE list_id=? AND is_draft=0 AND is_template=0`, lane).Scan(&cnt)
			var at, prev string
			_ = f.e.DB.QueryRow(`SELECT list_id, COALESCE(prev_list_id,'') FROM cards WHERE id=?`, old[0]).Scan(&at, &prev)
			var here string
			_ = f.e.DB.QueryRow(`SELECT list_id FROM cards WHERE id=?`, id).Scan(&here)
			if cnt != keep || at != f.arch || prev != lane || here != lane {
				t.Fatalf("column holds %d (want %d); oldest in %s prev %s (want the archive, the column); landed card in %s", cnt, keep, at, prev, here)
			}
			var after, quiet int
			_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE type='listUpdate' AND data LIKE '%"autoArchived":1%'`).Scan(&after)
			_ = f.e.DB.QueryRow(`SELECT COUNT(*) FROM actions WHERE card_id=? AND etype='card.archived' AND data LIKE '%"quiet":true%' AND data LIKE '%"auto":"archiveAfter"%'`, old[0]).Scan(&quiet)
			if after != before+1 || quiet != 1 {
				t.Fatalf("events: summaries %d -> %d, quiet card events %d", before, after, quiet)
			}
		})
	}
}

// A claimed card is skipped; when the claim is released (card.release without a move) or the lease lapses (the
// sweeper), the column's rule runs again and the card may go.
func TestArchiveAfterClaimEnds(t *testing.T) {
	f := seed(t)
	lane := f.doing
	a, b := f.card(t, lane, "a"), f.card(t, lane, "b")
	mustExec(t, f.e, `UPDATE card_placements SET list_changed_at='2024-01-01T00:00:00Z' WHERE card_id=? AND is_home=1`, a.ID)
	mustExec(t, f.e, `UPDATE card_placements SET list_changed_at='2024-01-02T00:00:00Z' WHERE card_id=? AND is_home=1`, b.ID)
	call[Card](t, f.e, "card.claim", ClaimIn{ID: a.ID})
	call[Card](t, f.e, "card.claim", ClaimIn{ID: b.ID})
	one := 1
	// both cards over the limit are claimed: nothing to archive, so no confirm is needed
	call[boards.List](t, f.e, "list.update", boards.ListUpdateIn{ID: lane, ArchiveAfter: &one})
	p := call[boards.ListArchivePreview](t, f.e, "list.archivePreview", boards.ListArchivePreviewIn{ID: lane})
	if p.Keep != 1 || p.Cards != 2 || p.WillArchive != 0 || p.SkippedClaimed != 1 {
		t.Fatalf("preview %+v", p)
	}
	listOf := func(id string) string {
		var l string
		_ = f.e.DB.QueryRow(`SELECT list_id FROM cards WHERE id=?`, id).Scan(&l)
		return l
	}
	// card.next only offers unclaimed cards: nothing here
	if s := call[*events.CardSnap](t, f.e, "card.next", NextIn{ListID: lane}); s != nil {
		t.Fatalf("card.next offered %s", s.ID)
	}
	// release a: it is the oldest, so it goes now
	call[Card](t, f.e, "card.release", ReleaseIn{ID: a.ID})
	if listOf(a.ID) != f.arch || listOf(b.ID) != lane {
		t.Fatalf("after release: a in %s, b in %s", listOf(a.ID), listOf(b.ID))
	}
	// add a card: b is claimed, the new card just landed: the column sits at 2
	c := f.card(t, lane, "c")
	if listOf(c.ID) != lane || listOf(b.ID) != lane {
		t.Fatal("claimed or landed card archived")
	}
	// b's lease lapses: the sweeper releases it and enforces the rule (b is older than c)
	mustExec(t, f.e, `UPDATE cards SET claimed_until='2020-01-01T00:00:00Z' WHERE id=?`, b.ID)
	if n, err := SweepClaims(f.e.Ctx, f.e.DB, f.e.Hub, time.Now()); err != nil || n != 1 {
		t.Fatalf("sweep %d %v", n, err)
	}
	if listOf(b.ID) != f.arch || listOf(c.ID) != lane {
		t.Fatalf("after the sweep: b in %s, c in %s", listOf(b.ID), listOf(c.ID))
	}
}

// Recurring cards are never auto-archived (for now): the retroactive pass and a landing both skip them (they still
// count), the oldest NON-recurring card goes instead, the preview and the 409 name them, a card both claimed and
// recurring counts once (as claimed), and once the next occurrence spawns the old card gives up its rule and the
// spawn's own landing archives it.
func TestArchiveAfterSkipsRecurringCards(t *testing.T) {
	f := seed(t)
	lane := f.doing
	listOf := func(id string) string {
		var l string
		_ = f.e.DB.QueryRow(`SELECT list_id FROM cards WHERE id=?`, id).Scan(&l)
		return l
	}
	recurring := func(name, changed string) Card {
		c := f.card(t, lane, name)
		call[Card](t, f.e, "card.due.set", DueIn{ID: c.ID, DueDate: time.Now().UTC().AddDate(0, 0, -1).Format(time.RFC3339)})
		call[Card](t, f.e, "card.recurrence.set", RecurrenceIn{ID: c.ID, Recurrence: `{"freq":"daily","count":3}`})
		mustExec(t, f.e, `UPDATE card_placements SET list_changed_at=? WHERE card_id=? AND is_home=1`, changed, c.ID)
		return c
	}
	r1 := recurring("weekly report", "2024-01-01T00:00:00Z") // the two oldest cards recur
	r2 := recurring("monthly invoice", "2024-01-02T00:00:00Z")
	plain := f.card(t, lane, "plain")
	mustExec(t, f.e, `UPDATE card_placements SET list_changed_at='2024-01-03T00:00:00Z' WHERE card_id=? AND is_home=1`, plain.ID)
	newer := f.card(t, lane, "newer")
	mustExec(t, f.e, `UPDATE card_placements SET list_changed_at='2024-01-04T00:00:00Z' WHERE card_id=? AND is_home=1`, newer.ID)

	// retroactive pass, keep 1: excess 3, but r1 and r2 stay; plain and newer go (oldest non-recurring first). Two
	// cards remain, so one stays over the limit because it recurs
	p := call[boards.ListArchivePreview](t, f.e, "list.archivePreview", boards.ListArchivePreviewIn{ID: lane, ArchiveAfter: 1})
	if p.Cards != 4 || p.WillArchive != 2 || p.SkippedRecurring != 1 || p.SkippedClaimed != 0 {
		t.Fatalf("preview %+v", p)
	}
	one := 1
	_, err := ops.Call(f.e, "list.update", &boards.ListUpdateIn{ID: lane, ArchiveAfter: &one})
	if err == nil || !strings.Contains(err.Error(), "would archive 2 cards") || !strings.Contains(err.Error(), "1 recurring card stays over the limit") {
		t.Fatalf("409 must name the recurring cards: %v", err)
	}
	call[boards.List](t, f.e, "list.update", boards.ListUpdateIn{ID: lane, ArchiveAfter: &one, ConfirmArchive: true})
	if listOf(r1.ID) != lane || listOf(r2.ID) != lane || listOf(plain.ID) != f.arch || listOf(newer.ID) != f.arch {
		t.Fatalf("retroactive: r1 %s r2 %s plain %s newer %s", listOf(r1.ID), listOf(r2.ID), listOf(plain.ID), listOf(newer.ID))
	}
	// on landing: the column holds only recurring cards; a new card lands and nothing can go (the landed card is
	// protected, the others recur): the column sits at 3
	landed := f.card(t, lane, "landed")
	if listOf(landed.ID) != lane || listOf(r1.ID) != lane || listOf(r2.ID) != lane {
		t.Fatal("a landing archived a recurring card or the card that landed")
	}
	// the next landing archives the oldest non-recurring card (landed), never r1/r2
	f.card(t, lane, "another")
	if listOf(landed.ID) != f.arch || listOf(r1.ID) != lane || listOf(r2.ID) != lane {
		t.Fatalf("second landing: landed %s r1 %s r2 %s", listOf(landed.ID), listOf(r1.ID), listOf(r2.ID))
	}
	// a card both claimed and recurring counts once, as claimed: 3 cards, keep 1, "another" goes, one card stays over
	// the limit and it is reported once (claimed first), not as claimed and recurring
	call[Card](t, f.e, "card.claim", ClaimIn{ID: r1.ID})
	p = call[boards.ListArchivePreview](t, f.e, "list.archivePreview", boards.ListArchivePreviewIn{ID: lane})
	if p.Cards != 3 || p.WillArchive != 1 || p.SkippedClaimed != 1 || p.SkippedRecurring != 0 {
		t.Fatalf("claimed+recurring preview %+v", p)
	}
	call[Card](t, f.e, "card.release", ReleaseIn{ID: r1.ID})
	// the next occurrence spawns: r1 gives up its rule in the same transaction, so the spawn's landing archives it
	before := listOf(r2.ID)
	if n, err := RunRecurrence(f.e.Ctx, f.e.DB, f.e.Hub, time.Now()); err != nil || n != 2 {
		t.Fatalf("spawned %d (err %v); expected 2 (r1 and r2)", n, err)
	}
	if listOf(r1.ID) != f.arch || listOf(r2.ID) != f.arch || before != lane {
		t.Fatalf("after the spawn the old occurrences should be archivable: r1 %s r2 %s", listOf(r1.ID), listOf(r2.ID))
	}
}
