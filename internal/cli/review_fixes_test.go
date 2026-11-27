package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

type errAfterReader struct {
	data []byte
	done bool
	err  error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if !r.done {
		r.done = true
		n := copy(p, r.data)
		return n, nil
	}
	return 0, r.err
}

func TestReadLimitedReturnsNilOnError(t *testing.T) {
	wantErr := errors.New("connection reset")
	b, err := readLimited(&errAfterReader{data: []byte(`{"item":`), err: wantErr})
	if err == nil || b != nil {
		t.Fatalf("expected nil, err; got %q, %v", b, err)
	}
}

func TestReadOutboxRejectsSymlinksAndCapsReads(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	outDir := filepath.Join(dir, "out")
	if err := os.MkdirAll(filepath.Join(outDir, "attachments"), 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("done"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(outDir, "result")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(outDir, "comment.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(outDir, "attachments", "leak.txt")); err != nil {
		t.Fatal(err)
	}
	res, cmt, files := readOutbox(&outbox{dir: dir, out: outDir})
	if res != "" || cmt != "" || len(files) != 0 {
		t.Fatalf("symlinks must be rejected: res=%q cmt=%q files=%v", res, cmt, files)
	}

	// Oversized result is capped at 256 B
	_ = os.Remove(filepath.Join(outDir, "result"))
	if err := os.WriteFile(filepath.Join(outDir, "result"), []byte(strings.Repeat("a", 500)), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _, _ = readOutbox(&outbox{dir: dir, out: outDir})
	if len(res) != 256 {
		t.Fatalf("result not capped: len=%d", len(res))
	}
}

func TestOpenSaveOverwriteRefusesSwapWithoutTruncating(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	b := newGbox(t)
	g := b.guard()
	p := b.write(filepath.Join(b.proj, "target.txt"), "original")
	outside := b.write(filepath.Join(b.home, "victim.txt"), "keep-me")
	g.afterStat = func() {
		_ = os.Remove(p)
		_ = os.Symlink(outside, p)
	}
	_, _, _, err := g.openSave(p, true)
	if err == nil {
		t.Fatal("symlink swap during overwrite must fail")
	}
	if strings.Contains(err.Error(), "<nil>") {
		t.Fatalf("error formatted <nil>: %v", err)
	}
	got, _ := os.ReadFile(outside)
	if string(got) != "keep-me" {
		t.Fatalf("outside file was modified: %q", got)
	}
}

func TestUnclosedBracketDoesNotPanic(t *testing.T) {
	v := map[string]any{"items": []any{map[string]any{"id": "1"}}}
	for _, expr := range []string{".[", ".items[", ".items[0"} {
		if _, err := EvalJQ(v, expr); err == nil {
			t.Errorf("%q: expected error", expr)
		}
	}
	if _, ok := lookup(v, splitPath("items[")); ok {
		t.Error("lookup on unclosed bracket should return false")
	}
	if _, ok := lookup(v, []string{"["}); ok {
		t.Error("lookup on bare '[' should return false")
	}
}

func TestResolveCardCacheKeyIncludesRaw(t *testing.T) {
	f := newFake(t)
	f.handleFunc("GET /api/v1/boards/{boardId}/cards", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query().Get("q")
		if q == "Alpha" {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "c-alpha", "name": "Alpha"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "c-beta", "name": "Beta"}}})
	})
	a := testApp(f)
	id1, err := a.Resolve(context.Background(), "card", "Alpha", Scope{BoardID: "b1"})
	if err != nil || id1 != "c-alpha" {
		t.Fatalf("Alpha: %q %v", id1, err)
	}
	id2, err := a.Resolve(context.Background(), "card", "Beta", Scope{BoardID: "b1"})
	if err != nil || id2 != "c-beta" {
		t.Fatalf("Beta: %q %v", id2, err)
	}
}

func TestParseDurRejectsNaNInfAndGarbage(t *testing.T) {
	for _, bad := range []string{"NaN", "Inf", "+Inf", "-Inf", "10abc", "abc"} {
		if _, err := parseDur(bad); err == nil {
			t.Errorf("parseDur(%q) should fail", bad)
		}
	}
	if d, err := parseDur("2.5"); err != nil || d != 2500*time.Millisecond {
		t.Fatalf("parseDur(2.5) = %v, %v", d, err)
	}
}

func TestFlagParseErrorPropagation(t *testing.T) {
	f := newFake(t)
	cases := [][]string{
		{"next", "--board", "Product", "--list", "Ready", "--wait", "bogus"},
		{"next", "--board", "Product", "--list", "Ready", "--interval", "bogus"},
		{"listen", "--board", "Product", "--list", "Ready", "--exec-timeout", "bogus"},
		{"listen", "--board", "Product", "--list", "Ready", "--grace", "bogus"},
		{"listen", "--board", "Product", "--list", "Ready", "--concurrency", "0"},
		{"listen", "--board", "Product", "--list", "Ready", "--heartbeat", "500ms"},
		{"agent", "run", "--board", "Product", "--listen", "Ready", "--grace", "bogus", "--", "true"},
		{"agent", "run", "--board", "Product", "--listen", "Ready", "--concurrency", "abc", "--", "true"},
		{"agent", "run", "--board", "Product", "--listen", "Ready", "--timeout", "bogus", "--", "true"},
		{"agent", "run", "--board", "Product", "--listen", "Ready", "--max-attempts", "2x", "--fail-list", "Review", "--", "true"},
		{"card", "list", "-z"},
	}
	for _, args := range cases {
		if code, _, _ := f.run(args...); code != ExitUsage {
			t.Errorf("%v: got exit %d, want %d", args, code, ExitUsage)
		}
	}
}

func TestAgentPromptGeneralIgnoresWeaveBoardAndHasHelp(t *testing.T) {
	f := newFake(t)
	t.Setenv("WEAVE_BOARD", "Product")
	var last string
	f.handleFunc("GET /api/v1/agent/prompt", func(w http.ResponseWriter, r *http.Request) {
		last = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"item":{"scope":"general","text":"hello\n"}}`))
	})
	code, _, errs := f.run("agent", "prompt", "--general")
	if code != 0 || !strings.Contains(last, "scope=general") || strings.Contains(last, "id=") {
		t.Fatalf("--general with WEAVE_BOARD: code=%d query=%q errs=%s", code, last, errs)
	}
	code, out, _ := f.run("agent", "prompt", "--help")
	if code != 0 || !strings.Contains(out, "--work-order") {
		t.Fatalf("agent prompt --help: code=%d out=%q", code, out)
	}
}

func TestGenericLazyEnvAndPaginationAndRedact(t *testing.T) {
	f := newFake(t)
	t.Setenv("WEAVE_BOARD", "NonExistentBoard")
	t.Setenv("WEAVE_PROJECT", "NonExistentProject")
	f.json("GET /api/v1/users", map[string]any{"items": []any{}})
	// Unrelated op must not try to resolve WEAVE_BOARD / WEAVE_PROJECT
	if code, _, errs := f.run("user", "list"); code != 0 {
		t.Fatalf("user list failed due to unrelated WEAVE_BOARD: %d %s", code, errs)
	}

	// redactParams masks sensitive keys and truncates UTF-8 safely
	rp := redactParams(Params{
		"secret":      "top-secret",
		"accessToken": "tok-123",
		"password":    "pw",
		"apiKey":      "k",
		"name":        strings.Repeat("é", 50),
	})
	for _, leaked := range []string{"top-secret", "tok-123", "pw"} {
		if strings.Contains(rp, leaked) {
			t.Fatalf("redactParams leaked %q: %s", leaked, rp)
		}
	}
	if !utf8.ValidString(rp) {
		t.Fatalf("redactParams broke UTF-8: %q", rp)
	}

	// fetchPaged returns [] not null, does not mutate caller params, preserves cursor with --all, caps page limit
	var gotLimits, gotCursors []string
	f.handleFunc("GET /api/v1/boards/{boardId}/cards", func(w http.ResponseWriter, r *http.Request) {
		gotLimits = append(gotLimits, r.URL.Query().Get("limit"))
		gotCursors = append(gotCursors, r.URL.Query().Get("cursor"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[]}`))
	})
	a := testApp(f)
	op, _ := a.mustOp(context.Background(), "card.list")
	callerParams := Params{"boardId": "b1", "cursor": "c-start"}
	r, err := a.fetchPaged(context.Background(), op, callerParams, nil, 500, true)
	if err != nil {
		t.Fatal(err)
	}
	if string(r.Body) != `{"items":[]}` {
		t.Fatalf("empty paged body = %s, want {\"items\":[]}", r.Body)
	}
	if callerParams["limit"] != nil || callerParams["cursor"] != "c-start" {
		t.Fatalf("callerParams mutated: %v", callerParams)
	}
	if len(gotLimits) != 1 || gotLimits[0] != "100" || gotCursors[0] != "c-start" {
		t.Fatalf("limits=%v cursors=%v", gotLimits, gotCursors)
	}
}

func TestStreamToFileBypassesResponseCapAndDryRunSkipsFileOpen(t *testing.T) {
	f := newFake(t)
	oldCap := maxResponseBytes
	maxResponseBytes = 32
	defer func() { maxResponseBytes = oldCap }()

	payload := strings.Repeat("0123456789", 10) // 100 bytes > 32
	f.handleFunc("GET /api/v1/attachments/{id}/download/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte(payload))
	})
	outPath := filepath.Join(t.TempDir(), "downloaded.bin")
	code, out, errs := f.run("attachment", "download", "a1", "file.bin", "-o", outPath)
	if code != 0 || !strings.Contains(out, `"bytes":100`) {
		t.Fatalf("stream -o failed: %d %s %s", code, out, errs)
	}
	got, err := os.ReadFile(outPath)
	if err != nil || string(got) != payload {
		t.Fatalf("saved file mismatch: %v %q", err, got)
	}

	// Dry-run on file upload must not fail trying to open a non-existent file
	code, out, errs = f.run("attachment", "create", "--cardId", "c1", "--file", "/no/such/file.bin", "--dry-run")
	if code != 0 || !strings.Contains(out, `"dryRun":true`) {
		t.Fatalf("dry-run upload should not read file: %d %s %s", code, out, errs)
	}
}

func TestMCPInlineImageFieldAndUTF8TruncateAndTempCleanup(t *testing.T) {
	// 1x1 valid PNG
	pngBytes := []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d,
		0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89, 0x00, 0x00, 0x00,
		0x0a, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00, 0x00, 0x00, 0x00, 0x49,
		0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
	}
	f := newFake(t)
	f.handleFunc("GET /api/v1/attachments/{id}/download/{name}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	})
	e := newMCPEnv(t, f, mcpConfig{})
	r := e.call("weave_attachment_download", map[string]any{"id": "a1", "name": "dot.png"})
	if r.IsError {
		t.Fatalf("download error: %s", resultText(r))
	}
	// JSON text must include "inline":true
	if !strings.Contains(resultText(r), `"inline":true`) {
		t.Fatalf("downloadResult JSON missing inline:true: %s", resultText(r))
	}

	// truncateUTF8 never splits multi-byte UTF-8
	s := []byte(strings.Repeat("é", 20))
	p, tr := truncateUTF8(s, 7)
	if !tr || !utf8.Valid(p) || len(p) != 6 {
		t.Fatalf("truncateUTF8: tr=%v len=%d valid=%v", tr, len(p), utf8.Valid(p))
	}
}

func TestMCPWriteRemovePropagateReadError(t *testing.T) {
	dir := t.TempDir() // trying to ReadFile a directory returns EISDIR (not ErrNotExist)
	if _, err := mcpWrite(dir, "mcpServers", map[string]any{"command": "weave"}, false, false); err == nil {
		t.Fatal("mcpWrite on unreadable path/dir must return error")
	}
	if _, err := mcpRemove(dir, "mcpServers", map[string]any{"command": "weave"}, false, false); err == nil {
		t.Fatal("mcpRemove on unreadable path/dir must return error")
	}
}

func TestConfigMaskShortTokenAndNoColorDoesNotMutateEnv(t *testing.T) {
	f := newFake(t)
	t.Setenv("WEAVE_TOKEN", "abc")
	code, out, _ := f.run("config", "get", "token")
	if code != 0 || strings.TrimSpace(out) != "********" {
		t.Fatalf("short token not masked: %d %q", code, out)
	}
	t.Setenv("NO_COLOR", "")
	_, _, _ = f.run("--no-color", "version")
	if os.Getenv("NO_COLOR") != "" {
		t.Fatal("--no-color must not mutate process NO_COLOR env")
	}
}

func TestExistingEventsPreservesListOrderAndListFullPassesLimit(t *testing.T) {
	f := newFake(t)
	var seenLists []string
	var seenLimit string
	f.handleFunc("GET /api/v1/boards/{boardId}/cards", func(w http.ResponseWriter, r *http.Request) {
		seenLists = append(seenLists, r.URL.Query().Get("listId"))
		seenLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	})
	a := testApp(f)
	cfg := &listenCfg{
		boards:    []string{"b1"},
		lists:     []string{"l3", "l1", "l4"},
		listBoard: map[string]string{"l3": "b1", "l1": "b1", "l4": "b1"},
	}
	if _, err := a.existingEvents(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(seenLists, []string{"l3", "l1", "l4"}) {
		t.Fatalf("existingEvents order: got %v want [l3 l1 l4]", seenLists)
	}

	f.json("GET /api/v1/lists/{id}", map[string]any{"item": map[string]any{"id": "l2", "wipLimit": 3}})
	_ = a.listFull(context.Background(), "b1", "l2")
	if seenLimit != "3" {
		t.Fatalf("listFull limit = %q, want 3", seenLimit)
	}
}

func TestSkillAbsPathAndCRLFAndCardCommentCompletion(t *testing.T) {
	e := &skillEnv{Home: "/home/u", Cwd: "/work"}
	if got := absPath(e, "~foo/bar"); got != filepath.Clean("/work/~foo/bar") {
		t.Fatalf("absPath(~foo/bar) = %q", got)
	}
	if got := absPath(e, "~/bar"); got != filepath.Clean("/home/u/bar") {
		t.Fatalf("absPath(~/bar) = %q", got)
	}

	// CRLF removeFile
	p := filepath.Join(t.TempDir(), "AGENTS.md")
	body := "hello\n"
	pl := place{Fmt: fmtSharedMD, Path: p}
	content := "before\r\n\r\n" + strings.ReplaceAll(renderBlock(body, "1.0", fileHash(pl, body)), "\n", "\r\n") + "\r\nafter\r\n"
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	act, err := removeFile(pl, body, true)
	if err != nil || act != "removed" {
		t.Fatalf("removeFile CRLF: %q %v", act, err)
	}
	rest, _ := os.ReadFile(p)
	if strings.Contains(string(rest), blockBeginPrefix) || !strings.Contains(string(rest), "before") || !strings.Contains(string(rest), "after") {
		t.Fatalf("CRLF rest: %q", string(rest))
	}

	// card comment --text completion
	f := newFake(t)
	_, out, _ := f.run("__complete", "card", "comment", "--te")
	if !strings.Contains(out, "--text") {
		t.Fatalf("card comment --text missing from completion: %q", out)
	}
}

func TestRingBufferAndTailBufBounds(t *testing.T) {
	l := &Listener{}
	l.defaults()
	for i := 0; i < maxSeenIDs+50; i++ {
		l.remember(string(rune('a'+i%26)) + "-" + time.Duration(i).String())
	}
	if len(l.seenRing) != maxSeenIDs || len(l.seen) != maxSeenIDs {
		t.Fatalf("seenRing=%d seen=%d", len(l.seenRing), len(l.seen))
	}

	var tb tailBuf
	for i := 0; i < 100; i++ {
		_, _ = tb.Write(bytes.Repeat([]byte("x"), 500))
	}
	if len(tb.buf) != maxTailBytes || cap(tb.buf) > 2*maxTailBytes {
		t.Fatalf("tailBuf len=%d cap=%d", len(tb.buf), cap(tb.buf))
	}
}

func TestHTTPClientClonesCustomClientWithTimeout(t *testing.T) {
	base := &http.Client{Transport: http.DefaultTransport}
	a := &App{HTTP: base}
	c := a.httpClient(5 * time.Second)
	if c == base || c.Transport != base.Transport || c.Timeout != 5*time.Second || base.Timeout != 0 {
		t.Fatalf("httpClient clone failed: c=%+v base=%+v", c, base)
	}
}
