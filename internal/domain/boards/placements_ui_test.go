package boards_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/ops"
)

// Phase 5a of "one card on several boards" (docs/DESIGN.md "Placements", the read half of the UI): the tile marker
// "also on N other boards" and its popover, and the card sheet on a context board (?board=): breadcrumb switcher,
// the read-only Boards row and the context threaded through the sheet's fragments. Uses placedWorld (c1: home Alpha,
// also on Beta (another project) and AlphaTwo; c2 on Alpha only).

// tileOf cuts one tile (or list-view row) out of a board body.
func tileOf(t *testing.T, body, id string) string {
	t.Helper()
	for _, start := range []string{`id="tile-` + id + `"`, `<tr data-id="` + id + `"`} {
		if i := strings.Index(body, start); i >= 0 {
			rest := body[i:]
			for _, end := range []string{"</article>", "</tr>"} {
				if j := strings.Index(rest, end); j >= 0 {
					return rest[:j]
				}
			}
		}
	}
	t.Fatalf("no tile %s in %s", id, body)
	return ""
}

var alsoLabel = regexp.MustCompile(`data-also-on[^>]*aria-label="Also on (\d+) other boards?"`)

func alsoCount(tile string) string {
	m := alsoLabel.FindStringSubmatch(tile)
	if m == nil {
		return ""
	}
	return m[1]
}

func TestAlsoOnMarkerCountsVisibleBoardsOnly(t *testing.T) {
	e := placedWorld(t)
	// "ab" views Alpha and Beta, not AlphaTwo
	if _, err := e.DB.Exec(`INSERT INTO users(id,email,name,role,created_at) VALUES('ab','ab@example.com','User ab','boardUser','x');
		INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('bmab1','A','ab','viewer','x'),('bmab2','B','ab','viewer','x')`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		user, board, want string
	}{
		{"both", "A", "2"}, {"both", "B", "2"}, {"both", "A2", "2"}, {"ab", "A", "1"}, {"ab", "B", "1"}, {"aonly", "A", ""}, {"bonly", "B", ""},
	} {
		for _, view := range []string{"kanban", "grid", "list"} {
			code, body := get(t, mux(asUser(e, tc.user)), "/ui/boards/"+tc.board+"/body?view="+view, true)
			if code != 200 {
				t.Fatalf("%s %s %s: %d", tc.user, tc.board, view, code)
			}
			tile := tileOf(t, body, "c1")
			if got := alsoCount(tile); got != tc.want {
				t.Errorf("%s on %s (%s): marker count %q, want %q: %s", tc.user, tc.board, view, got, tc.want, tile)
			}
			if tc.board != "A2" && strings.Contains(tileOf(t, body, cardOn(tc.board)), "data-also-on") {
				t.Errorf("%s on %s (%s): a single-board card has a marker", tc.user, tc.board, view)
			}
			// the marker is a button that never opens the card: no hx-get, no data-open-card, no link of its own
			if tc.want != "" && !regexp.MustCompile(`<button type="button" class="boards-also" data-also-on aria-haspopup="dialog" aria-expanded="false"`).MatchString(tile) {
				t.Errorf("%s on %s (%s): marker markup: %s", tc.user, tc.board, view, tile)
			}
			// the tile link (kanban, grid) opens the card on this board when it is on another one too
			if view != "list" && tc.want != "" && !strings.Contains(tile, `href="/cards/c1?board=`+tc.board+`"`) {
				t.Errorf("%s on %s (%s): tile link without the board: %s", tc.user, tc.board, view, tile)
			}
		}
	}
	// a Beta-only viewer's board never names Alpha (the marker is absent, the count would reveal it)
	_, body := get(t, mux(asUser(e, "bonly")), "/ui/boards/B/body", true)
	noAlpha(t, "Beta body", body)
}

// cardOn is a card placed on one board only, per board of placedWorld (AlphaTwo holds only c1).
func cardOn(board string) string {
	if board == "B" {
		return "cb"
	}
	return "c2"
}

func TestAlsoOnMarkerOnFilledTilesAndPublicBoard(t *testing.T) {
	e := placedWorld(t)
	if _, err := e.DB.Exec(`UPDATE boards SET card_color_mode='fill' WHERE id='A'`); err != nil {
		t.Fatal(err)
	}
	_, body := get(t, mux(asUser(e, "both")), "/ui/boards/A/body", true)
	tile := tileOf(t, body, "c1")
	if !strings.Contains(tile, "data-card-fill") || alsoCount(tile) != "2" {
		t.Errorf("filled tile without the marker: %s", tile)
	}
	// the public page: read-only, no marker (its tiles open nothing), and no other board is named
	if _, err := e.DB.Exec(`UPDATE boards SET is_public=1, public_token='tokB' WHERE id='B'`); err != nil {
		t.Fatal(err)
	}
	for _, who := range []*ops.Env{asUser(e, "outsider"), anon(e)} {
		code, page := get(t, mux(who), "/public/tokB", false)
		if code != 200 || !strings.Contains(page, `id="tile-c1"`) {
			t.Fatalf("public page %d: %s", code, page)
		}
		if strings.Contains(page, "data-also-on") {
			t.Errorf("public page shows the marker")
		}
		noAlpha(t, "public page", page)
	}
}

func anon(e *ops.Env) *ops.Env { c := *e; c.User = nil; return &c }

func TestAlsoOnPopoverListsVisibleOtherBoards(t *testing.T) {
	e := placedWorld(t)
	if _, err := e.DB.Exec(`INSERT INTO users(id,email,name,role,created_at) VALUES('ab','ab@example.com','User ab','boardUser','x');
		INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('bmab1','A','ab','viewer','x'),('bmab2','B','ab','viewer','x')`); err != nil {
		t.Fatal(err)
	}
	code, pop := get(t, mux(asUser(e, "both")), "/ui/cards/c1/placements-popover?board=A", true)
	if code != 200 || !strings.Contains(pop, "Also on 2 other boards") {
		t.Fatalf("popover %d: %s", code, pop)
	}
	for _, s := range []string{`href="/boards/B"`, "Beta review", "Beta review &middot; Beta project", `href="/boards/A2"`, "AlphaTwo col"} {
		if !strings.Contains(pop, s) {
			t.Errorf("popover lacks %q: %s", s, pop)
		}
	}
	// the tile's own board is not listed; AlphaTwo is in the same project, so no project name
	if strings.Contains(pop, `href="/boards/A"`) || strings.Contains(pop, "Alpha project") || strings.Contains(pop, ">home<") {
		t.Errorf("popover lists the tile's own board or a wrong project/home: %s", pop)
	}
	// from Beta: the home (Alpha) is marked, AlphaTwo too, Beta itself not
	_, pop = get(t, mux(asUser(e, "both")), "/ui/cards/c1/placements-popover?board=B", true)
	if !strings.Contains(pop, `href="/boards/A"`) || !strings.Contains(pop, ">home<") || !strings.Contains(pop, `href="/boards/A2"`) || strings.Contains(pop, `href="/boards/B"`) ||
		!strings.Contains(pop, "Alpha todo &middot; Alpha project") {
		t.Errorf("popover from Beta: %s", pop)
	}
	// a viewer of Alpha and Beta: Beta only, never AlphaTwo
	_, pop = get(t, mux(asUser(e, "ab")), "/ui/cards/c1/placements-popover?board=A", true)
	if !strings.Contains(pop, "Also on 1 other board<") || !strings.Contains(pop, "Beta review") || strings.Contains(pop, "AlphaTwo") || strings.Contains(pop, "A2") {
		t.Errorf("popover for ab: %s", pop)
	}
	// a viewer of Beta only, asking about Alpha: nothing of Alpha, nothing listed
	_, pop = get(t, mux(asUser(e, "bonly")), "/ui/cards/c1/placements-popover?board=A", true)
	noAlpha(t, "popover for bonly", pop)
	if !strings.Contains(pop, "not on any other board you can see") {
		t.Errorf("popover for bonly: %s", pop)
	}
	// strangers learn nothing (not_found); anonymous gets the signed-in route's answer (no body: sign in)
	if code, pop := get(t, mux(asUser(e, "outsider")), "/ui/cards/c1/placements-popover?board=B", true); code != 404 || strings.Contains(pop, "Beta") || strings.Contains(pop, "Alpha") {
		t.Errorf("outsider: %d %s", code, pop)
	}
	// (the real server answers 303 to /login, or HX-Redirect: /login for htmx; the authz sweep walks this route too)
	r := httptest.NewRequest("GET", "/ui/cards/c1/placements-popover?board=B", nil)
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	mux(anon(e)).ServeHTTP(w, r)
	if pop := w.Body.String(); w.Header().Get("HX-Redirect") != "/login" || strings.Contains(pop, "Beta") || strings.Contains(pop, "Alpha") || strings.Contains(pop, "boards-also") {
		t.Errorf("anonymous: %d %v %s", w.Code, w.Header(), pop)
	}
}

func TestCardSheetOnContextBoard(t *testing.T) {
	e := placedWorld(t)
	both := mux(asUser(e, "both"))
	code, sheet := get(t, both, "/ui/cards/c1?board=B", true)
	if code != 200 {
		t.Fatalf("sheet %d: %s", code, sheet)
	}
	for _, s := range []string{
		`data-board="B"`, `data-multi="1"`, `hx-vals='{"board":"B"}'`, // the context rides on every request of the sheet
		`<span>Beta</span>`, "Beta review", // crumb: the board switcher on Beta, Beta's column
		`class="cm-boardsw cards-pop"`, `href="/cards/c1?board=A" data-card-ctx="A"`, `href="/cards/c1?board=A2" data-card-ctx="A2"`,
		`data-prop="boards"`, `href="/boards/A"`, `href="/boards/A2"`, ">home<",
		"beta-label", // Beta's label on the card
	} {
		if !strings.Contains(sheet, s) {
			t.Errorf("sheet ?board=B lacks %q", s)
		}
	}
	// Beta's labels only on the card: the label chips of the rail are Beta's
	rail := sheet[strings.Index(sheet, `data-prop="labels"`):]
	rail = rail[:strings.Index(rail, "</details>")]
	if strings.Contains(rail, "alpha-label") || !strings.Contains(rail, "beta-label") {
		t.Errorf("labels row on Beta: %s", rail)
	}
	// the full page names the context board too
	if _, page := get(t, both, "/cards/c1?board=B", false); !strings.Contains(page, `<a href="/boards/B">&larr; Back to board</a>`) || !strings.Contains(page, `data-board="B"`) {
		t.Errorf("full page on Beta: %s", page)
	}
	// default (no hint): the home
	if _, sheet := get(t, both, "/ui/cards/c1", true); !strings.Contains(sheet, `data-board="A"`) || !strings.Contains(sheet, "Alpha todo") || !strings.Contains(sheet, `href="/cards/c1?board=B" data-card-ctx="B"`) {
		t.Errorf("sheet default context: %s", sheet)
	}
	// a single-board card: no switcher, no data-multi; the Boards row only for someone who may add it to another board
	// (phase 5b: "Add to board...", with the hint that archive and delete remove it everywhere), never with an "x"
	_, sheet = get(t, both, "/ui/cards/c2", true)
	if strings.Contains(sheet, "cm-boardsw") || strings.Contains(sheet, "data-multi") || strings.Contains(sheet, "data-unplace") {
		t.Errorf("single-board card shows the switcher or a remove control")
	}
	if !strings.Contains(sheet, `data-prop="boards"`) || !strings.Contains(sheet, `data-cap="card-add-to-board"`) || !strings.Contains(sheet, "Archive or delete the card to remove it everywhere") {
		t.Errorf("single-board card: no Boards row with Add to board for an editor of other boards")
	}
	if _, sheet = get(t, mux(asUser(e, "aonly")), "/ui/cards/c2", true); strings.Contains(sheet, `data-prop="boards"`) {
		t.Errorf("single-board card: a viewer gets the Boards row")
	}
	// a viewer of Beta only: the hint naming hidden Alpha is ignored (same sheet as without it), Alpha never shows
	bonly := mux(asUser(e, "bonly"))
	_, withHint := get(t, bonly, "/ui/cards/c1?board=A", true)
	_, noHint := get(t, bonly, "/ui/cards/c1", true)
	if withHint != noHint {
		t.Errorf("a hidden board hint changed the sheet")
	}
	noAlpha(t, "sheet for bonly", withHint)
	if strings.Contains(withHint, "cm-boardsw") || strings.Contains(withHint, `data-prop="boards"`) || !strings.Contains(withHint, `data-board="B"`) {
		t.Errorf("sheet for bonly: placement UI or wrong context")
	}
	// strangers: not found, with or without a hint
	if code, _ := get(t, mux(asUser(e, "outsider")), "/ui/cards/c1?board=B", true); code != 404 {
		t.Errorf("outsider sheet: %d", code)
	}
}

func TestCardSheetFragmentsFollowTheContext(t *testing.T) {
	e := placedWorld(t)
	both := mux(asUser(e, "both"))
	// board-wide custom-field groups: the context board's
	if _, f := get(t, both, "/ui/cards/c1/fields?board=B", true); !strings.Contains(f, "Beta fields") || strings.Contains(f, "Alpha fields") || !strings.Contains(f, "Card fields") {
		t.Errorf("fields on Beta: %s", f)
	}
	if _, f := get(t, both, "/ui/cards/c1/fields", true); !strings.Contains(f, "Alpha fields") || strings.Contains(f, "Beta fields") {
		t.Errorf("fields on the home: %s", f)
	}
	// the claim row and the description re-render on the context too (they read card.get with the hint)
	if code, _ := get(t, both, "/ui/cards/c1/claim?board=B", true); code != 200 {
		t.Errorf("claim on Beta: %d", code)
	}
	// a write from the sheet re-renders it on the board the form names (the inherited board field)
	post := func(path string, form url.Values) (int, string) {
		r := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("HX-Request", "true")
		w := httptest.NewRecorder()
		both.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}
	code, sheet := post("/ui/cards/c1/subscribe", url.Values{"board": {"B"}})
	if code != http.StatusOK || !strings.Contains(sheet, `data-board="B"`) || !strings.Contains(sheet, "Beta review") {
		t.Errorf("subscribe from Beta re-rendered %d on another board: %.400s", code, sheet)
	}
	code, sheet = post("/ui/cards/c1/update", url.Values{"board": {"B"}, "name": {"Shared card renamed"}})
	if code != http.StatusOK || !strings.Contains(sheet, `data-board="B"`) || !strings.Contains(sheet, "Shared card renamed") {
		t.Errorf("update from Beta re-rendered %d: %.400s", code, sheet)
	}
	// a Beta-only viewer's fragments never name Alpha, whatever the hint
	bonly := mux(asUser(e, "bonly"))
	for _, p := range []string{"/ui/cards/c1/fields?board=A", "/ui/cards/c1/tasks?board=A", "/ui/cards/c1/claim?board=A"} {
		_, f := get(t, bonly, p, true)
		noAlpha(t, p, f)
	}
}
