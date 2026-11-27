package admin_test

import (
	"io"
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
	if bk.Reader == nil {
		t.Fatal("backup did not return a streaming Reader")
	}
	data, err := io.ReadAll(bk.Reader)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if err := bk.Reader.Close(); err != nil {
		t.Fatalf("close backup: %v", err)
	}
	if len(data) < 100 || string(data[:6]) != "SQLite" {
		t.Fatalf("backup is not an sqlite file (%d bytes)", len(data))
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
