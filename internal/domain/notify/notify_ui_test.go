package notify_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

func TestWebhookValidationAndUIForms(t *testing.T) {
	w := seed(t)
	old := notify.RetryBackoff
	notify.RetryBackoff = nil
	defer func() { notify.RetryBackoff = old }()
	if _, err := ops.Call(w.alice, "webhook.create", &notify.WebhookCreateIn{Name: "x", URL: "http://h"}); err == nil {
		t.Fatal("non-admin must not create webhooks")
	}
	if _, err := ops.Call(w.admin, "webhook.create", &notify.WebhookCreateIn{Name: "x", URL: "http://h", Events: &notify.StringList{"nope"}}); err == nil {
		t.Fatal("unknown event accepted")
	}
	if _, err := ops.Call(w.admin, "webhook.create", &notify.WebhookCreateIn{Name: "x", URL: "ftp://h"}); err == nil {
		t.Fatal("bad URL accepted")
	}
	if len(notify.WebhookEvents) < 50 {
		t.Fatal("need 50+ events")
	}
	mux := http.NewServeMux()
	web.Mount(mux, func(r *http.Request) *ops.Env { c := *w.admin; c.Ctx = r.Context(); c.Req = r; return &c })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	post := func(path string, form url.Values) *http.Response {
		req, _ := http.NewRequest("POST", srv.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	r := post("/ui/admin/webhooks", url.Values{"name": {"ui"}, "url": {"http://127.0.0.1:1/h"}, "events": {"cardCreate", "boardUpdate"}, "excludedEvents": {"cardDelete"}})
	if r.StatusCode != 200 || r.Header.Get("HX-Redirect") == "" {
		t.Fatalf("create via UI: %d %v", r.StatusCode, r.Header)
	}
	l, _ := ops.Typed[notify.WebhookListOut](w.admin, "webhook.list", notify.WebhookListIn{})
	if len(l.Items) != 1 || len(l.Items[0].Events) != 2 || l.Items[0].ExcludedEvents[0] != "cardDelete" {
		t.Fatalf("webhooks = %+v", l.Items)
	}
	// clearing all checkboxes clears the filter
	post("/ui/admin/webhooks/"+l.Items[0].ID, url.Values{})
	l, _ = ops.Typed[notify.WebhookListOut](w.admin, "webhook.list", notify.WebhookListIn{})
	if len(l.Items[0].Events) != 0 {
		t.Fatalf("filter not cleared: %+v", l.Items[0])
	}
	// config form: unchecked checkbox sends hidden "false" only; checked sends "true" first
	post("/ui/admin/config", url.Values{"terms.enabled": {"false"}, "branding.appName": {"Form App"}})
	if notify.ConfigBool(w.admin, "terms.enabled") || notify.Config(w.admin, "branding.appName") != "Form App" {
		t.Fatal("config form not applied")
	}
	// agent prompt defaults page: one key per form; empty resets
	post("/ui/admin/agent-prompts", url.Values{"agentprompt.board": {"Board {board_name}\r\n"}})
	if notify.Config(w.admin, "agentprompt.board") != "Board {board_name}" {
		t.Fatalf("agent prompt default not saved: %q", notify.Config(w.admin, "agentprompt.board"))
	}
	if r, err := http.Get(srv.URL + "/admin/agent-prompts"); err != nil || r.StatusCode != 200 {
		t.Fatalf("agent prompts page: %v", err)
	} else {
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		for _, want := range []string{`name="agentprompt.general"`, `name="agentprompt.project"`, `name="agentprompt.runner"`, "Board {board_name}</textarea>", "Customised", "{questionnaire}", `aria-current="page">Agent prompts`} {
			if !strings.Contains(string(b), want) {
				t.Errorf("agent prompts page lacks %q", want)
			}
		}
	}
	post("/ui/admin/agent-prompts", url.Values{"agentprompt.board": {""}})
	if notify.Config(w.admin, "agentprompt.board") != "" {
		t.Fatal("agent prompt default not reset")
	}
	// dashboard form
	post("/ui/dashboard", url.Values{"panels": {"myCards", "overdue"}, "limit": {"5"}})
	d, _ := ops.Typed[notify.DashboardOut](w.admin, "dashboard.get", notify.NoInput{})
	if d.Limit != 5 || !d.Panels[1].Enabled || d.Panels[2].Enabled {
		t.Fatalf("dashboard form: %+v", d.Panels)
	}
	// reading a notification through the UI returns the list plus an out-of-band badge
	ops.Emit(w.alice, ops.Event{Type: "commentCreate", BoardID: "b1", CardID: "c1", Data: map[string]any{"text": "yo"}})
	notify.Wait()
	var nid string
	_ = w.admin.DB.QueryRow(`SELECT id FROM notifications WHERE user_id='bob'`).Scan(&nid)
	if nid == "" {
		t.Fatal("no notification")
	}
	mux2 := http.NewServeMux()
	web.Mount(mux2, func(r *http.Request) *ops.Env { c := *w.bob; c.Ctx = r.Context(); c.Req = r; return &c })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/ui/notifications/"+nid+"/read", nil)
	req.Header.Set("HX-Request", "true")
	mux2.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `id="notify-list"`) || !strings.Contains(rec.Body.String(), "hx-swap-oob") || strings.Contains(rec.Body.String(), "Mark read") {
		t.Fatalf("read via UI: %d %s", rec.Code, rec.Body.String())
	}
}

// The live bell's "New notifications" button reloads the list through GET /ui/notifications/list:
// the list plus the badge and the heading count out of band, honouring ?unread=1, only the caller's rows.
func TestNotificationListFragment(t *testing.T) {
	w := seed(t)
	ops.Emit(w.alice, ops.Event{Type: "cardMembershipCreate", BoardID: "b1", CardID: "c1", Data: map[string]any{"userId": "carol"}})
	notify.Wait()
	mux := http.NewServeMux()
	web.Mount(mux, func(r *http.Request) *ops.Env { c := *w.carol; c.Ctx = r.Context(); c.Req = r; return &c })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	get := func(path string) string {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("HX-Request", "true")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("%s: %d %s", path, resp.StatusCode, b)
		}
		return string(b)
	}
	body := get("/ui/notifications/list?unread=1")
	for _, want := range []string{`id="notify-list"`, `class="notify-item unread"`, `hx-swap-oob="innerHTML:#notif-badge"`,
		`id="notify-head-count" class="notify-head-count" hx-swap-oob="true"><span class="notify-count">1</span>`} {
		if !strings.Contains(body, want) {
			t.Fatalf("fragment lacks %q:\n%s", want, body)
		}
	}
	if _, err := ops.Typed[notify.CountOut](w.carol, "notification.read_all", notify.NoInput{}); err != nil {
		t.Fatal(err)
	}
	if body = get("/ui/notifications/list?unread=1"); !strings.Contains(body, "No unread notifications.") || strings.Contains(body, `class="notify-count"`) {
		t.Fatalf("after read_all:\n%s", body)
	}
	page := get("/notifications")
	if !strings.Contains(page, `id="notify-fresh"`) || !strings.Contains(page, `hidden hx-get="/ui/notifications/list"`) {
		t.Fatal("notifications page lacks the hidden refresh button")
	}
}
