//go:build windows

package cli

import (
	"path/filepath"
	"testing"
)

// The real Windows guard: UNC, device, ADS and reserved names are refused before any file access.
func TestGuardWindowsPaths(t *testing.T) {
	b := newGbox(t)
	g := b.guard()
	for _, p := range []string{`\\server\share\f.txt`, `\\?\` + filepath.Join(b.proj, "a.txt"), `\\.\PhysicalDrive0`,
		filepath.Join(b.proj, "a.txt") + ":stream", filepath.Join(b.proj, "CON"), filepath.Join(b.proj, "nul.txt"), filepath.Join(b.proj, "COM1")} {
		_, err := g.openUpload(p, 1<<20)
		wantRefused(t, err, "forbidden", "not allowed")
	}
	if !g.within(`C:\Users\A\proj`, `c:\users\a\PROJ\x.txt`) {
		t.Fatal("windows comparisons are case-insensitive")
	}
}
