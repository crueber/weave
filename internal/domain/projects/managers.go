package projects

import (
	"database/sql"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

// Manager is a project manager with the user's display fields.
type Manager struct {
	ID        string `json:"id"`
	ProjectID string `json:"projectId"`
	UserID    string `json:"userId"`
	Name      string `json:"name"`
	Email     string `json:"email"`
	IsOwner   bool   `json:"isOwner"`
	CreatedAt string `json:"createdAt"`
}

type ManagerAddIn struct {
	ID     string `path:"id" desc:"Project id"`
	UserID string `json:"userId" required:"true" desc:"User to promote (must be admin or projectOwner role)"`
}

type ManagerRemoveIn struct {
	ID        string `path:"id" desc:"Project id"`
	ManagerID string `path:"managerId" desc:"Project manager id (see project.manager.list)"`
}

// Managers lists the managers of a project (caller needs View).
func Managers(e *ops.Env, projectID string) ([]Manager, error) {
	if err := authz.RequireProject(e, projectID, authz.View); err != nil {
		return nil, err
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT m.id,m.project_id,m.user_id,u.name,u.email,
		COALESCE(p.owner_manager_id=m.id,0),m.created_at FROM project_managers m JOIN users u ON u.id=m.user_id
		JOIN projects p ON p.id=m.project_id WHERE m.project_id=? ORDER BY m.created_at,m.id`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Manager{}
	for rows.Next() {
		var m Manager
		if err := rows.Scan(&m.ID, &m.ProjectID, &m.UserID, &m.Name, &m.Email, &m.IsOwner, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func init() {
	ops.Register(ops.Op{Name: "project.manager.list", Summary: "List project managers", Method: "GET", Path: "/projects/{id}/managers",
		UI: "GET /ui/projects/{id}/settings/managers"}, func(e *ops.Env, in IDIn) ([]Manager, error) { return Managers(e, in.ID) })

	ops.Register(ops.Op{Name: "project.manager.add", Summary: "Add a project manager", Method: "POST", Path: "/projects/{id}/managers",
		UI: "POST /ui/projects/{id}/managers"}, func(e *ops.Env, in ManagerAddIn) (Manager, error) {
		if err := authz.RequireProject(e, in.ID, authz.Manage); err != nil {
			return Manager{}, err
		}
		var role string
		var deact bool
		if e.DB.QueryRowContext(e.Ctx, `SELECT role,is_deactivated FROM users WHERE id=? AND deleted_at IS NULL`, in.UserID).Scan(&role, &deact) != nil {
			return Manager{}, ops.NotFound("user")
		}
		if deact {
			return Manager{}, ops.Invalid("user is deactivated")
		}
		if role != "admin" && role != "projectOwner" {
			return Manager{}, ops.Invalid("only admin and projectOwner users can be project managers")
		}
		mid := db.NewID()
		if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO project_managers(id,project_id,user_id,created_at) VALUES(?,?,?,?)`, mid, in.ID, in.UserID, db.Now()); err != nil {
			if db.IsUnique(err) {
				return Manager{}, ops.Conflict("user is already a manager of this project")
			}
			return Manager{}, err
		}
		ms, _ := Managers(e, in.ID)
		var out Manager
		for _, m := range ms {
			if m.ID == mid {
				out = m
			}
		}
		publish(e, "projectManager.created", in.ID, out)
		e.Hub.Publish(realtime.Event{Name: "projectManager.created", Room: "user:" + in.UserID, Data: out})
		return out, nil
	})

	ops.Register(ops.Op{Name: "project.manager.remove", Summary: "Remove a project manager (the last manager cannot be removed)", Method: "DELETE",
		Path: "/projects/{id}/managers/{managerId}", UI: "DELETE /ui/projects/{id}/managers/{managerId}"},
		func(e *ops.Env, in ManagerRemoveIn) (DeleteOut, error) {
			if err := authz.RequireProject(e, in.ID, authz.Manage); err != nil {
				return DeleteOut{}, err
			}
			var userID string
			if e.DB.QueryRowContext(e.Ctx, `SELECT user_id FROM project_managers WHERE id=? AND project_id=?`, in.ManagerID, in.ID).Scan(&userID) != nil {
				return DeleteOut{}, ops.NotFound("project manager")
			}
			err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
				var n int
				_ = tx.QueryRow(`SELECT COUNT(*) FROM project_managers WHERE project_id=?`, in.ID).Scan(&n)
				if n <= 1 {
					return ops.Conflict("a project needs at least one manager")
				}
				var owner string
				_ = tx.QueryRow(`SELECT COALESCE(owner_manager_id,'') FROM projects WHERE id=?`, in.ID).Scan(&owner)
				if owner == in.ManagerID && e.User.Role != "admin" {
					return ops.Forbidden() // only an admin may remove the owning manager
				}
				if _, err := tx.Exec(`DELETE FROM project_managers WHERE id=?`, in.ManagerID); err != nil {
					return err
				}
				if owner == in.ManagerID {
					_, err := tx.Exec(`UPDATE projects SET owner_manager_id=(SELECT id FROM project_managers WHERE project_id=? ORDER BY created_at,id LIMIT 1) WHERE id=?`, in.ID, in.ID)
					return err
				}
				return nil
			})
			if err != nil {
				return DeleteOut{}, err
			}
			publish(e, "projectManager.deleted", in.ID, map[string]string{"id": in.ManagerID, "userId": userID})
			e.Hub.Publish(realtime.Event{Name: "projectManager.deleted", Room: "user:" + userID, Data: map[string]string{"id": in.ManagerID, "projectId": in.ID}})
			return DeleteOut{in.ManagerID}, nil
		})
}
