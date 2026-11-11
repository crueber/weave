package admin

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/crueber/weave/internal/config"
	_ "github.com/crueber/weave/internal/i18n" // registers the `t` template func
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// Version is stamped at build time via -ldflags.
var Version = "dev"

type NoInput struct{}
type VersionOut struct {
	Version string `json:"version"`
}

type StorageOut struct {
	DBBytes    int64 `json:"dbBytes" desc:"SQLite database + WAL size"`
	FilesBytes int64 `json:"filesBytes" desc:"attachments, avatars, backgrounds"`
	TotalBytes int64 `json:"totalBytes"`
}

type HealthOut struct {
	Status  string `json:"status"`
	Version string `json:"version"`
	DB      string `json:"db"`
	Time    string `json:"time"`
}

type StatsOut struct {
	Counts  map[string]int64 `json:"counts"`
	Storage StorageOut       `json:"storage"`
	Version string           `json:"version"`
}

type BackupIn struct{}

func dataDir(e *ops.Env) string {
	if c, ok := e.Svc["config"].(config.Config); ok && c.DataDir != "" {
		return c.DataDir
	}
	return ""
}

func dirSize(root string) int64 {
	var n int64
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if i, err := d.Info(); err == nil {
			n += i.Size()
		}
		return nil
	})
	return n
}

func storage(e *ops.Env) StorageOut {
	var s StorageOut
	dir := dataDir(e)
	if dir == "" {
		return s
	}
	s.FilesBytes = dirSize(filepath.Join(dir, "files"))
	s.TotalBytes = dirSize(dir)
	s.DBBytes = s.TotalBytes - s.FilesBytes
	return s
}

var statTables = []string{"users", "projects", "boards", "lists", "cards", "comments", "attachments", "webhooks", "notification_services", "actions"}

func init() {
	ops.Register(ops.Op{Name: "system.version", Summary: "Server version", Method: "GET", Path: "/version", Auth: ops.AuthPublic, UI: "exempt:footer"},
		func(e *ops.Env, _ NoInput) (VersionOut, error) { return VersionOut{Version}, nil })

	ops.Register(ops.Op{Name: "system.health", Summary: "Liveness/readiness (checks the database)", Method: "GET", Path: "/health", Auth: ops.AuthPublic, UI: "exempt:healthcheck endpoint (/healthz)"},
		func(e *ops.Env, _ NoInput) (HealthOut, error) {
			ctx, cancel := context.WithTimeout(e.Ctx, 3*time.Second)
			defer cancel()
			h := HealthOut{Status: "ok", Version: Version, DB: "ok", Time: time.Now().UTC().Format(time.RFC3339)}
			if err := e.DB.PingContext(ctx); err != nil {
				h.Status, h.DB = "degraded", err.Error()
			}
			return h, nil
		})

	ops.Register(ops.Op{Name: "system.storage", Summary: "Disk usage of the data directory (admin)", Method: "GET", Path: "/system/storage", Auth: ops.AuthAdmin, UI: "GET /admin"},
		func(e *ops.Env, _ NoInput) (StorageOut, error) { return storage(e), nil })

	ops.Register(ops.Op{Name: "admin.stats", Summary: "Row counts per entity, storage and version (admin)", Method: "GET", Path: "/admin/stats", Auth: ops.AuthAdmin, UI: "GET /admin"},
		func(e *ops.Env, _ NoInput) (StatsOut, error) {
			out := StatsOut{Counts: map[string]int64{}, Storage: storage(e), Version: Version}
			for _, t := range statTables {
				var n int64
				if err := e.DB.QueryRowContext(e.Ctx, "SELECT COUNT(*) FROM "+t).Scan(&n); err == nil {
					out.Counts[t] = n
				}
			}
			return out, nil
		})

	ops.Register(ops.Op{Name: "admin.backup", Summary: "Download a consistent SQLite snapshot (VACUUM INTO). Files under data/files must be copied separately.", Method: "GET", Path: "/admin/backup", Auth: ops.AuthAdmin, Binary: true, UI: "GET /admin/backup"},
		func(e *ops.Env, _ BackupIn) (*ops.Stream, error) {
			tmp, err := os.MkdirTemp(dataDir(e), ".backup-")
			if err != nil {
				return nil, err
			}
			defer os.RemoveAll(tmp)
			p := filepath.Join(tmp, "snapshot.db")
			if _, err := e.DB.ExecContext(e.Ctx, "VACUUM INTO ?", p); err != nil {
				return nil, fmt.Errorf("backup: %w", err)
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return nil, err
			}
			return &ops.Stream{Name: "weave-" + time.Now().UTC().Format("20060102-150405") + ".db", ContentType: "application/vnd.sqlite3", Data: b}, nil
		})

	web.Route("GET /admin", func(c *web.Ctx) error {
		st, err := ops.Typed[StatsOut](c.Env, "admin.stats", &NoInput{})
		if err != nil {
			return err
		}
		return c.Render("admin_index.html", st)
	})
	web.Route("GET /admin/backup", func(c *web.Ctx) error {
		v, err := ops.Call(c.Env, "admin.backup", &BackupIn{})
		if err != nil {
			return err
		}
		st := v.(*ops.Stream)
		c.W.Header().Set("Content-Type", st.ContentType)
		c.W.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", st.Name))
		_, err = c.W.Write(st.Data)
		return err
	})
}
