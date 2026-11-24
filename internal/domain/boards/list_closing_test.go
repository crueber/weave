package boards_test

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// The column setting "Close cards that land here" is lists.type closed. list.update's optional existingCards decides
// what happens to the cards already in the column: flip (default) closes / re-opens them, keep leaves them alone.
func TestListClosingSettingExistingCards(t *testing.T) {
	e := fixture(t)
	var evs []ops.Event
	prev := ops.EmitHook
	ops.EmitHook = func(_ *ops.Env, ev ops.Event) { evs = append(evs, ev) }
	t.Cleanup(func() { ops.EmitHook = prev })
	b := newBoard(t, e, "Closing")
	l := addList(t, e, b.ID, "Done")
	addCard(t, e, b.ID, l.ID, "k1", "K1", 1, "")
	addCard(t, e, b.ID, l.ID, "k2", "K2", 2, "")
	closed, active := "closed", "active"
	cardEvents := func() int {
		n := 0
		for _, ev := range evs {
			if ev.Type == "cardUpdate" {
				n++
			}
		}
		return n
	}
	listEvent := func() map[string]any {
		for _, ev := range evs {
			if ev.Type == "listUpdate" {
				return ev.Data
			}
		}
		return nil
	}

	// keep: the setting goes on, the cards already here stay open, no card events
	evs = nil
	out := call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Type: &closed, ExistingCards: "keep"})
	if out.Type != "closed" || isClosed(t, e, "k1") || isClosed(t, e, "k2") || cardEvents() != 0 {
		t.Fatalf("keep: type %s k1 %v k2 %v events %d", out.Type, isClosed(t, e, "k1"), isClosed(t, e, "k2"), cardEvents())
	}
	if d := listEvent(); d == nil || d["closes"] != true || d["existingCards"] != "keep" || d["closed"] != 0 {
		t.Fatalf("keep: list event %v", d)
	}
	// ... while a card that lands from now on is closed
	addCard(t, e, b.ID, addList(t, e, b.ID, "Todo").ID, "new", "New", 1, "")
	if _, err := ops.Call(e, "list.moveCards", &boards.ListMoveCardsIn{ID: listIDOf(t, e, b.ID, "Todo"), ListID: l.ID}); err != nil {
		t.Fatal(err)
	}
	if !isClosed(t, e, "new") {
		t.Fatal("a card that lands after the switch is closed")
	}
	// later edits of the column (rename, WIP, colour, description, existingCards with no type change) never re-land
	nm, desc, col := "Finished", "done work", "green"
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Name: &nm, WIPLimit: ip(5), Color: &col, Description: &desc})
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Type: &closed, ExistingCards: "flip"})
	if isClosed(t, e, "k1") || isClosed(t, e, "k2") {
		t.Fatal("kept cards must stay open through later column edits (and existingCards is ignored without a type change)")
	}

	// off with flip (the default when existingCards is omitted: backward compatible): closed cards re-open
	evs = nil
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Type: &active})
	if isClosed(t, e, "new") || cardEvents() != 1 {
		t.Fatalf("off/flip: new closed=%v events %d", isClosed(t, e, "new"), cardEvents())
	}
	if d := listEvent(); d["closes"] != false || d["reopened"] != 1 || d["existingCards"] != "flip" {
		t.Fatalf("off/flip list event %v", d)
	}
	// on with flip: every open card closes, one quiet event each
	evs = nil
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Type: &closed})
	if !isClosed(t, e, "k1") || !isClosed(t, e, "k2") || !isClosed(t, e, "new") || cardEvents() != 3 {
		t.Fatalf("on/flip: events %d", cardEvents())
	}
	// off with keep: the cards keep their check marks
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Type: &active, ExistingCards: "keep"})
	if !isClosed(t, e, "k1") {
		t.Fatal("off/keep must leave closed cards closed")
	}
	// the JSON field name the API, CLI and MCP use; an invalid value is rejected before anything is written
	if _, err := ops.CallMap(e, "list.update", map[string]any{"id": l.ID, "type": "closed", "existingCards": "keep"}); err != nil {
		t.Fatalf("existingCards over the API: %v", err)
	}
	mustFail(t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Type: &active, ExistingCards: "sometimes"}, "validation")
	if got := call[boards.List](t, e, "list.get", boards.ListRefIn{ID: l.ID}); got.Type != "closed" {
		t.Fatalf("an invalid existingCards must not switch the type: %s", got.Type)
	}
}

func listIDOf(t *testing.T, e *ops.Env, boardID, name string) string {
	t.Helper()
	var id string
	if err := e.DB.QueryRow(`SELECT id FROM lists WHERE board_id=? AND name=?`, boardID, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// list.typePreview counts what a switch would flip; read-only; Edit access; system lists are not_found.
func TestListTypePreview(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Preview")
	l := addList(t, e, b.ID, "Review")
	addCard(t, e, b.ID, l.ID, "p1", "P1", 1, "")
	addCard(t, e, b.ID, l.ID, "p2", "P2", 2, "")
	addCard(t, e, b.ID, l.ID, "p3", "P3", 3, "")
	addCard(t, e, b.ID, l.ID, "pd", "Draft", 4, "")
	if _, err := e.DB.Exec(`UPDATE cards SET is_draft=1 WHERE id='pd'`); err != nil {
		t.Fatal(err)
	}
	setClosed(t, e, "p3", true)
	p := call[boards.ListTypePreview](t, e, "list.typePreview", boards.ListTypePreviewIn{ID: l.ID})
	if p.Type != "closed" || p.CurrentType != "active" || p.Cards != 3 || p.WillClose != 2 || p.WillReopen != 0 || p.Name != "Review" {
		t.Fatalf("default preview (toggle to closed): %+v", p)
	}
	if p := call[boards.ListTypePreview](t, e, "list.typePreview", boards.ListTypePreviewIn{ID: l.ID, Type: "active"}); p.Changes() != 0 {
		t.Fatalf("same type changes nothing: %+v", p)
	}
	closed := "closed"
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Type: &closed})
	if p := call[boards.ListTypePreview](t, e, "list.typePreview", boards.ListTypePreviewIn{ID: l.ID}); p.Type != "active" || p.WillReopen != 3 || p.WillClose != 0 {
		t.Fatalf("preview to active: %+v", p)
	}
	mustFail(t, e, "list.typePreview", boards.ListTypePreviewIn{ID: l.ID, Type: "archive"}, "validation")
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	if _, err := ops.Call(testutil.As(e, viewer), "list.typePreview", &boards.ListTypePreviewIn{ID: l.ID}); err == nil {
		t.Fatal("a viewer may not preview a setting change")
	}
	mustFail(t, testutil.As(e, nobody), "list.typePreview", boards.ListTypePreviewIn{ID: l.ID}, "not_found")
	arch, _ := boards.SystemList(e.Ctx, e.DB, b.ID, "archive")
	mustFail(t, e, "list.typePreview", boards.ListTypePreviewIn{ID: arch}, "not_found")
}

// The UI: the sheet's "Close cards" control, the confirm with the exact count (flip by default, keep as the other
// choice), the column menu item, the header indicator, the read-only sheet, the column activity and the gallery.
func TestListClosingSettingUI(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Closing UI")
	l := addList(t, e, b.ID, "Done")
	empty := addList(t, e, b.ID, "Empty")
	for i, id := range []string{"u1", "u2", "u3", "u4", "u5", "u6", "u7", "u8", "u9", "u10", "u11", "u12"} {
		addCard(t, e, b.ID, l.ID, id, "Card "+id, float64(i+1), "")
	}
	m := mux(e)
	mustGet := func(path string, wants ...string) string {
		t.Helper()
		code, body := get(t, m, path, true)
		if code != 200 {
			t.Fatalf("GET %s: %d %s", path, code, body)
		}
		for _, w := range wants {
			if !strings.Contains(body, w) {
				t.Errorf("GET %s lacks %q", path, w)
			}
		}
		return body
	}
	// the sheet: the setting with its help text, off; the stale type wording is gone
	sheet := mustGet("/ui/lists/"+l.ID, `id="ls-pl-closes">Auto-close</span>`, "Close cards that land here",
		"Cards moved or added to this column are marked closed (with a check mark); moving them to another column re-opens them.",
		`data-closes-opt="closed" hx-get="/ui/lists/`+l.ID+`/type-preview?type=closed&amp;ctx=sheet"`, `id="ls-closes-confirm"`, `data-ls-type-val>Off</span>`)
	for _, gone := range []string{"Closed columns hold finished work", "Active column", "Closed column", `data-prop="type"`} {
		if strings.Contains(sheet, gone) {
			t.Errorf("sheet still says %q", gone)
		}
	}
	// the confirm: exact count, flip is the default (focused) choice, keep the other; sheet forms update the sheet
	conf := mustGet("/ui/lists/"+l.ID+"/type-preview?type=closed&ctx=sheet", "Close 12 cards now?", "12 open cards are already in Done. Either way",
		`name="existingCards" value="flip"><button class="primary" data-closes-default autofocus>Close 12 cards</button>`,
		`name="existingCards" value="keep"><button>Leave them open</button>`, `data-ls-form="type" data-type="closed"`, "data-closes-cancel")
	if strings.Contains(conf, `hx-trigger="load"`) {
		t.Error("a column with cards must ask, not apply at once")
	}
	// one card: singular wording
	one := addList(t, e, b.ID, "One")
	addCard(t, e, b.ID, one.ID, "solo", "Solo", 1, "")
	mustGet("/ui/lists/"+one.ID+"/type-preview?type=closed&ctx=sheet", "Close 1 card now?", "1 open card is already in One.", ">Close 1 card</button>")
	// an empty column: no question, a form that applies the setting when it loads
	mustGet("/ui/lists/"+empty.ID+"/type-preview?type=closed&ctx=sheet", `hx-trigger="load"`, `name="type" value="closed"`, `data-ls-form="type"`)
	// the same value: nothing to do
	if body := mustGet("/ui/lists/" + l.ID + "/type-preview?type=active&ctx=sheet"); strings.TrimSpace(body) != "" {
		t.Errorf("same type renders nothing, got %q", body)
	}
	// the menu flavour: items are menu items, no sheet hooks
	menu := mustGet("/ui/lists/"+l.ID+"/type-preview?type=closed&ctx=menu", "Close 12 cards now?", `role="menuitem"`)
	if strings.Contains(menu, "data-ls-form") {
		t.Error("the menu confirm must not carry the sheet's form hooks")
	}

	// apply through the UI route the confirm posts to (keep), then the board header and menu show the setting
	patch := func(vals url.Values) {
		t.Helper()
		r := httptest.NewRequest("PATCH", "/ui/lists/"+l.ID, strings.NewReader(vals.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		if w.Code >= 300 {
			t.Fatalf("PATCH %v: %d %s", vals, w.Code, w.Body.String())
		}
	}
	patch(url.Values{"type": {"closed"}, "existingCards": {"keep"}})
	if isClosed(t, e, "u1") {
		t.Fatal("keep from the UI left the cards open")
	}
	_, board := get(t, m, "/boards/"+b.ID, false)
	for _, want := range []string{`class="boards-col-closes" role="img" aria-label="Closes cards that land here"`,
		`class="boards-menuitem boards-closes-item is-on" role="menuitemcheckbox" aria-checked="true"`,
		`/ui/lists/` + l.ID + `/type-preview?type=active&amp;ctx=menu`, `id="lcc-` + l.ID + `"`,
		`class="boards-menuitem boards-closes-item" role="menuitemcheckbox" aria-checked="false"`} {
		if !strings.Contains(board, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	if strings.Contains(board, `<small class="boards-badge">closed</small>`) {
		t.Error("the plain text badge is gone")
	}
	// the confirm to switch it off counts the closed cards (none: they were kept open) -> applies at once
	mustGet("/ui/lists/"+l.ID+"/type-preview?type=active&ctx=menu", `hx-trigger="load"`)
	patch(url.Values{"type": {"active"}})
	patch(url.Values{"type": {"closed"}}) // flip (default): closes the 12
	if !isClosed(t, e, "u12") {
		t.Fatal("flip from the UI closes the cards")
	}
	mustGet("/ui/lists/"+l.ID+"/type-preview?type=active&ctx=sheet", "Re-open 12 cards?", "12 closed cards are in Done.", ">Re-open 12 cards</button>", ">Keep them closed</button>")
	mustGet("/ui/lists/"+l.ID, `data-ls-type-val><svg`, ">On</span>", `class="cards-badge cards-badge-closed" data-ls-type`)
	// the column activity names what happened
	mustGet("/ui/lists/"+l.ID+"/activity?limit=10", "set this column to close cards that land here and closed the 12 cards in it",
		"stopped closing cards that land here", "set this column to close cards that land here")

	// read-only member: the value, no controls; outsiders get not_found for the confirm
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	_, ro := get(t, mux(testutil.As(e, viewer)), "/ui/lists/"+l.ID, true)
	if !strings.Contains(ro, `data-pl="Auto-close"`) || strings.Contains(ro, "type-preview") {
		t.Errorf("read-only sheet: %s", ro)
	}
	// a viewer sees the board but may not edit it (forbidden); an outsider must not learn the column exists (not_found)
	for who, want := range map[*ops.Principal]int{viewer: 403, nobody: 404} {
		if code, _ := get(t, mux(testutil.As(e, who)), "/ui/lists/"+l.ID+"/type-preview?type=active", true); code != want {
			t.Errorf("%s confirm: %d, want %d", who.UserID, code, want)
		}
	}

	// the template gallery: a built-in template's closing column shows the mark and the note
	mustGet("/ui/board-templates/builtin:bug-tracker?projectId=p1", `class="tg-closes" role="img" aria-label="Closes cards that land here"`, "Columns that close cards")
}
