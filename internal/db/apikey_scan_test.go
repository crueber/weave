package db

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// API keys live in the api_keys table (migration 0901). The four one-key-per-user columns on users stay in the schema (SQLite
// cannot cheaply drop them) but nothing may read or write them any more: a reader of the old columns would silently see
// "no key" or a stale one. This scan fails on any non-test Go source or HTML template that names them. Migrations (.sql) and
// tests (which seed the old schema on purpose) are exempt.
var legacyKeyColumns = regexp.MustCompile(`api_key_hash|api_key_prefix|api_key_created_at|api_key_expires_at`)

func TestNoLegacyAPIKeyColumns(t *testing.T) {
	if !legacyKeyColumns.MatchString("SELECT api_key_hash FROM users") || legacyKeyColumns.MatchString("SELECT key_hash FROM api_keys") {
		t.Fatal("detector is wrong")
	}
	root := filepath.Join("..", "..")
	var bad []string
	for _, dir := range []string{"internal", "cmd"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			n := d.Name()
			if !(strings.HasSuffix(n, ".go") || strings.HasSuffix(n, ".html")) || strings.HasSuffix(n, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err == nil && legacyKeyColumns.Match(b) {
				bad = append(bad, filepath.ToSlash(p))
			}
			return nil
		})
	}
	if len(bad) > 0 {
		t.Fatalf("these files use the legacy users.api_key_* columns (use the api_keys table): %v", bad)
	}
}
