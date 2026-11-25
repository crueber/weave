package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// mcpEnv is an MCP client session connected in memory to newMCPServer, which talks HTTP to the
// fake Weave server. Guard roots are temp dirs: proj (the cwd) and tmp.
type mcpEnv struct {
	t      *testing.T
	f      *fake
	cs     *mcp.ClientSession
	ms     *mcpServer
	init   *mcp.InitializeResult
	stderr *lockedBuf
	proj   string
	tmp    string
	home   string
}

func newMCPEnv(t *testing.T, f *fake, cfg mcpConfig) *mcpEnv {
	t.Helper()
	root := t.TempDir()
	e := &mcpEnv{t: t, f: f, stderr: &lockedBuf{}, proj: filepath.Join(root, "proj"), tmp: filepath.Join(root, "tmp"), home: filepath.Join(root, "home")}
	for _, d := range []string{e.proj, e.tmp, e.home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if cfg.Guard == nil {
		cfg.Guard = &guardEnv{GOOS: goos(), Cwd: e.proj, Home: e.home, Temp: e.tmp, ConfigDir: os.Getenv("WEAVE_CONFIG_DIR"), CacheDir: os.Getenv("WEAVE_CACHE_DIR"), Getenv: os.Getenv}
	}
	ctx := context.Background()
	a := &App{Stdin: strings.NewReader(""), Stdout: io.Discard, Stderr: e.stderr, Timeout: 30 * time.Second, ctx: ctx}
	a.Cfg = resolveConfig("", "", "")
	srv, ms := newMCPServer(ctx, a, cfg)
	e.ms = ms
	ct, st := mcp.NewInMemoryTransports()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.cs, e.init = cs, cs.InitializeResult()
	t.Cleanup(func() { cs.Close(); ss.Close() })
	return e
}

func (e *mcpEnv) call(name string, args map[string]any) *mcp.CallToolResult {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	r, err := e.cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		e.t.Fatalf("%s: protocol error: %v", name, err)
	}
	return r
}

func resultText(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func resultJSON(t *testing.T, r *mcp.CallToolResult) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(resultText(r)), &m); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, resultText(r))
	}
	return m
}

func errCode(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if !r.IsError {
		t.Fatalf("expected an error result, got %s", resultText(r))
	}
	m := resultJSON(t, r)
	e, _ := m["error"].(map[string]any)
	return str(e, "code")
}

func (e *mcpEnv) toolNames() map[string]*mcp.Tool {
	e.t.Helper()
	out := map[string]*mcp.Tool{}
	for tool, err := range e.cs.Tools(context.Background(), nil) {
		if err != nil {
			e.t.Fatal(err)
		}
		out[tool.Name] = tool
	}
	return out
}

func TestMCPCuratedNamesAreRegisteredOps(t *testing.T) {
	reg := map[string]bool{}
	for _, o := range SnapshotOps() { // the generated registry snapshot, not the overlay
		reg[o.Name] = true
	}
	if n := len(mcpCurated); n < 25 || n > 30 {
		t.Fatalf("curated tool count %d, want 25-30", n)
	}
	seen := map[string]bool{}
	for _, n := range mcpCurated {
		if !reg[n] {
			t.Errorf("curated tool %q is not a registered op (make skill?)", n)
		}
		if seen[n] {
			t.Errorf("duplicate curated op %q", n)
		}
		seen[n] = true
	}
}

var toolNameRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

func TestMCPToolNamesValidAndUnique(t *testing.T) {
	seen := map[string]string{}
	for _, o := range newCatalog("t", SnapshotOps()).All() {
		n := mcpToolName(o.Name)
		if !toolNameRe.MatchString(n) {
			t.Errorf("invalid tool name %q", n)
		}
		if prev, ok := seen[n]; ok {
			t.Errorf("tool name %q collides: %s and %s", n, prev, o.Name)
		}
		seen[n] = o.Name
		s := mcpInputSchema(o)
		if s["type"] != "object" {
			t.Errorf("%s: schema type %v", o.Name, s["type"])
		}
	}
	for _, n := range []string{mcpOpsTool, mcpCallTool} {
		if !toolNameRe.MatchString(n) || seen[n] != "" {
			t.Errorf("meta tool %q invalid or colliding", n)
		}
	}
}

// TestMCPAnnotationsGolden pins the mechanical annotations of every op (go test -run Golden -update).
func TestMCPAnnotationsGolden(t *testing.T) {
	var b strings.Builder
	for _, o := range newCatalog("t", SnapshotOps()).All() {
		a := mcpAnnotate(o)
		flags := []string{}
		for _, f := range []struct {
			on   bool
			name string
		}{{a.ReadOnly, "readOnly"}, {a.Destructive, "destructive"}, {a.Idempotent, "idempotent"}, {a.OpenWorld, "openWorld"}} {
			if f.on {
				flags = append(flags, f.name)
			}
		}
		b.WriteString(mcpToolName(o.Name) + " " + o.Method + " " + strings.Join(flags, ",") + "\n")
	}
	golden := filepath.Join("testdata", "mcp_annotations.golden")
	if *updateGolden {
		_ = os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/cli -run Golden -update)", err)
	}
	if string(want) != b.String() {
		t.Fatalf("annotations changed; review and run go test ./internal/cli -run Golden -update\n%s", b.String())
	}
}

func TestMCPCompoundVerbSafety(t *testing.T) {
	for _, tc := range []struct {
		name, method string
		destructive  bool
		ann          mcpAnnotations
	}{
		{"card.delete.bulk", "POST", true, mcpAnnotations{Destructive: true}},
		{"card.restore.bulk", "POST", false, mcpAnnotations{}},
		{"card.archive.list", "GET", false, mcpAnnotations{ReadOnly: true, Idempotent: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := &Op{Name: tc.name, Method: tc.method}
			if got := mcpDestructive(o); got != tc.destructive {
				t.Errorf("mcpDestructive(%s) = %v, want %v", tc.name, got, tc.destructive)
			}
			if got := mcpAnnotate(o); got != tc.ann {
				t.Errorf("mcpAnnotate(%s) = %+v, want %+v", tc.name, got, tc.ann)
			}
			if got := mcpOpSummary(o, nil)["destructive"] == true; got != tc.destructive {
				t.Errorf("mcpOpSummary(%s) destructive = %v, want %v", tc.name, got, tc.destructive)
			}
			if got := strings.Contains(mcpDescription(o), "Destructive:"); got != tc.destructive {
				t.Errorf("mcpDescription(%s) destructive warning = %v, want %v", tc.name, got, tc.destructive)
			}
		})
	}
}

func TestMCPToolsList(t *testing.T) {
	f := newFake(t)
	e := newMCPEnv(t, f, mcpConfig{})
	tools := e.toolNames()
	if len(tools) != len(mcpCurated)+2 {
		t.Fatalf("got %d tools, want %d", len(tools), len(mcpCurated)+2)
	}
	for _, n := range mcpCurated {
		tool := tools[mcpToolName(n)]
		if tool == nil {
			t.Fatalf("missing tool for %s", n)
		}
		if tool.Annotations == nil || tool.Annotations.DestructiveHint == nil || tool.Annotations.OpenWorldHint == nil {
			t.Fatalf("%s: destructive/openWorld hints must be explicit", tool.Name)
		}
		if !strings.Contains(tool.Description, "op "+n) {
			t.Fatalf("%s: description %q", tool.Name, tool.Description)
		}
	}
	if tools["weave_card_get"].Annotations.ReadOnlyHint != true || *tools["weave_card_get"].Annotations.DestructiveHint {
		t.Fatal("card.get must be read-only and not destructive")
	}
	if !*tools["weave_card_label_remove"].Annotations.DestructiveHint {
		t.Fatal("DELETE must be destructive")
	}
	if !*tools[mcpCallTool].Annotations.DestructiveHint || tools[mcpOpsTool].Annotations.ReadOnlyHint != true {
		t.Fatal("meta tool annotations")
	}
	up := tools["weave_attachment_create"]
	if up.Annotations.ReadOnlyHint || *up.Annotations.DestructiveHint || up.Annotations.IdempotentHint || !*up.Annotations.OpenWorldHint {
		t.Fatalf("upload annotations %+v", up.Annotations)
	}
	props := up.InputSchema.(map[string]any)["properties"].(map[string]any)
	for _, k := range []string{"path", "contentBase64", "filename", "cardId", "url", "name"} {
		if props[k] == nil {
			t.Fatalf("attachment.create schema lacks %s: %v", k, props)
		}
	}
	if props["file"] != nil {
		t.Fatal("the raw file field must be replaced by path/contentBase64")
	}
	dl := tools["weave_attachment_download"].InputSchema.(map[string]any)["properties"].(map[string]any)
	if dl["saveTo"] == nil || dl["overwrite"] == nil {
		t.Fatal("download tool needs saveTo/overwrite")
	}
	req := tools["weave_card_move"].InputSchema.(map[string]any)["required"]
	if b, _ := json.Marshal(req); string(b) != `["id","listId"]` {
		t.Fatalf("card.move required %s", b)
	}
}

func TestMCPCallReadAndWrite(t *testing.T) {
	f := newFake(t)
	f.trackCards(nil)
	e := newMCPEnv(t, f, mcpConfig{})
	r := e.call("weave_card_get", map[string]any{"id": "c1"})
	if r.IsError {
		t.Fatal(resultText(r))
	}
	sc, _ := r.StructuredContent.(map[string]any)
	if str(itemOf(sc), "id") != "c1" {
		t.Fatalf("structured %v", r.StructuredContent)
	}
	r = e.call("weave_card_move", map[string]any{"id": "c1", "listId": "l3"})
	if r.IsError {
		t.Fatal(resultText(r))
	}
	if !strings.Contains(f.body("POST /api/v1/cards/c1/move"), `"listId":"l3"`) {
		t.Fatalf("move body %q", f.body("POST /api/v1/cards/c1/move"))
	}
	// numbers given as strings are coerced; list ops get a default limit
	f.json("GET /api/v1/boards/{boardId}/cards", map[string]any{"items": []any{}})
	e.call("weave_card_list", map[string]any{"boardId": "b1"})
	if f.called("limit=50") == 0 {
		t.Fatalf("default limit not sent: %v", f.calls)
	}
	// "@file" is never expanded: strings go out literally
	f.json("POST /api/v1/cards/{cardId}/comments", map[string]any{"item": map[string]any{"id": "m1"}})
	e.call("weave_comment_create", map[string]any{"cardId": "c1", "text": "@/etc/passwd"})
	if b := f.body("POST /api/v1/cards/c1/comments"); !strings.Contains(b, `"text":"@/etc/passwd"`) {
		t.Fatalf("comment body %q", b)
	}
	// path traversal in an id is rejected before any request
	r = e.call("weave_card_get", map[string]any{"id": ".."})
	if errCode(t, r) != "validation" {
		t.Fatal("dot-dot id must be rejected")
	}
	r = e.call("weave_card_move", map[string]any{"id": "c1"})
	if errCode(t, r) != "validation" || !strings.Contains(resultText(r), "listId") {
		t.Fatalf("missing required: %s", resultText(r))
	}
}

func TestMCPWeaveCallParityAndConfirm(t *testing.T) {
	f := newFake(t)
	e := newMCPEnv(t, f, mcpConfig{})
	// every op is reachable: as a named tool or through weave_call (weave_ops knows them all)
	tools := e.toolNames()
	for _, o := range e.ms.catalog().All() {
		if tools[mcpToolName(o.Name)] != nil {
			continue
		}
		if o.Auth == "admin" {
			continue // hidden from this non-admin key's weave_ops (checked below), weave_call still forwards
		}
		r := e.call(mcpOpsTool, map[string]any{"op": o.Name})
		if r.IsError {
			t.Fatalf("weave_ops cannot describe %s: %s", o.Name, resultText(r))
		}
	}
	f.json("PUT /api/v1/cards/{id}/due", map[string]any{"item": map[string]any{"id": "c1"}})
	r := e.call(mcpCallTool, map[string]any{"op": "card.due.set", "args": map[string]any{"id": "c1", "dueDate": "2026-12-01"}})
	if r.IsError || !strings.Contains(f.body("PUT /api/v1/cards/c1/due"), "2026-12-01") {
		t.Fatalf("weave_call: %s", resultText(r))
	}
	// destructive ops need confirm:true and the refusal names the dedicated tool when there is one
	f.json("DELETE /api/v1/cards/{id}", map[string]any{"item": map[string]any{"id": "c1"}})
	r = e.call(mcpCallTool, map[string]any{"op": "card.delete", "args": map[string]any{"id": "c1"}})
	if errCode(t, r) != "confirm_required" || f.called("DELETE /api/v1/cards/c1") != 0 {
		t.Fatalf("delete without confirm: %s", resultText(r))
	}
	r = e.call(mcpCallTool, map[string]any{"op": "card.label.remove", "args": map[string]any{"id": "c1", "labelId": "x"}})
	if !strings.Contains(resultText(r), "weave_card_label_remove") {
		t.Fatalf("refusal must name the dedicated tool: %s", resultText(r))
	}
	r = e.call(mcpCallTool, map[string]any{"op": "card.archive", "args": map[string]any{"id": "c1"}})
	if errCode(t, r) != "confirm_required" {
		t.Fatal("archive must need confirm")
	}
	r = e.call(mcpCallTool, map[string]any{"op": "card.delete", "args": map[string]any{"id": "c1"}, "confirm": true})
	if r.IsError || f.called("DELETE /api/v1/cards/c1") != 1 {
		t.Fatalf("confirmed delete: %s", resultText(r))
	}
	r = e.call(mcpCallTool, map[string]any{"op": "nope.nothing"})
	if errCode(t, r) != "not_found" {
		t.Fatal("unknown op")
	}
	// search
	r = e.call(mcpOpsTool, map[string]any{"query": "due date"})
	if !strings.Contains(resultText(r), "card.due.set") {
		t.Fatalf("search: %s", resultText(r))
	}
}

func TestMCPWeaveCallBulkArchiveSafety(t *testing.T) {
	f := newFake(t)
	f.json("POST /api/v1/boards/{boardId}/archive/delete", map[string]any{"item": map[string]any{"deleted": 1}})
	f.json("POST /api/v1/boards/{boardId}/archive/restore", map[string]any{"item": map[string]any{"restored": 1}})
	f.json("GET /api/v1/boards/{boardId}/archive", map[string]any{"items": []any{}, "total": 0})
	e := newMCPEnv(t, f, mcpConfig{})

	args := map[string]any{"boardId": "b1", "cardIds": []any{"c1"}}
	r := e.call(mcpCallTool, map[string]any{"op": "card.delete.bulk", "args": args})
	if errCode(t, r) != "confirm_required" || f.called("POST /api/v1/boards/b1/archive/delete") != 0 {
		t.Fatalf("bulk delete without confirm: %s", resultText(r))
	}
	r = e.call(mcpCallTool, map[string]any{"op": "card.delete.bulk", "args": args, "confirm": true})
	if r.IsError || f.called("POST /api/v1/boards/b1/archive/delete") != 1 {
		t.Fatalf("confirmed bulk delete: %s", resultText(r))
	}

	r = e.call(mcpCallTool, map[string]any{"op": "card.restore.bulk", "args": args})
	if r.IsError || f.called("POST /api/v1/boards/b1/archive/restore") != 1 {
		t.Fatalf("bulk restore must not need confirm: %s", resultText(r))
	}
	r = e.call(mcpCallTool, map[string]any{"op": "card.archive.list", "args": map[string]any{"boardId": "b1", "kind": "trash"}})
	if r.IsError || f.called("GET /api/v1/boards/b1/archive?kind=trash") != 1 {
		t.Fatalf("archive list must not need confirm: %s", resultText(r))
	}
}

func TestMCPAdminToolsHidden(t *testing.T) {
	cur := []string{"card.get", "group.create"}
	f := newFake(t) // whoami role: boardUser
	e := newMCPEnv(t, f, mcpConfig{Curated: cur})
	tools := e.toolNames()
	if tools["weave_group_create"] != nil || tools["weave_card_get"] == nil {
		t.Fatalf("non-admin sees admin tool: %v", tools)
	}
	r := e.call(mcpOpsTool, map[string]any{"tag": "group"})
	if strings.Contains(resultText(r), "group.create") {
		t.Fatalf("weave_ops lists admin ops to a non-admin: %s", resultText(r))
	}
	// whoami failing: show everything (the server is the real gate)
	f2 := newFake(t)
	f2.status("GET /api/v1/whoami", 500, "error", "down")
	f2.status("GET /api/v1/auth/whoami", 500, "error", "down")
	f2.status("GET /api/v1/auth/me", 500, "error", "down")
	if newMCPEnv(t, f2, mcpConfig{Curated: cur}).toolNames()["weave_group_create"] == nil {
		t.Fatal("admin tool must be shown when whoami fails")
	}
	f3 := newFake(t)
	f3.json("GET /api/v1/auth/whoami", map[string]any{"item": map[string]any{"user": map[string]any{"id": "u1"}, "role": "admin"}})
	if newMCPEnv(t, f3, mcpConfig{Curated: cur}).toolNames()["weave_group_create"] == nil {
		t.Fatal("admin must see admin tools")
	}
}

func TestMCPErrorMapping(t *testing.T) {
	f := newFake(t)
	e := newMCPEnv(t, f, mcpConfig{})
	for code, want := range map[int]string{404: "not_found", 403: "forbidden", 409: "conflict", 422: "validation"} {
		f.status("GET /api/v1/cards/{id}", code, want, "nope")
		r := e.call("weave_card_get", map[string]any{"id": "c1"})
		if got := errCode(t, r); got != want {
			t.Fatalf("HTTP %d -> %s, want %s", code, got, want)
		}
	}
}

func TestMCPTruncationAndDescriptions(t *testing.T) {
	long := strings.Repeat("x", 5000)
	f := newFake(t)
	f.json("GET /api/v1/cards/{id}", map[string]any{"item": map[string]any{"id": "c1", "description": long}})
	e := newMCPEnv(t, f, mcpConfig{})
	r := e.call("weave_card_get", map[string]any{"id": "c1"})
	m := resultJSON(t, r)
	d := str(itemOf(m), "description")
	if !strings.Contains(d, "cut at 2000 of 5000") || len(d) > 2200 {
		t.Fatalf("description not cut: %d", len(d))
	}
	r = e.call("weave_card_get", map[string]any{"id": "c1", "full": true})
	if str(itemOf(resultJSON(t, r)), "description") != long || f.called("full=true") == 0 {
		t.Fatal("full:true must return the whole description and pass full to the server")
	}
	t.Setenv("WEAVE_MCP_MAX_RESULT", "300")
	e2 := newMCPEnv(t, f, mcpConfig{})
	r = e2.call("weave_card_get", map[string]any{"id": "c1", "full": true})
	txt := resultText(r)
	if !strings.Contains(txt, "[truncated: showing") || r.StructuredContent != nil || len(txt) > 600 {
		t.Fatalf("truncation: structured=%v len=%d", r.StructuredContent != nil, len(txt))
	}
}

func TestMCPTokenNeverInOutput(t *testing.T) {
	const tok = "wk_SuperSecretToken_9f8e7d"
	f := newFake(t)
	t.Setenv("WEAVE_TOKEN", tok)
	// a misbehaving proxy that echoes the Authorization header into an error
	f.handleFunc("GET /api/v1/cards/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(502)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "bad_gateway", "message": "upstream saw " + r.Header.Get("Authorization")}})
	})
	f.json("GET /api/v1/agent/prompt", map[string]any{"item": map[string]any{"text": "connect with " + tok}})
	e := newMCPEnv(t, f, mcpConfig{})
	var all bytes.Buffer
	b, _ := json.Marshal(e.init)
	all.Write(b)
	for _, tool := range e.toolNames() {
		b, _ := json.Marshal(tool)
		all.Write(b)
	}
	b, _ = json.Marshal(e.call("weave_card_get", map[string]any{"id": "c1"}))
	all.Write(b)
	b, _ = json.Marshal(e.call("weave_auth_whoami", nil))
	all.Write(b)
	pr, err := e.cs.GetPrompt(context.Background(), &mcp.GetPromptParams{Name: "about_weave"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(pr)
	all.Write(b)
	all.WriteString(e.stderr.String())
	if strings.Contains(all.String(), tok) {
		t.Fatalf("token leaked: %s", all.String())
	}
	if !strings.Contains(all.String(), "[redacted]") {
		t.Fatal("expected the echoed token to be redacted")
	}
}

func TestMCPPromptsAndResources(t *testing.T) {
	f := newFake(t)
	f.handleFunc("GET /api/v1/agent/prompt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"item": map[string]any{"text": "PROMPT " + r.URL.RawQuery}})
	})
	f.json("GET /api/v1/boards/{id}/tree", map[string]any{"item": map[string]any{"id": "b1", "name": "Product",
		"lists": []any{
			map[string]any{"id": "l1", "name": "Ready", "description": "do it", "cards": []any{map[string]any{"id": "c1"}, map[string]any{"id": "c2", "claimedBy": "u1"}}},
			map[string]any{"id": "l2", "name": "Done", "cards": []any{}},
		}}})
	e := newMCPEnv(t, f, mcpConfig{})
	ctx := context.Background()
	var names []string
	for p, err := range e.cs.Prompts(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, p.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "about_weave,board,column,project,runner" {
		t.Fatalf("prompts %v", names)
	}
	pr, err := e.cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "column", Arguments: map[string]string{"board": "Product", "list": "Ready"}})
	if err != nil {
		t.Fatal(err)
	}
	txt := pr.Messages[0].Content.(*mcp.TextContent).Text
	if !strings.Contains(txt, "scope=list") || !strings.Contains(txt, "id=l1") {
		t.Fatalf("column prompt %q", txt)
	}
	if _, err := e.cs.GetPrompt(ctx, &mcp.GetPromptParams{Name: "board"}); err == nil {
		t.Fatal("board prompt without a board must fail")
	}
	rr, err := e.cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "weave://board/b1"})
	if err != nil {
		t.Fatal(err)
	}
	var view map[string]any
	_ = json.Unmarshal([]byte(rr.Contents[0].Text), &view)
	cols := view["columns"].([]any)
	c0 := cols[0].(map[string]any)
	if len(cols) != 2 || c0["cardCount"].(float64) != 2 || c0["claimedCount"].(float64) != 1 || c0["description"] != "do it" {
		t.Fatalf("board view %s", rr.Contents[0].Text)
	}
	rr, err = e.cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "weave://card/c7"})
	if err != nil || !strings.Contains(rr.Contents[0].Text, `"c7"`) {
		t.Fatalf("card resource: %v", err)
	}
	f.status("GET /api/v1/cards/{id}", 404, "not_found", "no")
	if _, err := e.cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "weave://card/c8"}); err == nil {
		t.Fatal("missing card must be an error")
	}
}

// ---- uploads / downloads end to end ----

type gotUpload struct {
	field, filename, content string
	form                     map[string]string
}

func (f *fake) captureUploads(pattern string) *[]gotUpload {
	var got []gotUpload
	f.handleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		u := gotUpload{form: map[string]string{}}
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(p)
			if p.FileName() != "" {
				u.field, u.filename, u.content = p.FormName(), p.FileName(), string(b)
			} else {
				u.form[p.FormName()] = string(b)
			}
		}
		f.mu.Lock()
		got = append(got, u)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"item": map[string]any{"id": "a1", "name": u.filename}})
	})
	return &got
}

func TestMCPUploadEndToEnd(t *testing.T) {
	f := newFake(t)
	got := f.captureUploads("POST /api/v1/cards/{cardId}/attachments")
	e := newMCPEnv(t, f, mcpConfig{})
	if err := os.WriteFile(filepath.Join(e.proj, "notes.txt"), []byte("hello weave"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := e.call("weave_attachment_create", map[string]any{"cardId": "c1", "path": "notes.txt", "name": "@/etc/passwd"})
	if r.IsError {
		t.Fatal(resultText(r))
	}
	if len(*got) != 1 {
		t.Fatal("no upload reached the server")
	}
	u := (*got)[0]
	if u.field != "file" || u.filename != "notes.txt" || u.content != "hello weave" || u.form["name"] != "@/etc/passwd" {
		t.Fatalf("multipart %+v", u)
	}
	up := resultJSON(t, r)["uploaded"].(map[string]any)
	if up["bytes"].(float64) != 11 || up["sha256"] != sha([]byte("hello weave")) || up["sniffedMime"] == nil {
		t.Fatalf("uploaded %v", up)
	}
	if strings.Contains(resultText(r), "hello weave") {
		t.Fatal("file bytes must never be echoed")
	}
	// base64
	r = e.call("weave_attachment_create", map[string]any{"cardId": "c1", "contentBase64": base64.StdEncoding.EncodeToString([]byte("b64 body")), "filename": "../x/b.txt"})
	if r.IsError || (*got)[1].content != "b64 body" || (*got)[1].filename != "b.txt" {
		t.Fatalf("base64 upload: %s %+v", resultText(r), (*got)[1])
	}
	// oversize base64 is refused before decoding
	t.Setenv("WEAVE_MCP_MAX_BASE64_MB", "0.001")
	e2 := newMCPEnv(t, f, mcpConfig{})
	r = e2.call("weave_attachment_create", map[string]any{"cardId": "c1", "contentBase64": base64.StdEncoding.EncodeToString(make([]byte, 4096)), "filename": "big"})
	if errCode(t, r) != "validation" || !strings.Contains(resultText(r), "limit") {
		t.Fatalf("oversize base64: %s", resultText(r))
	}
	// "-" is stdin = the protocol pipe
	r = e.call("weave_attachment_create", map[string]any{"cardId": "c1", "path": "-"})
	if errCode(t, r) != "validation" || !strings.Contains(resultText(r), "stdin") {
		t.Fatalf("dash: %s", resultText(r))
	}
	// a link attachment (no file) still works
	f.json("POST /api/v1/cards/{cardId}/attachments", map[string]any{"item": map[string]any{"id": "a2"}})
	r = e.call("weave_attachment_create", map[string]any{"cardId": "c1", "url": "https://example.com"})
	if r.IsError || !strings.Contains(f.body("POST /api/v1/cards/c1/attachments"), "example.com") {
		t.Fatalf("link attachment: %s", resultText(r))
	}
	// project.background.upload requires a file
	r = e.call(mcpCallTool, map[string]any{"op": "project.background.upload", "args": map[string]any{"id": "p1"}})
	if errCode(t, r) != "validation" {
		t.Fatal("required file")
	}
}

func TestMCPUploadGrowingFileFails(t *testing.T) {
	f := newFake(t)
	f.captureUploads("POST /api/v1/cards/{cardId}/attachments")
	t.Setenv("WEAVE_MCP_MAX_UPLOAD_MB", "0.0001") // ~104 bytes
	e := newMCPEnv(t, f, mcpConfig{})
	p := filepath.Join(e.proj, "grow.txt")
	_ = os.WriteFile(p, []byte("small"), 0o644)
	e.ms.guard.afterStat = func() {
		fh, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
		fh.Write(make([]byte, 4096))
		fh.Close()
	}
	r := e.call("weave_attachment_create", map[string]any{"cardId": "c1", "path": "grow.txt"})
	if errCode(t, r) != "validation" || !strings.Contains(resultText(r), "grown") {
		t.Fatalf("growing file: %s", resultText(r))
	}
}

func TestMCPDownload(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 100)...)
	f := newFake(t)
	f.handleFunc("GET /api/v1/attachments/{id}/download/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Disposition", `attachment; filename="../../evil.png"`)
		w.Write(png)
	})
	f.handleFunc("GET /api/v1/export/boards/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte("<html><script>x</script></html>"))
	})
	e := newMCPEnv(t, f, mcpConfig{})
	r := e.call("weave_attachment_download", map[string]any{"id": "a1", "name": "pic.png"})
	if r.IsError {
		t.Fatal(resultText(r))
	}
	it := itemOf(r.StructuredContent)
	p := str(it, "path")
	if !strings.HasPrefix(p, filepath.Join(e.tmp, "weave-mcp")+string(filepath.Separator)) || filepath.Base(p) != "evil.png" {
		t.Fatalf("saved at %q", p)
	}
	b, _ := os.ReadFile(p)
	st, _ := os.Stat(p)
	if !bytes.Equal(b, png) || st.Mode().Perm()&0o111 != 0 || it["mime"] != "image/png" {
		t.Fatalf("saved file %v %v", st.Mode(), it)
	}
	var img *mcp.ImageContent
	for _, c := range r.Content {
		if ic, ok := c.(*mcp.ImageContent); ok {
			img = ic
		}
	}
	if img == nil || img.MIMEType != "image/png" || !bytes.Equal(img.Data, png) {
		t.Fatal("small png must be inlined")
	}
	// saveTo inside the cwd root; no overwrite unless asked
	r = e.call("weave_attachment_download", map[string]any{"id": "a1", "name": "x", "saveTo": "out.png"})
	if r.IsError {
		t.Fatal(resultText(r))
	}
	r = e.call("weave_attachment_download", map[string]any{"id": "a1", "name": "x", "saveTo": "out.png"})
	if errCode(t, r) != "conflict" {
		t.Fatal("existing saveTo needs overwrite")
	}
	if r = e.call("weave_attachment_download", map[string]any{"id": "a1", "name": "x", "saveTo": "out.png", "overwrite": true}); r.IsError {
		t.Fatal(resultText(r))
	}
	_ = os.MkdirAll(filepath.Join(e.proj, ".git", "hooks"), 0o755)
	r = e.call("weave_attachment_download", map[string]any{"id": "a1", "name": "x", "saveTo": ".git/hooks/pre-commit"})
	if errCode(t, r) != "forbidden" {
		t.Fatalf(".git target: %s", resultText(r))
	}
	r = e.call("weave_attachment_download", map[string]any{"id": "a1", "name": "x", "saveTo": filepath.Join(e.home, "x.png")})
	if errCode(t, r) != "forbidden" {
		t.Fatal("saveTo outside roots")
	}
	// HTML export: saved, never inlined
	r = e.call(mcpCallTool, map[string]any{"op": "export.board", "args": map[string]any{"id": "b1"}})
	if r.IsError || len(r.Content) != 1 || !strings.HasPrefix(str(itemOf(r.StructuredContent), "mime"), "text/html") {
		t.Fatalf("export: %s", resultText(r))
	}
	// oversize download
	t.Setenv("WEAVE_MCP_MAX_DOWNLOAD_MB", "0.00001")
	e2 := newMCPEnv(t, f, mcpConfig{})
	r = e2.call("weave_attachment_download", map[string]any{"id": "a1", "name": "x"})
	if errCode(t, r) != "validation" {
		t.Fatalf("oversize download: %s", resultText(r))
	}
}

func TestMCPPrintConfigHasNoToken(t *testing.T) {
	newFake(t)
	t.Setenv("WEAVE_TOKEN", "wk_PrintConfigSecret_123")
	for _, tool := range mcpTargetIDs() {
		for _, scope := range []string{"user", "project"} {
			var out, errb bytes.Buffer
			a := &App{Stdout: &out, Stderr: &errb, SkillEnv: &skillEnv{GOOS: "linux", Home: t.TempDir(), Cwd: t.TempDir(), Exe: "/opt/weave"}}
			a.Cfg = resolveConfig("", "", "")
			if code := a.mcpPrintConfig(tool, scope); code != 0 {
				t.Fatalf("%s: exit %d %s", tool, code, errb.String())
			}
			all := out.String() + errb.String()
			if strings.Contains(all, "wk_PrintConfigSecret_123") || strings.Contains(out.String(), "WEAVE_TOKEN") {
				t.Fatalf("%s leaks the token: %s", tool, all)
			}
			if !strings.Contains(out.String(), "/opt/weave") {
				t.Fatalf("%s: %s", tool, out.String())
			}
		}
	}
}
