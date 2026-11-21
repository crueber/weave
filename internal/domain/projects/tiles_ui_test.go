package projects_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/domain/projects"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
	"github.com/crueber/weave/internal/web"
)

// The project page tiles: empty state, conditional chips, most recently active first, outsiders see nothing.
func TestProjectPageBoardTiles(t *testing.T) {
	e, _, owner, plain := setup(t)
	p := call[projects.Project](t, e, "project.create", projects.CreateIn{Name: "Tiles"})
	quiet := call[boards.Board](t, e, "board.create", boards.BoardCreateIn{ProjectID: p.ID, Name: "Quiet board"})
	busy := call[boards.Board](t, e, "board.create", boards.BoardCreateIn{ProjectID: p.ID, Name: "Busy board"})
	empty := call[boards.Board](t, e, "board.create", boards.BoardCreateIn{ProjectID: p.ID, Name: "Empty board"})
	for _, b := range []boards.Board{quiet, busy} {
		l := call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Todo"})
		if _, err := e.DB.Exec(`INSERT INTO cards(id,board_id,list_id,position,name,list_changed_at,created_at) VALUES(?,?,?,1,'Hidden card title',?,?)`,
			db.NewID(), b.ID, l.ID, db.Now(), db.Now()); err != nil {
			t.Fatal(err)
		}
	}
	// activity: busy is newest, quiet went silent long ago, empty in between
	_, _ = e.DB.Exec(`UPDATE actions SET created_at='2020-01-01T00:00:00Z' WHERE board_id=?`, quiet.ID)
	_, _ = e.DB.Exec(`UPDATE actions SET created_at='2021-01-01T00:00:00Z' WHERE board_id=?`, empty.ID)
	get := func(u *ops.Principal) *httptest.ResponseRecorder {
		mux := http.NewServeMux()
		web.Mount(mux, func(r *http.Request) *ops.Env { c := *testutil.As(e, u); c.Req = r; c.Ctx = r.Context(); return &c })
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/projects/"+p.ID, nil))
		return w
	}
	w := get(owner)
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("code %d: %s", w.Code, body)
	}
	bi, ei, qi := strings.Index(body, "Busy board"), strings.Index(body, "Empty board"), strings.Index(body, "Quiet board")
	if bi < 0 || !(bi < ei && ei < qi) {
		t.Fatalf("order busy=%d empty=%d quiet=%d (want most recently active first)", bi, ei, qi)
	}
	if strings.Index(body, "+ Create board") < qi {
		t.Fatal("the Create board tile stays last")
	}
	if !strings.Contains(body, "No cards yet") || !strings.Contains(body, `class="sr-only">: 1 open card, `) {
		t.Fatalf("empty state / sentence missing:\n%s", body)
	}
	for _, no := range []string{"overdue", "due this week", "claimed", "listening", "0 open", "Hidden card title"} {
		if strings.Contains(body, no) {
			t.Errorf("page must not contain %q when the count is 0 (or card names ever)", no)
		}
	}
	if strings.Count(body, `class="projects-board pb"`) != 3 || strings.Count(body, "<a class=\"projects-board") != 3 {
		t.Fatal("each tile is exactly one link")
	}
	// an outsider gets no tiles (project not found)
	if w := get(plain); w.Code == 200 && strings.Contains(w.Body.String(), "Busy board") {
		t.Fatal("outsider sees board tiles")
	}
}
