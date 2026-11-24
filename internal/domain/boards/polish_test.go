package boards_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// TestLeaveBoardControl: Board settings offer "Leave board" to a member with a direct membership (the op lets any
// member remove their own), disabled with the reason for a sole owner (the last-owner rule); leaving redirects away
// from the settings page.
func TestLeaveBoardControl(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Leavable")
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: editor.UserID, Role: "owner"})
	page := func(p *ops.Principal) (int, string) {
		return get(t, mux(testutil.As(e, p)), "/boards/"+b.ID+"/settings?section=members", false)
	}
	code, html := page(viewer)
	if code != 200 || !strings.Contains(html, `data-cap="board-leave"`) || strings.Contains(html, `aria-disabled="true">Leave board`) {
		t.Fatalf("viewer: %d, the leave control must be there and enabled", code)
	}
	// the sole owner sees it disabled with the reason
	if _, html := page(editor); !strings.Contains(html, `aria-disabled="true">Leave board`) || !strings.Contains(html, "only owner") {
		t.Fatal("sole owner: the leave control must be disabled with the reason")
	}
	// leaving: the viewer's membership goes, and the response sends them away from the settings
	var mid string
	_ = e.DB.QueryRow(`SELECT id FROM board_memberships WHERE board_id=? AND user_id=?`, b.ID, viewer.UserID).Scan(&mid)
	r := httptest.NewRequest("DELETE", "/ui/board-members/"+mid, nil)
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	mux(testutil.As(e, viewer)).ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Header().Get("HX-Redirect") == "" {
		t.Fatalf("leave: %d redirect %q %s", w.Code, w.Header().Get("HX-Redirect"), w.Body.String())
	}
	if code, _ := page(viewer); code != 404 {
		t.Fatalf("after leaving the settings answer %d", code)
	}
}

// TestWorkerQuickEditOnOwnCards: a worker with a viewer membership edits the cards they are a member of (the
// worker rule), so those tiles offer quick edit and the popover opens; other cards stay read-only.
func TestWorkerQuickEditOnOwnCards(t *testing.T) {
	e := fixture(t)
	worker := &ops.Principal{UserID: "u-work", Role: "worker", Name: "Wo Rker"}
	if _, err := e.DB.Exec(`INSERT INTO users(id,email,name,role,created_at) VALUES('u-work','w@x.io','Wo Rker','worker','2024-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	b := newBoard(t, e, "Work")
	l := addList(t, e, b.ID, "Todo")
	addCard(t, e, b.ID, l.ID, "mine", "Mine", 1, "")
	addCard(t, e, b.ID, l.ID, "other", "Other", 2, "")
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: worker.UserID, Role: "viewer"})
	if _, err := e.DB.Exec(`INSERT INTO card_memberships(card_id,user_id) VALUES('mine','u-work')`); err != nil {
		t.Fatal(err)
	}
	m := mux(testutil.As(e, worker))
	code, html := get(t, m, "/boards/"+b.ID, false)
	if code != 200 {
		t.Fatalf("board: %d", code)
	}
	if n := strings.Count(html, `class="boards-tile-edit" data-quick`); n != 1 {
		t.Fatalf("worker sees %d quick-edit buttons, want 1 (their own card)", n)
	}
	if code, _ := get(t, m, "/ui/cards/mine/quick", true); code != 200 {
		t.Fatalf("quick edit of the worker's card: %d", code)
	}
	if code, _ := get(t, m, "/ui/cards/other/quick", true); code != 403 {
		t.Fatalf("quick edit of another card: %d, want 403", code)
	}
	// the popover's rename goes through card.update, which the worker rule allows on their own card only
	if w := post(m, "/ui/cards/mine/quick", url.Values{"name": {"Mine, renamed"}}, true); w.Code != 200 {
		t.Fatalf("worker rename of their card: %d %s", w.Code, w.Body.String())
	}
	if got := strings.Join(cardNames(t, e, l.ID), ","); got != "Mine, renamed,Other" {
		t.Fatalf("names after rename: %s", got)
	}
	if w := post(m, "/ui/cards/other/quick", url.Values{"name": {"nope"}}, true); w.Code != 403 {
		t.Fatalf("worker rename of another card: %d, want 403", w.Code)
	}
}
