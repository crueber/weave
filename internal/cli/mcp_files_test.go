package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// gbox is a guard sandbox: home, a project (the cwd), a temp dir and Weave's config/cache dirs.
type gbox struct {
	t                           *testing.T
	home, proj, tmp, cfg, cache string
	env                         map[string]string
}

func newGbox(t *testing.T) *gbox {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	b := &gbox{t: t, home: filepath.Join(root, "home"), proj: filepath.Join(root, "home", "proj"), tmp: filepath.Join(root, "tmp"),
		env: map[string]string{}}
	b.cfg = filepath.Join(b.proj, "cfg-weave") // deliberately inside a root
	b.cache = filepath.Join(b.tmp, "cache-weave")
	for _, d := range []string{b.home, b.proj, b.tmp, b.cfg, b.cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func (b *gbox) guard() *fileGuard {
	return newFileGuard(guardEnv{GOOS: runtime.GOOS, Cwd: b.proj, Home: b.home, Temp: b.tmp, ConfigDir: b.cfg, CacheDir: b.cache,
		Getenv: func(k string) string { return b.env[k] }})
}

func (b *gbox) write(p, content string) string {
	b.t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		b.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		b.t.Fatal(err)
	}
	return p
}

func wantRefused(t *testing.T, err error, code, substr string) {
	t.Helper()
	ge, ok := err.(*guardErr)
	if !ok {
		t.Fatalf("want %s refusal, got %v", code, err)
	}
	if ge.code != code || !strings.Contains(ge.msg, substr) {
		t.Fatalf("want %s %q, got %s %q", code, substr, ge.code, ge.msg)
	}
}

func TestGuardAllowsFilesInRoots(t *testing.T) {
	b := newGbox(t)
	g := b.guard()
	b.write(filepath.Join(b.proj, "docs", "a.txt"), "A")
	for _, p := range []string{"docs/a.txt", filepath.Join(b.proj, "docs", "a.txt"), b.write(filepath.Join(b.tmp, "t.txt"), "T")} {
		gf, err := g.openUpload(p, 1<<20)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		gf.Close()
	}
}

func TestGuardTraversalAndOutsideRoots(t *testing.T) {
	b := newGbox(t)
	g := b.guard()
	b.write(filepath.Join(b.home, "secret.txt"), "s")
	_, err := g.openUpload("../secret.txt", 1<<20)
	wantRefused(t, err, "forbidden", "outside the allowed upload roots")
	if !strings.Contains(err.Error(), "WEAVE_MCP_UPLOAD_ROOTS") || !strings.Contains(err.Error(), "cwd=") {
		t.Fatalf("message must be actionable: %v", err)
	}
	_, err = g.openUpload(filepath.Join(b.home, "secret.txt"), 1<<20)
	wantRefused(t, err, "forbidden", "outside")
	_, err = g.openUpload(filepath.Join(b.proj, "missing.txt"), 1<<20)
	wantRefused(t, err, "not_found", "no such file")
}

func TestGuardSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	b := newGbox(t)
	g := b.guard()
	outside := b.write(filepath.Join(b.home, "outside.txt"), "o")
	if err := os.Symlink(outside, filepath.Join(b.proj, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	_, err := g.openUpload("escape.txt", 1<<20)
	wantRefused(t, err, "forbidden", "outside")
	// a symlink inside the roots pointing at a .ssh key that is itself inside a root
	key := b.write(filepath.Join(b.tmp, ".ssh", "id_ed25519"), "KEY")
	_ = os.Symlink(key, filepath.Join(b.proj, "harmless.txt"))
	_, err = g.openUpload("harmless.txt", 1<<20)
	wantRefused(t, err, "forbidden", "credential/config")
}

func TestGuardDenyList(t *testing.T) {
	b := newGbox(t)
	g := b.guard()
	for _, rel := range []string{".env", ".env.production", "certs/server.pem", "tls.key", "id_rsa", ".netrc", ".git-credentials", ".npmrc", ".pypirc",
		".git/config", ".ssh/known_hosts", ".aws/credentials", ".kube/config", ".docker/config.json", ".config/gh/hosts.yml", ".config/gcloud/creds.json", ".gnupg/x", ".azure/x"} {
		p := b.write(filepath.Join(b.proj, filepath.FromSlash(rel)), "x")
		_, err := g.openUpload(p, 1<<20)
		wantRefused(t, err, "forbidden", "credential/config")
	}
	// Weave's own config and cache dirs, even inside a root
	_, err := g.openUpload(b.write(filepath.Join(b.cfg, "config.json"), "{}"), 1<<20)
	wantRefused(t, err, "forbidden", "credential/config")
	_, err = g.openUpload(b.write(filepath.Join(b.cache, "ops.json"), "{}"), 1<<20)
	wantRefused(t, err, "forbidden", "credential/config")
}

func TestGuardNonRegularFiles(t *testing.T) {
	b := newGbox(t)
	g := b.guard()
	_, err := g.openUpload(b.proj, 1<<20)
	wantRefused(t, err, "validation", "directory")
	_ = os.MkdirAll(filepath.Join(b.proj, "sub"), 0o755)
	_, err = g.openUpload("sub", 1<<20)
	wantRefused(t, err, "validation", "directory")
	_, err = g.openUpload("-", 1<<20)
	wantRefused(t, err, "validation", "stdin")
	if runtime.GOOS != "windows" {
		b.env["WEAVE_MCP_UPLOAD_ROOTS"] = "/dev"
		g2 := b.guard()
		done := make(chan error, 1)
		go func() { _, err := g2.openUpload("/dev/zero", 1<<20); done <- err }()
		select {
		case err := <-done:
			wantRefused(t, err, "validation", "not a regular file")
		case <-time.After(5 * time.Second):
			t.Fatal("/dev/zero blocked")
		}
	}
}

func TestGuardOversizeSparse(t *testing.T) {
	b := newGbox(t)
	g := b.guard()
	p := filepath.Join(b.proj, "sparse.bin")
	fh, _ := os.Create(p)
	_ = fh.Truncate(10 << 20)
	fh.Close()
	_, err := g.openUpload(p, 1<<20)
	wantRefused(t, err, "validation", "over the upload limit")
}

func TestGuardCapReaderFailsOnGrowth(t *testing.T) {
	cr := newCapReader(strings.NewReader(strings.Repeat("x", 100)), 10)
	buf := make([]byte, 64)
	var err error
	for err == nil {
		_, err = cr.Read(buf)
	}
	wantRefused(t, cr.err, "validation", "larger than the upload limit")
	if cr.n != 10 {
		t.Fatalf("read %d bytes past the cap", cr.n)
	}
}

func TestGuardRoots(t *testing.T) {
	b := newGbox(t)
	// cwd = $HOME drops the cwd root
	g := newFileGuard(guardEnv{GOOS: runtime.GOOS, Cwd: b.home, Home: b.home, Temp: b.tmp})
	if len(g.roots) != 1 || g.roots[0] != b.tmp || !strings.Contains(g.rootsDesc, "cwd not allowed") {
		t.Fatalf("roots %v (%s)", g.roots, g.rootsDesc)
	}
	b.write(filepath.Join(b.home, "notes.txt"), "n")
	_, err := g.openUpload("notes.txt", 1<<20)
	wantRefused(t, err, "forbidden", "outside")
	// cwd = / drops it too
	g = newFileGuard(guardEnv{GOOS: runtime.GOOS, Cwd: string(filepath.Separator), Home: b.home, Temp: b.tmp})
	if len(g.roots) != 1 {
		t.Fatalf("roots %v", g.roots)
	}
	// WEAVE_MCP_UPLOAD_ROOTS replaces the defaults
	other := filepath.Join(b.home, "shared")
	_ = os.MkdirAll(other, 0o755)
	b.env["WEAVE_MCP_UPLOAD_ROOTS"] = other
	g = b.guard()
	if len(g.roots) != 1 || g.roots[0] != other {
		t.Fatalf("roots %v", g.roots)
	}
	_, err = g.openUpload(b.write(filepath.Join(b.proj, "a.txt"), "a"), 1<<20)
	wantRefused(t, err, "forbidden", "WEAVE_MCP_UPLOAD_ROOTS="+other)
	// component boundary: /x/shared-evil is not inside /x/shared
	evil := b.write(filepath.Join(b.home, "shared-evil", "f.txt"), "e")
	_, err = g.openUpload(evil, 1<<20)
	wantRefused(t, err, "forbidden", "outside")
}

func TestGuardCaseInsensitiveOnDarwinWindows(t *testing.T) {
	g := &fileGuard{goos: "darwin", deny: []string{"/Users/a/Library/Application Support/weave"}}
	if !g.within("/Users/a/proj", "/users/A/PROJ/x") || g.within("/Users/a/proj", "/Users/a/project") {
		t.Fatal("darwin comparisons are case-insensitive and component bounded")
	}
	if !g.denied("/Users/a/.SSH/x") || !g.denied("/users/a/library/application support/WEAVE/config.json") || !g.denied("/p/.ENV") {
		t.Fatal("deny list must be case-insensitive on darwin")
	}
	lg := &fileGuard{goos: "linux"}
	if lg.within("/home/a/proj", "/home/a/PROJ/x") {
		t.Fatal("linux is case-sensitive")
	}
}

func TestWindowsPathProblem(t *testing.T) {
	for _, p := range []string{`\\server\share\f.txt`, `\\?\C:\x`, `\\.\PhysicalDrive0`, `//server/share/x`, `C:\x\f.txt:secret`, `f.txt::$DATA`,
		`C:\x\CON`, `nul.txt`, `C:\x\com1.log`, `LPT9`, `aux.`} {
		if windowsPathProblem(p) == "" {
			t.Errorf("%q must be refused", p)
		}
	}
	for _, p := range []string{`C:\Users\a\f.txt`, `rel\console.txt`, `d:/x/com10.txt`, `notes.txt`} {
		if m := windowsPathProblem(p); m != "" {
			t.Errorf("%q refused: %s", p, m)
		}
	}
	g := &fileGuard{goos: "windows", cwd: `C:\w`}
	_, err := g.absOf(`\\server\share\x`)
	wantRefused(t, err, "forbidden", "UNC")
}

func TestGuardSaveTo(t *testing.T) {
	b := newGbox(t)
	g := b.guard()
	f, r, p, err := g.openSave("out/../result.txt", false)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	r.Close()
	if p != filepath.Join(b.proj, "result.txt") {
		t.Fatalf("saved at %s", p)
	}
	_, _, _, err = g.openSave("result.txt", false)
	wantRefused(t, err, "conflict", "overwrite")
	_, _, _, err = g.openSave(filepath.Join(b.home, "x.txt"), false)
	wantRefused(t, err, "forbidden", "outside")
	_, _, _, err = g.openSave(".env", false)
	wantRefused(t, err, "forbidden", "credential")
	_, _, _, err = g.openSave("nodir/x.txt", false)
	wantRefused(t, err, "not_found", "does not exist")
	if runtime.GOOS != "windows" {
		_ = os.Symlink(filepath.Join(b.home, "target.txt"), filepath.Join(b.proj, "link.txt"))
		_, _, _, err = g.openSave("link.txt", true)
		wantRefused(t, err, "forbidden", "symlink")
	}
}
