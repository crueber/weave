package authz_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The project-owner role is gone (migration 0103): no code, template, script, config sample, compose file or generated
// skill may mention it again. Hand-written docs (*.md outside skills/) may, to explain the removal; migrations keep
// their history; the listed tests exercise the upgrade and the ignored legacy IdP group on purpose.
func TestProjectOwnerRoleRemoved(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skip("repository root not found")
	}
	re := regexp.MustCompile(`projectOwner|OWNER_GROUP|ownerGroup|weave-project-owners`)
	allowed := map[string]bool{
		"internal/authz/roles_removed_test.go":                    true,
		"internal/db/migrate_roles_test.go":                       true, // upgrades a projectOwner account
		"internal/domain/auth/auth_test.go":                       true, // the legacy IdP group grants nothing
		"internal/db/migrations/0001_core.sql":                    true, // never edited
		"internal/db/migrations/0103_drop_project_owner_role.sql": true,
	}
	var hits []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch d.Name() {
			case ".git", ".claude", "node_modules", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if allowed[rel] || (strings.HasSuffix(rel, ".md") && !strings.HasPrefix(rel, "skills/")) {
			return nil
		}
		switch strings.ToLower(filepath.Ext(rel)) {
		case ".png", ".webp", ".webm", ".jpg", ".gif", ".ico", ".woff2", ".db":
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil || len(b) > 4<<20 {
			return nil
		}
		if loc := re.FindIndex(b); loc != nil {
			hits = append(hits, rel+": "+string(b[loc[0]:loc[1]]))
		}
		return nil
	})
	if len(hits) > 0 {
		t.Fatalf("the removed project-owner role is still referenced:\n%s", strings.Join(hits, "\n"))
	}
}
