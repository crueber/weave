package notify_test

import (
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
	l, _ := ops.Typed[notify.WebhookListOut](w.admin, "webhook.list", notify.NoInput{})
	if len(l.Items) != 1 || len(l.Items[0].Events) != 2 || l.Items[0].ExcludedEvents[0] != "cardDelete" {
		t.Fatalf("webhooks = %+v", l.Items)
	}
	// clearing all checkboxes clears the filter
	post("/ui/admin/webhooks/"+l.Items[0].ID, url.Values{})
	l, _ = ops.Typed[notify.WebhookListOut](w.admin, "webhook.list", notify.NoInput{})
	if len(l.Items[0].Events) != 0 {
		t.Fatalf("filter not cleared: %+v", l.Items[0])
	}
	// config form: unchecked checkbox sends hidden "false" only; checked sends "true" first
	post("/ui/admin/config", url.Values{"terms.enabled": {"false"}, "branding.appName": {"Form App"}})
	if notify.ConfigBool(w.admin, "terms.enabled") || notify.Config(w.admin, "branding.appName") != "Form App" {
		t.Fatal("config form not applied")
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
