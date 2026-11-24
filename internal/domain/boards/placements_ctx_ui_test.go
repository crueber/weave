package boards_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/ops"
)

// Phase 5b of "one card on several boards" (docs/DESIGN.md "Placements", the write half of the UI): every write the
// board page and the card sheet make from a NON-home context (card c1: home Alpha, also on Beta and AlphaTwo; the
// user "both" edits all three) acts on the context board's placement (labels, moves, drops, close, duplicate), and
// the card-level actions (archive, restore, delete) work from any context and cover every placement.

// withSystemLists gives the placed world's boards their archive and trash columns (the SQL fixture has none).
func withSystemLists(t *testing.T, e *ops.Env) {
	t.Helper()
	if _, err := e.DB.Exec(`INSERT INTO lists(id,board_id,type,position,name,created_at) VALUES
		('laA','A','archive',90,NULL,'x'),('ltA','A','trash',91,NULL,'x'),('lbA','B','archive',90,NULL,'x'),('lbT','B','trash',91,NULL,'x'),
		('la2A','A2','archive',90,NULL,'x'),('la2T','A2','trash',91,NULL,'x')`); err != nil {
		t.Fatal(err)
	}
}

// placementOf is the column of a card's placement on a board ("" when it is not placed there).
func placementOf(t *testing.T, e *ops.Env, card, board string) string {
	t.Helper()
	var l string
	_ = e.DB.QueryRow(`SELECT list_id FROM card_placements WHERE card_id=? AND board_id=?`, card, board).Scan(&l)
	return l
}

func hasLabel(t *testing.T, e *ops.Env, card, label string) bool {
	t.Helper()
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM card_labels WHERE card_id=? AND label_id=?`, card, label).Scan(&n)
	return n > 0
}

func mustOK(t *testing.T, what string, code int, body string) {
	t.Helper()
	if code >= 300 {
		t.Fatalf("%s: %d %s", what, code, body)
	}
}

func TestSheetLabelsFromAnotherBoard(t *testing.T) {
	e := placedWorld(t)
	m := mux(asUser(e, "both"))
	_, _ = e.DB.Exec(`DELETE FROM card_labels WHERE card_id='c1' AND label_id='labB'`)
	// the sheet on Beta: its hx-vals carry board=B on every request
	w := post(m, "/ui/cards/c1/labels", url.Values{"labelId": {"labB"}, "board": {"B"}}, true)
	mustOK(t, "add a Beta label from the Beta sheet", w.Code, w.Body.String())
	if !hasLabel(t, e, "c1", "labB") {
		t.Fatal("Beta label not added")
	}
	if !strings.Contains(w.Body.String(), `data-board="B"`) {
		t.Errorf("the sheet re-rendered on another board: %.300s", w.Body.String())
	}
	w = post(m, "/ui/cards/c1/labels/labB/remove", url.Values{"board": {"B"}}, true)
	mustOK(t, "remove a Beta label from the Beta sheet", w.Code, w.Body.String())
	if hasLabel(t, e, "c1", "labB") {
		t.Fatal("Beta label not removed")
	}
	// an Alpha label from the Beta context is not Beta's vocabulary: refused, nothing changes
	if w = post(m, "/ui/cards/c1/labels/labA/remove", url.Values{"board": {"B"}}, true); w.Code < 400 || !hasLabel(t, e, "c1", "labA") {
		t.Errorf("an Alpha label removed from the Beta context: %d", w.Code)
	}
	// the quick edit and the keyboard label picker (board page of Beta)
	for _, part := range []string{"", "labels"} {
		w = post(m, "/ui/cards/c1/quick/label", url.Values{"labelId": {"labB"}, "on": {"1"}, "board": {"B"}, "_part": {part}}, true)
		mustOK(t, "quick label on", w.Code, w.Body.String())
		if !hasLabel(t, e, "c1", "labB") {
			t.Fatalf("quick (%q): Beta label not added", part)
		}
		w = post(m, "/ui/cards/c1/quick/label", url.Values{"labelId": {"labB"}, "on": {"0"}, "board": {"B"}, "_part": {part}}, true)
		mustOK(t, "quick label off", w.Code, w.Body.String())
		if hasLabel(t, e, "c1", "labB") {
			t.Fatalf("quick (%q): Beta label not removed", part)
		}
	}
}

func TestSheetMovesFromAnotherBoard(t *testing.T) {
	e := placedWorld(t)
	m := mux(asUser(e, "both"))
	// the sheet's column switcher on Beta: the Beta placement moves, Alpha's stays
	w := post(m, "/ui/cards/c1/move", url.Values{"listId": {"lb1"}, "_part": {"modal"}, "board": {"B"}}, true)
	mustOK(t, "sheet move on Beta", w.Code, w.Body.String())
	if got := placementOf(t, e, "c1", "B"); got != "lb1" {
		t.Errorf("Beta placement in %q, want lb1", got)
	}
	if got := placementOf(t, e, "c1", "A"); got != "la1" {
		t.Errorf("Alpha placement moved to %q", got)
	}
	// a drag and drop on the Beta board
	w = post(m, "/ui/boards/B/cards/drop", url.Values{"id": {"c1"}, "container": {"lb2"}}, true)
	mustOK(t, "drop on Beta", w.Code, w.Body.String())
	if got := placementOf(t, e, "c1", "B"); got != "lb2" {
		t.Errorf("after the drop: Beta placement in %q, want lb2", got)
	}
	if got := placementOf(t, e, "c1", "A"); got != "la1" {
		t.Errorf("after the drop: Alpha placement moved to %q", got)
	}
	// the sheet's column switcher on Beta never offers a column of another board the card is already on (Alpha's
	// columns would be a 409: the board switcher shows the card there instead)
	_, body := get(t, m, "/ui/cards/c1?board=B", true)
	sw := between(body, `data-pop="list"`, `</details>`)
	if strings.Contains(sw, `"listId":"la1"`) || strings.Contains(sw, `"listId":"la2a"`) {
		t.Errorf("the Beta column switcher offers a column of a board the card is already on: %s", sw)
	}
	if !strings.Contains(sw, `"listId":"lb1"`) {
		t.Errorf("the Beta column switcher lacks Beta's columns: %s", sw)
	}
	// from the home context the home placement moves, as before
	w = post(m, "/ui/cards/c1/move", url.Values{"listId": {"la2"}, "_part": {"modal"}, "board": {"A"}}, true)
	mustOK(t, "sheet move on Alpha", w.Code, w.Body.String())
	if placementOf(t, e, "c1", "A") != "la2" || placementOf(t, e, "c1", "B") != "lb2" {
		t.Errorf("home move: A=%s B=%s", placementOf(t, e, "c1", "A"), placementOf(t, e, "c1", "B"))
	}
}

func TestSheetCloseFromAnotherBoard(t *testing.T) {
	e := placedWorld(t)
	m := mux(asUser(e, "both"))
	// Beta has no closing column: closing from Beta sets the per-card flag in place and leaves Alpha's placement alone
	w := post(m, "/ui/cards/c1/close", url.Values{"closed": {"true"}, "board": {"B"}}, true)
	mustOK(t, "close from Beta", w.Code, w.Body.String())
	var closed bool
	_ = e.DB.QueryRow(`SELECT is_closed FROM cards WHERE id='c1'`).Scan(&closed)
	if !closed {
		t.Fatal("not closed")
	}
	if placementOf(t, e, "c1", "A") != "la1" || placementOf(t, e, "c1", "B") != "lb2" {
		t.Errorf("close from Beta moved a placement: A=%s B=%s", placementOf(t, e, "c1", "A"), placementOf(t, e, "c1", "B"))
	}
	if !strings.Contains(w.Body.String(), `data-board="B"`) || !strings.Contains(w.Body.String(), `data-closed="1"`) {
		t.Errorf("sheet after close: %.300s", w.Body.String())
	}
	// the board shortcut x (no sheet): the board page sends its board
	w = post(m, "/ui/cards/c1/close", url.Values{"closed": {"false"}, "_part": {"none"}, "board": {"B"}}, true)
	mustOK(t, "re-open from the Beta board", w.Code, w.Body.String())
	_ = e.DB.QueryRow(`SELECT is_closed FROM cards WHERE id='c1'`).Scan(&closed)
	if closed {
		t.Error("not re-opened")
	}
	// from Alpha (which has a closing column) the Alpha placement moves into it
	w = post(m, "/ui/cards/c1/close", url.Values{"closed": {"true"}, "board": {"A"}}, true)
	mustOK(t, "close from Alpha", w.Code, w.Body.String())
	if placementOf(t, e, "c1", "A") != "la2" || placementOf(t, e, "c1", "B") != "lb2" {
		t.Errorf("close from Alpha: A=%s B=%s", placementOf(t, e, "c1", "A"), placementOf(t, e, "c1", "B"))
	}
}

func TestSheetDuplicateFromAnotherBoard(t *testing.T) {
	e := placedWorld(t)
	m := mux(asUser(e, "both"))
	w := post(m, "/ui/cards/c1/duplicate", url.Values{"board": {"B"}}, true)
	mustOK(t, "duplicate from Beta", w.Code, w.Body.String())
	var board string
	_ = e.DB.QueryRow(`SELECT board_id FROM cards WHERE name='Shared card (copy)'`).Scan(&board)
	if board != "B" {
		t.Errorf("the copy is on %q, want the context board B", board)
	}
}

func TestCardLevelActionsFromAnotherBoard(t *testing.T) {
	e := placedWorld(t)
	withSystemLists(t, e)
	m := mux(asUser(e, "both"))
	// archive from the Beta sheet: every placement goes to its board's archive
	w := post(m, "/ui/cards/c1/archive", url.Values{"trash": {"false"}, "board": {"B"}}, true)
	mustOK(t, "archive from Beta", w.Code, w.Body.String())
	for b, want := range map[string]string{"A": "laA", "B": "lbA", "A2": "la2A"} {
		if got := placementOf(t, e, "c1", b); got != want {
			t.Errorf("after archive: %s placement in %q, want %q", b, got, want)
		}
	}
	// the board's archive key (quick route, Beta board) and its restore
	w = post(m, "/ui/cards/c1/quick/restore", url.Values{"board": {"B"}}, true)
	mustOK(t, "restore from the Beta board", w.Code, w.Body.String())
	for b, want := range map[string]string{"A": "la1", "B": "lb2", "A2": "la2a"} {
		if got := placementOf(t, e, "c1", b); got != want {
			t.Errorf("after restore: %s placement in %q, want %q", b, got, want)
		}
	}
	w = post(m, "/ui/cards/c1/quick/archive", url.Values{"trash": {"1"}, "board": {"B"}}, true)
	mustOK(t, "trash from the Beta board", w.Code, w.Body.String())
	for b, want := range map[string]string{"A": "ltA", "B": "lbT", "A2": "la2T"} {
		if got := placementOf(t, e, "c1", b); got != want {
			t.Errorf("after trash: %s placement in %q, want %q", b, got, want)
		}
	}
	// delete permanently from the Beta sheet (the card is in the trash on every board)
	w = post(m, "/ui/cards/c1/delete", url.Values{"board": {"B"}}, true)
	mustOK(t, "delete from Beta", w.Code, w.Body.String())
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM cards WHERE id='c1'`).Scan(&n)
	if n != 0 {
		t.Error("card not deleted")
	}
	if !strings.Contains(w.Header().Get("HX-Trigger"), `"boardId":"B"`) {
		t.Errorf("delete from Beta: the sheet is told to go back to %s", w.Header().Get("HX-Trigger"))
	}
}

// A user who edits the card on Alpha but only views Beta: from the Beta context the sheet offers no Beta column
// switcher and no Beta label picker (those write Beta's placement, which they may not change), but the card-level
// actions (title, archive) stay, since they edit the card.
func TestSheetContextReadOnlyBoard(t *testing.T) {
	e := placedWorld(t)
	if _, err := e.DB.Exec(`INSERT INTO users(id,email,name,role,created_at) VALUES('aedit','aedit@example.com','User aedit','boardUser','x');
		INSERT INTO board_memberships(id,board_id,user_id,role,created_at) VALUES('bmae1','A','aedit','editor','x'),('bmae2','B','aedit','viewer','x')`); err != nil {
		t.Fatal(err)
	}
	m := mux(asUser(e, "aedit"))
	_, body := get(t, m, "/ui/cards/c1?board=B", true)
	if !strings.Contains(body, `data-board="B"`) {
		t.Fatalf("sheet not on Beta: %.300s", body)
	}
	if strings.Contains(body, `data-pop="list"`) {
		t.Error("a Beta viewer gets the Beta column switcher")
	}
	if strings.Contains(body, `data-pop="labels"`) {
		t.Error("a Beta viewer gets the Beta label picker")
	}
	if !strings.Contains(body, `data-cap="card-edit"`) {
		t.Error("the card title should stay editable (card-level edit)")
	}
	// and the op agrees: a Beta move is forbidden for them
	if w := post(m, "/ui/cards/c1/move", url.Values{"listId": {"lb1"}, "board": {"B"}}, true); w.Code != 403 {
		t.Errorf("Beta move by a Beta viewer: %d", w.Code)
	}
	// on Alpha everything is there
	_, body = get(t, m, "/ui/cards/c1?board=A", true)
	if !strings.Contains(body, `data-pop="list"`) || !strings.Contains(body, `data-pop="labels"`) {
		t.Error("Alpha editor lost the Alpha column switcher or labels")
	}
}

// between cuts the text from the first from up to the next to (inclusive of neither); "" when absent.
func between(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return ""
	}
	s = s[i+len(from):]
	if j := strings.Index(s, to); j >= 0 {
		return s[:j]
	}
	return s
}
