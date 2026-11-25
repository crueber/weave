package cards

import (
	"bytes"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crueber/weave/internal/ops"
)

func archiveUIRowHTML(t *testing.T, body, id string) string {
	t.Helper()
	start := `<tr class="archive-row" data-card-id="` + id + `"`
	i := strings.Index(body, start)
	if i < 0 {
		t.Fatalf("archive row %s missing from %s", id, body)
	}
	rest := body[i:]
	j := strings.Index(rest, "</tr>")
	if j < 0 {
		t.Fatalf("archive row %s is not closed", id)
	}
	return rest[:j]
}

func archiveUIColgroup(t *testing.T, body string) string {
	t.Helper()
	start := strings.Index(body, `<colgroup class="archive-columns">`)
	if start < 0 {
		t.Fatalf("archive colgroup missing from %s", body)
	}
	rest := body[start:]
	end := strings.Index(rest, "</colgroup>")
	if end < 0 {
		t.Fatalf("archive colgroup is not closed: %s", rest)
	}
	return rest[:end]
}

func archiveUIForm(t *testing.T, body, id string) string {
	t.Helper()
	start := `id="` + id + `"`
	i := strings.Index(body, start)
	if i < 0 {
		t.Fatalf("form %s missing from %s", id, body)
	}
	// Include the opening tag so form action/target assertions can use this helper.
	formStart := strings.LastIndex(body[:i], "<form")
	if formStart < 0 {
		t.Fatalf("form %s has no opening tag", id)
	}
	rest := body[formStart:]
	j := strings.Index(rest, "</form>")
	if j < 0 {
		t.Fatalf("form %s is not closed", id)
	}
	return rest[:j]
}

var archiveUILoadMoreRE = regexp.MustCompile(`id="archive-load-more"[^>]*hx-get="([^"]+)"`)

func archiveUILoadMore(t *testing.T, body string) string {
	t.Helper()
	m := archiveUILoadMoreRE.FindStringSubmatch(body)
	if len(m) != 2 {
		t.Fatalf("load-more URL missing from %s", body)
	}
	return html.UnescapeString(m[1])
}

func archiveUIIDs(body string) []string {
	matches := regexp.MustCompile(`data-card-id="([^"]+)"`).FindAllStringSubmatch(body, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

func archiveUIJSONPost(t *testing.T, m http.Handler, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(payload))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w
}

func TestArchiveUIRoutesAndCapabilities(t *testing.T) {
	for _, tc := range []struct{ op, ui string }{
		{"card.archive.list", "GET /ui/boards/{boardId}/archive"},
		{"card.restore.bulk", "POST /ui/boards/{boardId}/archive/restore"},
		{"card.delete.bulk", "POST /ui/boards/{boardId}/archive/delete"},
	} {
		if got := ops.Get(tc.op).UI; got != tc.ui {
			t.Errorf("%s UI = %q, want %q", tc.op, got, tc.ui)
		}
	}

	f := seed(t)
	a := archiveViewCard(t, f, f.doing, "Archived <card>", "2026-01-02T00:00:00Z")
	call[Card](t, f.e, "card.label.add", LabelIn{ID: a.ID, LabelID: f.labelA})
	call[Card](t, f.e, "card.member.add", MemberIn{ID: a.ID, UserID: f.vw})
	trashed := f.card(t, f.todo, "Trashed card")
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: trashed.ID, Trash: true})

	editor := mount(f, f.e)
	w := do(editor, http.MethodGet, "/ui/boards/"+f.board+"/archive?show=archived&view=list", nil)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("archive response = %d %s: %s", w.Code, w.Header().Get("Content-Type"), body)
	}
	for _, want := range []string{
		`id="archive-view" class="archive-view" data-kind="archived" data-board-id="b1" data-total="1" data-can-restore="true"`,
		`id="archive-total">1</strong>`, `class="archive-month"`, "January 2026", `data-open-card="` + a.ID + `"`,
		`href="/cards/` + a.ID + `?board=b1"`, "Archived &lt;card&gt;", "l-doing", "High", "VE",
		`data-archive-check="card" data-cap="restoreArchived"`, `id="archive-bulk"`, `data-archive-bulk="restore"`,
		`<th>Actions</th>`, `hx-target="#board-body" hx-swap="innerHTML"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("editor archive lacks %q: %s", want, body)
		}
	}
	if row := archiveUIRowHTML(t, body, a.ID); !strings.Contains(row, `name="cardIds" value="[&#34;`+a.ID+`&#34;]"`) {
		t.Errorf("row action does not carry a single JSON card id: %s", row)
	}
	if strings.Contains(body, `name="label"`) || strings.Contains(body, `name="member"`) || strings.Contains(body, `name="due"`) {
		t.Errorf("archive fragment duplicated the visible common filters: %s", body)
	}
	filters := archiveUIForm(t, body, "archive-filters")
	if !strings.Contains(filters, `hx-get="/ui/boards/b1/archive"`) || !strings.Contains(filters, `hx-target="#board-body"`) || !strings.Contains(filters, `hx-swap="innerHTML"`) {
		t.Errorf("archive filter HTMX contract: %s", filters)
	}
	if strings.Contains(filters, "hx-push-url") {
		t.Errorf("archive facet form must not push its /ui URL: %s", filters)
	}

	w = do(mount(f, f.viewer), http.MethodGet, "/ui/boards/"+f.board+"/archive?show=archived", nil)
	body = w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, `data-can-restore="false"`) || !strings.Contains(body, "Archived &lt;card&gt;") {
		t.Fatalf("viewer archive = %d: %s", w.Code, body)
	}
	for _, forbidden := range []string{`data-archive-check=`, `id="archive-bulk"`, `data-archive-bulk=`, `class="archive-row-action"`, `data-cap="restoreArchived"`, `<th>Actions</th>`} {
		if strings.Contains(body, forbidden) {
			t.Errorf("viewer archive exposes mutation control %q", forbidden)
		}
	}
	if w = do(mount(f, f.stranger), http.MethodGet, "/ui/boards/"+f.board+"/archive?show=archived", nil); w.Code != http.StatusNotFound {
		t.Errorf("outsider archive = %d, want 404: %s", w.Code, w.Body.String())
	}

	w = do(editor, http.MethodGet, "/ui/boards/"+f.board+"/archive?show=trash", nil)
	body = w.Body.String()
	row := archiveUIRowHTML(t, body, trashed.ID)
	if w.Code != http.StatusOK || !strings.Contains(body, `data-kind="trash"`) || !strings.Contains(row, "Delete forever") || !strings.Contains(row, "Restore") {
		t.Fatalf("trash row actions = %d: %s", w.Code, row)
	}
	if !strings.Contains(row, `hx-confirm="Delete this card forever? This cannot be undone."`) || !strings.Contains(row, `data-confirm-title="Delete forever?"`) {
		t.Errorf("trash delete confirmation missing: %s", row)
	}
}

func TestArchiveUITableLayoutMarkup(t *testing.T) {
	f := seed(t)
	archived := archiveViewCard(t, f, f.todo, "Layout archived", "2026-01-02T00:00:00Z")
	call[Card](t, f.e, "card.place", PlaceIn{ID: archived.ID, ListID: f.list2})
	call[Card](t, f.e, "card.label.add", LabelIn{ID: archived.ID, LabelID: f.labelA, BoardID: f.board})
	call[Card](t, f.e, "card.member.add", MemberIn{ID: archived.ID, UserID: f.vw})
	trashed := f.card(t, f.todo, "Layout trashed")
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: trashed.ID, Trash: true})

	for _, tc := range []struct {
		name, show, id, dateLabel, cardName, from, alsoTitle string
		who                                                  *ops.Env
		columns, cells                                       int
		actions                                              bool
	}{
		{name: "editor archive", show: "archived", id: archived.ID, dateLabel: "Archived", cardName: "Layout archived", from: "l-todo", alsoTitle: "Also active on b2.", who: f.e, columns: 7, cells: 7, actions: true},
		{name: "viewer archive", show: "archived", id: archived.ID, dateLabel: "Archived", cardName: "Layout archived", from: "l-todo", who: f.viewer, columns: 5, cells: 5},
		{name: "editor trash", show: "trash", id: trashed.ID, dateLabel: "Trashed", cardName: "Layout trashed", from: "l-todo", who: f.e, columns: 7, cells: 7, actions: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(mount(f, tc.who), http.MethodGet, "/ui/boards/"+f.board+"/archive?show="+tc.show, nil)
			body := w.Body.String()
			if w.Code != http.StatusOK {
				t.Fatalf("archive response = %d: %s", w.Code, body)
			}
			for _, want := range []string{
				`<div class="archive-tablewrap">`, `<table class="archive-table">`, `<colgroup class="archive-columns">`,
				`<col class="archive-col-card">`, `<col class="archive-col-from">`, `<col class="archive-col-date">`,
				`<col class="archive-col-labels">`, `<col class="archive-col-members">`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("archive table lacks %q: %s", want, body)
				}
			}
			colgroup := archiveUIColgroup(t, body)
			if got := strings.Count(colgroup, `<col `); got != tc.columns {
				t.Errorf("archive colgroup has %d columns, want %d: %s", got, tc.columns, colgroup)
			}
			for _, want := range []string{`<col class="archive-col-select">`, `<col class="archive-col-actions">`} {
				if got := strings.Contains(colgroup, want); got != tc.actions {
					t.Errorf("archive colgroup contains %q = %t, want %t: %s", want, got, tc.actions, colgroup)
				}
			}

			row := archiveUIRowHTML(t, body, tc.id)
			if got := strings.Count(row, `<td`); got != tc.cells {
				t.Errorf("archive row has %d cells, want %d: %s", got, tc.cells, row)
			}
			for _, want := range []string{
				`<td class="archive-labels"><div class="archive-cell">`,
				`<td class="archive-members"><div class="archive-cell">`,
				`<td class="archive-date" data-label="` + tc.dateLabel + `">`,
				`<a class="archive-card-link" href="/cards/` + tc.id + `?board=b1" title="` + tc.cardName + `" data-open-card="` + tc.id + `">`,
				`<td class="archive-from" title="` + tc.from + `">`,
			} {
				if !strings.Contains(row, want) {
					t.Errorf("archive row lacks %q: %s", want, row)
				}
			}
			if tc.alsoTitle != "" {
				if want := `<div class="archive-also" title="` + tc.alsoTitle + `">`; !strings.Contains(row, want) {
					t.Errorf("archive row lacks also-active tooltip %q: %s", want, row)
				}
			} else if strings.Contains(row, `class="archive-also"`) {
				t.Errorf("archive row unexpectedly names another active board: %s", row)
			}
			actionWrapper := `<td class="archive-actions"><div class="archive-cell archive-cell-actions">`
			if got := strings.Contains(row, actionWrapper); got != tc.actions {
				t.Errorf("archive row action wrapper = %t, want %t: %s", got, tc.actions, row)
			}
			month := `<tr class="archive-month"><th scope="row" colspan="` + strconv.Itoa(tc.columns) + `">`
			if !strings.Contains(body, month) {
				t.Errorf("archive month heading lacks %q: %s", month, body)
			}
		})
	}
}

func TestArchiveUIMonthGroupingOnlyForArchivedSort(t *testing.T) {
	f := seed(t)
	jan := archiveViewCard(t, f, f.todo, "Alpha", "2026-01-15T12:00:00Z")
	feb := archiveViewCard(t, f, f.doing, "Beta", "2026-02-15T12:00:00Z")
	janAgain := archiveViewCard(t, f, f.todo, "Gamma", "2026-01-15T12:00:00Z")
	m := mount(f, f.e)
	base := "/ui/boards/" + f.board + "/archive?show=archived&view=list"

	w := do(m, http.MethodGet, base, nil)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("default archive = %d: %s", w.Code, body)
	}
	if got := strings.Count(body, `class="archive-month"`); got != 2 {
		t.Errorf("default archive has %d month headings, want 2: %s", got, body)
	}
	for _, want := range []string{"January 2026", "February 2026"} {
		if got := strings.Count(body, want); got != 1 {
			t.Errorf("default archive has %d %q headings, want 1: %s", got, want, body)
		}
	}

	for _, sortBy := range []string{"name", "from"} {
		w := do(m, http.MethodGet, base+"&sort="+sortBy, nil)
		body := w.Body.String()
		if w.Code != http.StatusOK {
			t.Fatalf("%s-sorted archive = %d: %s", sortBy, w.Code, body)
		}
		if strings.Contains(body, `class="archive-month"`) {
			t.Errorf("%s-sorted archive has month headings: %s", sortBy, body)
		}
		for _, id := range []string{jan.ID, feb.ID, janAgain.ID} {
			if !strings.Contains(body, `data-card-id="`+id+`"`) {
				t.Errorf("%s-sorted archive is missing card %s: %s", sortBy, id, body)
			}
		}
	}
}

func TestArchiveRowsUseLocalMonth(t *testing.T) {
	previousLocal := time.Local
	time.Local = time.FixedZone("archive-test", -8*60*60)
	t.Cleanup(func() { time.Local = previousLocal })

	items := []ArchiveListItem{
		{ID: "a", ArchivedAt: "2026-01-31T15:30:00-08:00"},
		{ID: "b", ArchivedAt: "2026-01-31T16:30:00-08:00"},
		{ID: "c", ArchivedAt: "2026-02-01T00:30:00-08:00"},
	}
	rows := archiveRows(items, "archived", "")
	want := []struct {
		month    string
		newMonth bool
	}{
		{"January 2026", true},
		{"January 2026", false},
		{"February 2026", true},
	}
	if len(rows) != len(want) {
		t.Fatalf("archiveRows returned %d rows, want %d", len(rows), len(want))
	}
	for i, row := range rows {
		if row.Month != want[i].month || row.NewMonth != want[i].newMonth {
			t.Errorf("row %d month = %q, new = %t; want %q, %t", i, row.Month, row.NewMonth, want[i].month, want[i].newMonth)
		}
	}
	if title := archiveDate(items[1].ArchivedAt); !strings.HasPrefix(title, "Jan 31, 2026") {
		t.Errorf("local date title = %q, want Jan 31, 2026", title)
	}
}

func TestArchiveUICursorPagesDoNotDuplicateRows(t *testing.T) {
	f := seed(t)
	archiveViewBatch(t, f.e, f.board, f.todo, f.arch, "page-feb", "Paged", "2026-02-15T12:00:00Z", 60)
	archiveViewBatch(t, f.e, f.board, f.todo, f.arch, "page-jan", "Paged", "2026-01-15T12:00:00Z", 60)
	m := mount(f, f.e)
	seen := map[string]bool{}
	path := "/ui/boards/" + f.board + "/archive?show=archived&view=list"
	for page, want := range []int{50, 50, 20} {
		w := do(m, http.MethodGet, path, nil)
		body := w.Body.String()
		if w.Code != http.StatusOK {
			t.Fatalf("page %d = %d: %s", page+1, w.Code, body)
		}
		ids := archiveUIIDs(body)
		if len(ids) != want {
			t.Fatalf("page %d has %d rows, want %d: %s", page+1, len(ids), want, body)
		}
		wantMonths := []int{1, 1, 0}[page]
		if got := strings.Count(body, `class="archive-month"`); got != wantMonths {
			t.Errorf("page %d has %d month headings, want %d: %s", page+1, got, wantMonths, body)
		}
		if page == 1 {
			if strings.Contains(body, "February 2026") || !strings.Contains(body, "January 2026") {
				t.Errorf("continuation page repeated February or omitted January: %s", body)
			}
		}
		for _, id := range ids {
			if seen[id] {
				t.Fatalf("duplicate archive row %s across cursor pages", id)
			}
			seen[id] = true
		}
		if page == 0 {
			if !strings.Contains(body, `data-total="120"`) || !strings.Contains(body, `id="archive-load-more"`) || !strings.Contains(body, `hx-trigger="click"`) {
				t.Fatalf("first page total/pager contract: %s", body)
			}
			path = archiveUILoadMore(t, body)
			continue
		}
		if strings.Contains(body, `id="archive-view"`) {
			t.Fatalf("cursor page duplicated the fragment root: %s", body)
		}
		if !strings.Contains(body, `id="archive-pager"`) || !strings.Contains(body, `hx-swap-oob="outerHTML"`) {
			t.Fatalf("cursor page did not replace the pager: %s", body)
		}
		if page < 2 {
			path = archiveUILoadMore(t, body)
		}
	}
	if len(seen) != 120 {
		t.Fatalf("cursor pages produced %d unique rows, want 120", len(seen))
	}
}

func TestArchiveUIFiltersAndSortingPassthrough(t *testing.T) {
	f := seed(t)
	a := archiveViewCard(t, f, f.todo, "Alpha match", "2026-04-02T00:00:00Z")
	archiveViewCard(t, f, f.todo, "Beta other", "2026-04-03T00:00:00Z")
	call[Card](t, f.e, "card.label.add", LabelIn{ID: a.ID, LabelID: f.labelA})
	call[Card](t, f.e, "card.member.add", MemberIn{ID: a.ID, UserID: f.vw})
	q := url.Values{
		"show": {"archived"}, "view": {"list"}, "q": {"Alpha"}, "label": {f.labelA}, "member": {f.vw},
		"fromListId": {f.todo}, "since": {"2026-04-01"}, "until": {"2026-04-30"}, "sort": {"name"}, "dir": {"asc"},
	}
	w := do(mount(f, f.e), http.MethodGet, "/ui/boards/"+f.board+"/archive?"+q.Encode(), nil)
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(body, a.ID) || strings.Contains(body, "Beta other") {
		t.Fatalf("filtered archive = %d: %s", w.Code, body)
	}
	filters := archiveUIForm(t, body, "archive-filters")
	for _, want := range []string{
		`name="q" value="Alpha"`, `name="labelId" value="` + f.labelA + `"`, `name="memberId" value="` + f.vw + `"`,
		`name="fromListId"`, `id="archive-from"`, `name="since" value="2026-04-01"`, `name="until" value="2026-04-30"`,
		`name="sort"`, `name="dir"`, `name="show" value="archived"`, `name="kind" value="archived"`, `name="view" value="list"`,
	} {
		if !strings.Contains(filters, want) {
			t.Errorf("archive filters lack %q: %s", want, filters)
		}
	}
	row := archiveUIRowHTML(t, body, a.ID)
	restore := html.UnescapeString(row)
	for _, want := range []string{"labelId=" + f.labelA, "memberId=" + f.vw, "fromListId=" + f.todo, "since=2026-04-01", "until=2026-04-30", "sort=name", "dir=asc", "view=list"} {
		if !strings.Contains(restore, want) {
			t.Errorf("row action URL lost %q: %s", want, restore)
		}
	}
}

func TestArchiveUIDeleteOnlyWhenServerSaysCanDelete(t *testing.T) {
	m := seedMB(t)
	full := m.card(t, m.todo, "fully trashed")
	m.place(t, m.e, full.ID, m.b3Todo)
	call[Card](t, m.e, "card.archive", ArchiveIn{ID: full.ID, Trash: true})
	partial := m.card(t, m.todo, "partially trashed")
	m.place(t, m.e, partial.ID, m.b3Todo)
	call[Card](t, m.e, "card.move", MoveIn{ID: partial.ID, BoardID: m.board, ListID: m.trash})

	w := do(mount(m.fx, m.e), http.MethodGet, "/ui/boards/"+m.board+"/archive?show=trash", nil)
	body := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("trash = %d: %s", w.Code, body)
	}
	if row := archiveUIRowHTML(t, body, full.ID); !strings.Contains(row, "Delete forever") {
		t.Errorf("fully trashed row has no delete action: %s", row)
	}
	if row := archiveUIRowHTML(t, body, partial.ID); strings.Contains(row, "Delete forever") || !strings.Contains(row, "Restore") {
		t.Errorf("partially trashed row guessed delete safety: %s", row)
	}
}

func TestArchiveUIBulkSelectAllAndExactCountConflict(t *testing.T) {
	f := seed(t)
	archived := archiveViewCard(t, f, f.todo, "still archived", "2026-05-01T00:00:00Z")
	trashedA := f.card(t, f.todo, "trash one")
	trashedB := f.card(t, f.doing, "trash two")
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: trashedA.ID, Trash: true})
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: trashedB.ID, Trash: true})
	m := mount(f, f.e)
	path := "/ui/boards/" + f.board + "/archive?show=trash&view=list"
	w := do(m, http.MethodGet, path, nil)
	bulk := archiveUIForm(t, w.Body.String(), "archive-bulk")
	for _, want := range []string{`name="all" value="false"`, `name="confirmCount" value="2"`, `name="kind" value="trash"`, `data-archive-select-all`, `Select all 2 matching this filter`, `data-archive-bulk="restore"`, `Restore selected`, `data-archive-delete-url=`} {
		if !strings.Contains(bulk, want) {
			t.Errorf("bulk form lacks %q: %s", want, bulk)
		}
	}

	w = do(m, http.MethodPost, "/ui/boards/"+f.board+"/archive/restore?show=trash&kind=trash", url.Values{
		"all": {"true"}, "confirmCount": {"999"}, "kind": {"trash"},
	})
	if w.Code != http.StatusConflict || w.Header().Get("HX-Retarget") != "#toasts" || !strings.Contains(w.Body.String(), "current matching count (2)") {
		t.Fatalf("stale exact-count confirmation = %d %v: %s", w.Code, w.Header(), w.Body.String())
	}
	if got := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Kind: "trash", Limit: 200}); got.Total != 2 {
		t.Fatalf("conflicted bulk restore changed trash: %+v", got)
	}

	w = do(m, http.MethodPost, "/ui/boards/"+f.board+"/archive/restore?show=trash&kind=trash&view=list", url.Values{
		"all": {"true"}, "confirmCount": {"2"}, "kind": {"trash"},
	})
	body := w.Body.String()
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("HX-Trigger"), `"toast":"Restored 2, skipped 0"`) || !strings.Contains(body, `data-total="0"`) || !strings.Contains(body, "Nothing in Trash yet.") {
		t.Fatalf("trash-only restore all = %d %v: %s", w.Code, w.Header(), body)
	}
	if got := archiveViewList(t, f.e, ArchiveListIn{BoardID: f.board, Kind: "archived", Limit: 200}); got.Total != 1 || got.Items[0].ID != archived.ID {
		t.Fatalf("Kind=trash restore touched archived cards: %+v", got)
	}

	// Explicit selections may carry the bulk form's hidden filters/empty confirmation; the UI handler
	// removes them before binding the op, exactly as the client script's empty confirmCount requires.
	w = do(m, http.MethodPost, "/ui/boards/"+f.board+"/archive/restore?show=archived", url.Values{
		"all": {"false"}, "confirmCount": {""}, "cardIds": {archiveCardIDs(archived.ID)}, "q": {"stale filter"}, "kind": {"archived"},
	})
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("HX-Trigger"), `"toast":"Restored 1, skipped 0"`) {
		t.Fatalf("selected restore with bulk hidden fields = %d %v: %s", w.Code, w.Header(), w.Body.String())
	}
}

func TestArchiveUIJSONBulkResponses(t *testing.T) {
	f := seed(t)
	archived := archiveViewCard(t, f, f.todo, "JSON restore", "2026-05-02T00:00:00Z")
	trashed := f.card(t, f.todo, "JSON delete")
	call[Card](t, f.e, "card.archive", ArchiveIn{ID: trashed.ID, Trash: true})
	m := mount(f, f.e)

	w := archiveUIJSONPost(t, m, "/ui/boards/"+f.board+"/archive/restore", map[string]any{"cardIds": []string{archived.ID}})
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Header().Get("HX-Trigger"), `"toast":"Restored 1, skipped 0"`) {
		t.Fatalf("JSON restore response = %d %v: %s", w.Code, w.Header(), w.Body.String())
	}
	var restored RestoreBulkOut
	if err := json.Unmarshal(w.Body.Bytes(), &restored); err != nil || restored.Restored != 1 || restored.Skipped != 0 {
		t.Fatalf("JSON restore body = %s (%v)", w.Body.String(), err)
	}

	w = archiveUIJSONPost(t, m, "/ui/boards/"+f.board+"/archive/delete", map[string]any{"cardIds": []string{trashed.ID}})
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" || !strings.Contains(w.Header().Get("HX-Trigger"), `"toast":"Deleted 1, skipped 0"`) {
		t.Fatalf("JSON delete response = %d %v: %s", w.Code, w.Header(), w.Body.String())
	}
	var deleted DeleteBulkOut
	if err := json.Unmarshal(w.Body.Bytes(), &deleted); err != nil || deleted.Deleted != 1 || deleted.Skipped != 0 {
		t.Fatalf("JSON delete body = %s (%v)", w.Body.String(), err)
	}
}

func TestArchiveUIEmptyStates(t *testing.T) {
	f := seed(t)
	m := mount(f, f.e)
	for _, tc := range []struct{ show, want string }{
		{"archived", "Nothing archived yet."},
		{"trash", "Nothing in Trash yet."},
	} {
		w := do(m, http.MethodGet, "/ui/boards/"+f.board+"/archive?show="+tc.show, nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), tc.want) || !strings.Contains(w.Body.String(), `data-total="0"`) {
			t.Errorf("%s empty state = %d: %s", tc.show, w.Code, w.Body.String())
		}
	}
	w := do(m, http.MethodGet, "/ui/boards/"+f.board+"/archive?show=archived&q=missing", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "No archived cards match these filters.") {
		t.Fatalf("filtered archive empty state = %d: %s", w.Code, w.Body.String())
	}
	w = do(m, http.MethodGet, "/ui/boards/"+f.board+"/archive?show=trash&q=missing", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "No trashed cards match these filters.") {
		t.Fatalf("filtered trash empty state = %d: %s", w.Code, w.Body.String())
	}
}
