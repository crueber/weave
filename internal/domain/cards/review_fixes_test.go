package cards

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

func TestRenderMarkdownInlineSafety(t *testing.T) {
	got := string(RenderMarkdown("`a_b_c *d* [x](https://example.com)` and [my_link](https://example.com/a_b_c) and *mixed_ and *ok* and _also_"))
	if !strings.Contains(got, "<code>a_b_c *d* [x](https://example.com)</code>") {
		t.Fatalf("inline code mangled: %s", got)
	}
	if !strings.Contains(got, `href="https://example.com/a_b_c"`) {
		t.Fatalf("link url mangled: %s", got)
	}
	if strings.Contains(got, "<em>mixed") {
		t.Fatalf("mixed *foo_ became italic: %s", got)
	}
	if !strings.Contains(got, "<em>ok</em>") || !strings.Contains(got, "<em>also</em>") {
		t.Fatalf("valid italics missing: %s", got)
	}
}

func TestCardValidationAndShareFixes(t *testing.T) {
	f := seed(t)
	c := f.card(t, f.todo, "target")

	// NaN / Inf / oversized locationName
	nan, inf, okLng := math.NaN(), math.Inf(1), 10.0
	if _, err := ops.Typed[Card](f.e, "card.location.set", LocationIn{ID: c.ID, Lat: &nan, Lng: &okLng}); err == nil {
		t.Fatal("accepted NaN lat")
	}
	if _, err := ops.Typed[Card](f.e, "card.location.set", LocationIn{ID: c.ID, Lat: &inf, Lng: &okLng}); err == nil {
		t.Fatal("accepted +Inf lat")
	}
	okLat := 10.0
	if _, err := ops.Typed[Card](f.e, "card.location.set", LocationIn{ID: c.ID, Lat: &okLat, Lng: &okLng, Name: strings.Repeat("x", ops.MaxNameLen+1)}); err == nil {
		t.Fatal("accepted oversized locationName")
	}

	// Clearing dueDate while setting isDueCompleted=true
	due := "2026-12-01"
	call[Card](t, f.e, "card.update", UpdateIn{ID: c.ID, DueDate: &due})
	empty, yes := "", true
	if _, err := ops.Typed[Card](f.e, "card.update", UpdateIn{ID: c.ID, DueDate: &empty, IsDueCompleted: &yes}); err == nil {
		t.Fatal("accepted clearing dueDate with isDueCompleted=true")
	}

	// Drafts & templates cannot be closed or shared; draftCreate enforces description cap
	call[InboxOut](t, f.admin, "card.inbox.set", InboxSetIn{BoardID: f.board, Enabled: true})
	if _, err := ops.Typed[Card](f.e, "card.draft.create", DraftCreateIn{BoardID: f.board, Name: "big", Description: strings.Repeat("a", ops.MaxDescBytes+1)}); err == nil {
		t.Fatal("accepted oversized draft description")
	}
	dr := call[Card](t, f.e, "card.draft.create", DraftCreateIn{BoardID: f.board, Name: "  draft  "})
	if dr.Name != "draft" {
		t.Fatalf("draft name not trimmed: %q", dr.Name)
	}
	if _, err := ops.Typed[Card](f.e, "card.close", CloseIn{ID: dr.ID}); err == nil {
		t.Fatal("closed a draft")
	}
	if _, err := ops.Typed[ShareOut](f.e, "card.share.create", IDIn{ID: dr.ID}); err == nil {
		t.Fatal("shared a draft")
	}

	tpl := call[Card](t, f.e, "card.template.save", TemplateSaveIn{ID: c.ID, Name: "  My Tpl  "})
	if tpl.Name != "My Tpl" {
		t.Fatalf("template name not trimmed: %q", tpl.Name)
	}
	if _, err := ops.Typed[Card](f.e, "card.close", CloseIn{ID: tpl.ID}); err == nil {
		t.Fatal("closed a template")
	}
	if _, err := ops.Typed[ShareOut](f.e, "card.share.create", IDIn{ID: tpl.ID}); err == nil {
		t.Fatal("shared a template")
	}
	if _, err := ops.Typed[Card](f.e, "card.duplicate", DuplicateIn{ID: c.ID, Name: strings.Repeat("a", ops.MaxCardNameLen+1)}); err == nil {
		t.Fatal("accepted oversized duplicate name")
	}

	// card.edit.list filters out rows older than 10 minutes
	stale := time.Now().UTC().Add(-15 * time.Minute).Format(time.RFC3339)
	if _, err := f.e.DB.Exec(`INSERT INTO card_editing(card_id,user_id,field,touched_at) VALUES(?,?,?,?)`, c.ID, f.e.User.UserID, "description", stale); err != nil {
		t.Fatal(err)
	}
	el := call[EditListOut](t, f.e, "card.edit.list", IDIn{ID: c.ID})
	if len(el.Editors) != 0 {
		t.Fatalf("expected stale editor filtered out, got %+v", el.Editors)
	}

	// positionFor before first card with small position triggers rebalance instead of halving below 1
	p1 := f.card(t, f.doing, "first")
	p2 := f.card(t, f.doing, "second")
	if _, err := f.e.DB.Exec(`UPDATE card_placements SET position=1.0 WHERE card_id=? AND list_id=?`, p1.ID, f.doing); err != nil {
		t.Fatal(err)
	}
	moved := call[Card](t, f.e, "card.move", MoveIn{ID: p2.ID, ListID: f.doing, BeforeID: p1.ID})
	if moved.Position == nil || *moved.Position < 1 {
		t.Fatalf("expected rebalance so position >= 1, got %v", moved.Position)
	}
}

func TestShareMultiBoardAuthAndLabels(t *testing.T) {
	m := seedMB(t)
	c := m.card(t, m.todo, "shared")
	m.place(t, m.e, c.ID, m.list2)

	// Add a label on board1 and a label on board2
	call[Card](t, m.e, "card.label.add", LabelIn{ID: c.ID, LabelID: m.labelA, BoardID: m.board})
	call[Card](t, m.e, "card.label.add", LabelIn{ID: c.ID, LabelID: m.labelOther, BoardID: m.board2})

	// User who can edit board2 only (cannot edit home board1) cannot create/revoke share links
	b2only := m.user(t, "u-b2edit", map[string]string{m.board2: "editor"})
	if _, err := ops.Typed[ShareOut](b2only, "card.share.create", IDIn{ID: c.ID}); err == nil {
		t.Fatal("non-home editor created share link")
	}

	sh := call[ShareOut](t, m.e, "card.share.create", IDIn{ID: c.ID})
	if _, err := ops.Typed[Deleted](b2only, "card.share.revoke", IDIn{ID: c.ID}); err == nil {
		t.Fatal("non-home editor revoked share link")
	}

	// shareView shows only home board's labels
	anon := &ops.Env{Ctx: m.e.Ctx, DB: m.e.DB, Hub: m.e.Hub, Svc: m.e.Svc}
	v := call[SharedCard](t, anon, "card.share.view", ShareViewIn{Token: sh.Token})
	if len(v.Labels) != 1 {
		t.Fatalf("shareView leaked non-home labels: %+v", v.Labels)
	}

	// Direct draft with a share token is hidden from shareView (single-board card to preserve placement invariants)
	solo := m.card(t, m.todo, "solo-draft")
	tok := db.Token(16)
	if _, err := m.e.DB.Exec(`UPDATE cards SET is_draft=1, share_token=? WHERE id=?`, tok, solo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ops.Typed[SharedCard](anon, "card.share.view", ShareViewIn{Token: tok}); err == nil {
		t.Fatal("shareView returned a draft")
	}
}
