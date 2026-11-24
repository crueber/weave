package projects

import (
	"context"
	"html/template"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/domain/auth"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// Admin console > Orphaned projects (/admin/orphans): the ownerless projects (project.orphans), a picker per
// project over the people who may manage (project.reassign), and the recovery log (project.recovery.list).

type orphansData struct {
	Orphans    []Orphan
	Candidates []candidate
	Recoveries []Recovery
	Error      string
	Flash      string
}

func renderOrphans(c *web.Ctx, errm, flash string) error {
	d := orphansData{Error: errm, Flash: flash}
	var err error
	if d.Orphans, err = ops.Typed[[]Orphan](c.Env, "project.orphans", struct{}{}); err != nil {
		return err
	}
	if len(d.Orphans) > 0 {
		// the admin's directory (every active account), minus guests and agent accounts
		if d.Candidates, err = managerCandidates(c.Env, nil); err != nil {
			return err
		}
	}
	if d.Recoveries, err = ops.Typed[[]Recovery](c.Env, "project.recovery.list", RecoveryListIn{Limit: 50}); err != nil {
		return err
	}
	if c.IsHTMX() && c.R.Method != "GET" {
		return c.RenderBlock("projects_admin_orphans.html", "orphans-body", d)
	}
	return c.Render("projects_admin_orphans.html", d)
}

// recoveryOnly hides the page from everyone who may not use the recovery path (not_found, like other admin pages).
func recoveryOnly(h func(*web.Ctx) error) func(*web.Ctx) error {
	return func(c *web.Ctx) error {
		if !authz.ProjectRecoveryAdmin(c.Env) {
			return ops.NotFound("page")
		}
		return h(c)
	}
}

// projectsWhen renders a stored RFC3339 time as "2 Jan 2006" (with the UTC time when withTime).
func projectsWhen(s string, withTime bool) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	if withTime {
		return t.UTC().Format("2 Jan 2006, 15:04 UTC")
	}
	return t.UTC().Format("2 Jan 2006")
}

func init() {
	// the admin Users page warns before a deactivation or deletion leaves projects without an active manager
	auth.OwnershipImpact = func(ctx context.Context, d *db.DB, userIDs []string) map[string]int {
		m, _ := WouldOrphan(ctx, d, userIDs)
		return m
	}
	web.AddFuncs(template.FuncMap{"projects_when": projectsWhen})
	web.Route("GET /admin/orphans", recoveryOnly(func(c *web.Ctx) error { return renderOrphans(c, "", "") }))
	web.Route("POST /ui/admin/orphans/{id}/reassign", recoveryOnly(func(c *web.Ctx) error {
		out, err := c.Do("project.reassign")
		if err != nil {
			if oe, ok := err.(*ops.Error); ok {
				return renderOrphans(c, oe.Msg, "")
			}
			return err
		}
		r, _ := out.(Recovery)
		return renderOrphans(c, "", r.UserName+" now manages "+r.ProjectName)
	}))
}
