package boards_test

import (
	"net/http/httptest"
	"testing"

	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/testutil"
)

// On a public board anyone signed in may view (and export) it, but the settings page and its
// fragments are for members and managers only.
func TestPublicBoardSettingsAreMembersOnly(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Pub")
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: editor.UserID})
	call[boards.Board](t, e, "board.setPublic", boards.BoardPublicIn{ID: b.ID, IsPublic: true})

	stranger := mux(testutil.As(e, nobody))
	r := httptest.NewRecorder()
	stranger.ServeHTTP(r, httptest.NewRequest("GET", "/boards/"+b.ID+"/settings", nil))
	if r.Code != 303 || r.Header().Get("Location") != "/boards/"+b.ID {
		t.Fatalf("non-member settings page: %d -> %q, want redirect to the board", r.Code, r.Header().Get("Location"))
	}
	if code, _ := get(t, stranger, "/boards/"+b.ID, false); code != 200 {
		t.Errorf("a public board stays viewable for non-members: %d", code)
	}
	for _, p := range []string{"/ui/boards/" + b.ID + "/members", "/ui/boards/" + b.ID + "/sharing"} {
		if code, _ := get(t, stranger, p, true); code != 404 {
			t.Errorf("%s as non-member: %d, want 404", p, code)
		}
	}
	// viewing level is enough to export a public board
	if code, _ := get(t, stranger, "/ui/boards/"+b.ID+"/export?format=csv", false); code != 200 {
		t.Errorf("export of a public board as viewer: %d", code)
	}
	// members keep the page
	if code, _ := get(t, mux(testutil.As(e, editor)), "/boards/"+b.ID+"/settings", false); code != 200 {
		t.Errorf("member settings page: %d", code)
	}
	if code, _ := get(t, mux(e), "/boards/"+b.ID+"/settings", false); code != 200 {
		t.Errorf("admin settings page: %d", code)
	}
	// a private board stays invisible to non-members (404, not a redirect)
	priv := newBoard(t, e, "Priv")
	if code, _ := get(t, stranger, "/boards/"+priv.ID+"/settings", false); code != 404 {
		t.Errorf("private board settings as outsider: %d, want 404", code)
	}
}
