package boards_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/testutil"
)

// The card shortcuts (boards_cardkeys.js): keyboard pickers for labels / due date / location, Delete = move to trash
// with Undo, the tile's time-tracking and location indicators, and the 204 answers the board uses.
func TestCardShortcutRoutes(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Keys")
	l := addList(t, e, b.ID, "Todo")
	lab := call[boards.Label](t, e, "label.create", boards.LabelCreateIn{BoardID: b.ID, Name: "Urgent", Color: "berry-red"})
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	addCard(t, e, b.ID, l.ID, "k1", "Keyed card", 1, "2030-01-15T09:30:00Z")
	m := mux(e)

	// pickers render for editors with the card's current state
	if code, body := get(t, m, "/ui/cards/k1/quick/labels", true); code != 200 || !strings.Contains(body, `data-kpick="labels"`) || !strings.Contains(body, `data-label="`+lab.ID+`"`) || !strings.Contains(body, `aria-pressed="false"`) {
		t.Fatalf("labels picker: %d %s", code, body)
	}
	if code, body := get(t, m, "/ui/cards/k1/quick/members", true); code != 200 || !strings.Contains(body, `data-kpick="members"`) || !strings.Contains(body, `data-member="`+viewer.UserID+`"`) || !strings.Contains(body, `aria-pressed="false"`) {
		t.Fatalf("members picker: %d %s", code, body)
	}
	if code, body := get(t, m, "/ui/cards/k1/quick/due", true); code != 200 || !strings.Contains(body, `value="2030-01-15T09:30"`) || !strings.Contains(body, "data-due-clear") {
		t.Fatalf("due picker: %d %s", code, body)
	}
	if code, body := get(t, m, "/ui/cards/k1/quick/place", true); code != 200 || !strings.Contains(body, `name="lat"`) || strings.Contains(body, "data-place-clear") {
		t.Fatalf("place picker: %d %s", code, body)
	}
	// toggling a label from the picker re-renders the picker (not the quick-edit popover)
	w := post(m, "/ui/cards/k1/quick/label", url.Values{"labelId": {lab.ID}, "on": {"1"}, "_part": {"labels"}}, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data-kpick="labels"`) || !strings.Contains(w.Body.String(), `aria-pressed="true"`) || strings.Contains(w.Body.String(), "data-quick-name") {
		t.Fatalf("picker label toggle: %d %s", w.Code, w.Body.String())
	}
	// toggling a member from the picker assigns/unassigns and re-renders the members picker
	w = post(m, "/ui/cards/k1/quick/member", url.Values{"userId": {viewer.UserID}, "on": {"1"}}, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data-kpick="members"`) || !strings.Contains(w.Body.String(), `aria-pressed="true"`) {
		t.Fatalf("picker member assign: %d %s", w.Code, w.Body.String())
	}
	var assigned int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM card_memberships WHERE card_id='k1' AND user_id=?`, viewer.UserID).Scan(&assigned)
	if assigned != 1 {
		t.Fatalf("expected member assigned, count=%d", assigned)
	}
	w = post(m, "/ui/cards/k1/quick/member", url.Values{"userId": {viewer.UserID}, "on": {"0"}}, true)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data-kpick="members"`) || strings.Contains(w.Body.String(), `aria-pressed="true"`) {
		t.Fatalf("picker member unassign: %d %s", w.Code, w.Body.String())
	}
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM card_memberships WHERE card_id='k1' AND user_id=?`, viewer.UserID).Scan(&assigned)
	if assigned != 0 {
		t.Fatalf("expected member unassigned, count=%d", assigned)
	}
	// due date and location from the pickers answer 204 (the board refreshes the tile itself)
	if w = post(m, "/ui/cards/k1/due", url.Values{"dueDate": {"2031-02-03T17:00"}, "_part": {"none"}}, true); w.Code != 204 {
		t.Fatalf("due: %d %s", w.Code, w.Body.String())
	}
	if w = post(m, "/ui/cards/k1/location", url.Values{"locationName": {"Harbour"}, "lat": {"52.37"}, "lng": {"4.89"}, "_part": {"none"}}, true); w.Code != 204 {
		t.Fatalf("location: %d %s", w.Code, w.Body.String())
	}
	if _, body := get(t, m, "/ui/cards/k1/quick/place", true); !strings.Contains(body, `value="Harbour"`) || !strings.Contains(body, `value="52.37"`) || !strings.Contains(body, "data-place-clear") {
		t.Fatalf("place picker after set: %s", body)
	}
	// the tile shows the location and, while the stopwatch runs, a tracking indicator
	if _, body := get(t, m, "/ui/cards/k1/tile", true); !strings.Contains(body, "boards-place") || !strings.Contains(body, "Harbour") || strings.Contains(body, "boards-timer") {
		t.Fatalf("tile before start: %s", body)
	}
	if w = post(m, "/ui/cards/k1/time/start", url.Values{}, true); w.Code != 200 {
		t.Fatalf("time start: %d %s", w.Code, w.Body.String())
	}
	if w = post(m, "/ui/cards/k1/time/start", url.Values{}, true); w.Code != 409 {
		t.Errorf("second start must conflict: %d", w.Code)
	}
	if _, body := get(t, m, "/ui/cards/k1/tile", true); !strings.Contains(body, "boards-timer") {
		t.Fatalf("tile while running: %s", body)
	}
	if w = post(m, "/ui/cards/k1/time/stop", url.Values{}, true); w.Code != 200 {
		t.Fatalf("time stop: %d", w.Code)
	}
	// close / re-open with _part=none
	if w = post(m, "/ui/cards/k1/close", url.Values{"closed": {"true"}, "_part": {"none"}}, true); w.Code != 204 {
		t.Fatalf("close: %d %s", w.Code, w.Body.String())
	}
	if _, body := get(t, m, "/ui/cards/k1/tile", true); !strings.Contains(body, "is-closed") {
		t.Errorf("closed tile: %s", body)
	}
	if w = post(m, "/ui/cards/k1/close", url.Values{"closed": {"false"}, "_part": {"none"}}, true); w.Code != 204 {
		t.Fatalf("re-open: %d", w.Code)
	}

	// viewers: no pickers, no writes
	vm := mux(testutil.As(e, viewer))
	for _, k := range []string{"labels", "members", "due", "place"} {
		if code, _ := get(t, vm, "/ui/cards/k1/quick/"+k, true); code != 403 && code != 404 {
			t.Errorf("viewer picker %s: %d", k, code)
		}
	}
	for path, vals := range map[string]url.Values{
		"/ui/cards/k1/quick/archive": {"trash": {"1"}}, "/ui/cards/k1/close": {"closed": {"true"}, "_part": {"none"}},
		"/ui/cards/k1/due": {"dueDate": {""}, "_part": {"none"}}, "/ui/cards/k1/location": {"clear": {"true"}, "_part": {"none"}},
		"/ui/cards/k1/time/start": {}, "/ui/cards/k1/quick/label": {"labelId": {lab.ID}, "on": {"0"}, "_part": {"labels"}},
		"/ui/cards/k1/quick/member": {"userId": {viewer.UserID}, "on": {"1"}},
	} {
		if w := post(vm, path, vals, true); w.Code != 403 && w.Code != 404 {
			t.Errorf("viewer POST %s: %d", path, w.Code)
		}
	}
	var due *string
	_ = e.DB.QueryRow(`SELECT due_date FROM cards WHERE id='k1'`).Scan(&due)
	if due == nil {
		t.Error("viewer cleared the due date")
	}

	// Delete on a tile = move to the trash list, with an Undo that restores it
	w = post(m, "/ui/cards/k1/quick/archive", url.Values{"trash": {"1"}}, true)
	if w.Code != 204 || !strings.Contains(w.Header().Get("HX-Trigger"), "moved to trash") || !strings.Contains(w.Header().Get("HX-Trigger"), "/quick/restore") {
		t.Fatalf("trash: %d %q", w.Code, w.Header().Get("HX-Trigger"))
	}
	var lt string
	_ = e.DB.QueryRow(`SELECT l.type FROM cards c JOIN lists l ON l.id=c.list_id WHERE c.id='k1'`).Scan(&lt)
	if lt != "trash" {
		t.Fatalf("card is in a %q list, want trash", lt)
	}
	if w = post(m, "/ui/cards/k1/quick/restore", url.Values{}, true); w.Code != 204 {
		t.Fatalf("restore: %d", w.Code)
	}
	if got := cardNames(t, e, l.ID); len(got) != 1 {
		t.Fatalf("not restored: %v", got)
	}
}
