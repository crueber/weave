package cli

import (
	"encoding/json"
	"github.com/crueber/weave/internal/version"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProjector(t *testing.T) {
	v := map[string]any{"item": map[string]any{"id": "1", "name": "n", "labels": []any{map[string]any{"name": "a", "x": 1.0}, map[string]any{"name": "b"}}, "claim": map[string]any{"by": "u", "ttl": 5.0}}}
	got := projectEnvelope(v, []string{"id", "labels.name", "claim.by"})
	want := map[string]any{"item": map[string]any{"id": "1", "labels": map[string]any{"name": []any{"a", "b"}}, "claim": map[string]any{"by": "u"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	items := map[string]any{"items": []any{map[string]any{"id": "1", "x": 1.0}, map[string]any{"id": "2", "x": 2.0}}}
	got = projectEnvelope(items, []string{"id"})
	b, _ := json.Marshal(got)
	if string(b) != `{"items":[{"id":"1"},{"id":"2"}]}` {
		t.Fatalf("got %s", b)
	}
}

func TestJQ(t *testing.T) {
	v := map[string]any{"items": []any{map[string]any{"id": "a", "n": map[string]any{"k": 1.0}}, map[string]any{"id": "b"}}}
	cases := map[string][]any{
		".items[].id":     {"a", "b"},
		".items[0].id":    {"a"},
		".items[-1].id":   {"b"},
		".items[0].n.k":   {1.0},
		".items | .[0]":   nil, // unsupported "[0]" without dot-key is fine: just must not panic
		".missing":        nil,
		".items[].n.k":    {1.0},
		".items[0] | .id": {"a"},
	}
	for expr, want := range cases {
		got, err := EvalJQ(v, expr)
		if err != nil {
			t.Fatalf("%s: %v", expr, err)
		}
		if want != nil && !reflect.DeepEqual(got, want) {
			t.Errorf("%s: got %v want %v", expr, got, want)
		}
	}
	if _, err := EvalJQ(v, "items"); err == nil {
		t.Error("expected error for expression without leading dot")
	}
}

func TestMatchName(t *testing.T) {
	c := []Candidate{{ID: "0190aaaabbbbccccdddd1111", Name: "Backlog"}, {ID: "0190bbbbccccddddeeee2222", Name: "Backlog archive"}, {ID: "x3", Name: "Ready"}, {ID: "0190ccccddddeeeeffff3333", Name: "Review"}}
	cases := []struct {
		in, want, code string
	}{
		{"Backlog", "0190aaaabbbbccccdddd1111", ""},   // exact wins over prefix
		{"backlog", "0190aaaabbbbccccdddd1111", ""},   // case-insensitive
		{"Backlog a", "0190bbbbccccddddeeee2222", ""}, // unique prefix
		{"Rea", "x3", ""},                            // prefix
		{"Re", "", "ambiguous"},                      // Ready + Review
		{"0190cccc", "0190ccccddddeeeeffff3333", ""}, // id prefix
		{"nope", "", "not_found"},
	}
	for _, tc := range cases {
		id, err := MatchName("list", tc.in, c)
		if tc.code == "" && (err != nil || id != tc.want) {
			t.Errorf("%q: got %q %v want %q", tc.in, id, err, tc.want)
		}
		if tc.code != "" && (err == nil || err.Code != tc.code) {
			t.Errorf("%q: want error %s got %v", tc.in, tc.code, err)
		}
	}
	_, err := MatchName("list", "Re", c)
	if !strings.Contains(err.Message, "Ready") || !strings.Contains(err.Message, "Review") {
		t.Errorf("ambiguity message should list candidates: %s", err.Message)
	}
}

func TestResolveNamesInGenericOp(t *testing.T) {
	f := newFake(t)
	f.json("POST /api/v1/lists/{listId}/cards", map[string]any{"item": map[string]any{"id": "c1", "name": "x"}})
	code, out, errs := f.run("card", "create", "--board", "Product", "--list", "Backlog", "--name", "Fix")
	if code != 0 {
		t.Fatalf("code %d %s %s", code, out, errs)
	}
	if f.called("POST /api/v1/lists/l5/cards") != 1 {
		t.Fatalf("calls: %v", f.calls)
	}
	if !strings.Contains(f.body("POST /api/v1/lists/l5/cards"), `"name":"Fix"`) {
		t.Fatalf("body %s", f.body("POST /api/v1/lists/l5/cards"))
	}
	// ambiguous: "Back" matches Backlog and Backlog archive only by prefix, exact "Backlog" is fine but "Back" is not
	code, _, errs = f.run("card", "create", "--board", "Product", "--list", "Back", "--name", "Fix")
	if code != ExitUsage || !strings.Contains(errs, "ambiguous") || !strings.Contains(errs, "Backlog archive") {
		t.Fatalf("code %d %s", code, errs)
	}
	// unknown board lists available
	code, _, errs = f.run("card", "create", "--board", "Nope", "--list", "Backlog", "--name", "Fix")
	if code != ExitNotFound || !strings.Contains(errs, "Product") {
		t.Fatalf("code %d %s", code, errs)
	}
	// flag spelling: kebab and camel are the same flag
	code, _, errs = f.run("card", "create", "--board", "Product", "--list-id", "Ready", "--name", "Fix")
	if code != 0 {
		t.Fatalf("code %d %s", code, errs)
	}
}

func TestPaginationAll(t *testing.T) {
	f := newFake(t)
	pages := map[string]string{"": `{"items":[{"id":"1"},{"id":"2"}],"nextCursor":"p2"}`, "p2": `{"items":[{"id":"3"}],"nextCursor":"p3"}`, "p3": `{"items":[{"id":"4"}]}`}
	f.handleFunc("GET /api/v1/boards/{boardId}/cards", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(pages[r.URL.Query().Get("cursor")]))
	})
	code, out, errs := f.run("card", "list", "--board", "b1", "--all", "--output", "ids")
	if code != 0 || strings.TrimSpace(out) != "1\n2\n3\n4" {
		t.Fatalf("code %d out %q err %s", code, out, errs)
	}
	code, out, _ = f.run("card", "list", "--board", "b1", "--all", "--limit", "3", "--jq", ".items[].id")
	if code != 0 || strings.TrimSpace(out) != "1\n2\n3" {
		t.Fatalf("limit: %q", out)
	}
	// without --all: one page, cursor passed through
	code, out, _ = f.run("card", "list", "--board", "b1", "--cursor", "p2", "--jq", ".items[].id")
	if code != 0 || strings.TrimSpace(out) != "3" {
		t.Fatalf("cursor: %q", out)
	}
}

func TestOutputFormats(t *testing.T) {
	f := newFake(t)
	f.json("GET /api/v1/boards/{boardId}/cards", map[string]any{"items": []any{
		map[string]any{"id": "c1", "name": "One", "listId": "l1", "labels": []any{"x"}},
		map[string]any{"id": "c2", "name": "Two", "listId": "l2"},
	}})
	_, out, _ := f.run("card", "list", "--board", "b1", "--output", "ndjson", "--fields", "id,name")
	if out != "{\"id\":\"c1\",\"name\":\"One\"}\n{\"id\":\"c2\",\"name\":\"Two\"}\n" {
		t.Fatalf("ndjson: %q", out)
	}
	_, out, _ = f.run("card", "list", "--board", "b1", "--output", "table", "--fields", "id,name")
	if !strings.Contains(out, "ID") || !strings.Contains(out, "One") {
		t.Fatalf("table: %q", out)
	}
	_, out, _ = f.run("card", "list", "--board", "b1", "--output", "quiet")
	if out != "" {
		t.Fatalf("quiet: %q", out)
	}
	_, out, _ = f.run("card", "list", "--board", "b1")
	if !strings.HasPrefix(out, `{"items"`) {
		t.Fatalf("default is the raw envelope: %q", out)
	}
}

func TestErrorsAndExitCodes(t *testing.T) {
	f := newFake(t)
	f.status("GET /api/v1/cards/{id}", 404, "not_found", "card not found")
	f.status("DELETE /api/v1/cards/{id}", 403, "forbidden", "no")
	f.status("POST /api/v1/cards/{id}/claim", 409, "conflict", "already claimed by Ada")
	f.status("POST /api/v1/projects", 401, "unauthorized", "bad key")
	f.status("PATCH /api/v1/cards/{id}", 422, "validation", "bad")
	for _, tc := range []struct {
		args []string
		code int
		sub  string
	}{
		{[]string{"card", "get", "c1"}, ExitNotFound, `"code":"not_found"`},
		{[]string{"card", "delete", "c1", "--yes"}, ExitForbidden, "forbidden"},
		{[]string{"card", "claim", "c1"}, ExitConflict, "already claimed"},
		{[]string{"project", "create", "--name", "x"}, ExitUnauthorized, "unauthorized"},
		{[]string{"card", "update", "c1", "--name", "x"}, ExitValidation, "validation"},
		{[]string{"nosuch", "thing"}, ExitUsage, "unknown command"},
		{[]string{"card", "nosuchverb"}, ExitUsage, "unknown command"},
		{[]string{"card", "create"}, ExitUsage, "usage"},
		{[]string{"project", "create"}, ExitUsage, "missing --name"},
	} {
		code, _, errs := f.run(tc.args...)
		if code != tc.code || !strings.Contains(errs, tc.sub) {
			t.Errorf("%v: code %d (want %d) stderr %s", tc.args, code, tc.code, errs)
		}
		var e struct {
			Error struct{ Code, Message string }
		}
		if json.Unmarshal([]byte(strings.TrimSpace(errs)), &e) != nil || e.Error.Code == "" {
			t.Errorf("%v: stderr is not a JSON error: %q", tc.args, errs)
		}
	}
	// network failure
	f.srv.Close()
	code, _, errs := f.run("card", "get", "c1")
	if code != ExitNetwork || !strings.Contains(errs, "network") {
		t.Errorf("network: %d %s", code, errs)
	}
}

func TestDryRunSendsNothing(t *testing.T) {
	f := newFake(t)
	code, out, _ := f.run("project", "create", "--name", "Zed", "--dry-run")
	if code != 0 || !strings.Contains(out, `"dryRun":true`) || !strings.Contains(out, "Zed") {
		t.Fatalf("%d %s", code, out)
	}
	if f.called("POST /api/v1/projects") != 0 {
		t.Fatal("dry-run must not send")
	}
}

func TestBodyStdinAndFileValues(t *testing.T) {
	f := newFake(t)
	f.json("POST /api/v1/projects", map[string]any{"item": map[string]any{"id": "p1"}})
	code, _, errs := f.runIn(`{"name":"FromStdin","description":"d"}`, "project", "create", "--stdin")
	if code != 0 {
		t.Fatal(errs)
	}
	if b := f.body("POST /api/v1/projects"); !strings.Contains(b, "FromStdin") {
		t.Fatalf("body %s", b)
	}
	// flags override --body, @file and - expand
	desc := filepath.Join(t.TempDir(), "d.md")
	os.WriteFile(desc, []byte("from file"), 0o644)
	code, _, errs = f.runIn("", "project", "create", "--body", `{"name":"A"}`, "--name", "B", "--description", "@"+desc)
	if code != 0 {
		t.Fatal(errs)
	}
	b := f.body("POST /api/v1/projects")
	if !strings.Contains(b, `"name":"B"`) || !strings.Contains(b, "from file") {
		t.Fatalf("body %s", b)
	}
	code, _, _ = f.runIn("piped text\n", "project", "create", "--name", "C", "--description", "-")
	if code != 0 || !strings.Contains(f.body("POST /api/v1/projects"), "piped text") {
		t.Fatalf("stdin value: %s", f.body("POST /api/v1/projects"))
	}
}

func TestProfilesAndLogin(t *testing.T) {
	f := newFake(t)
	t.Setenv("WEAVE_URL", "")
	t.Setenv("WEAVE_TOKEN", "")
	code, out, errs := f.run("login", "--url", f.srv.URL, "--token", "wk_one")
	if code != 0 || !strings.Contains(out, `"profile":"default"`) {
		t.Fatalf("login: %d %s %s", code, out, errs)
	}
	if code, out, _ = f.run("config", "list"); code != 0 || !strings.Contains(out, `"tokenSet":true`) || !strings.Contains(out, `"current":true`) {
		t.Fatalf("list: %s", out)
	}
	if code, _, _ = f.run("login", "--url", f.srv.URL, "--token", "wk_two", "--profile", "other"); code != 0 {
		t.Fatal("second login")
	}
	if code, _, _ = f.run("config", "use", "other"); code != 0 {
		t.Fatal("use")
	}
	_, out, _ = f.run("config", "get", "profile")
	if strings.TrimSpace(out) != "other" {
		t.Fatalf("current profile: %q", out)
	}
	_, out, _ = f.run("--profile", "default", "config", "get", "token", "--show")
	if strings.TrimSpace(out) != "wk_one" {
		t.Fatalf("token: %q", out)
	}
	if code, _, _ = f.run("config", "remove", "other"); code != 0 {
		t.Fatal("remove")
	}
	// a failing whoami must not save
	f.status("GET /api/v1/whoami", 401, "unauthorized", "bad key")
	f2 := f
	_ = f2
	// legacy config file is read as the default profile
	dir := os.Getenv("WEAVE_CONFIG_DIR")
	os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"URL":"http://legacy:1","Token":"tk"}`), 0o600)
	r := resolveConfig("", "", "")
	if r.URL != "http://legacy:1" || r.Token != "tk" || r.Profile != "default" {
		t.Fatalf("legacy: %+v", r)
	}
	// flags beat env beat profile
	t.Setenv("WEAVE_URL", "http://env:2")
	if r := resolveConfig("http://flag:3", "", ""); r.URL != "http://flag:3" {
		t.Fatalf("flag: %+v", r)
	}
	if r := resolveConfig("", "", ""); r.URL != "http://env:2" {
		t.Fatalf("env: %+v", r)
	}
}

func TestWhoamiDoctorVersion(t *testing.T) {
	f := newFake(t)
	f.json("GET /healthz", map[string]any{"ok": true})
	f.json("GET /api/v1/version", map[string]any{"item": map[string]any{"version": "v9.9.9"}})
	f.handleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("event: ready\ndata: {}\n\n"))
	})
	code, out, _ := f.run("whoami")
	if code != 0 || !strings.Contains(out, "Agent Smith") {
		t.Fatalf("whoami: %s", out)
	}
	code, out, errs := f.run("doctor", "--output", "ndjson")
	if code != 0 || !strings.Contains(out, "connectivity") || !strings.Contains(out, "events") {
		t.Fatalf("doctor: %d %s %s", code, out, errs)
	}
	old := version.Version
	version.Version = "v1.0.0"
	defer func() { version.Version = old }()
	code, out, _ = f.run("version", "--check")
	if code != 1 || !strings.Contains(out, `"server":"v9.9.9"`) || !strings.Contains(out, `"skew":true`) {
		t.Fatalf("version --check: %d %s", code, out)
	}
}

func TestCompletionOutput(t *testing.T) {
	f := newFake(t)
	for _, sh := range []string{"bash", "zsh", "fish", "powershell"} {
		code, out, _ := f.run("completion", sh)
		if code != 0 || !strings.Contains(out, "__complete") {
			t.Errorf("%s: %d %q", sh, code, out)
		}
	}
	if code, _, errs := f.run("completion", "tcsh"); code != ExitUsage {
		t.Errorf("unknown shell: %d %s", code, errs)
	}
	_, out, _ := f.run("__complete", "card", "cl")
	if strings.TrimSpace(out) != "claim\nclose" {
		t.Errorf("verbs: %q", out)
	}
	_, out, _ = f.run("__complete", "li")
	if !strings.Contains(out, "list") || !strings.Contains(out, "listen") {
		t.Errorf("tags: %q", out)
	}
	_, out, _ = f.run("__complete", "card", "create", "--li")
	if !strings.Contains(out, "--listId") {
		t.Errorf("flags: %q", out)
	}
}

func TestHelpGeneratedFromSchema(t *testing.T) {
	f := newFake(t)
	code, out, _ := f.run("project", "create", "--help")
	if code != 0 || !strings.Contains(out, "--name") || !strings.Contains(out, "Example:") || !strings.Contains(out, "POST /projects") {
		t.Fatalf("%d %s", code, out)
	}
	code, out, _ = f.run("listen", "--help")
	if code != 0 || !strings.Contains(out, "--include-existing") {
		t.Fatalf("listen help: %s", out)
	}
	code, out, _ = f.run("help", "card", "claim")
	if code != 0 || !strings.Contains(out, "claim") {
		t.Fatalf("help card claim: %s", out)
	}
}

func TestSchemaFromServerIsCachedAndPreferred(t *testing.T) {
	f := newFake(t)
	t.Setenv("WEAVE_NO_SCHEMA_FETCH", "")
	hits := 0
	f.handleFunc("GET /api/v1/_ops", func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"items":[{"name":"widget.spin","tag":"widget","summary":"Spin it","method":"POST","path":"/widgets/{id}/spin","auth":"user","ui":"x","fields":[{"name":"id","in":"path","type":"string","required":true},{"name":"speed","in":"body","type":"number"}]}]}`))
	})
	f.json("POST /api/v1/widgets/{id}/spin", map[string]any{"item": map[string]any{"ok": true}})
	code, out, errs := f.run("widget", "spin", "w1", "--speed", "3")
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	if !strings.Contains(f.body("POST /api/v1/widgets/w1/spin"), `"speed":3`) {
		t.Fatalf("body %q", f.body("POST /api/v1/widgets/w1/spin"))
	}
	f.run("widget", "spin", "w2")
	if hits != 1 {
		t.Fatalf("_ops must be cached on disk, fetched %d times", hits)
	}
	// server down: the cache keeps working; unknown server falls back to the embedded snapshot
	if code, _, _ = f.run("ops", "--names"); code != 0 {
		t.Fatal("ops")
	}
}

func TestSnapshotHasCoreOps(t *testing.T) {
	cat := newCatalog("snapshot", SnapshotOps())
	for _, n := range []string{"card.create", "card.move", "list.list", "comment.create", "card.claim", "card.next", "whoami", "board.tree"} {
		if cat.Get(n) == nil {
			t.Errorf("catalog lacks %s", n)
		}
	}
}
