// Package testutil builds an in-memory-ish Env for op tests.
package testutil

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

// Env returns an Env on a fresh temp SQLite DB, with the given principal (nil = anonymous).
func Env(t *testing.T, u *ops.Principal) *ops.Env {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return &ops.Env{Ctx: context.Background(), DB: d, User: u, Hub: realtime.New(), Svc: map[string]any{}}
}

// As returns a copy of e acting as another principal.
func As(e *ops.Env, u *ops.Principal) *ops.Env { c := *e; c.User = u; return &c }
