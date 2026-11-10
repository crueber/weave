package cardextras_test

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/cardextras"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
	"github.com/crueber/weave/internal/web"
)

type uiClient struct {
	t    *testing.T
	h    http.Handler
	user *ops.Principal
	base *ops.Env
}

func newUI(t *testing.T) *uiClient {
	e := fixture(t)
	c := &uiClient{t: t, base: e, user: editor}
	mux := http.NewServeMux()
	web.Mount(mux, func(r *http.Request) *ops.Env {
		env := testutil.As(e, c.user)
		env.Ctx, env.Req = r.Context(), r
		return env
	})
	c.h = mux
	return c
}

func (c *uiClient) do(method, path string, form url.Values) *httptest.ResponseRecorder {
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	return w
}

func (c *uiClient) ok(method, path string, form url.Values, want ...string) string {
	c.t.Helper()
	w := c.do(method, path, form)
	if w.Code != 200 {
		c.t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
	}
	for _, s := range want {
		if !strings.Contains(w.Body.String(), s) {
			c.t.Fatalf("%s %s: missing %q in\n%s", method, path, s, w.Body.String())
		}
	}
	return w.Body.String()
}

func TestUIFragments(t *testing.T) {
	c := newUI(t)

	// tasks
	c.ok("GET", "/ui/cards/c1/tasks", nil, `id="x-tasks"`, "New task list")
	body := c.ok("POST", "/ui/cards/c1/task-lists", url.Values{"name": {"Checklist"}}, "Checklist")
	tl := between(body, `data-id="`, `"`, 0)
	c.ok("POST", "/ui/task-lists/"+tl+"/tasks", url.Values{"name": {"Write tests"}, "assigneeUserId": {"uview"}, "linkedCardId": {""}}, "Write tests", "Vic")
	body = c.ok("POST", "/ui/task-lists/"+tl+"/tasks", url.Values{"name": {"Second"}}, "Second")
	tasks := call[[]cardextras.TaskListItem](t, c.base, "tasklist.list", cardextras.TaskListsIn{CardID: "c1"})[0].Tasks
	c.ok("POST", "/ui/tasks/"+tasks[0].ID, url.Values{"isCompleted": {"true"}}, "done")
	c.ok("POST", "/ui/tasks/reorder", url.Values{"id": {tasks[1].ID}, "container": {tl}, "beforeId": {tasks[0].ID}}, "Second")
	w := c.do("POST", "/ui/tasks/reorder?_card=c1", url.Values{"id": {tasks[1].ID}, "container": {tl}, "afterId": {tasks[0].ID}})
	if w.Header().Get("HX-Retarget") != "#x-tasks" || w.Header().Get("HX-Reswap") != "outerHTML" {
		t.Fatalf("reorder headers: %v", w.Header())
	}
	c.ok("POST", "/ui/task-lists/"+tl+"", url.Values{"hideCompletedTasks": {"true"}}, "completed hidden")
	c.ok("POST", "/ui/tasks/"+tasks[1].ID+"/delete", nil)
	c.ok("POST", "/ui/task-lists/"+tl+"/delete", nil, "No task lists yet")

	// comments
	c.ok("GET", "/ui/cards/c1/comments", nil, "Comments", "No comments yet")
	c.ok("POST", "/ui/cards/c1/comments", url.Values{"text": {"<b>hi</b>"}}, "&lt;b&gt;hi&lt;/b&gt;", "(1)")
	cm := call[cardextras.CommentsOut](t, c.base, "comment.list", cardextras.CommentsIn{CardID: "c1"}).Items[0]
	c.ok("POST", "/ui/comments/"+cm.ID, url.Values{"text": {"changed"}}, "changed", "(edited)")
	c.user = viewer
	c.ok("GET", "/ui/cards/c1/comments", nil, "do not have permission")
	if w := c.do("POST", "/ui/cards/c1/comments", url.Values{"text": {"x"}}); w.Code != 403 {
		t.Fatalf("viewer comment: %d", w.Code)
	}
	c.user = outsider
	if w := c.do("GET", "/ui/cards/c1/comments", nil); w.Code != 404 {
		t.Fatalf("outsider: %d", w.Code)
	}
	c.user = editor
	c.ok("POST", "/ui/comments/"+cm.ID+"/delete", nil, "(0)")

	// attachments: multipart upload, link, thumbnail + download routes, media page
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "pic.png")
	_, _ = fw.Write(pngBytes(500, 300))
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/ui/cards/c1/attachments", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("HX-Request", "true")
	rec := httptest.NewRecorder()
	c.h.ServeHTTP(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "pic.png") || !strings.Contains(rec.Body.String(), "cover") {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body.String())
	}
	c.ok("POST", "/ui/cards/c1/attachments", url.Values{"url": {"https://example.org/doc"}}, "example.org")
	atts := call[[]cardextras.AttachmentItem](t, c.base, "attachment.list", cardextras.CardIDIn{CardID: "c1"})
	img := atts[0]
	dl := c.do("GET", img.DownloadURL, nil)
	if dl.Code != 200 || dl.Header().Get("Content-Type") != "image/png" || !strings.HasPrefix(dl.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("download: %d %v", dl.Code, dl.Header())
	}
	th := c.do("GET", img.ThumbnailURL, nil)
	if th.Code != 200 || th.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("thumb: %d", th.Code)
	}
	c.user = outsider
	if w := c.do("GET", img.DownloadURL, nil); w.Code != 404 {
		t.Fatalf("outsider download: %d", w.Code)
	}
	c.user = editor
	c.ok("GET", "/boards/b1/media?type=image", nil, "pic.png", "Images")
	if strings.Contains(c.ok("GET", "/boards/b1/media?type=image", nil), "example.org") {
		t.Fatal("link shown in image filter")
	}
	c.ok("GET", "/boards/b1/media?type=doc", nil, "example.org")
	c.ok("POST", "/ui/attachments/"+img.ID+"/cover", url.Values{"unset": {"true"}}, "Make cover")
	c.ok("POST", "/ui/attachments/"+img.ID+"/delete", nil)

	// custom fields incl. repeatable table
	c.ok("GET", "/ui/cards/c1/fields", nil, "No custom fields")
	c.ok("POST", "/ui/custom-field-groups", url.Values{"cardId": {"c1"}, "name": {"Parts"}, "repeatable": {"on"}}, "Parts", "table")
	g := call[[]cardextras.GroupItem](t, c.base, "customfieldgroup.list", cardextras.GroupListIn{CardID: "c1"})[0]
	c.ok("POST", "/ui/custom-fields", url.Values{"_card": {"c1"}, "groupId": {g.ID}, "name": {"Kind"}, "type": {"select"}, "options": {"gear, cog\nwheel"}}, "Kind", "gear, cog, wheel")
	c.ok("POST", "/ui/custom-fields", url.Values{"_card": {"c1"}, "groupId": {g.ID}, "name": {"Qty"}, "type": {"number"}}, "Qty")
	c.ok("POST", "/ui/cards/c1/custom-field-groups/"+g.ID+"/rows", nil, "Remove")
	c.ok("POST", "/ui/cards/c1/custom-field-groups/"+g.ID+"/rows", nil)
	g = call[[]cardextras.GroupItem](t, c.base, "customfieldgroup.list", cardextras.GroupListIn{CardID: "c1"})[0]
	c.ok("POST", "/ui/cards/c1/custom-field-values", url.Values{"groupId": {g.ID}, "fieldId": {g.Fields[1].ID}, "rowId": {g.Rows[0].ID}, "content": {"12"}}, `value="12"`)
	if w := c.do("POST", "/ui/cards/c1/custom-field-values", url.Values{"groupId": {g.ID}, "fieldId": {g.Fields[1].ID}, "rowId": {g.Rows[0].ID}, "content": {"abc"}}); w.Code != 422 {
		t.Fatalf("invalid number: %d", w.Code)
	}
	c.ok("POST", "/ui/custom-field-rows/reorder?_card=c1", url.Values{"id": {g.Rows[1].ID}, "container": {g.ID}, "beforeId": {g.Rows[0].ID}}, "Remove")
	c.ok("POST", "/ui/custom-field-groups/reorder?_card=c1", url.Values{"id": {g.ID}}, "Parts")
	c.ok("POST", "/ui/custom-field-groups", url.Values{"_card": {"c1"}, "boardId": {"b1"}, "name": {"Everyone"}}, "board-wide")

	// time
	c.ok("GET", "/ui/cards/c1/time", nil, "Time tracking", "Start")
	c.ok("POST", "/ui/cards/c1/time/start", nil, "running", "Stop")
	c.ok("POST", "/ui/cards/c1/time/stop", nil, "Start")
	c.ok("POST", "/ui/cards/c1/time-entries", url.Values{"hours": {"1"}, "minutes": {"5"}, "note": {"fixing"}}, "1h 05m 00s", "fixing")

	// activity
	c.ok("GET", "/ui/cards/c1/activity", nil, "Activity", "commented")
	c.ok("GET", "/ui/boards/b1/activity", nil, "Board activity", "Card one")
	c.user = outsider
	if w := c.do("GET", "/ui/boards/b1/activity", nil); w.Code != 404 {
		t.Fatalf("outsider board activity: %d", w.Code)
	}
}

// between returns the nth (0-based) substring between start and end.
func between(s, start, end string, n int) string {
	for i := 0; ; i++ {
		a := strings.Index(s, start)
		if a < 0 {
			return ""
		}
		s = s[a+len(start):]
		b := strings.Index(s, end)
		if i == n {
			return s[:b]
		}
	}
}
