package server_test

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSecretsNeverInEventsOrOutputs(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Leaky")
	ch, cancel := e.srv.Hub.Subscribe(func(string) bool { return true })
	defer cancel()
	wh := e.must(e.admin, "POST", "/webhooks", map[string]any{"name": "w", "url": "http://127.0.0.1:1/hook?k=URLTOKEN", "secret": "SEKRET123", "accessToken": "ACCTOK456"})
	if wh["hasSecret"] != true || wh["hasAccessToken"] != true || wh["secret"] != nil || wh["accessToken"] != nil {
		t.Fatalf("webhook.create output must be write-only: %v", wh)
	}
	e.must(e.admin, "PATCH", "/webhooks/"+id(wh), map[string]any{"name": "w2"})
	_, list := e.api(e.admin, "GET", "/webhooks", nil)
	lb, _ := json.Marshal(list)
	if strings.Contains(string(lb), "SEKRET123") || strings.Contains(string(lb), "ACCTOK456") || !strings.Contains(string(lb), `"hasSecret":true`) {
		t.Fatalf("webhook.list leaks or lacks flag: %s", lb)
	}
	e.must(e.admin, "POST", "/notification-services", map[string]any{"boardId": bd.id, "url": "generic://example.com/hook?token=SVCTOKEN789", "format": "text"})
	e.must(e.admin, "DELETE", "/webhooks/"+id(wh), nil)
	var all strings.Builder
	for done := false; !done; {
		select {
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			all.Write(b)
		default:
			done = true
		}
	}
	for _, bad := range []string{"SEKRET123", "ACCTOK456", "URLTOKEN", "SVCTOKEN789"} {
		if strings.Contains(all.String(), bad) {
			t.Fatalf("realtime events leak %q: %s", bad, all.String())
		}
	}
	if !strings.Contains(all.String(), "webhook.created") {
		t.Fatalf("expected webhook events to be published: %s", all.String())
	}
	// the Manage-only REST list still shows the service URL to the board manager
	_, sl := e.api(e.admin, "GET", "/notification-services?boardId="+bd.id, nil)
	sb, _ := json.Marshal(sl)
	if !strings.Contains(string(sb), "SVCTOKEN789") {
		t.Fatalf("managers must still see the service URL: %s", sb)
	}
}
