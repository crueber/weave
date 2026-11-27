package projects

import (
	"context"
	"sort"
	"strings"

	"github.com/crueber/weave/internal/ops"
)

var errNoSuchUser = ops.NotFound("user")

// Preflight is the read-only report behind the host-only `weave admin preflight`, run on a deployed data directory
// BEFORE upgrading to ownership-based permissions (admins lose their implicit access to every project and board).
// Like OwnerlessProjects it only reads columns that exist since the first release, so it works on an old schema
// opened with db.OpenReadOnly. Names are included: it is a tool for the host operator, not an API.

// ProjectRef is a project in the preflight report.
type ProjectRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Boards int    `json:"boards"`
}

// AdminReach lists the projects one administrator reaches only through the admin bypass: they are neither a
// manager of the project nor a member (direct or through a group) of any of its boards.
type AdminReach struct {
	UserID      string       `json:"userId"`
	Name        string       `json:"name"`
	Email       string       `json:"email"`
	Role        string       `json:"role"`
	Projects    int          `json:"projects" desc:"projects on the instance"`
	Unreachable []ProjectRef `json:"unreachable" desc:"projects this person will no longer see"`
}

// AdminOnlyBoard is a board whose active managers (active project managers of its project plus active board
// owners) are all administrators: once the bypass is gone, they manage it only as managers/owners, so nothing
// changes for them, but nobody else can.
type AdminOnlyBoard struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	ProjectID   string   `json:"projectId"`
	ProjectName string   `json:"projectName"`
	Managers    []string `json:"managers" desc:"the admins who manage it, as 'Name <email>'"`
}

// PreflightReport is the whole report.
type PreflightReport struct {
	Ownerless       []Orphan         `json:"ownerless"`
	Admins          []AdminReach     `json:"admins"`
	AdminOnlyBoards []AdminOnlyBoard `json:"adminOnlyBoards"`
	Projects        int              `json:"projects"`
	Boards          int              `json:"boards"`
}

// NeedsAction reports whether anything in the report should be fixed before the upgrade: ownerless projects or
// projects an admin will stop seeing. Admin-only boards are informational (nothing changes for them on upgrade:
// their admins manage them as managers or owners), so a single-admin instance can still come out clean.
func (r PreflightReport) NeedsAction() bool {
	if len(r.Ownerless) > 0 {
		return true
	}
	for _, a := range r.Admins {
		if len(a.Unreachable) > 0 {
			return true
		}
	}
	return false
}

// Preflight builds the report. email limits section (b) to one person (any account, so an operator can check
// someone who is about to become an admin); empty means every active human administrator.
func Preflight(ctx context.Context, q queryer, email string) (PreflightReport, error) {
	var r PreflightReport
	var err error
	if err = q.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM projects),(SELECT COUNT(*) FROM boards)`).Scan(&r.Projects, &r.Boards); err != nil {
		return r, err
	}
	if r.Ownerless, err = OwnerlessProjects(ctx, q); err != nil {
		return r, err
	}
	if r.Admins, err = adminReach(ctx, q, email); err != nil {
		return r, err
	}
	r.AdminOnlyBoards, err = adminOnlyBoards(ctx, q)
	return r, err
}

func adminReach(ctx context.Context, q queryer, email string) ([]AdminReach, error) {
	sel := `SELECT id,name,email,role FROM users WHERE deleted_at IS NULL AND `
	var args []any
	if email != "" {
		sel += `email=? COLLATE NOCASE`
		args = append(args, strings.TrimSpace(email))
	} else {
		sel += `role='admin' AND is_deactivated=0 AND is_agent=0`
	}
	rows, err := q.QueryContext(ctx, sel+` ORDER BY name COLLATE NOCASE, email`, args...)
	if err != nil {
		return nil, err
	}
	out := []AdminReach{}
	for rows.Next() {
		var a AdminReach
		if err := rows.Scan(&a.UserID, &a.Name, &a.Email, &a.Role); err != nil {
			rows.Close()
			return nil, err
		}
		a.Unreachable = []ProjectRef{}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if email != "" && len(out) == 0 {
		return nil, errNoSuchUser
	}
	for i := range out {
		a := &out[i]
		// the non-admin branch of authz.ProjectLevel: a manager row, or any direct or group membership on one of
		// the project's boards (public boards do not count: they give View on a board, not on the project)
		rows, err := q.QueryContext(ctx, `SELECT p.id,p.name,(SELECT COUNT(*) FROM boards b WHERE b.project_id=p.id),
			NOT EXISTS(SELECT 1 FROM project_managers m WHERE m.project_id=p.id AND m.user_id=?1)
			AND NOT EXISTS(SELECT 1 FROM boards b JOIN board_memberships bm ON bm.board_id=b.id WHERE b.project_id=p.id AND
				(bm.user_id=?1 OR bm.group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?1)))
			FROM projects p ORDER BY p.name COLLATE NOCASE, p.id`, a.UserID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var p ProjectRef
			var lost bool
			if err := rows.Scan(&p.ID, &p.Name, &p.Boards, &lost); err != nil {
				rows.Close()
				return nil, err
			}
			a.Projects++
			if lost {
				a.Unreachable = append(a.Unreachable, p)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func adminOnlyBoards(ctx context.Context, q queryer) ([]AdminOnlyBoard, error) {
	rows, err := q.QueryContext(ctx, `SELECT b.id,b.name,p.id,p.name,u.id,u.name,u.email,u.role FROM boards b JOIN projects p ON p.id=b.project_id
		JOIN project_managers m ON m.project_id=b.project_id JOIN users u ON u.id=m.user_id WHERE `+activeManagerSQL+`
		UNION
		SELECT b.id,b.name,p.id,p.name,u.id,u.name,u.email,u.role FROM boards b JOIN projects p ON p.id=b.project_id
		JOIN board_memberships bm ON bm.board_id=b.id JOIN users u ON u.id=bm.user_id WHERE bm.role='owner' AND `+activeManagerSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type acc struct {
		b      AdminOnlyBoard
		nonAdm bool
		seen   map[string]bool
	}
	boards := map[string]*acc{}
	for rows.Next() {
		var bid, bname, pid, pname, uid, uname, uemail, role string
		if err := rows.Scan(&bid, &bname, &pid, &pname, &uid, &uname, &uemail, &role); err != nil {
			return nil, err
		}
		a := boards[bid]
		if a == nil {
			a = &acc{b: AdminOnlyBoard{ID: bid, Name: bname, ProjectID: pid, ProjectName: pname}, seen: map[string]bool{}}
			boards[bid] = a
		}
		if role != "admin" {
			a.nonAdm = true
		}
		if !a.seen[uid] {
			a.seen[uid] = true
			a.b.Managers = append(a.b.Managers, uname+" <"+uemail+">")
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []AdminOnlyBoard{}
	for _, a := range boards {
		if !a.nonAdm {
			sort.Strings(a.b.Managers)
			out = append(out, a.b)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !strings.EqualFold(out[i].ProjectName, out[j].ProjectName) {
			return strings.ToLower(out[i].ProjectName) < strings.ToLower(out[j].ProjectName)
		}
		if !strings.EqualFold(out[i].Name, out[j].Name) {
			return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}
