package admin

import (
	"context"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/config"
	_ "github.com/crueber/weave/internal/i18n" // registers the `t` template func
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/version"
	"github.com/crueber/weave/internal/web"
)

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
	switch c := e.Svc["config"].(type) {
	case config.Config:
		return c.DataDir
	case *config.Config:
		if c != nil {
			return c.DataDir
		}
	}
	return ""
}

func storage(e *ops.Env) StorageOut {
	var s StorageOut
	dir := dataDir(e)
	if dir == "" {
		return s
	}
	filesDir := filepath.Join(dir, "files")
	filesPrefix := filesDir + string(os.PathSeparator)
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".backup-") {
				return filepath.SkipDir
			}
			return nil
		}
		if i, err := d.Info(); err == nil {
			sz := i.Size()
			s.TotalBytes += sz
			if strings.HasPrefix(p, filesPrefix) {
				s.FilesBytes += sz
			}
		}
		return nil
	})
	s.DBBytes = s.TotalBytes - s.FilesBytes
	return s
}

type cleanupReadCloser struct {
	f   *os.File
	dir string
}

func (c *cleanupReadCloser) Read(p []byte) (int, error) { return c.f.Read(p) }

func (c *cleanupReadCloser) Close() error {
	err := c.f.Close()
	_ = os.RemoveAll(c.dir)
	return err
}

func adminOnly(h func(*web.Ctx) error) func(*web.Ctx) error {
	return func(c *web.Ctx) error {
		if !authz.SystemAdmin(c.Env) {
			return ops.NotFound("page")
		}
		return h(c)
	}
}

// humanBytes renders a size for the admin overview ("12.4 MB").
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 4; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

func init() {
	web.AddFuncs(template.FuncMap{"adminBytes": humanBytes, "adminLabel": func(s string) string { return strings.ReplaceAll(s, "_", " ") }})
	ops.Register(ops.Op{Name: "system.version", Summary: "Server version", Method: "GET", Path: "/version", Auth: ops.AuthPublic, UI: "exempt:footer"},
		func(e *ops.Env, _ NoInput) (VersionOut, error) { return VersionOut{version.Version}, nil })

	ops.Register(ops.Op{Name: "system.health", Summary: "Liveness/readiness (checks the database)", Method: "GET", Path: "/health", Auth: ops.AuthPublic, UI: "exempt:healthcheck endpoint (/healthz)"},
		func(e *ops.Env, _ NoInput) (HealthOut, error) {
			ctx, cancel := context.WithTimeout(e.Ctx, 3*time.Second)
			defer cancel()
			h := HealthOut{Status: "ok", Version: version.Version, DB: "ok", Time: time.Now().UTC().Format(time.RFC3339)}
			if err := e.DB.PingContext(ctx); err != nil {
				log.Printf("admin: system.health db ping failed: %v", err)
				h.Status, h.DB = "degraded", "unavailable"
			}
			return h, nil
		})

	ops.Register(ops.Op{Name: "system.storage", Summary: "Disk usage of the data directory (admin)", Method: "GET", Path: "/system/storage", Auth: ops.AuthAdmin, UI: "GET /admin"},
		func(e *ops.Env, _ NoInput) (StorageOut, error) { return storage(e), nil })

	ops.Register(ops.Op{Name: "admin.stats", Summary: "Row counts per entity, storage and version (admin)", Method: "GET", Path: "/admin/stats", Auth: ops.AuthAdmin, UI: "GET /admin"},
		func(e *ops.Env, _ NoInput) (StatsOut, error) {
			var users, projects, boards, lists, cards, comments, attachments, webhooks, notifyServices, actions int64
			err := e.DB.QueryRowContext(e.Ctx, `SELECT
				(SELECT COUNT(*) FROM users),
				(SELECT COUNT(*) FROM projects),
				(SELECT COUNT(*) FROM boards),
				(SELECT COUNT(*) FROM lists),
				(SELECT COUNT(*) FROM cards),
				(SELECT COUNT(*) FROM comments),
				(SELECT COUNT(*) FROM attachments),
				(SELECT COUNT(*) FROM webhooks),
				(SELECT COUNT(*) FROM notification_services),
				(SELECT COUNT(*) FROM actions)`).Scan(
				&users, &projects, &boards, &lists, &cards, &comments, &attachments, &webhooks, &notifyServices, &actions)
			if err != nil {
				return StatsOut{}, err
			}
			return StatsOut{
				Counts: map[string]int64{
					"users": users, "projects": projects, "boards": boards, "lists": lists, "cards": cards,
					"comments": comments, "attachments": attachments, "webhooks": webhooks,
					"notification_services": notifyServices, "actions": actions,
				},
				Storage: storage(e),
				Version: version.Version,
			}, nil
		})

	ops.Register(ops.Op{Name: "admin.backup", Summary: "Download a consistent SQLite snapshot (VACUUM INTO). Files under data/files must be copied separately.", Method: "GET", Path: "/admin/backup", Auth: ops.AuthAdmin, Binary: true, UI: "GET /admin/backup"},
		func(e *ops.Env, _ BackupIn) (*ops.Stream, error) {
			dir := dataDir(e)
			if dir == "" {
				return nil, fmt.Errorf("backup: data directory is not configured")
			}
			tmp, err := os.MkdirTemp(dir, ".backup-")
			if err != nil {
				return nil, err
			}
			p := filepath.Join(tmp, "snapshot.db")
			if _, err := e.DB.ExecContext(e.Ctx, "VACUUM INTO ?", p); err != nil {
				_ = os.RemoveAll(tmp)
				return nil, fmt.Errorf("backup: %w", err)
			}
			f, err := os.Open(p)
			if err != nil {
				_ = os.RemoveAll(tmp)
				return nil, err
			}
			return &ops.Stream{
				Name:        "weave-" + time.Now().UTC().Format("20060102-150405") + ".db",
				ContentType: "application/vnd.sqlite3",
				Reader:      &cleanupReadCloser{f: f, dir: tmp},
			}, nil
		})

	web.Route("GET /admin", adminOnly(func(c *web.Ctx) error {
		st, err := ops.Typed[StatsOut](c.Env, "admin.stats", &NoInput{})
		if err != nil {
			return err
		}
		return c.Render("admin_index.html", st)
	}))
	web.Route("GET /admin/backup", adminOnly(func(c *web.Ctx) error {
		v, err := ops.Call(c.Env, "admin.backup", &BackupIn{})
		if err != nil {
			return err
		}
		st := v.(*ops.Stream)
		if st.Reader != nil {
			defer st.Reader.Close()
		}
		c.W.Header().Set("Content-Type", st.ContentType)
		c.W.Header().Set("Cache-Control", "no-store")
		cd := mime.FormatMediaType("attachment", map[string]string{"filename": st.Name})
		if cd == "" {
			cd = "attachment"
		}
		c.W.Header().Set("Content-Disposition", cd)
		if st.Reader != nil {
			_, err = io.Copy(c.W, st.Reader)
			return err
		}
		_, err = c.W.Write(st.Data)
		return err
	}))
}
