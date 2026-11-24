package boards_test

import (
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
	"github.com/crueber/weave/internal/web"
)

// Phase 5b (docs/DESIGN.md "Placements"): the "Add to board..." picker, card.place / card.unplace through the card
// sheet and the tile quick edit, the wording of the card-level actions, and the shortcut b. placedWorld: c1 is on
// Alpha (home), Beta (another project) and AlphaTwo; c2 is on Alpha only; "both" edits all three boards.

// morePickerBoards adds boards the picker must leave out for "both": a board template they edit, a board they only
// view, a board they cannot see, and a board with no open column; plus one more board they edit (Gamma, project PB).
func morePickerBoards(t *testing.T, e *ops.Env) {
	t.Helper()
	if _, err := e.DB.Exec(`INSERT INTO boards(id,project_id,position,name,is_template,created_at) VALUES
		('T','PA',5,'Template board',1,'x'),('V','PA',6,'Viewed board',0,'x'),('H','PB',7,'Hidden board',0,'x'),('N','PB',8,'Columnless board',0,'x'),('G','PB',2,'Gamma',0,'x');
		INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES('lt','T','active',1,'Tpl col','x'),('lv','V','active',1,'Viewed col','x'),
			('lh','H','active',1,'Hidden col','x'),('ln','N','archive',1,NULL,'x'),('lg1','G','closed',1,'Gamma done','x'),('lg2','G','active',2,'Gamma doing','x');
		INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('bmT','T','both','editor','x'),('bmV','V','both','viewer','x'),
			('bmN','N','both','editor','x'),('bmG','G','both','owner','x')`); err != nil {
		t.Fatal(err)
	}
}

func TestPlacePickerLists(t *testing.T) {
	e := placedWorld(t)
	morePickerBoards(t, e)
	both := mux(asUser(e, "both"))
	code, body := get(t, both, "/ui/cards/c2/place-picker?board=A", true)
	if code != 200 {
		t.Fatalf("picker: %d %s", code, body)
	}
	boardsIn := regexp.MustCompile(`data-place-board="([^"]+)"`).FindAllStringSubmatch(body, -1)
	var got []string
	for _, m := range boardsIn {
		got = append(got, m[1])
	}
	// by project name (Alpha project, Beta project), then board position; Alpha (the card's board) is not offered
	if strings.Join(got, ",") != "A2,B,G" {
		t.Errorf("picker boards %v, want A2,B,G (no template, viewed, hidden, columnless or current board)", got)
	}
	for _, hidden := range []string{"Hidden board", "Template board", "Viewed board", "Columnless board"} {
		if strings.Contains(body, hidden) {
			t.Errorf("picker names %q", hidden)
		}
	}
	// grouped by project, each board's columns (open and closing ones only), the first OPEN column the default
	if !strings.Contains(body, `<div class="cm-place-ph">Beta project</div>`) || !strings.Contains(body, `<div class="cm-place-ph">Alpha project</div>`) {
		t.Errorf("no project groups: %s", body)
	}
	gamma := between(body, `data-cols-for="G"`, `</div>
  </div>`)
	if !strings.Contains(gamma, `data-place-list="lg1"`) || !strings.Contains(gamma, "closes cards") || !regexp.MustCompile(`is-default" data-place-list="lg2"`).MatchString(gamma) {
		t.Errorf("Gamma columns: %s", gamma)
	}
	if strings.Contains(between(body, `data-cols-for="B"`, `data-cols-for`), "Native card") {
		t.Error("the picker lists cards")
	}
	// a card already on every board you can edit: the picker says so
	if _, body := get(t, mux(asUser(e, "both")), "/ui/cards/c1/place-picker?board=A", true); strings.Contains(body, "data-place-board=\"A2\"") || strings.Contains(body, `data-place-board="B"`) || !strings.Contains(body, `data-place-board="G"`) {
		t.Errorf("c1 picker: %s", body)
	}
	_, _ = e.DB.Exec(`DELETE FROM board_memberships WHERE id='bmG'`)
	if _, body := get(t, mux(asUser(e, "both")), "/ui/cards/c1/place-picker?board=A", true); !strings.Contains(body, "already on every board you can edit") {
		t.Errorf("c1 picker with no target: %s", body)
	}
	// people who may not edit the card: the op's answers (forbidden for a viewer, not found for an outsider)
	if code, _ := get(t, mux(asUser(e, "aonly")), "/ui/cards/c2/place-picker", true); code != 403 {
		t.Errorf("viewer picker: %d", code)
	}
	if code, body := get(t, mux(asUser(e, "outsider")), "/ui/cards/c2/place-picker", true); code != 404 || strings.Contains(body, "Solo card") {
		t.Errorf("outsider picker: %d", code)
	}
	// drafts and card templates are never placed: no picker targets, no Boards row
	_, _ = e.DB.Exec(`UPDATE cards SET is_draft=1 WHERE id='c2'`)
	if _, body := get(t, both, "/ui/cards/c2/place-picker", true); strings.Contains(body, "data-place-board") {
		t.Errorf("a draft offers boards: %s", body)
	}
	if _, body := get(t, both, "/ui/cards/c2", true); strings.Contains(body, `data-cap="card-add-to-board"`) {
		t.Error("a draft offers Add to board")
	}
}

func TestPlaceAndRemoveThroughTheSheet(t *testing.T) {
	e := placedWorld(t)
	both := mux(asUser(e, "both"))
	// place c2 (Alpha) on Beta, another project, from the Alpha sheet: the sheet comes back on Alpha with the Boards row
	w := post(both, "/ui/cards/c2/placements", url.Values{"listId": {"lb1"}, "board": {"A"}, "_part": {"modal"}}, true)
	mustOK(t, "place on Beta", w.Code, w.Body.String())
	if placementOf(t, e, "c2", "B") != "lb1" || placementOf(t, e, "c2", "A") != "la1" {
		t.Fatalf("placements after place: A=%s B=%s", placementOf(t, e, "c2", "A"), placementOf(t, e, "c2", "B"))
	}
	sh := w.Body.String()
	if !strings.Contains(sh, `data-board="A"`) || !strings.Contains(sh, `data-multi="1"`) || !strings.Contains(sh, `data-unplace="B"`) || !strings.Contains(sh, `data-unplace="A"`) {
		t.Errorf("sheet after place: %.600s", sh)
	}
	if tr := w.Header().Get("HX-Trigger"); !strings.Contains(tr, `"cards:placement"`) || !strings.Contains(tr, `"toast":"Added to Beta"`) || !strings.Contains(tr, `"placed":true`) {
		t.Errorf("place trigger: %s", tr)
	}
	// again: a conflict, shown as an error (the picker shows it inline)
	if w = post(both, "/ui/cards/c2/placements", url.Values{"listId": {"lb2"}, "board": {"A"}}, true); w.Code != 409 || !strings.Contains(w.Body.String(), "already on that board") {
		t.Errorf("second place on Beta: %d %s", w.Code, w.Body.String())
	}
	// a viewer of Alpha cannot place it anywhere
	if w = post(mux(asUser(e, "aonly")), "/ui/cards/c2/placements", url.Values{"listId": {"la2a"}}, true); w.Code != 403 {
		t.Errorf("viewer place: %d", w.Code)
	}
	// the tile on the Beta board: the quick edit offers "Remove from this board", and the archive says it covers both
	_, q := get(t, both, "/ui/cards/c2/quick?board=B", true)
	if !strings.Contains(q, `data-cap="card-remove-from-board" hx-post="/ui/cards/c2/placements/B/delete"`) || !strings.Contains(q, "This card is also on 1 other board; it will be archived on all of them.") {
		t.Errorf("quick edit of a card on two boards: %s", q)
	}
	// remove it from Beta through the Beta sheet (the context board itself): the sheet falls back to Alpha
	w = post(both, "/ui/cards/c2/placements/B/delete", url.Values{"board": {"B"}, "_part": {"modal"}}, true)
	mustOK(t, "remove from Beta", w.Code, w.Body.String())
	if placementOf(t, e, "c2", "B") != "" {
		t.Fatal("still on Beta")
	}
	if !strings.Contains(w.Body.String(), `data-board="A"`) || strings.Contains(w.Body.String(), "data-multi") {
		t.Errorf("sheet after removing its own board: %.400s", w.Body.String())
	}
	if tr := w.Header().Get("HX-Trigger"); !strings.Contains(tr, `"toast":"Removed from Beta"`) || !strings.Contains(tr, `"placed":false`) {
		t.Errorf("unplace trigger: %s", tr)
	}
	// the last board cannot be removed (no "x" offers it; the route says why)
	if w = post(both, "/ui/cards/c2/placements/A/delete", url.Values{"board": {"A"}}, true); w.Code != 422 || !strings.Contains(w.Body.String(), "archive or delete the card instead") {
		t.Errorf("remove the last board: %d %s", w.Code, w.Body.String())
	}
	// single board again: the quick edit has no remove and asks no confirmation for archive
	if _, q = get(t, both, "/ui/cards/c2/quick?board=A", true); strings.Contains(q, "card-remove-from-board") || strings.Contains(q, "hx-confirm") {
		t.Errorf("quick edit of a single-board card: %s", q)
	}
}

func TestSheetWordingOnSeveralBoards(t *testing.T) {
	e := placedWorld(t)
	_, sheet := get(t, mux(asUser(e, "both")), "/ui/cards/c1?board=B", true)
	menu := between(sheet, `data-pop="menu"`, `</details>`)
	for _, want := range []string{
		`data-unplace="B" data-board-name="Beta">Remove from this board<small>It stays on its 2 other boards</small>`,
		`hx-confirm="This card is also on 2 other boards; it will be archived on all of them." data-confirm-title="Archive this card?"`,
		`>Archive card<small>On all 3 boards it is on</small>`,
		`hx-confirm="This card is also on 2 other boards; it goes to the trash on all of them."`,
	} {
		if !strings.Contains(menu, want) {
			t.Errorf("menu lacks %q: %s", want, menu)
		}
	}
	// the Boards row: an x on each board (the editor edits all three), the Add button, no last-board hint
	row := between(sheet, `id="cm-boards-prop"`, `</div></div>`)
	if strings.Count(row, `data-cap="card-remove-from-board"`) != 3 || strings.Contains(row, "Archive or delete the card") {
		t.Errorf("Boards row: %s", row)
	}
	// a viewer of Beta only sees one board: no remove, no wording about other boards (they are hidden from them)
	_, sheet = get(t, mux(asUser(e, "bonly")), "/ui/cards/c1?board=B", true)
	if strings.Contains(sheet, "data-unplace") || strings.Contains(sheet, "other board") || strings.Contains(sheet, `id="cm-boards-prop" hidden`) == false {
		t.Errorf("Beta viewer sheet: %.400s", between(sheet, `data-pop="menu"`, `</details>`))
	}
	// the help overlay of the sheet has b
	_, sheet = get(t, mux(asUser(e, "both")), "/ui/cards/c2", true)
	if !strings.Contains(sheet, `<dt><kbd>b</kbd></dt><dd>Add to board&hellip;</dd>`) {
		t.Error("sheet help lacks b")
	}
}

// The UI never reveals a hidden placement by offering "Remove from this board": card.unplace counts every placement
// (a team may take a card off its board even when it does not see the card's other boards), but the sheet and the
// tile only offer it when another placement is VISIBLE (authz.CardCaps.CanRemoveFromBoard).
func TestRemoveNotOfferedForAHiddenPlacement(t *testing.T) {
	e := placedWorld(t)
	// bonly gets Edit on Beta; c1 is on Alpha and AlphaTwo too, both hidden from them
	_, _ = e.DB.Exec(`UPDATE board_memberships SET role='editor' WHERE board_id='B' AND user_id='bonly'`)
	m := mux(asUser(e, "bonly"))
	_, sheet := get(t, m, "/ui/cards/c1?board=B", true)
	_, quick := get(t, m, "/ui/cards/c1/quick?board=B", true)
	for name, html := range map[string]string{"sheet": sheet, "quick edit": quick} {
		if strings.Contains(html, "card-remove-from-board") || strings.Contains(html, "data-unplace") || strings.Contains(html, "other board") {
			t.Errorf("%s offers Remove from this board / names other boards", name)
		}
	}
	// the op itself allows it (documented divergence)
	if w := post(m, "/ui/cards/c1/placements/B/delete", url.Values{}, true); w.Code != 204 {
		t.Errorf("unplace by a Beta editor: %d %s", w.Code, w.Body.String())
	}
}

func TestPlacementsLiveFragment(t *testing.T) {
	e := placedWorld(t)
	code, body := get(t, mux(asUser(e, "both")), "/ui/cards/c1/placements?board=B", true)
	if code != 200 || !strings.Contains(body, `data-boards-live data-board="B" data-multi="1"`) {
		t.Fatalf("live fragment: %d %.300s", code, body)
	}
	for _, id := range []string{`id="cm-crumb-board"`, `id="cm-boards-prop"`, `id="cm-menu"`} {
		if !strings.Contains(body, id) {
			t.Errorf("live fragment lacks %s", id)
		}
	}
	if code, _ := get(t, mux(asUser(e, "outsider")), "/ui/cards/c1/placements", true); code != 404 {
		t.Errorf("outsider live fragment: %d", code)
	}
	// a Beta viewer's fragment never names Alpha
	_, body = get(t, mux(asUser(e, "bonly")), "/ui/cards/c1/placements?board=A", true)
	noAlpha(t, "bonly live fragment", body)
}

// The shortcut b: in the board's help, handled by cards_place.js (loaded by the board page and the sheet), and not
// taken by any other card or board shortcut.
func TestAddToBoardShortcut(t *testing.T) {
	e := placedWorld(t)
	_, page := get(t, mux(asUser(e, "both")), "/boards/A", false)
	if !strings.Contains(page, `<dt><kbd>b</kbd></dt><dd>Add to board&hellip;`) || !strings.Contains(page, `<script src="/static/cards_place.js" defer></script>`) {
		t.Error("board page: no b in the help or no cards_place.js")
	}
	read := func(n string) string {
		b, err := fs.ReadFile(web.Static(), n)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if js := read("cards_place.js"); !strings.Contains(js, "e.key !== 'b'") || !strings.Contains(js, "htmx:confirm") {
		t.Error("cards_place.js lacks the b handler or the styled confirmation")
	}
	for _, f := range []string{"boards_keys.js", "boards_cardkeys.js", "cards_ui.js"} {
		if regexp.MustCompile(`case 'b'|k === 'b'|key === 'b'|\bb: 1`).MatchString(read(f)) {
			t.Errorf("%s also handles b", f)
		}
	}
}

// The picker reads a constant number of queries whatever the number of boards it offers.
func TestPlacePickerQueryCountIsConstant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { testutil.CheckPlacements(t, d) })
	e := testutil.Env(t, nil)
	e.DB = d
	placedWorldOn(t, e)
	cdb, err := sql.Open("sqlite-counting", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	count := func() int64 {
		ce := asUser(e, "both")
		ce.DB = &db.DB{DB: cdb}
		queries.Store(0)
		if code, body := get(t, mux(ce), "/ui/cards/c2/place-picker?board=A", true); code != 200 {
			t.Fatalf("picker %d %s", code, body)
		}
		return queries.Load()
	}
	before := count()
	for i := 0; i < 4; i++ {
		bid := fmt.Sprintf("X%d", i)
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO boards(id,project_id,position,name,created_at) VALUES(?,'PB',?,?,'x')`, []any{bid, 10 + i, "More " + bid}},
			{`INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES(?,?,'active',1,'c1','x'),(?,?,'active',2,'c2','x')`, []any{"l1" + bid, bid, "l2" + bid, bid}},
			{`INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES(?,?,'both','editor','x')`, []any{"bm" + bid, bid}},
		} {
			if _, err := d.Exec(q.sql, q.args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	if after := count(); after != before {
		t.Errorf("picker: %d queries with 2 target boards, %d with 6 (want equal)", before, after)
	}
}
