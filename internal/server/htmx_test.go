package server_test

import (
	"strings"
	"testing"
)

func TestHTMXVendoredAndHistoryDisabled(t *testing.T) {
	e := newEnv(t)
	_, js := e.rawReq("GET", "/static/htmx.min.js", nil, nil)
	if !strings.Contains(js, `version:"2.0.11"`) {
		t.Fatalf("htmx must be vendored at 2.0.11")
	}
	_, glue := e.rawReq("GET", "/static/weave.js", nil, nil)
	if !strings.Contains(glue, "htmx.config.historyCacheSize = 0") {
		t.Fatal("history cache must be disabled")
	}
	if strings.Contains(glue, "allowEval") || strings.Contains(glue, "allowScriptTags") {
		t.Fatal("allowEval/allowScriptTags must stay at their defaults")
	}
	auth := map[string]string{"Authorization": "Bearer " + e.admin}
	for _, p := range []string{"/admin/users", "/admin/config", "/admin/webhooks", "/settings"} {
		code, body := e.rawReq("GET", p, auth, nil)
		if code != 200 || !strings.Contains(body, `<main id="main" hx-history="false">`) {
			t.Errorf("%s: %d, hx-history=false missing", p, code)
		}
	}
}
