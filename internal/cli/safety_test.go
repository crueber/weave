package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestInsecureURLWarning(t *testing.T) {
	for u, warn := range map[string]bool{
		"http://localhost:8080": false, "http://127.0.0.1:8080": false, "http://[::1]:8080": false, "http://10.1.2.3": false,
		"http://192.168.0.5:8080": false, "http://172.16.9.9": false, "http://169.254.1.1": false, "http://x.localhost": false,
		"https://weave.example.com": false, "http://weave.example.com": true, "http://8.8.8.8": true, "http://172.32.0.1": true,
	} {
		if got := insecureURLWarning(u) != ""; got != warn {
			t.Errorf("insecureURLWarning(%q) warns=%v, want %v", u, got, warn)
		}
	}
}

func TestInsecureWarningPrintedOnceToStderr(t *testing.T) {
	var errb bytes.Buffer
	a := &App{Stderr: &errb, Cfg: Resolved{URL: "http://weave.example.com"}}
	a.warnInsecure()
	a.warnInsecure()
	if n := strings.Count(errb.String(), "plain http"); n != 1 {
		t.Fatalf("warning printed %d times: %q", n, errb.String())
	}
}

func TestTokenFlagWarning(t *testing.T) {
	f := newFake(t)
	f.json("GET /api/v1/auth/whoami", map[string]any{"item": map[string]any{"user": map[string]any{"id": "u1", "name": "x"}}})
	_, _, errs := f.run("--token", "wk_literal123", "whoami")
	if !strings.Contains(errs, "--token") || !strings.Contains(errs, "WEAVE_TOKEN") {
		t.Fatalf("no warning for a literal --token: %q", errs)
	}
	_, _, errs = f.runIn("wk_fromstdin\n", "login", "--token", "-", "--no-verify")
	if strings.Contains(errs, "visible in the process list") {
		t.Fatalf("`--token -` must not warn: %q", errs)
	}
}

func TestRedirectPolicy(t *testing.T) {
	mk := func(u string) *http.Request { r, _ := http.NewRequest("GET", u, nil); return r }
	if err := redirectPolicy(true)(mk("http://a.example/x"), []*http.Request{mk("https://a.example/")}); err == nil {
		t.Error("https->http must be refused")
	}
	if err := redirectPolicy(false)(mk("http://a.example/x"), []*http.Request{mk("https://a.example/")}); err == nil {
		t.Error("https->http must be refused even without a token")
	}
	if err := redirectPolicy(true)(mk("https://evil.example/x"), []*http.Request{mk("https://a.example/")}); err == nil {
		t.Error("host change with a token must be refused")
	}
	if err := redirectPolicy(false)(mk("https://other.example/x"), []*http.Request{mk("https://a.example/")}); err != nil {
		t.Errorf("host change without a token is allowed: %v", err)
	}
	if err := redirectPolicy(true)(mk("https://a.example/y"), []*http.Request{mk("https://a.example/")}); err != nil {
		t.Errorf("same-host redirect must work: %v", err)
	}
	if err := redirectPolicy(true)(mk("http://a.example/y"), []*http.Request{mk("http://a.example/")}); err != nil {
		t.Errorf("http->http same host unchanged: %v", err)
	}

	// end to end: a server that redirects to a second host must not receive the token
	var leaked bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("Authorization") != ""
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	a := &App{Cfg: Resolved{URL: redir.URL, Token: "wk_secret"}}
	req, _ := http.NewRequest("GET", redir.URL+"/x", nil)
	req.Header.Set("Authorization", "Bearer wk_secret")
	if resp, err := a.httpClient(time.Second).Do(req); err == nil {
		resp.Body.Close()
		t.Fatal("redirect to another host with a token must fail")
	}
	if leaked {
		t.Fatal("token followed the redirect")
	}
}

func TestResponseSizeLimit(t *testing.T) {
	old := maxResponseBytes
	maxResponseBytes = 1024
	defer func() { maxResponseBytes = old }()
	if _, err := readLimited(strings.NewReader(strings.Repeat("a", 1024))); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if _, err := readLimited(strings.NewReader(strings.Repeat("a", 1025))); err == nil {
		t.Fatal("over the limit must fail")
	}
}

func TestCleanText(t *testing.T) {
	for in, want := range map[string]string{
		"plain": "plain", "a\x1b[31mred\x1b[0m": "a[31mred[0m", "bell\x07": "bell", "nul\x00x": "nulx", "c1\u0085\u009bx": "c1x",
		"tab\there": "tab here", "line\nbreak": "line break", "crlf\r\n": "crlf  ", "unicode é 日本": "unicode é 日本",
	} {
		if got := cleanText(in, false); got != want {
			t.Errorf("cleanText(%q, false) = %q, want %q", in, got, want)
		}
	}
	if got := cleanText("a\tb\nc\x1b[2J", true); got != "a\tb\nc[2J" {
		t.Errorf("free text keeps tab/newline: %q", got)
	}
}

func TestTableAndJQOutputStripControlCharacters(t *testing.T) {
	var out bytes.Buffer
	a := &App{Stdout: &out, Out: OutOpts{Format: "table"}}
	rows := []any{map[string]any{"id": "1", "name": "evil\x1b]0;pwned\x07 name"}}
	if err := a.writeTable(rows); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Fatalf("table output has control characters: %q", out.String())
	}
	out.Reset()
	a.Out = OutOpts{Format: "json", JQ: ".name"}
	if err := a.emit(map[string]any{"name": "x\x1b[31m"}, nil); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Fatalf("jq text output has control characters: %q", out.String())
	}
}

func TestRedactAndCapTail(t *testing.T) {
	tok := "wk_abcdef0123456789"
	in := "start\nAuthorization: Bearer " + tok + "\nother wk_ZZZ999 key\nend"
	got := redactSecrets(in, tok)
	if strings.Contains(got, tok) || strings.Contains(got, "wk_ZZZ999") || strings.Count(got, "[redacted]") != 2 {
		t.Fatalf("redactSecrets: %q", got)
	}
	// custom (non wk_) token values are masked too
	if got := redactSecrets("key=sekrit-token-value", "sekrit-token-value"); strings.Contains(got, "sekrit") {
		t.Fatalf("exact token not masked: %q", got)
	}
	long := strings.Repeat("é", 5000)
	if c := capTail(long, 2048); len(c) > 2048+len("…") || !strings.HasSuffix(c, "é") {
		t.Fatalf("capTail len=%d", len(c))
	}
	if sanitizeTail(strings.Repeat("x", 10000)+tok, tok) == "" || strings.Contains(sanitizeTail(strings.Repeat("x", 10000)+tok, tok), tok) {
		t.Fatal("sanitizeTail must cap and redact")
	}
}

func TestAgentRunFailureCommentHasNoTokenAndIsCapped(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("needs sh")
	}
	f := newFake(t)
	f.json("POST /api/v1/cards/{id}/claim", map[string]any{"item": map[string]any{"claimedBy": "u1"}})
	f.json("POST /api/v1/cards/{id}/release", map[string]any{"item": map[string]any{}})
	f.json("POST /api/v1/cards/{id}/move", map[string]any{"item": map[string]any{}})
	f.json("POST /api/v1/cards/{cardId}/comments", map[string]any{"item": map[string]any{"id": "m"}})
	cliSSE(f, ev("e1", "card.created", nil))
	script := `i=0; while [ $i -lt 400 ]; do echo "padding line $i padding padding padding"; i=$((i+1)); done; echo "token=$WEAVE_TOKEN and wk_LEAKED1234567890 here" >&2; echo "token=wk_test and wk_OTHERKEY99" >&2; exit 3`
	code, _, errs := f.run("agent", "run", "--board", "Product", "--listen", "Ready", "--fail-list", "Review",
		"--once", "--no-cursor", "--", "sh", "-c", script)
	if code != 0 {
		t.Fatalf("%d %s", code, errs)
	}
	body := f.body("POST /api/v1/cards/c-e1/comments")
	if body == "" {
		t.Fatalf("no failure comment: %v", f.calls)
	}
	var m map[string]any
	_ = json.Unmarshal([]byte(body), &m)
	text, _ := m["text"].(string)
	if strings.Contains(text, "wk_test") || strings.Contains(text, "LEAKED") || strings.Contains(text, "OTHERKEY") {
		t.Fatalf("comment leaks a token: %q", text)
	}
	if !strings.Contains(text, "[redacted]") {
		t.Fatalf("expected redaction markers: %q", text)
	}
	if len(text) > 2048+400 {
		t.Fatalf("comment tail not capped: %d bytes", len(text))
	}
}

func TestCompletionQuote(t *testing.T) {
	for in, want := range map[string]string{
		"board": "board", "--dry-run": "--dry-run", "card.create": "card.create", "My Board": "'My Board'", "it's": `'it'\''s'`,
		"a;rm -rf": "'a;rm -rf'", "$(x)": "'$(x)'", "x\x1b[31m": "x[31m", /* control dropped, then [ ] needs quoting */
	} {
		got := completionQuote(in)
		if in == "x\x1b[31m" {
			want = "'x[31m'"
		}
		if got != want {
			t.Errorf("completionQuote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConfigAndCachePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("posix permissions")
	}
	dir := filepath.Join(t.TempDir(), "weave")
	if err := os.MkdirAll(dir, 0o755); err != nil { // pre-existing, too-open directory
		t.Fatal(err)
	}
	t.Setenv("WEAVE_CONFIG_DIR", dir)
	if err := saveConfigFile(ConfigFile{Current: "default", Profiles: map[string]Profile{"default": {URL: "https://x", Token: "wk_t"}}}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Errorf("config dir mode %v, want 0700", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Join(dir, "config.json")); st.Mode().Perm() != 0o600 {
		t.Errorf("config file mode %v, want 0600", st.Mode().Perm())
	}
	// a fresh nested config dir is created private as well
	t.Setenv("WEAVE_CONFIG_DIR", filepath.Join(t.TempDir(), "a", "b"))
	if err := saveConfigFile(ConfigFile{}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(configDir()); st.Mode().Perm() != 0o700 {
		t.Errorf("new config dir mode %v", st.Mode().Perm())
	}

	cdir := filepath.Join(t.TempDir(), "cache", "weave")
	t.Setenv("WEAVE_CACHE_DIR", cdir)
	a := &App{}
	a.writeCache(cacheFile{URL: "https://x", Ops: []Op{{Name: "a.b"}}})
	if st, err := os.Stat(schemaCachePath("https://x")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("cache file: %v %v", st, err)
	}
	if st, _ := os.Stat(cdir); st.Mode().Perm() != 0o700 {
		t.Errorf("cache dir mode %v", st.Mode().Perm())
	}
}

func TestNoWorldWritableCacheFallback(t *testing.T) {
	t.Setenv("WEAVE_CACHE_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	if d := cacheDir(); d != "" {
		if strings.HasPrefix(d, os.TempDir()) {
			t.Fatalf("cache must not fall back to the temp dir: %s", d)
		}
	}
	if schemaCachePath("https://x") == "" {
		return // caching disabled: fine
	}
}

var _ = url.PathEscape
