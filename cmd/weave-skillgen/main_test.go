package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/cli/opsbridge"
	"github.com/crueber/weave/internal/skillgen"
)

// TestGeneratedSkillAndSnapshotAreFresh fails when the op registry (or the hand-written skill
// sources) changed without regenerating skills/weave and internal/cli/snapshot.json.
func TestGeneratedSkillAndSnapshotAreFresh(t *testing.T) {
	root := filepath.Join("..", "..")
	files := skillgen.Render(opsbridge.FromRegistry())
	var stale []string
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		if err != nil || !bytes.Equal(got, want) {
			stale = append(stale, name)
		}
	}
	// files in skills/weave that the generator no longer produces are stale too
	_ = filepath.WalkDir(filepath.Join(root, "skills", "weave"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			if _, ok := files[filepath.ToSlash(rel)]; !ok {
				stale = append(stale, filepath.ToSlash(rel)+" (orphan)")
			}
		}
		return nil
	})
	if len(stale) > 0 {
		sort.Strings(stale)
		t.Fatalf("generated files are stale: %s\n\nAn op was added or changed (or skill sources were edited) without regenerating.\nRun `make skill` and commit the result.", strings.Join(stale, ", "))
	}
}

// TestSlimCLIDoesNotLinkServer guards the slim release binary: no SQLite, db, ops registry or server.
func TestSlimCLIDoesNotLinkServer(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "../weave-cli")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, bad := range []string{"modernc.org/sqlite", "weave/internal/db", "weave/internal/ops", "weave/internal/server", "weave/internal/domain", "net/http/httptest"} {
		if strings.Contains(string(out), bad) {
			t.Errorf("slim CLI links %s; keep internal/cli free of server packages", bad)
		}
	}
}
