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

func ip(n int) *int { return &n }

// The soft WIP limit: validation, clearing (0 and empty), and that it travels with duplicates, templates, export/import.
func TestListWIPLimit(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "WIP")
	l := addList(t, e, b.ID, "Doing")
	if l.WIPLimit != nil {
		t.Fatal("new lists have no limit")
	}
	up := call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, WIPLimit: ip(3)})
	if up.WIPLimit == nil || *up.WIPLimit != 3 {
		t.Fatalf("set: %+v", up.WIPLimit)
	}
	// unrelated updates keep it
	nm := "Doing now"
	up = call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Name: &nm})
	if up.WIPLimit == nil || *up.WIPLimit != 3 {
		t.Fatal("rename must keep the limit")
	}
	mustFail(t, e, "list.update", boards.ListUpdateIn{ID: l.ID, WIPLimit: ip(-1)}, "validation")
	mustFail(t, e, "list.update", boards.ListUpdateIn{ID: l.ID, WIPLimit: ip(boards.MaxWIPLimit + 1)}, "validation")
	up = call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, WIPLimit: ip(boards.MaxWIPLimit)})
	if *up.WIPLimit != boards.MaxWIPLimit {
		t.Fatal("cap value is allowed")
	}
	up = call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, WIPLimit: ip(0)})
	if up.WIPLimit != nil {
		t.Fatal("0 clears the limit")
	}
	cr := call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Created", WIPLimit: ip(2)})
	if cr.WIPLimit == nil || *cr.WIPLimit != 2 {
		t.Fatal("create with limit")
	}
	mustFail(t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Bad", WIPLimit: ip(-5)}, "validation")

	// UI form: empty string clears (the sheet sends "" when the limit is switched off), a number sets
	m := mux(e)
	patch := func(vals url.Values) int {
		r := httptest.NewRequest("PATCH", "/ui/lists/"+l.ID, strings.NewReader(vals.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r)
		return w.Code
	}
	if c := patch(url.Values{"wipLimit": {"4"}}); c != 204 {
		t.Fatalf("ui set: %d", c)
	}
	if g, _ := boards.GetList(e.Ctx, e.DB, l.ID); g.WIPLimit == nil || *g.WIPLimit != 4 {
		t.Fatal("ui set not stored")
	}
	if c := patch(url.Values{"name": {"x"}}); c != 204 {
		t.Fatalf("ui rename: %d", c)
	}
	if g, _ := boards.GetList(e.Ctx, e.DB, l.ID); g.WIPLimit == nil || *g.WIPLimit != 4 {
		t.Fatal("a form without wipLimit must not change it")
	}
	if c := patch(url.Values{"wipLimit": {""}}); c != 204 {
		t.Fatalf("ui clear: %d", c)
	}
	if g, _ := boards.GetList(e.Ctx, e.DB, l.ID); g.WIPLimit != nil {
		t.Fatal("empty must clear")
	}
	if c := patch(url.Values{"wipLimit": {"abc"}}); c != 400 && c != 422 {
		t.Fatalf("garbage is a validation error, got %d", c)
	}

	// board.tree (agents) exposes it
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, WIPLimit: ip(5)})
	tree := call[boards.TreeOut](t, e, "board.tree", boards.TreeIn{ID: b.ID})
	found := false
	for _, tl := range tree.Lists {
		if tl.ID == l.ID && tl.WIPLimit != nil && *tl.WIPLimit == 5 {
			found = true
		}
	}
	if !found {
		t.Fatal("board.tree must carry wipLimit")
	}

	// template -> instantiate keeps it
	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: b.ID, IsTemplate: true})
	nb := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p2", TemplateID: b.ID, Name: "Inst"})
	if !hasLimit(t, e, nb.ID, "x", 5) {
		t.Fatalf("instantiate lost the limit: %+v", call[[]boards.List](t, e, "list.list", boards.ListListIn{BoardID: nb.ID}))
	}
	// export/import round trip
	st := call[*ops.Stream](t, e, "board.export", boards.ExportIn{ID: b.ID})
	if !strings.Contains(string(st.Data), `"wipLimit": 5`) && !strings.Contains(string(st.Data), `"wipLimit":5`) {
		t.Fatalf("export lacks wipLimit: %s", st.Data)
	}
	b2 := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: string(st.Data), Name: "Copy"})
	if !hasLimit(t, e, b2.ID, "x", 5) {
		t.Fatal("import lost the limit")
	}
	// untrusted file: out-of-range limits are dropped
	bad := strings.Replace(string(st.Data), `"wipLimit": 5`, `"wipLimit": 999999`, 1)
	b3 := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: bad, Name: "Bad"})
	for _, l := range call[[]boards.List](t, e, "list.list", boards.ListListIn{BoardID: b3.ID}) {
		if l.Name != nil && *l.Name == "x" && l.WIPLimit != nil {
			t.Fatalf("out-of-range import limit kept: %d", *l.WIPLimit)
		}
	}
}

func hasLimit(t *testing.T, e *ops.Env, boardID, name string, want int) bool {
	t.Helper()
	for _, l := range call[[]boards.List](t, e, "list.list", boards.ListListIn{BoardID: boardID}) {
		if l.Name != nil && *l.Name == name {
			return l.WIPLimit != nil && *l.WIPLimit == want
		}
	}
	return false
}

// The column sheet: fragment + shareable page, access rules, read-only rendering, header badge.
func TestListSheetRoutes(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Sheet")
	l := addList(t, e, b.ID, "Review")
	other := addList(t, e, b.ID, "Done")
	desc := "check the diff <b>carefully</b>"
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: l.ID, Description: &desc, WIPLimit: ip(1)})
	addCard(t, e, b.ID, l.ID, "c1", "one", 1, "")
	addCard(t, e, b.ID, l.ID, "c2", "two", 2, "")
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	m := mux(e)

	// fragment for an editor: forms, "Connect an agent" button, no card list, escaped description
	code, body := get(t, m, "/ui/lists/"+l.ID, true)
	if code != 200 {
		t.Fatalf("fragment: %d %s", code, body)
	}
	for _, want := range []string{`id="list-sheet"`, `class="ls cm"`, `data-ls-form="name"`, `data-ls-form="desc"`, `data-pop="closes"`, `/ui/lists/` + l.ID + `/type-preview?type=closed&amp;ctx=sheet`, `data-ls-form="wip"`, `data-ls-form="sort"`, `data-ls-form="move"`, `hx-get="/ui/agent-prompt?scope=list&amp;id=` + l.ID + `"`, `data-ls-delete`, `data-ls-archive`,
		`value="` + other.ID + `"`, "WIP 2/1, over the limit", `class="cards-badge ls-wipchip cards-badge-warn is-over"`, `data-ls-nav="` + other.ID + `"`, `hx-get="/ui/lists/` + l.ID + `/activity"`, `data-ls-desc-edit`, `cm-desc-view is-editable`, `&lt;b&gt;carefully&lt;/b&gt;`} {
		if !strings.Contains(body, want) {
			t.Errorf("sheet lacks %q", want)
		}
	}
	for _, bad := range []string{"boards-tile", "<b>carefully</b>", "<html"} {
		if strings.Contains(body, bad) {
			t.Errorf("sheet must not contain %q", bad)
		}
	}
	if strings.Contains(body, `value="`+l.ID+`"`) {
		t.Error("the column itself must not be a move target")
	}

	// shareable full page: the board with the sheet marker, view links stay on /boards/{id}
	code, body = get(t, m, "/lists/"+l.ID, false)
	if code != 200 || !strings.Contains(body, `id="open-list" data-list="`+l.ID+`"`) || !strings.Contains(body, `href="/boards/`+b.ID) {
		t.Fatalf("full page: %d", code)
	}
	if strings.Count(body, `id="open-list"`) != 1 {
		t.Error("marker once")
	}
	// board header: settings icon is a real button, WIP badge flags the column
	_, board := get(t, m, "/boards/"+b.ID, false)
	for _, want := range []string{`<button type="button" class="boards-menuitem boards-settings-item" role="menuitem" data-list-sheet="` + l.ID + `" data-cap="column-settings">`, `boards-wip is-over`} {
		if !strings.Contains(board, want) {
			t.Errorf("board lacks %q", want)
		}
	}
	if strings.Contains(board, "boards-col-settings") {
		t.Error("the column header has no settings icon any more (it moved into the menu)")
	}
	for _, gone := range []string{"Collapse list</button>", "data-collapse-btn", "List settings", `>Sort cards<`, "data-add-top", "boards-col-add", "data-rename-btn", "Rename list", "Add card to top", "boards-col-desc", ">Column settings&hellip;<"} {
		if strings.Contains(board, gone) {
			t.Errorf("menu still has %q", gone)
		}
	}

	// read-only member: sheet renders without controls
	_, body = get(t, mux(testutil.As(e, viewer)), "/ui/lists/"+l.ID, true)
	if !strings.Contains(body, `id="list-sheet"`) || strings.Contains(body, "data-ls-form=") || !strings.Contains(body, "/ui/agent-prompt?scope=list") || strings.Contains(body, "data-ls-delete") || strings.Contains(body, "data-ls-desc-edit") || strings.Contains(body, "cm-menu") {
		t.Errorf("read-only sheet shows controls: %s", body)
	}
	// outsiders and system lists are not_found
	if code, _ = get(t, mux(testutil.As(e, nobody)), "/ui/lists/"+l.ID, true); code != 404 {
		t.Errorf("outsider fragment: %d", code)
	}
	if code, _ = get(t, mux(testutil.As(e, nobody)), "/lists/"+l.ID, false); code != 404 {
		t.Errorf("outsider page: %d", code)
	}
	arch, _ := boards.SystemList(e.Ctx, e.DB, b.ID, "archive")
	for _, p := range []string{"/ui/lists/" + arch, "/lists/" + arch, "/ui/lists/nope"} {
		if code, _ = get(t, m, p, true); code != 404 {
			t.Errorf("GET %s: %d, want 404", p, code)
		}
	}
	mustFail(t, testutil.As(e, nobody), "list.get", boards.ListRefIn{ID: l.ID}, "not_found")
	mustFail(t, e, "list.get", boards.ListRefIn{ID: arch}, "not_found")
}

// list.activity: column-scoped events (moves in/out, column changes), access rules, paging.
func TestListActivity(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Act")
	a := addList(t, e, b.ID, "Todo")
	c := addList(t, e, b.ID, "Doing")
	addCard(t, e, b.ID, a.ID, "c1", "one", 1, "")
	addCard(t, e, b.ID, a.ID, "c2", "two", 2, "")
	nm := "Backlog"
	call[boards.List](t, e, "list.update", boards.ListUpdateIn{ID: a.ID, Name: &nm})
	call[boards.ListMoveCardsOut](t, e, "list.moveCards", boards.ListMoveCardsIn{ID: a.ID, ListID: c.ID})

	into := call[[]boards.ListActivityItem](t, e, "list.activity", boards.ListActivityIn{ID: c.ID})
	var moved int
	for _, it := range into {
		if it.Type == "card.moved" && strings.Contains(it.Summary, "here from “Backlog”") && it.Actor == admin.Name {
			moved++
		}
	}
	if moved != 2 {
		t.Fatalf("target column should list 2 moves in, got %d: %+v", moved, into)
	}
	out := call[[]boards.ListActivityItem](t, e, "list.activity", boards.ListActivityIn{ID: a.ID})
	var left, upd, created bool
	for _, it := range out {
		left = left || (it.Type == "card.moved" && strings.Contains(it.Summary, "to “Doing”"))
		upd = upd || it.Summary == "updated this column"
		created = created || it.Summary == "created this column"
	}
	if !left || !upd || !created {
		t.Fatalf("source column feed incomplete (left=%v updated=%v created=%v): %+v", left, upd, created, out)
	}
	// newest first, paged by the event-id cursor
	if len(out) < 3 {
		t.Fatalf("feed too short: %+v", out)
	}
	one := call[[]boards.ListActivityItem](t, e, "list.activity", boards.ListActivityIn{ID: a.ID, Limit: 1})
	rest := call[[]boards.ListActivityItem](t, e, "list.activity", boards.ListActivityIn{ID: a.ID, Limit: 100, Before: one[0].ID})
	if len(one) != 1 || one[0].ID != out[0].ID || len(rest) != len(out)-1 || rest[0].ID != out[1].ID {
		t.Fatalf("paging broke: %+v / %+v", one, rest)
	}
	mustFail(t, e, "list.activity", boards.ListActivityIn{ID: a.ID, Before: "x"}, "validation")

	// access: outsiders and system lists are not_found; the HTML feed renders for members
	mustFail(t, testutil.As(e, nobody), "list.activity", boards.ListActivityIn{ID: a.ID}, "not_found")
	arch, _ := boards.SystemList(e.Ctx, e.DB, b.ID, "archive")
	mustFail(t, e, "list.activity", boards.ListActivityIn{ID: arch}, "not_found")
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	m := mux(testutil.As(e, viewer))
	code, body := get(t, m, "/ui/lists/"+c.ID+"/activity", true)
	if code != 200 || !strings.Contains(body, "moved") || !strings.Contains(body, "Ada Admin") || strings.Contains(body, "<html") {
		t.Fatalf("activity fragment: %d %s", code, body)
	}
	if code, _ = get(t, mux(testutil.As(e, nobody)), "/ui/lists/"+c.ID+"/activity", true); code != 404 {
		t.Errorf("outsider fragment: %d", code)
	}
}
