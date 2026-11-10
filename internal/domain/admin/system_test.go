package admin_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/crueber/weave/internal/config"
	_ "github.com/crueber/weave/internal/domain/admin"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

func call(t *testing.T, e *ops.Env, name string) any {
	t.Helper()
	v, err := ops.Call(e, name, reflect.New(ops.Get(name).In).Interface())
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

func TestSystemOps(t *testing.T) {
	admin := &ops.Principal{UserID: "a", Role: "admin"}
	e := testutil.Env(t, admin)
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, "files"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "files", "x"), make([]byte, 100), 0o644)
	e.Svc["config"] = config.Config{DataDir: dir}

	call(t, e, "system.health")
	call(t, e, "system.version")
	st := call(t, e, "system.storage")
	if reflect.ValueOf(st).FieldByName("FilesBytes").Int() != 100 {
		t.Fatalf("storage = %+v", st)
	}
	call(t, e, "admin.stats")

	bk := call(t, e, "admin.backup").(*ops.Stream)
	if len(bk.Data) < 100 || string(bk.Data[:6]) != "SQLite" {
		t.Fatalf("backup is not an sqlite file (%d bytes)", len(bk.Data))
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Fatalf("backup temp dir not cleaned: %v", ents)
	}

	// non-admin is rejected
	u := testutil.As(e, &ops.Principal{UserID: "u", Role: "boardUser"})
	if _, err := ops.Call(u, "admin.backup", &admin_BackupIn); err == nil {
		t.Fatal("non-admin allowed")
	}
}

var admin_BackupIn = struct{}{}
