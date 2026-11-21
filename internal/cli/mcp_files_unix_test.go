//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func within5s(t *testing.T, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("blocked (a FIFO open must never hang the server)")
		return nil
	}
}

func TestGuardFIFO(t *testing.T) {
	b := newGbox(t)
	g := b.guard()
	p := filepath.Join(b.proj, "pipe")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skip("mkfifo:", err)
	}
	err := within5s(t, func() error { _, err := g.openUpload("pipe", 1<<20); return err })
	wantRefused(t, err, "validation", "not a regular file")
}

// A regular file swapped for a FIFO between the checks and the open is refused without blocking.
func TestGuardFIFOSwapRace(t *testing.T) {
	b := newGbox(t)
	g := b.guard()
	p := b.write(filepath.Join(b.proj, "swap.txt"), "regular")
	g.afterStat = func() {
		_ = os.Remove(p)
		_ = syscall.Mkfifo(p, 0o600)
	}
	err := within5s(t, func() error { _, err := g.openUpload("swap.txt", 1<<20); return err })
	wantRefused(t, err, "forbidden", "changed while it was being opened")
}
