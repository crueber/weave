package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// sbox is a sandbox for `weave skill`: a temp HOME and project, an injected OS and PATH. Nothing here
// touches the real home directory or PATH.
type sbox struct {
	t    *testing.T
	home string
	proj string
	goos string
	env  map[string]string
	bins map[string]bool
}

func newSbox(t *testing.T) *sbox {
	t.Helper()
	root := t.TempDir()
	b := &sbox{t: t, home: filepath.Join(root, "home"), proj: filepath.Join(root, "proj"), goos: "linux", env: map[string]string{}, bins: map[string]bool{}}
	for _, d := range []string{b.home, filepath.Join(b.proj, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func (b *sbox) senv() *skillEnv {
	return &skillEnv{GOOS: b.goos, Home: b.home, Cwd: b.proj,
		Getenv: func(k string) string { return b.env[k] },
		LookPath: func(n string) (string, error) {
			if b.bins[n] {
				return "/bin/" + n, nil
			}
			return "", errors.New("not found")
		}}
}

func (b *sbox) mkdir(parts ...string) {
	b.t.Helper()
	if err := os.MkdirAll(filepath.Join(append([]string{b.home}, parts...)...), 0o755); err != nil {
		b.t.Fatal(err)
	}
}

func (b *sbox) run(args ...string) (int, []map[string]any, string) {
	b.t.Helper()
	var out, errb bytes.Buffer
	a := &App{Stdout: &out, Stderr: &errb, SkillEnv: b.senv()}
	code := cmdSkill(a, args)
	var env struct {
		Items []map[string]any `json:"items"`
	}
	_ = json.Unmarshal(out.Bytes(), &env)
	return code, env.Items, out.String() + errb.String()
}

func (b *sbox) read(parts ...string) string {
	b.t.Helper()
	by, err := os.ReadFile(filepath.Join(parts...))
	if err != nil {
		b.t.Fatal(err)
	}
	return string(by)
}

// tree lists every file under root, relative and slash separated.
func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out
}

func rowFor(rows []map[string]any, target string) map[string]any {
	for _, r := range rows {
		if r["target"] == target {
			return r
		}
	}
	return nil
}

func TestSkillPathResolution(t *testing.T) {
	cases := []struct {
		name  string
		goos  string
		env   map[string]string
		tgt   string
		scope string
		want  func(b *sbox) string
	}{
		{"claude user", "linux", nil, "claude", "user", func(b *sbox) string { return filepath.Join(b.home, ".claude", "skills", "weave") }},
		{"claude CLAUDE_CONFIG_DIR", "linux", map[string]string{"CLAUDE_CONFIG_DIR": "/cfg/claude"}, "claude", "user", func(b *sbox) string { return filepath.Join("/cfg/claude", "skills", "weave") }},
		{"claude windows", "windows", nil, "claude", "user", func(b *sbox) string { return filepath.Join(b.home, ".claude", "skills", "weave") }},
		{"claude project", "linux", nil, "claude", "project", func(b *sbox) string { return filepath.Join(b.proj, ".claude", "skills", "weave") }},
		{"omp user", "linux", nil, "omp", "user", func(b *sbox) string { return filepath.Join(b.home, ".omp", "agent", "skills", "weave") }},
		{"omp PI_CONFIG_DIR", "linux", map[string]string{"PI_CONFIG_DIR": "/pc"}, "omp", "user", func(b *sbox) string { return filepath.Join("/pc", "agent", "skills", "weave") }},
		{"omp project", "linux", nil, "omp", "project", func(b *sbox) string { return filepath.Join(b.proj, ".omp", "skills", "weave") }},
		{"omp windows", "windows", nil, "omp", "user", func(b *sbox) string { return filepath.Join(b.home, ".omp", "agent", "skills", "weave") }},
		{"zed skill", "darwin", nil, "zed", "user", func(b *sbox) string { return filepath.Join(b.home, ".agents", "skills", "weave") }},
		{"codex universal", "darwin", nil, "codex", "user", func(b *sbox) string { return filepath.Join(b.home, ".agents", "skills", "weave") }},
		{"codex project", "linux", nil, "codex", "project", func(b *sbox) string { return filepath.Join(b.proj, ".agents", "skills", "weave") }},
		{"opencode", "linux", nil, "opencode", "user", func(b *sbox) string { return filepath.Join(b.home, ".agents", "skills", "weave") }},
		{"pi", "linux", map[string]string{"PI_CODING_AGENT_DIR": "/x/pi"}, "pi", "user", func(b *sbox) string { return filepath.Join(b.home, ".agents", "skills", "weave") }},
		{"cline", "linux", nil, "cline", "user", func(b *sbox) string { return filepath.Join(b.home, ".cline", "skills", "weave") }},
		{"cline project", "linux", nil, "cline", "project", func(b *sbox) string { return filepath.Join(b.proj, ".cline", "skills", "weave") }},
		{"antigravity", "linux", map[string]string{"GEMINI_CLI_HOME": "/g"}, "antigravity", "user", func(b *sbox) string { return filepath.Join("/g", ".gemini", "config", "skills", "weave") }},
		{"zed-agents-md linux xdg", "linux", map[string]string{"XDG_CONFIG_HOME": "/xdg"}, "zed-agents-md", "user", func(b *sbox) string { return filepath.Join("/xdg", "zed", "AGENTS.md") }},
		{"zed-agents-md linux default", "linux", nil, "zed-agents-md", "user", func(b *sbox) string { return filepath.Join(b.home, ".config", "zed", "AGENTS.md") }},
		{"zed-agents-md windows appdata", "windows", map[string]string{"APPDATA": "C:/Roaming"}, "zed-agents-md", "user", func(b *sbox) string { return filepath.Join("C:/Roaming", "Zed", "AGENTS.md") }},
		{"zed-agents-md project", "linux", nil, "zed-agents-md", "project", func(b *sbox) string { return filepath.Join(b.proj, "AGENTS.md") }},
		{"codex-agents-md CODEX_HOME", "linux", map[string]string{"CODEX_HOME": "/ch"}, "codex-agents-md", "user", func(b *sbox) string { return filepath.Join("/ch", "AGENTS.md") }},
		{"copilot-instructions COPILOT_HOME", "linux", map[string]string{"COPILOT_HOME": "/cp"}, "copilot-instructions", "user", func(b *sbox) string { return filepath.Join("/cp", "copilot-instructions.md") }},
		{"cursor-rules project", "linux", nil, "cursor-rules", "project", func(b *sbox) string { return filepath.Join(b.proj, ".cursor", "rules", "weave.mdc") }},
		{"gemini-md GEMINI_CLI_HOME", "linux", map[string]string{"GEMINI_CLI_HOME": "/gh"}, "gemini-md", "user", func(b *sbox) string { return filepath.Join("/gh", ".gemini", "GEMINI.md") }},
		{"opencode-agents-md xdg", "linux", map[string]string{"XDG_CONFIG_HOME": "/xdg"}, "opencode-agents-md", "user", func(b *sbox) string { return filepath.Join("/xdg", "opencode", "AGENTS.md") }},
		{"cline-rules user", "darwin", nil, "cline-rules", "user", func(b *sbox) string { return filepath.Join(b.home, "Documents", "Cline", "Rules", "weave.md") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b := newSbox(t)
			b.goos, b.env = c.goos, c.env
			r := b.senv().resolve(findTarget(c.tgt), c.scope, "")
			if r.Skip != "" {
				t.Fatalf("skipped: %s", r.Skip)
			}
			if want := c.want(b); r.Place.Path != want {
				t.Fatalf("path %q, want %q", r.Place.Path, want)
			}
		})
	}
	// scope availability
	b := newSbox(t)
	if r := b.senv().resolve(findTarget("cursor-rules"), "user", ""); r.Skip == "" {
		t.Fatal("cursor-rules has no user scope")
	}
	if r := b.senv().resolve(findTarget("continue"), "user", ""); r.Skip == "" {
		t.Fatal("continue has no user scope")
	}
}

func TestSkillRegistryInvariants(t *testing.T) {
	seen := map[string]bool{}
	for _, tg := range agentTargets {
		for _, n := range append([]string{tg.ID}, tg.Aliases...) {
			if seen[n] {
				t.Fatalf("duplicate target name %s", n)
			}
			seen[n] = true
		}
		if tg.Fmt == fmtOwnedMD && tg.ID != "dir" && tg.Project == "" && tg.User == nil {
			t.Fatalf("%s has no location", tg.ID)
		}
		if !tg.Verified && !strings.Contains(tg.Note, "") {
			t.Fatal("unreachable")
		}
	}
	for _, id := range []string{"claude", "codex", "opencode", "pi", "omp", "gemini", "zed", "agents-md", "dir"} {
		if findTarget(id) == nil {
			t.Fatalf("missing target %s", id)
		}
	}
}

func TestSkillDetection(t *testing.T) {
	b := newSbox(t)
	e := b.senv()
	if e.detected(findTarget("claude"), "user") {
		t.Fatal("nothing seeded yet")
	}
	b.mkdir(".claude")
	b.bins["opencode"] = true
	b.env["CODEX_HOME"] = filepath.Join(b.home, "alt-codex")
	b.mkdir("alt-codex")
	b.mkdir(".pi", "agent")
	for id, want := range map[string]bool{"claude": true, "opencode": true, "codex": true, "pi": true, "gemini": false, "cursor": false} {
		if got := e.detected(findTarget(id), "user"); got != want {
			t.Errorf("detected(%s)=%v want %v", id, got, want)
		}
	}
	// project scope also detects a tool by its project directory
	if err := os.MkdirAll(filepath.Join(b.proj, ".kiro"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !e.detected(findTarget("kiro"), "project") || e.detected(findTarget("kiro"), "user") {
		t.Fatal("project marker detection")
	}
}

func TestSkillAutoInstallCreatesExactFiles(t *testing.T) {
	b := newSbox(t)
	b.mkdir(".claude")
	b.mkdir(".config", "opencode") // reads ~/.agents/skills
	b.bins["codex"] = true         // also ~/.agents/skills
	b.mkdir(".cline")
	b.mkdir(".config", "zed") // reads ~/.agents/skills too
	code, rows, out := b.run("install")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	want := map[string]string{"claude": "created", "opencode": "created", "codex": "created", "cline": "created"}
	for tg, act := range want {
		r := rowFor(rows, tg)
		if r == nil || r["action"] != act {
			t.Errorf("%s: %v", tg, r)
		}
	}
	if r := rowFor(rows, "opencode"); r != nil && r["sharedWith"] != "codex" {
		t.Errorf("opencode sharedWith %v", r["sharedWith"])
	}
	skillFilesWant := append(sortedKeys(skillFiles()), manifestName)
	sort.Strings(skillFilesWant)
	for _, base := range []string{".claude/skills/weave", ".agents/skills/weave", ".cline/skills/weave"} {
		got := tree(t, filepath.Join(b.home, filepath.FromSlash(base)))
		if strings.Join(got, ",") != strings.Join(skillFilesWant, ",") {
			t.Errorf("%s files %v want %v", base, got, skillFilesWant)
		}
	}
	if r := rowFor(rows, "zed"); r == nil || r["sharedWith"] != "codex" {
		t.Errorf("zed row %v", r)
	}
	if sk := b.read(b.home, ".agents", "skills", "weave", "SKILL.md"); !strings.Contains(sk, "untrusted data") {
		t.Error("skill lost its security section")
	}
	// nothing outside the expected places
	all := tree(t, b.home)
	for _, f := range all {
		if !(strings.HasPrefix(f, ".claude/skills/weave/") || strings.HasPrefix(f, ".agents/skills/weave/") ||
			strings.HasPrefix(f, ".cline/skills/weave/")) {
			t.Errorf("unexpected file %s", f)
		}
	}
	// the project dir is untouched
	if got := tree(t, b.proj); len(got) != 0 {
		t.Errorf("project touched: %v", got)
	}
	// permissions: dirs 0700 for config dirs created by us, files 0644
	if runtimeUnix() {
		fi, _ := os.Stat(filepath.Join(b.home, ".agents"))
		if fi.Mode().Perm() != 0o700 {
			t.Errorf(".agents mode %v", fi.Mode().Perm())
		}
		fi, _ = os.Stat(filepath.Join(b.home, ".claude", "skills", "weave", "SKILL.md"))
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("SKILL.md mode %v", fi.Mode().Perm())
		}
	}
}

func runtimeUnix() bool { return filepath.Separator == '/' }

func TestSkillAutoFallbackAndUnverifiedHint(t *testing.T) {
	b := newSbox(t)
	code, rows, out := b.run("install")
	if code != 0 || len(rows) != 1 || rows[0]["target"] != "agents" || rows[0]["action"] != "created" {
		t.Fatalf("fallback: %d %s", code, out)
	}
	// omp detected: not written automatically, hint shown
	b2 := newSbox(t)
	agentTargets = append(agentTargets, &agentTarget{ID: "fake-unverified", Name: "Fake", Fmt: fmtSkillDir, Verified: false,
		User: homeJoin(".fake", "skills"), Project: ".fake/skills", Detect: homeDot(".fake")})
	t.Cleanup(func() { agentTargets = agentTargets[:len(agentTargets)-1] })
	b2.mkdir(".fake")
	b2.mkdir(".claude")
	code, rows, out = b2.run("install")
	if code != 0 {
		t.Fatal(out)
	}
	r := rowFor(rows, "fake-unverified")
	if r == nil || r["action"] != "skipped" || !strings.Contains(r["reason"].(string), "--target fake-unverified") {
		t.Fatalf("omp hint: %v", r)
	}
	if _, err := os.Stat(filepath.Join(b2.home, ".fake", "skills")); err == nil {
		t.Fatal("unverified target must not be written automatically")
	}
	// explicit opt-in writes it
	code, rows, out = b2.run("install", "--target", "fake-unverified")
	if code != 0 || rowFor(rows, "fake-unverified")["action"] != "created" {
		t.Fatalf("omp opt-in: %s", out)
	}
	if _, err := os.Stat(filepath.Join(b2.home, ".fake", "skills", "weave", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
}

func TestSkillAllVersusAuto(t *testing.T) {
	b := newSbox(t)
	b.mkdir(".claude")
	_, auto, _ := b.run("install", "--dry-run")
	if len(auto) != 1 {
		t.Fatalf("auto should only hit detected tools: %v", auto)
	}
	code, all, out := b.run("install", "--target", "all", "--dry-run")
	if code != 0 {
		t.Fatal(out)
	}
	for _, id := range []string{"claude", "codex", "gemini", "pi", "cline", "kiro", "droid", "zed", "omp", "warp", "agents"} {
		if rowFor(all, id) == nil {
			t.Errorf("--target all missing %s", id)
		}
	}
	for _, id := range []string{"zed-agents-md", "cursor-rules", "agents-md", "aider", "dir"} {
		if rowFor(all, id) != nil {
			t.Errorf("--target all must not include opt-in/unverified %s", id)
		}
	}
}

func TestSkillUnknownTarget(t *testing.T) {
	b := newSbox(t)
	code, _, out := b.run("install", "--target", "claude,nope")
	if code != ExitUsage || !strings.Contains(out, "unknown --target nope") || !strings.Contains(out, "opencode") || !strings.Contains(out, "omp") {
		t.Fatalf("%d %s", code, out)
	}
	if code, _, _ := b.run("install", "--scope", "galaxy"); code != ExitUsage {
		t.Fatal("bad scope")
	}
	if code, _, _ := b.run("install", "--target", "dir"); code != ExitUsage {
		t.Fatal("dir needs --dir")
	}
}

func TestSkillIdempotentAndUserContentPreserved(t *testing.T) {
	b := newSbox(t)
	zedFile := filepath.Join(b.home, ".config", "zed", "AGENTS.md")
	if err := os.MkdirAll(filepath.Dir(zedFile), 0o755); err != nil {
		t.Fatal(err)
	}
	user := "# My rules\n\nbe nice\n"
	if err := os.WriteFile(zedFile, []byte(user), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, rows, out := b.run("install", "--target", "zed-agents-md"); code != 0 || rows[0]["action"] != "updated" {
		t.Fatalf("%d %s", code, out)
	}
	first := b.read(zedFile)
	if !strings.HasPrefix(first, user) {
		t.Fatalf("user content must stay first:\n%s", first)
	}
	if _, rows, _ := b.run("install", "--target", "zed-agents-md"); rows[0]["action"] != "unchanged" {
		t.Fatalf("reinstall: %v", rows)
	}
	if b.read(zedFile) != first || strings.Count(first, blockBeginPrefix) != 1 {
		t.Fatal("not idempotent")
	}
	// user adds text after the block; a forced re-render keeps it
	if err := os.WriteFile(zedFile, []byte(first+"\n## Mine\nkeep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, rows, _ := b.run("install", "--target", "zed-agents-md"); rows[0]["action"] != "unchanged" {
		t.Fatal("text outside the block must not matter")
	}
	// uninstall removes only the block
	if code, rows, _ := b.run("uninstall", "--target", "zed-agents-md"); code != 0 || rows[0]["action"] != "removed" {
		t.Fatal("uninstall")
	}
	left := b.read(zedFile)
	if strings.Contains(left, "weave-skill") || !strings.Contains(left, "be nice") || !strings.Contains(left, "## Mine") {
		t.Fatalf("uninstall damaged user text:\n%s", left)
	}
	// a file that only held our block disappears
	other := filepath.Join(b.home, ".gemini", "GEMINI.md")
	b.run("install", "--target", "gemini-md")
	if _, err := os.Stat(other); err != nil {
		t.Fatal("gemini-md not written")
	}
	b.run("uninstall", "--target", "gemini-md")
	if _, err := os.Stat(other); err == nil {
		t.Fatal("empty file should be removed")
	}
}

func TestSkillModifiedProtectionAndStale(t *testing.T) {
	b := newSbox(t)
	b.mkdir(".claude")
	b.run("install")
	skillMD := filepath.Join(b.home, ".claude", "skills", "weave", "SKILL.md")
	orig := b.read(skillMD)
	// simulate an older release: our content, recorded in the manifest, but different
	old := skillFiles()
	old["SKILL.md"] = []byte("old version\n")
	dir := filepath.Join(b.home, ".claude", "skills", "weave")
	if err := writeDir(dir, old, 0o644, "0.0.1"); err != nil {
		t.Fatal(err)
	}
	if code, rows, _ := b.run("check"); code != ExitError || rows[0]["state"] != stStale {
		t.Fatalf("stale must fail check: %d %v", code, rows)
	}
	// stale + unmodified: install updates without --force
	if code, rows, out := b.run("install"); code != 0 || rows[0]["action"] != "updated" {
		t.Fatalf("stale update: %d %s", code, out)
	}
	if b.read(skillMD) != orig {
		t.Fatal("not restored")
	}
	// local edit: protected without --force
	if err := os.WriteFile(skillMD, []byte("my edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, rows, _ := b.run("check"); code != ExitError || rows[0]["state"] != stModified {
		t.Fatalf("modified: %d %v", code, rows)
	}
	code, rows, _ := b.run("install")
	if code != ExitConflict || rows[0]["action"] != "conflict" || b.read(skillMD) != "my edit" {
		t.Fatalf("must refuse: %d %v", code, rows)
	}
	// uninstall keeps the edited file
	code, rows, _ = b.run("uninstall")
	if code != ExitConflict || rows[0]["action"] != "kept-modified" || b.read(skillMD) != "my edit" {
		t.Fatalf("uninstall kept: %d %v", code, rows)
	}
	if code, rows, _ := b.run("install", "--force"); code != 0 || rows[0]["action"] != "updated" || b.read(skillMD) != orig {
		t.Fatalf("force: %v", rows)
	}
	// a block edited by hand is protected the same way
	b.mkdir(".config", "zed")
	b.run("install", "--target", "zed-agents-md")
	zedFile := filepath.Join(b.home, ".config", "zed", "AGENTS.md")
	edited := strings.Replace(b.read(zedFile), "Golden rules", "Silver rules", 1)
	if err := os.WriteFile(zedFile, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := b.run("install", "--target", "zed-agents-md"); code != ExitConflict {
		t.Fatal("edited block must need --force")
	}
	if code, _, _ := b.run("install", "--target", "zed-agents-md", "--force"); code != 0 || !strings.Contains(b.read(zedFile), "Golden rules") {
		t.Fatal("force restores block")
	}
}

func TestSkillUninstallRemovesOnlyOurs(t *testing.T) {
	b := newSbox(t)
	b.mkdir(".claude")
	b.run("install")
	dir := filepath.Join(b.home, ".claude", "skills", "weave")
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(b.home, ".claude", "skills", "other-skill", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, rows, out := b.run("uninstall"); code != 0 || rows[0]["action"] != "removed" {
		t.Fatalf("%d %s", code, out)
	}
	if got := tree(t, dir); len(got) != 1 || got[0] != "notes.txt" {
		t.Fatalf("left %v", got)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("foreign skill removed")
	}
	// nothing of ours: reported, not an error
	if code, rows, _ := b.run("uninstall", "--target", "claude"); code != 0 || rows[0]["action"] != "not-installed" {
		t.Fatalf("second uninstall: %v", rows)
	}
	// a pre-existing unrelated "weave" folder is not ours
	b2 := newSbox(t)
	foreign := filepath.Join(b2.home, ".claude", "skills", "weave", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(foreign, []byte("someone else's"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := b2.run("install", "--target", "claude"); code != ExitConflict {
		t.Fatal("foreign folder must need --force")
	}
	if _, rows, _ := b2.run("uninstall", "--target", "claude"); rows[0]["action"] != "not-installed" || b2.read(foreign) != "someone else's" {
		t.Fatalf("foreign uninstall: %v", rows)
	}
}

func TestSkillDryRunWritesNothing(t *testing.T) {
	b := newSbox(t)
	b.mkdir(".claude")
	b.mkdir(".config", "zed")
	before := tree(t, b.home)
	code, rows, out := b.run("install", "--dry-run")
	if code != 0 || len(rows) != 2 {
		t.Fatalf("%d %s", code, out)
	}
	for _, r := range rows {
		if !strings.HasPrefix(r["action"].(string), "would-") {
			t.Errorf("action %v", r["action"])
		}
	}
	if after := tree(t, b.home); strings.Join(after, ",") != strings.Join(before, ",") {
		t.Fatalf("dry run wrote: %v", after)
	}
	b.run("install")
	if code, rows, _ := b.run("uninstall", "--dry-run"); code != 0 || rows[0]["action"] != "would-remove" {
		t.Fatalf("uninstall dry-run: %v", rows)
	}
	if _, err := os.Stat(filepath.Join(b.home, ".claude", "skills", "weave", "SKILL.md")); err != nil {
		t.Fatal("dry-run uninstall removed files")
	}
}

func TestSkillProjectScope(t *testing.T) {
	b := newSbox(t)
	sub := filepath.Join(b.proj, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	e := b.senv()
	e.Cwd = sub
	if e.projectRoot() != b.proj {
		t.Fatalf("project root %s", e.projectRoot())
	}
	code, rows, out := b.run("install", "--scope", "project", "--target", "claude,codex,cursor-rules,aider,zed-agents-md")
	if code != 0 {
		t.Fatal(out)
	}
	for _, id := range []string{"claude", "codex", "cursor-rules", "aider", "zed-agents-md"} {
		if rowFor(rows, id) == nil {
			t.Fatalf("no row %s", id)
		}
	}
	got := tree(t, b.proj)
	for _, f := range got {
		if strings.HasPrefix(f, ".git/") {
			continue
		}
		ok := strings.HasPrefix(f, ".claude/skills/weave/") || strings.HasPrefix(f, ".agents/skills/weave/") ||
			f == ".cursor/rules/weave.mdc" || f == "CONVENTIONS.md" || f == "AGENTS.md"
		if !ok {
			t.Errorf("unexpected project file %s", f)
		}
	}
	if tree(t, b.home) != nil && len(tree(t, b.home)) != 0 {
		t.Fatalf("user home touched by project install: %v", tree(t, b.home))
	}
	mdc := b.read(b.proj, ".cursor", "rules", "weave.mdc")
	if !strings.HasPrefix(mdc, "---\ndescription: ") || !strings.Contains(mdc, "alwaysApply: false") || !strings.Contains(mdc, blockBeginPrefix) {
		t.Fatalf("mdc:\n%s", mdc[:300])
	}
	if strings.Contains(mdc[:strings.Index(mdc, blockBeginPrefix)], "weave-skill") {
		t.Fatal("frontmatter must precede the marker")
	}
	if r := rowFor(rows, "aider"); !strings.Contains(r["note"].(string), "read: CONVENTIONS.md") {
		t.Fatalf("aider note: %v", r["note"])
	}
	// owned file: idempotent, and uninstall deletes it
	if _, rows, _ := b.run("install", "--scope", "project", "--target", "cursor-rules"); rows[0]["action"] != "unchanged" {
		t.Fatalf("mdc idempotent: %v", rows)
	}
	b.run("uninstall", "--scope", "project", "--target", "cursor-rules")
	if _, err := os.Stat(filepath.Join(b.proj, ".cursor", "rules", "weave.mdc")); err == nil {
		t.Fatal("mdc should be removed")
	}
	// cursor-rules has no user scope
	if _, rows, _ := b.run("install", "--target", "cursor-rules"); rows[0]["action"] != "skipped" {
		t.Fatalf("user scope skip: %v", rows)
	}
}

func TestSkillOwnedFileProtection(t *testing.T) {
	b := newSbox(t)
	f := filepath.Join(b.proj, ".cursor", "rules", "weave.mdc")
	if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f, []byte("my own weave rule\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := b.run("install", "--scope", "project", "--target", "cursor-rules"); code != ExitConflict {
		t.Fatal("must not overwrite a foreign file")
	}
	if b.read(f) != "my own weave rule\n" {
		t.Fatal("clobbered")
	}
	if code, _, _ := b.run("install", "--scope", "project", "--target", "cursor-rules", "--force"); code != 0 {
		t.Fatal("force")
	}
}

func TestSkillLegacyBlockIsUpgraded(t *testing.T) {
	b := newSbox(t)
	f := filepath.Join(b.proj, "AGENTS.md")
	old := "# Repo\n\n" + legacyBegin + "\n## Weave kanban (CLI skill)\nold text\n" + legacyEnd + "\n\nafter\n"
	if err := os.WriteFile(f, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, rows, _ := b.run("check", "--scope", "project", "--target", "agents-md"); code != ExitError || rows[0]["state"] != stStale {
		t.Fatalf("legacy must be stale: %v", rows)
	}
	if code, _, out := b.run("install", "--scope", "project", "--target", "agents-md"); code != 0 {
		t.Fatal(out)
	}
	got := b.read(f)
	if strings.Contains(got, "old text") || strings.Contains(got, legacyBegin) || !strings.Contains(got, "# Repo") || !strings.Contains(got, "after") {
		t.Fatalf("legacy upgrade:\n%s", got)
	}
}

func TestSkillCustomDirAndLegacyDirFlag(t *testing.T) {
	b := newSbox(t)
	d := filepath.Join(t.TempDir(), "custom")
	if code, _, out := b.run("install", "--target", "dir", "--dir", d); code != 0 {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(d, "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	d2 := t.TempDir()
	if code, _, out := b.run("install", "--dir", d2); code != 0 {
		t.Fatal(out)
	}
	if _, err := os.Stat(filepath.Join(d2, "weave", "SKILL.md")); err != nil {
		t.Fatal("--dir alone keeps meaning <dir>/weave")
	}
	if code, _, _ := b.run("install", "--target", "claude,codex", "--dir", d2); code != ExitUsage {
		t.Fatal("--dir with several targets")
	}
}

func TestSkillRefusesSymlinkLeaf(t *testing.T) {
	if !runtimeUnix() {
		t.Skip("symlinks")
	}
	b := newSbox(t)
	outside := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	skills := filepath.Join(b.home, ".claude", "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(skills, "weave")); err != nil {
		t.Fatal(err)
	}
	code, rows, _ := b.run("install", "--target", "claude")
	if code != ExitConflict || rows[0]["action"] != "conflict" {
		t.Fatalf("symlinked skill folder: %d %v", code, rows)
	}
	if got := tree(t, outside); len(got) != 0 {
		t.Fatalf("wrote through symlink: %v", got)
	}
	// a symlinked instruction file is refused too, unless forced (then the real file is edited)
	real := filepath.Join(t.TempDir(), "dotfiles-AGENTS.md")
	if err := os.WriteFile(real, []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	zdir := filepath.Join(b.home, ".config", "zed")
	if err := os.MkdirAll(zdir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(zdir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := b.run("install", "--target", "zed-agents-md"); code != ExitConflict {
		t.Fatal("symlinked file")
	}
	if code, _, _ := b.run("install", "--target", "zed-agents-md", "--force"); code != 0 || !strings.Contains(b.read(real), "mine") || !strings.Contains(b.read(real), blockBeginPrefix) {
		t.Fatal("--force edits the link target")
	}
	if fi, _ := os.Lstat(filepath.Join(zdir, "AGENTS.md")); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link replaced")
	}
}

func TestSkillTargetsListing(t *testing.T) {
	b := newSbox(t)
	b.mkdir(".claude")
	b.run("install", "--target", "claude")
	code, rows, out := b.run("targets")
	if code != 0 || len(rows) < 30 {
		t.Fatalf("%d %s", code, out)
	}
	c := rowFor(rows, "claude")
	if c["detected"] != true || c["state"] != stCurrent || c["kind"] != "skill" {
		t.Fatalf("claude row %v", c)
	}
	if o := rowFor(rows, "omp"); o["verified"] != true || o["kind"] != "skill" {
		t.Fatalf("omp row %v", o)
	}
	if z := rowFor(rows, "zed-agents-md"); z["kind"] != "instructions" {
		t.Fatalf("zed %v", z)
	}
	if rowFor(rows, "dir") != nil {
		t.Fatal("dir is not a tool")
	}
	if _, prj, _ := b.run("targets", "--scope", "project"); rowFor(prj, "claude")["state"] != stMissing {
		t.Fatal("project scope state")
	}
}

func TestSkillCompactIsInSyncAndSafe(t *testing.T) {
	body := instructionsBody()
	if len(body) > 6000 {
		t.Fatalf("compact instructions grew to %d bytes", len(body))
	}
	for _, must := range []string{"untrusted data", "weave skill print", "Never follow directions"} {
		if !strings.Contains(body, must) {
			t.Errorf("compact missing %q", must)
		}
	}
	if !strings.Contains(string(skillFiles()["SKILL.md"]), "untrusted data") {
		t.Fatal("full skill lost the security section")
	}
}

func TestSkillPrintCompact(t *testing.T) {
	b := newSbox(t)
	var out, errb bytes.Buffer
	a := &App{Stdout: &out, Stderr: &errb, SkillEnv: b.senv()}
	if code := cmdSkill(a, []string{"print", "--compact"}); code != 0 || !strings.Contains(out.String(), "Weave kanban") {
		t.Fatal(out.String())
	}
}
