package boards_test

import (
	"database/sql"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// inboxSection returns the Inbox panel of a rendered board page ("" when there is none).
func inboxSection(body string) string {
	i := strings.Index(body, `<section class="boards-col boards-inbox"`)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], "</section>")
	if j < 0 {
		return body[i:]
	}
	return body[i : i+j]
}

func inboxBoard(t *testing.T, e *ops.Env) (boards.Board, boards.List, boards.List) {
	t.Helper()
	b := newBoard(t, e, "Inbox board")
	todo, doing := addList(t, e, b.ID, "Todo"), addList(t, e, b.ID, "Doing")
	on := true
	call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, HasInbox: &on})
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: editor.UserID, Role: "editor"})
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	return b, todo, doing
}

func draft(t *testing.T, e *ops.Env, boardID, name string) string {
	t.Helper()
	if _, err := boards.CallByForm(e, "card.draft.create", url.Values{"boardId": {boardID}, "name": {name}}); err != nil {
		t.Fatalf("draft: %v", err)
	}
	var id string
	_ = e.DB.QueryRow(`SELECT id FROM cards WHERE board_id=? AND name=? AND is_draft=1`, boardID, name).Scan(&id)
	if id == "" {
		t.Fatalf("draft %q not stored", name)
	}
	return id
}

func TestInboxPanelOnTheBoard(t *testing.T) {
	e := fixture(t)
	b, todo, _ := inboxBoard(t, e)
	draft(t, e, b.ID, "Idea one")
	draft(t, e, b.ID, "Idea <two>")
	_, _ = boards.CallByForm(e, "card.create", url.Values{"listId": {todo.ID}, "name": {"Real card"}})

	// an editor sees the panel first, with its drafts, the composer and the move control
	code, body := get(t, mux(testutil.As(e, editor)), "/boards/"+b.ID, false)
	if code != 200 {
		t.Fatalf("board: %d", code)
	}
	panel := inboxSection(body)
	if panel == "" {
		t.Fatal("no Inbox panel on a board with the inbox on")
	}
	if strings.Index(body, `class="boards-col boards-inbox"`) > strings.Index(body, `id="col-`+todo.ID+`"`) {
		t.Error("the Inbox must be the first column")
	}
	for _, want := range []string{"Ideas not yet on the board", "Idea one", "Idea &lt;two&gt;", `<span data-count>2</span>`,
		`action="/ui/boards/` + b.ID + `/inbox"`, `data-publish`, `value="` + todo.ID + `"`, `/avatars/`} {
		if !strings.Contains(panel, want) {
			t.Errorf("panel misses %q", want)
		}
	}
	if strings.Contains(panel, "Real card") {
		t.Error("a published card must not be in the Inbox")
	}
	// drafts are never board tiles: they appear only inside the panel
	if strings.Count(body, "Idea one") != strings.Count(panel, "Idea one") {
		t.Error("a draft leaked out of the Inbox panel")
	}
	// the live-refresh body carries the panel too
	if _, frag := get(t, mux(testutil.As(e, editor)), "/ui/boards/"+b.ID+"/body", true); inboxSection(frag) == "" {
		t.Error("body fragment without the Inbox")
	}
	// list and grid views point to it instead
	for _, v := range []string{"list", "grid"} {
		_, lb := get(t, mux(testutil.As(e, editor)), "/boards/"+b.ID+"?view="+v, false)
		if inboxSection(lb) != "" || !strings.Contains(lb, `class="boards-inbox-link"`) || strings.Contains(lb, "Idea one") {
			t.Errorf("%s view: want only the Inbox link", v)
		}
	}

	// a viewer sees the drafts, but neither the composer nor the move control
	_, vb := get(t, mux(testutil.As(e, viewer)), "/boards/"+b.ID, false)
	vp := inboxSection(vb)
	if vp == "" || !strings.Contains(vp, "Idea one") {
		t.Fatal("viewer: no Inbox")
	}
	if strings.Contains(vp, "data-composer") || strings.Contains(vp, "data-publish") {
		t.Error("viewer must not get the composer or the move control")
	}

	// inbox off: nothing at all
	off := false
	call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, HasInbox: &off})
	_, ob := get(t, mux(testutil.As(e, editor)), "/boards/"+b.ID, false)
	if inboxSection(ob) != "" || strings.Contains(ob, "Idea one") || strings.Contains(ob, "boards-inbox-link") {
		t.Error("inbox off must render no panel and no drafts")
	}
	if c, _ := get(t, mux(testutil.As(e, editor)), "/ui/boards/"+b.ID+"/inbox", true); c != 404 {
		t.Errorf("panel fragment with the inbox off: %d", c)
	}
}

func TestInboxNeverOnPublicViews(t *testing.T) {
	e := fixture(t)
	b, _, _ := inboxBoard(t, e)
	draft(t, e, b.ID, "Secret idea")
	pb := call[boards.Board](t, e, "board.setPublic", boards.BoardPublicIn{ID: b.ID, IsPublic: true})

	// anonymous public page
	code, body := get(t, mux(testutil.As(e, nil)), "/public/"+pb.PublicToken, false)
	if code != 200 {
		t.Fatalf("public page: %d", code)
	}
	if strings.Contains(body, "Secret idea") || inboxSection(body) != "" {
		t.Error("the public page must not show drafts")
	}
	// a signed-in non-member who can see the board only because it is public
	code, body = get(t, mux(testutil.As(e, nobody)), "/boards/"+b.ID, false)
	if code != 200 {
		t.Fatalf("public board for a non-member: %d", code)
	}
	if strings.Contains(body, "Secret idea") || inboxSection(body) != "" {
		t.Error("a non-member must not see the drafts of a public board")
	}
	if c, _ := get(t, mux(testutil.As(e, nobody)), "/ui/boards/"+b.ID+"/inbox", true); c != 404 {
		t.Errorf("panel fragment for a non-member: %d", c)
	}
	// the board's own members still do
	if _, mb := get(t, mux(testutil.As(e, viewer)), "/boards/"+b.ID, false); !strings.Contains(inboxSection(mb), "Secret idea") {
		t.Error("members keep the Inbox on a public board")
	}
}

func TestInboxCaptureAndPublishThroughTheUI(t *testing.T) {
	e := fixture(t)
	b, todo, doing := inboxBoard(t, e)
	m := mux(testutil.As(e, editor))

	// capture: several lines, answered with draft tiles only (the card sheet never opens)
	w := post(m, "/ui/boards/"+b.ID+"/inbox", url.Values{"name": {"first idea\nsecond idea"}}, true)
	if w.Code != 200 {
		t.Fatalf("capture: %d %s", w.Code, w.Body.String())
	}
	if n := strings.Count(w.Body.String(), `class="boards-tile boards-draft"`); n != 2 {
		t.Fatalf("want 2 draft tiles, got %d: %s", n, w.Body.String())
	}
	if w.Header().Get("HX-Retarget") != "" || strings.Contains(w.Body.String(), "card-modal") || !strings.Contains(w.Header().Get("HX-Trigger"), "boards:added") {
		t.Error("capture must answer like the column composer")
	}
	var first string
	_ = e.DB.QueryRow(`SELECT id FROM cards WHERE name='first idea' AND is_draft=1`).Scan(&first)
	if first == "" {
		t.Fatal("draft not stored")
	}
	// empty input: nothing; viewers cannot capture
	if w = post(m, "/ui/boards/"+b.ID+"/inbox", url.Values{"name": {" \n "}}, true); w.Code != 204 {
		t.Errorf("empty capture: %d", w.Code)
	}
	if w = post(mux(testutil.As(e, viewer)), "/ui/boards/"+b.ID+"/inbox", url.Values{"name": {"x"}}, true); w.Code != 403 && w.Code != 404 {
		t.Errorf("viewer capture: %d", w.Code)
	}

	// drafts never show up as tiles, in counts or in card.list
	_, body := get(t, m, "/boards/"+b.ID, false)
	if strings.Contains(strings.Replace(body, inboxSection(body), "", 1), "first idea") {
		t.Error("a draft is rendered outside the Inbox")
	}

	// move to column: the draft becomes a card at the end of Doing and leaves the Inbox
	_, _ = boards.CallByForm(e, "card.create", url.Values{"listId": {doing.ID}, "name": {"already there"}})
	if w = post(mux(testutil.As(e, viewer)), "/ui/boards/"+b.ID+"/inbox/publish", url.Values{"id": {first}, "listId": {doing.ID}}, true); w.Code != 403 && w.Code != 404 {
		t.Errorf("viewer publish: %d", w.Code)
	}
	w = post(m, "/ui/boards/"+b.ID+"/inbox/publish", url.Values{"id": {first}, "listId": {doing.ID}}, true)
	if w.Code != 200 {
		t.Fatalf("publish: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `id="tile-`+first+`"`) || strings.Contains(w.Body.String(), "boards-draft") {
		t.Errorf("publish must answer with the new card tile: %s", w.Body.String())
	}
	if !strings.Contains(w.Header().Get("HX-Trigger"), "boards:published") {
		t.Errorf("trigger: %q", w.Header().Get("HX-Trigger"))
	}
	if got := strings.Join(cardNames(t, e, doing.ID), ","); got != "already there,first idea" {
		t.Errorf("Doing: %s", got)
	}
	_, body = get(t, m, "/boards/"+b.ID, false)
	if strings.Contains(inboxSection(body), "first idea") || !strings.Contains(body, `id="tile-`+first+`"`) {
		t.Error("published card must leave the Inbox and be on the board")
	}
	// publishing a card that is no draft, or into another board's list, fails
	if w = post(m, "/ui/boards/"+b.ID+"/inbox/publish", url.Values{"id": {first}, "listId": {todo.ID}}, true); w.Code != 404 {
		t.Errorf("re-publish: %d", w.Code)
	}
	other := newBoard(t, e, "Other")
	ol := addList(t, e, other.ID, "Elsewhere")
	var second string
	_ = e.DB.QueryRow(`SELECT id FROM cards WHERE name='second idea' AND is_draft=1`).Scan(&second)
	// the card sheet of a draft says it is in the Inbox (its list_id is only a placeholder)
	if _, sheet := get(t, m, "/ui/cards/"+second, true); !strings.Contains(sheet, `<span class="cm-list-ro">Inbox</span>`) {
		t.Error("draft card sheet must show Inbox as its place")
	}
	if w = post(m, "/ui/boards/"+b.ID+"/inbox/publish", url.Values{"id": {second}, "listId": {ol.ID}}, true); w.Code != 404 {
		t.Errorf("foreign list: %d", w.Code)
	}

	// drag from the Inbox onto a column: the drop endpoint publishes the draft at the dropped position
	var top string
	_ = e.DB.QueryRow(`SELECT id FROM cards WHERE name='already there'`).Scan(&top)
	w = post(m, "/ui/boards/"+b.ID+"/cards/drop", url.Values{"id": {second}, "container": {doing.ID}, "beforeId": {top}}, true)
	if w.Code != 204 {
		t.Fatalf("drop: %d %s", w.Code, w.Body.String())
	}
	if got := strings.Join(cardNames(t, e, doing.ID), ","); got != "second idea,already there,first idea" {
		t.Errorf("after drop: %s", got)
	}
	var stillDraft bool
	_ = e.DB.QueryRow(`SELECT is_draft FROM cards WHERE id=?`, second).Scan(&stillDraft)
	if stillDraft {
		t.Error("dropped draft is still a draft")
	}
}

func TestInboxSettingsToggle(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Toggle")
	addList(t, e, b.ID, "Todo")
	m := mux(e)
	// the settings form posts the checkbox plus its hidden "false" fallback
	r := httptest.NewRequest("PATCH", "/ui/boards/"+b.ID, strings.NewReader(url.Values{"_section": {"views"}, "hasInbox": {"true", "false"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("settings: %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Adds an Inbox column") || !strings.Contains(w.Body.String(), "first column of the") {
		t.Error("settings: help text or pointer to the Inbox missing")
	}
	if d := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: b.ID}); !d.Board.HasInbox {
		t.Fatal("hasInbox not persisted")
	}
	if _, body := get(t, m, "/boards/"+b.ID, false); inboxSection(body) == "" || !strings.Contains(body, "Nothing in the inbox.") {
		t.Error("the board must show the (empty) Inbox once it is on")
	}
	// the op route turns it off again
	if w := post(m, "/ui/boards/"+b.ID+"/inbox/settings", url.Values{"enabled": {"false"}}, true); w.Code != 204 {
		t.Fatalf("inbox/settings: %d", w.Code)
	}
	if _, body := get(t, m, "/boards/"+b.ID, false); inboxSection(body) != "" {
		t.Error("inbox still shown after turning it off")
	}
}

// The Inbox costs a constant number of queries on the board page, whatever the number of drafts.
func TestInboxQueryCountIsConstant(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	e := fixture(t)
	e.DB = d
	for _, u := range []*ops.Principal{admin, editor, viewer} {
		_, _ = e.DB.Exec(`INSERT INTO users(id,email,name,role,created_at) VALUES(?,?,?,?,'2024-01-01T00:00:00Z')`, u.UserID, u.UserID+"@x.io", u.Name, u.Role)
	}
	_, _ = e.DB.Exec(`INSERT INTO projects(id,name,created_at) VALUES('p1','Proj','2024-01-01T00:00:00Z')`)
	b, _, _ := inboxBoard(t, e)
	cdb, err := sql.Open("sqlite-counting", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer cdb.Close()
	cdb.SetMaxOpenConns(1)
	ce := testutil.As(e, editor)
	ce.DB = &db.DB{DB: cdb}
	m := mux(ce)
	count := func(wantPanel bool) int64 {
		queries.Store(0)
		code, body := get(t, m, "/ui/boards/"+b.ID+"/body", true)
		if code != 200 || (inboxSection(body) != "") != wantPanel {
			t.Fatalf("body: %d, panel %v", code, inboxSection(body) != "")
		}
		return queries.Load()
	}
	draft(t, e, b.ID, "one")
	one := count(true)
	for i := 0; i < 11; i++ {
		draft(t, e, b.ID, "more")
	}
	many := count(true)
	off := false
	call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, HasInbox: &off})
	none := count(false)
	if one != many || one-none > 3 {
		t.Fatalf("queries: inbox off %d, 1 draft %d, 12 drafts %d (want equal, at most 3 more than off)", none, one, many)
	}
	t.Logf("queries: inbox off %d, on %d", none, one)
}
