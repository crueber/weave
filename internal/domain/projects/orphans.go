package projects

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

// Ownerless projects and the admin recovery path (see authz/recovery.go for the rules).
//
// A project is ownerless when none of its project_managers rows belongs to an ACTIVE manager: a person who is not
// soft-deleted, not deactivated, not an agent account and not a guest (the same people CanBeManager accepts, alive).
// Hard-deleted users take their manager rows with them (ON DELETE CASCADE), so a project whose managers were all
// deleted simply has no rows. A board is unmanageable when its project is ownerless AND it has no active board owner
// (a user membership with role owner held by an active person).
//
// Every query here uses only columns that exist since the first release (0001 + 0800), because the host-only
// `weave admin preflight` runs them against a deployed, not yet upgraded database (db.OpenReadOnly).

// activeManagerSQL is THE predicate for "this account still counts as a manager" (alias u = users).
const activeManagerSQL = `u.deleted_at IS NULL AND u.is_deactivated=0 AND u.is_agent=0 AND u.role<>'guest'`

// FormerManager is a manager row that no longer counts, with the reason.
type FormerManager struct {
	UserID string `json:"userId"`
	Name   string `json:"name"`
	Email  string `json:"email"`
	State  string `json:"state" desc:"why the manager no longer counts: deactivated | deleted | agent | guest"`
}

// Orphan is an ownerless project as the recovery listing shows it: identifiers and counts only, never content.
type Orphan struct {
	ID                 string          `json:"id"`
	Name               string          `json:"name"`
	CreatedAt          string          `json:"createdAt"`
	Boards             int             `json:"boards" desc:"number of boards in the project"`
	BoardsWithoutOwner int             `json:"boardsWithoutOwner" desc:"boards with no active board owner either: nobody but an admin can manage them"`
	FormerManagers     []FormerManager `json:"formerManagers" desc:"last known managers (deactivated or soft-deleted accounts); empty when they were deleted outright"`
}

// Recovery is one audit-trail entry of the recovery path.
type Recovery struct {
	ID          string          `json:"id"`
	ProjectID   string          `json:"projectId"`
	ProjectName string          `json:"projectName" desc:"project name at the time of the recovery"`
	UserID      string          `json:"userId" desc:"the new manager"`
	UserName    string          `json:"userName"`
	UserEmail   string          `json:"userEmail"`
	ActorID     string          `json:"actorId,omitempty" desc:"the admin who reassigned it (empty for the host command)"`
	ActorName   string          `json:"actorName,omitempty"`
	ActorEmail  string          `json:"actorEmail,omitempty"`
	Source      string          `json:"source" desc:"admin (project.reassign) | host (weave admin adopt)"`
	Previous    []FormerManager `json:"previous" desc:"the managers the project had at that moment"`
	CreatedAt   string          `json:"createdAt"`
}

type ReassignIn struct {
	ID     string `path:"id" desc:"Project id of an ownerless project (see project.orphans)"`
	UserID string `json:"userId" required:"true" desc:"The new manager: an active person who is not a guest and not an agent account"`
}

type RecoveryListIn struct {
	ProjectID string `query:"projectId" desc:"Only entries of this project"`
	Limit     int    `query:"limit" desc:"Most recent entries to return (default 50, max 500)"`
}

type queryer interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func managerState(deleted, deactivated, agent bool, role string) string {
	switch {
	case deleted:
		return "deleted"
	case deactivated:
		return "deactivated"
	case agent:
		return "agent"
	case role == "guest":
		return "guest"
	}
	return "active"
}

// projectManagersOf returns every manager row of the projects with its state (active ones included).
func projectManagersOf(ctx context.Context, q queryer, projectIDs []string) (map[string][]FormerManager, error) {
	out := map[string][]FormerManager{}
	if len(projectIDs) == 0 {
		return out, nil
	}
	ph := "?" + strings.Repeat(",?", len(projectIDs)-1)
	args := make([]any, len(projectIDs))
	for i, id := range projectIDs {
		args[i] = id
	}
	rows, err := q.QueryContext(ctx, `SELECT m.project_id,u.id,u.name,u.email,u.deleted_at IS NOT NULL,u.is_deactivated,u.is_agent,u.role
		FROM project_managers m JOIN users u ON u.id=m.user_id WHERE m.project_id IN (`+ph+`) ORDER BY m.created_at,m.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pid, role string
		var f FormerManager
		var del, deact, agent bool
		if err := rows.Scan(&pid, &f.UserID, &f.Name, &f.Email, &del, &deact, &agent, &role); err != nil {
			return nil, err
		}
		f.State = managerState(del, deact, agent, role)
		out[pid] = append(out[pid], f)
	}
	return out, rows.Err()
}

// OwnerlessProjects lists the projects without an active manager, with board counts and last known managers.
// It takes a plain database handle so the host tools can run it on a read-only, not yet upgraded database.
func OwnerlessProjects(ctx context.Context, q queryer) ([]Orphan, error) {
	rows, err := q.QueryContext(ctx, `SELECT p.id,p.name,p.created_at,
		(SELECT COUNT(*) FROM boards b WHERE b.project_id=p.id),
		(SELECT COUNT(*) FROM boards b WHERE b.project_id=p.id AND NOT EXISTS(SELECT 1 FROM board_memberships bm JOIN users u ON u.id=bm.user_id
			WHERE bm.board_id=b.id AND bm.role='owner' AND `+activeManagerSQL+`))
		FROM projects p WHERE NOT EXISTS(SELECT 1 FROM project_managers m JOIN users u ON u.id=m.user_id WHERE m.project_id=p.id AND `+activeManagerSQL+`)
		ORDER BY p.name COLLATE NOCASE, p.id`)
	if err != nil {
		return nil, err
	}
	out := []Orphan{}
	var ids []string
	for rows.Next() {
		var o Orphan
		if err := rows.Scan(&o.ID, &o.Name, &o.CreatedAt, &o.Boards, &o.BoardsWithoutOwner); err != nil {
			rows.Close()
			return nil, err
		}
		o.FormerManagers = []FormerManager{}
		out = append(out, o)
		ids = append(ids, o.ID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ms, err := projectManagersOf(ctx, q, ids)
	if err != nil {
		return nil, err
	}
	for i := range out {
		for _, m := range ms[out[i].ID] {
			if m.State != "active" { // always true for an ownerless project; kept as a guard
				out[i].FormerManagers = append(out[i].FormerManagers, m)
			}
		}
	}
	return out, nil
}

// WouldOrphan counts, per user, the projects that become ownerless if that user stops counting as a manager
// (deactivated or deleted): the user is an active manager of the project and no OTHER active manager exists.
// Counts only (the admin Users page warns with them); unknown ids are absent.
func WouldOrphan(ctx context.Context, q queryer, userIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(userIDs) == 0 {
		return out, nil
	}
	args := make([]any, len(userIDs))
	for i, id := range userIDs {
		args[i] = id
	}
	rows, err := q.QueryContext(ctx, `SELECT m.user_id,COUNT(DISTINCT m.project_id) FROM project_managers m JOIN users u ON u.id=m.user_id
		WHERE m.user_id IN (?`+strings.Repeat(",?", len(userIDs)-1)+`) AND `+activeManagerSQL+` AND NOT EXISTS(
			SELECT 1 FROM project_managers m2 JOIN users u ON u.id=m2.user_id WHERE m2.project_id=m.project_id AND m2.user_id<>m.user_id AND `+activeManagerSQL+`)
		GROUP BY m.user_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// IsOwnerless reports whether one project has no active manager (false for an unknown project).
func IsOwnerless(ctx context.Context, q queryer, projectID string) (bool, error) {
	var exists, active int
	err := q.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE((SELECT COUNT(*) FROM project_managers m JOIN users u ON u.id=m.user_id
		WHERE m.project_id=? AND `+activeManagerSQL+`),0) FROM projects WHERE id=?`, projectID, projectID).Scan(&exists, &active)
	if err != nil {
		return false, err
	}
	return exists == 1 && active == 0, nil
}

// recoveryActor is who performed a recovery (nil = the host command).
type recoveryActor struct{ ID, Name, Email string }

// errNotOwnerless is the refusal for a project that still has an active manager: reassigning it would let an
// admin take over a project its managers still run.
var errNotOwnerless = ops.Conflict("this project still has an active manager; only its managers can add another (project.manager.add)")

// assignManager makes userID a manager of projectID and writes the audit row, in one transaction. With
// requireOwnerless the project must have no active manager at that moment (re-checked inside the transaction).
// The project's owning manager (owner_manager_id) moves to the new manager when the current one is missing or
// inactive.
func assignManager(ctx context.Context, d *db.DB, projectID, userID string, actor *recoveryActor, source string, requireOwnerless bool) (Recovery, string, error) {
	r := Recovery{ID: db.NewID(), ProjectID: projectID, UserID: userID, Source: source, CreatedAt: db.Now(), Previous: []FormerManager{}}
	mid := db.NewID()
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		var owner string
		if tx.QueryRowContext(ctx, `SELECT name,COALESCE(owner_manager_id,'') FROM projects WHERE id=?`, projectID).Scan(&r.ProjectName, &owner) != nil {
			return ops.NotFound("project")
		}
		var role string
		var deact, agent bool
		if tx.QueryRowContext(ctx, `SELECT name,email,role,is_deactivated,is_agent FROM users WHERE id=? AND deleted_at IS NULL`, userID).
			Scan(&r.UserName, &r.UserEmail, &role, &deact, &agent) != nil {
			return ops.NotFound("user")
		}
		if deact {
			return ops.Invalid("user is deactivated")
		}
		if !CanBeManager(role, agent) {
			return ops.Invalid("guests and agent accounts cannot be project managers")
		}
		ms, err := projectManagersOf(ctx, tx, []string{projectID})
		if err != nil {
			return err
		}
		for _, m := range ms[projectID] {
			if m.State == "active" && requireOwnerless {
				return errNotOwnerless
			}
			if m.UserID == userID {
				return ops.Conflict("user is already a manager of this project")
			}
			r.Previous = append(r.Previous, m)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES(?,?,?,?)`, mid, projectID, userID, r.CreatedAt); err != nil {
			if db.IsUnique(err) {
				return ops.Conflict("user is already a manager of this project")
			}
			return err
		}
		// the owning manager moves to the new manager unless an active person still holds it
		var ownerActive int
		_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_managers m JOIN users u ON u.id=m.user_id WHERE m.id=? AND m.project_id=? AND `+activeManagerSQL,
			owner, projectID).Scan(&ownerActive)
		if ownerActive == 0 {
			if _, err := tx.ExecContext(ctx, `UPDATE projects SET owner_manager_id=?,updated_at=? WHERE id=?`, mid, r.CreatedAt, projectID); err != nil {
				return err
			}
		}
		var aid, aname, aemail any
		if actor != nil {
			r.ActorID, r.ActorName, r.ActorEmail = actor.ID, actor.Name, actor.Email
			aid, aname, aemail = actor.ID, actor.Name, actor.Email
		}
		prev, _ := json.Marshal(r.Previous)
		_, err = tx.ExecContext(ctx, `INSERT INTO project_recoveries(id,project_id,project_name,user_id,user_name,user_email,actor_id,actor_name,actor_email,source,previous,created_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, r.ID, projectID, r.ProjectName, userID, r.UserName, r.UserEmail, aid, aname, aemail, source, string(prev), r.CreatedAt)
		return err
	})
	if err != nil {
		return Recovery{}, "", err
	}
	return r, mid, nil
}

// Adopt is the host-only break-glass behind `weave admin adopt --project ID --email E` (direct database access, like
// `weave admin mint-key`): it makes the person with that e-mail address a manager of the project, whether or not
// the project is ownerless, and records it in the recovery audit trail (source host) and the server log.
func Adopt(ctx context.Context, d *db.DB, projectID, email string) (Recovery, error) {
	email = strings.TrimSpace(email)
	if projectID == "" || email == "" {
		return Recovery{}, ops.Invalid("project and email are required")
	}
	var uid string
	if d.QueryRowContext(ctx, `SELECT id FROM users WHERE email=? AND deleted_at IS NULL`, email).Scan(&uid) != nil {
		return Recovery{}, ops.NotFound("user")
	}
	r, _, err := assignManager(ctx, d, projectID, uid, nil, "host", false)
	if err != nil {
		return Recovery{}, err
	}
	log.Printf("projects: AUDIT %s (%s) made a manager of project %s (%s) via host command (recovery %s)", r.UserEmail, r.UserID, r.ProjectID, r.ProjectName, r.ID)
	return r, nil
}

// Recoveries lists the audit trail, newest first.
func Recoveries(ctx context.Context, q queryer, projectID string, limit int) ([]Recovery, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	where, args := "", []any{}
	if projectID != "" {
		where, args = " WHERE project_id=?", append(args, projectID)
	}
	rows, err := q.QueryContext(ctx, `SELECT id,project_id,project_name,user_id,user_name,user_email,COALESCE(actor_id,''),COALESCE(actor_name,''),
		COALESCE(actor_email,''),source,previous,created_at FROM project_recoveries`+where+` ORDER BY created_at DESC,id DESC LIMIT ?`, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Recovery{}
	for rows.Next() {
		var r Recovery
		var prev string
		if err := rows.Scan(&r.ID, &r.ProjectID, &r.ProjectName, &r.UserID, &r.UserName, &r.UserEmail, &r.ActorID, &r.ActorName, &r.ActorEmail, &r.Source, &prev, &r.CreatedAt); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(prev), &r.Previous) != nil || r.Previous == nil {
			r.Previous = []FormerManager{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func init() {
	ops.Register(ops.Op{Name: "project.orphans", Summary: "List ownerless projects: no active manager left (admin recovery; ids, names, counts and former managers only, no content)",
		Method: "GET", Path: "/projects/orphans", Auth: ops.AuthAdmin, UI: "GET /admin/orphans"},
		func(e *ops.Env, _ struct{}) ([]Orphan, error) {
			if err := authz.RequireProjectRecovery(e); err != nil {
				return nil, err
			}
			return OwnerlessProjects(e.Ctx, e.DB)
		})

	ops.Register(ops.Op{Name: "project.reassign", Summary: "Give an ownerless project a new manager (admin recovery; refused while the project has an active manager; audited)",
		Method: "POST", Path: "/projects/{id}/reassign", Auth: ops.AuthAdmin, UI: "POST /ui/admin/orphans/{id}/reassign"},
		func(e *ops.Env, in ReassignIn) (Recovery, error) {
			if err := authz.RequireProjectRecovery(e); err != nil {
				return Recovery{}, err
			}
			actor := &recoveryActor{ID: e.User.UserID, Name: e.User.Name}
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT name,email FROM users WHERE id=?`, e.User.UserID).Scan(&actor.Name, &actor.Email)
			r, mid, err := assignManager(e.Ctx, e.DB, in.ID, in.UserID, actor, "admin", true)
			if err != nil {
				return Recovery{}, err
			}
			log.Printf("projects: AUDIT ownerless project %s (%s) reassigned to %s (%s) by admin %s (%s) (recovery %s)",
				r.ProjectID, r.ProjectName, r.UserID, r.UserEmail, actor.ID, actor.Email, r.ID)
			// after commit: ids only on the global room (every signed-in user hears it), the new manager's own room
			// gets the same projectManager.created event project.manager.add sends, so their home page picks it up
			ops.Emit(e, ops.Event{Type: "projectReassign", Name: "project.reassigned", Room: "global",
				Data: map[string]any{"item": map[string]any{"projectId": r.ProjectID, "userId": r.UserID, "by": actor.ID, "recoveryId": r.ID}}})
			m := Manager{ID: mid, ProjectID: r.ProjectID, UserID: r.UserID, Name: r.UserName, IsOwner: true, CreatedAt: r.CreatedAt}
			e.Hub.Publish(realtime.Event{Name: "projectManager.created", Room: "user:" + r.UserID, Data: m})
			publish(e, "projectManager.created", r.ProjectID, m)
			return r, nil
		})

	ops.Register(ops.Op{Name: "project.recovery.list", Summary: "Audit trail of project recoveries: who gave which ownerless project to whom, and when (admin)",
		Method: "GET", Path: "/projects/recoveries", Auth: ops.AuthAdmin, UI: "GET /admin/orphans"},
		func(e *ops.Env, in RecoveryListIn) ([]Recovery, error) {
			if err := authz.RequireProjectRecovery(e); err != nil {
				return nil, err
			}
			return Recoveries(e.Ctx, e.DB, in.ProjectID, in.Limit)
		})
}
