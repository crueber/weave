package boards_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/testutil"
)

func post(m http.Handler, path string, vals url.Values, htmx bool) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, strings.NewReader(vals.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if htmx {
		r.Header.Set("HX-Request", "true")
	}
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	return w
}

func TestQuickAddCards(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Quick")
	l := addList(t, e, b.ID, "Todo")
	m := mux(e)
	path := "/ui/lists/" + l.ID + "/cards/quick"

	// several pasted lines create several cards, in order, and return only tiles
	w := post(m, path, url.Values{"name": {"alpha\nbeta\r\n\n- gamma  "}}, true)
	if w.Code != 200 {
		t.Fatalf("quick add: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Count(body, `<article class="boards-tile`) != 3 {
		t.Errorf("want 3 tiles, got body %s", body)
	}
	if w.Header().Get("HX-Retarget") != "" || strings.Contains(body, "card-modal") {
		t.Error("quick add must never retarget to or render the card modal")
	}
	if !strings.Contains(w.Header().Get("HX-Trigger"), "boards:added") {
		t.Errorf("missing boards:added trigger: %q", w.Header().Get("HX-Trigger"))
	}
	if strings.Contains(body, "<html") || strings.Contains(body, "boards-shell") {
		t.Error("response must be a fragment")
	}
	if got := strings.Join(cardNames(t, e, l.ID), ","); got != "alpha,beta,gamma" {
		t.Fatalf("order: %s", got)
	}

	// add to top keeps the pasted order above the existing cards
	w = post(m, path, url.Values{"name": {"top1\ntop2"}, "top": {"1"}}, true)
	if w.Code != 200 {
		t.Fatalf("top add: %d %s", w.Code, w.Body.String())
	}
	if got := strings.Join(cardNames(t, e, l.ID), ","); got != "top1,top2,alpha,beta,gamma" {
		t.Fatalf("top order: %s", got)
	}

	// empty input does nothing; plain form posts bounce back to the board
	if w = post(m, path, url.Values{"name": {"  \n "}}, true); w.Code != 204 {
		t.Errorf("empty: %d", w.Code)
	}
	if w = post(m, path, url.Values{"name": {"plain"}}, false); w.Code != 303 || w.Header().Get("Location") != "/boards/"+b.ID {
		t.Errorf("no-js post: %d %s", w.Code, w.Header().Get("Location"))
	}

	// viewers cannot add; unknown lists 404
	if w = post(mux(testutil.As(e, viewer)), path, url.Values{"name": {"x"}}, true); w.Code != 404 && w.Code != 403 {
		t.Errorf("viewer add: %d", w.Code)
	}
	if w = post(m, "/ui/lists/nope/cards/quick", url.Values{"name": {"x"}}, true); w.Code != 404 {
		t.Errorf("unknown list: %d", w.Code)
	}
}

func TestQuickAddLists(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "QuickLists")
	m := mux(e)
	path := "/ui/boards/" + b.ID + "/lists/quick"
	w := post(m, path, url.Values{"name": {"One\nTwo"}}, true)
	if w.Code != 200 {
		t.Fatalf("quick lists: %d %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Count(body, `class="boards-col"`) != 2 || strings.Index(body, "One") > strings.Index(body, "Two") {
		t.Errorf("columns: %s", body)
	}
	if !strings.Contains(body, `data-composer="card"`) || !strings.Contains(body, "This list is empty") {
		t.Error("new columns should carry their add-card composer and empty hint")
	}
	if strings.Contains(body, "boards-shell") || strings.Contains(body, `data-composer="list"`) {
		t.Error("response must only contain the new columns")
	}
	if w = post(m, path, url.Values{"name": {""}}, true); w.Code != 204 {
		t.Errorf("empty: %d", w.Code)
	}
	if w = post(m, path, url.Values{"name": {"Three"}}, false); w.Code != 303 {
		t.Errorf("no-js: %d", w.Code)
	}
}

func TestQuickTileEdit(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "QuickEdit")
	l := addList(t, e, b.ID, "Todo")
	lab := call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "Bug", Color: "berry-red"})
	addCard(t, e, b.ID, l.ID, "q1", "Old name", 1, "")
	m := mux(e)

	if _, body := get(t, m, "/ui/cards/q1/tile", true); !strings.Contains(body, `id="tile-q1"`) || !strings.Contains(body, "Old name") {
		t.Errorf("tile fragment: %s", body)
	}
	if _, body := get(t, m, "/ui/cards/q1/quick", true); !strings.Contains(body, "boards-quick") || !strings.Contains(body, "Bug") {
		t.Errorf("quick popover: %s", body)
	}
	// rename returns the refreshed tile out-of-band and asks to close the popover
	w := post(m, "/ui/cards/q1/quick", url.Values{"name": {"New name"}}, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "New name") || !strings.Contains(w.Body.String(), `hx-swap-oob`) || !strings.Contains(w.Header().Get("HX-Trigger"), "boards:closepop") {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	if w = post(m, "/ui/cards/q1/quick", url.Values{"name": {" "}}, true); w.Code != 400 && w.Code != 422 {
		t.Errorf("blank rename: %d", w.Code)
	}
	// label toggle
	w = post(m, "/ui/cards/q1/quick/label", url.Values{"labelId": {lab.ID}, "on": {"1"}}, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "is-on") {
		t.Fatalf("label on: %d %s", w.Code, w.Body.String())
	}
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM card_labels WHERE card_id='q1'`).Scan(&n)
	if n != 1 {
		t.Errorf("label not applied")
	}
	w = post(m, "/ui/cards/q1/quick/label", url.Values{"labelId": {lab.ID}, "on": {"0"}}, true)
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM card_labels WHERE card_id='q1'`).Scan(&n)
	if w.Code != 200 || n != 0 {
		t.Errorf("label off: %d %d", w.Code, n)
	}
	// archive offers an undo that restores the card to its list
	w = post(m, "/ui/cards/q1/quick/archive", url.Values{}, true)
	if w.Code != 204 || !strings.Contains(w.Header().Get("HX-Trigger"), "boards:undo") {
		t.Fatalf("archive: %d %q", w.Code, w.Header().Get("HX-Trigger"))
	}
	if got := cardNames(t, e, l.ID); len(got) != 0 {
		t.Fatalf("card still in list: %v", got)
	}
	if w = post(m, "/ui/cards/q1/quick/restore", url.Values{}, true); w.Code != 204 {
		t.Fatalf("restore: %d %s", w.Code, w.Body.String())
	}
	if got := cardNames(t, e, l.ID); len(got) != 1 {
		t.Fatalf("card not restored: %v", got)
	}
}
