package cli

import (
	"net/http"
	"strings"
	"testing"
)

func TestAgentPromptShortcut(t *testing.T) {
	f := newFake(t)
	var last string
	f.handleFunc("GET /api/v1/agent/prompt", func(w http.ResponseWriter, r *http.Request) {
		last = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"item":{"scope":"board","title":"t","text":"PROMPT TEXT $WEAVE_TOKEN\n","vars":{}}}`))
	})
	f.json("GET /api/v1/projects", map[string]any{"items": []any{map[string]any{"id": "p1", "name": "Product"}}})
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"agent", "prompt"}, []string{"scope=general"}},
		{[]string{"agent", "prompt", "--general", "--compact", "--no-install"}, []string{"scope=general", "compact=true", "install=false"}},
		{[]string{"agent", "prompt", "--board", "Product"}, []string{"scope=board", "id=b1"}},
		{[]string{"agent", "prompt", "--board", "Product", "--list", "Ready"}, []string{"scope=list", "id=l1"}},
		{[]string{"agent", "prompt", "--runner", "--board", "Product"}, []string{"scope=runner", "id=b1"}},
		{[]string{"agent", "prompt", "--runner"}, []string{"scope=runner"}},
		{[]string{"agent", "prompt", "--project", "Product"}, []string{"scope=project", "id=p1"}},
	}
	for _, tc := range cases {
		code, out, errs := f.run(tc.args...)
		if code != 0 {
			t.Fatalf("%v: %d %s", tc.args, code, errs)
		}
		if out != "PROMPT TEXT $WEAVE_TOKEN\n" {
			t.Fatalf("%v prints raw text, got %q", tc.args, out)
		}
		for _, w := range tc.want {
			if !strings.Contains(last, w) {
				t.Errorf("%v: query %q lacks %q", tc.args, last, w)
			}
		}
	}
	if code, _, _ := f.run("agent", "prompt", "--work-order", "--board", "Product"); code != ExitUsage {
		t.Errorf("work order without list/card: %d", code)
	}
	if code, _, _ := f.run("agent", "prompt", "--list", "Ready"); code != ExitUsage {
		t.Errorf("--list without --board: %d", code)
	}
}
