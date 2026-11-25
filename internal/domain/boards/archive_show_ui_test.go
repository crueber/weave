package boards_test

import (
	"html"
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
)

func TestBoardShowArchiveStatePreservesViewAndFilters(t *testing.T) {
	e := placedWorld(t)
	m := mux(asUser(e, "both"))
	code, page := get(t, m, "/boards/A?show=archived&view=list&q=zebra&label=labA&member=both", false)
	if code != 200 {
		t.Fatalf("archive board page = %d: %s", code, page)
	}
	for _, want := range []string{
		`id="board-shell" data-show="archived"`,
		`id="board-filter"`, `name="q" value="zebra"`, `name="view" value="list"`, `name="show" value="archived"`,
		`name="labelId"`, `name="memberId"`,
		`data-show="active"`, `data-show="archived"`, `data-show="trash"`,
		`hx-get="/ui/boards/A/archive?`, `hx-target="#board-body" hx-swap="innerHTML"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("archive board page lacks %q: %s", want, page)
		}
	}
	if strings.Contains(page, `class="view-switch boards-views"`) || strings.Contains(page, `class="boards-dd view-pick"`) {
		t.Errorf("archive mode still renders the kanban/list/grid switcher: %s", page)
	}
	decoded := html.UnescapeString(page)
	for _, want := range []string{
		`href="/boards/A?label=labA&member=both&q=zebra&view=list"`,
		`href="/boards/A?label=labA&member=both&q=zebra&show=archived&view=list"`,
		`href="/boards/A?label=labA&member=both&q=zebra&show=trash&view=list"`,
		`hx-push-url="/boards/A?label=labA&member=both&q=zebra&show=archived&view=list"`,
		`hx-push-url="/boards/A?label=labA&member=both&q=zebra&show=trash&view=list"`,
		`data-body-url="/ui/boards/A/archive?label=labA&member=both&q=zebra&show=archived&view=list"`,
	} {
		if !strings.Contains(decoded, want) {
			t.Errorf("Show URL lost state %q: %s", want, decoded)
		}
	}
	activeAt := strings.Index(page, `data-show="active"`)
	if activeAt < 0 {
		t.Fatal("Active Show link missing")
	}
	activeStart := strings.LastIndex(page[:activeAt], "<a ")
	if activeStart < 0 {
		t.Fatal("Active Show link has no opening anchor")
	}
	activeAnchor := page[activeStart:]
	if end := strings.Index(activeAnchor, ">"); end >= 0 {
		activeAnchor = activeAnchor[:end]
	}
	decodedActive := html.UnescapeString(activeAnchor)
	if !strings.Contains(decodedActive, `href="/boards/A?label=labA&member=both&q=zebra&view=list"`) || !strings.Contains(decodedActive, `data-show="active"`) || !strings.Contains(decodedActive, `data-show-link="active"`) {
		t.Errorf("Active Show link lost its board URL or state hooks: %s", decodedActive)
	}
	for _, attr := range []string{"hx-get", "hx-target", "hx-swap", "hx-push-url"} {
		if strings.Contains(activeAnchor, attr) {
			t.Errorf("Active Show link must be a normal board-page navigation, not HTMX: %s", activeAnchor)
		}
	}
	if strings.Contains(decoded, `hx-push-url="/ui/`) {
		t.Errorf("a filter or Show request pushes a fragment URL: %s", decoded)
	}
	filterStart := strings.Index(page, `<form id="board-filter"`)
	if filterStart < 0 {
		t.Fatal("common board filter form missing")
	}
	filterForm := page[filterStart:]
	if end := strings.Index(filterForm, "</form>"); end >= 0 {
		filterForm = filterForm[:end]
	}
	if strings.Contains(filterForm, "hx-push-url") {
		t.Errorf("archive common filter form must not push a URL: %s", filterForm)
	}

	code, body := get(t, m, "/ui/boards/A/body?show=trash&view=grid&q=zebra&label=labA&member=both", true)
	if code != 200 || !strings.Contains(body, `data-body-url="/ui/boards/A/archive?`) || !strings.Contains(body, `show=trash`) || !strings.Contains(body, `view=grid`) {
		t.Fatalf("trash body shell = %d: %s", code, body)
	}
	if strings.Contains(body, `id="tile-c1"`) || strings.Contains(body, `class="boards-lists"`) {
		t.Errorf("trash body shell rendered active kanban cards: %s", body)
	}

	code, body = get(t, m, "/ui/boards/A/body?view=list&q=zebra", true)
	if code != 200 || !strings.Contains(body, `data-id="c1"`) || strings.Contains(body, `hx-get="/ui/boards/A/archive?`) {
		t.Fatalf("active body did not restore the list view = %d: %s", code, body)
	}
}

func TestBoardArchiveSingularFiltersSelectTheCommonControls(t *testing.T) {
	e := placedWorld(t)
	m := mux(asUser(e, "both"))
	code, page := get(t, m, "/boards/A?show=archived&view=list&q=zebra&labelId=labA&memberId=both", false)
	if code != http.StatusOK {
		t.Fatalf("archive board page = %d: %s", code, page)
	}
	for _, want := range []string{
		`<select name="labelId" aria-label="Filter by label">`,
		`<option value="">Any label</option>`,
		`<option value="labA" selected>alpha-label</option>`,
		`<option value="labP">project-label</option>`,
		`<select name="memberId" aria-label="Filter by member">`,
		`<option value="">Any member</option>`,
		`<option value="me">Me</option>`,
		`<option value="none">Unassigned</option>`,
		`<option value="both" selected>User both</option>`,
		`<option value="aonly">User aonly</option>`,
		`name="q" value="zebra"`, `name="view" value="list"`, `name="show" value="archived"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("common archive filter lacks %q: %s", want, page)
		}
	}
	for _, unwanted := range []string{`type="checkbox" name="label"`, `type="checkbox" name="member"`} {
		if strings.Contains(page, unwanted) {
			t.Errorf("archive mode rendered multi-select controls: %q in %s", unwanted, page)
		}
	}
	for _, memberID := range []string{"me", "none"} {
		_, page = get(t, m, "/boards/A?show=trash&view=grid&memberId="+memberID, false)
		want := `<option value="` + memberID + `" selected>`
		if !strings.Contains(page, want) {
			t.Errorf("trash member filter %q is not selected: %q missing from %s", memberID, want, page)
		}
	}

	_, active := get(t, m, "/boards/A?view=list&label=labA&member=both", false)
	for _, want := range []string{
		`type="checkbox" name="label" value="labA" checked`,
		`type="checkbox" name="member" value="both" checked`,
	} {
		if !strings.Contains(active, want) {
			t.Errorf("active mode checkbox filter changed: %q missing from %s", want, active)
		}
	}
	for _, unwanted := range []string{`name="labelId"`, `name="memberId"`} {
		if strings.Contains(active, unwanted) {
			t.Errorf("active mode rendered archive select controls: %q in %s", unwanted, active)
		}
	}
}

func TestBoardArchiveShowBypassesDefaultViewRedirect(t *testing.T) {
	e := placedWorld(t)
	who := asUser(e, "both")
	for _, defaultView := range []string{"calendar", "map"} {
		t.Run(defaultView, func(t *testing.T) {
			call[boards.Board](t, who, "board.update", boards.BoardUpdateIn{ID: "A", DefaultView: &defaultView})
			m := mux(who)
			for _, show := range []string{"archived", "trash"} {
				code, page := get(t, m, "/boards/A?show="+show, false)
				if code != http.StatusOK {
					t.Errorf("bare %s board page = %d, want 200: %s", show, code, page)
				}
				if !strings.Contains(page, `id="board-shell" data-show="`+show+`"`) {
					t.Errorf("bare %s board page lost its Show state: %s", show, page)
				}
			}
			for _, query := range []string{"", "?show=active", "?q=zebra"} {
				if code, _ := get(t, m, "/boards/A"+query, false); code != http.StatusSeeOther {
					t.Errorf("ordinary active board URL %q = %d, want redirect", query, code)
				}
			}
			if code, _ := get(t, m, "/boards/A?view=list", false); code != http.StatusOK {
				t.Errorf("explicit active view was redirected: %d", code)
			}
		})
	}
}
