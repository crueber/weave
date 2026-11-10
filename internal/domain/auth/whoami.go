package auth

import (
	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
)

type WhoamiBoard struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	Role        string `json:"role" desc:"viewer | commenter | editor | manager (project manager or admin)"`
}

type WhoamiOut struct {
	User    *User         `json:"user"`
	Role    string        `json:"role"`
	IsAgent bool          `json:"isAgent"`
	Boards  []WhoamiBoard `json:"boards"`
}

func levelName(l authz.Level) string {
	switch l {
	case authz.Manage:
		return "manager"
	case authz.Edit:
		return "editor"
	case authz.Comment:
		return "commenter"
	}
	return "viewer"
}

func init() {
	ops.Register(ops.Op{Name: "auth.whoami", Summary: "The caller (user, role, isAgent) and every board they can access with their role", Method: "GET", Path: "/auth/whoami",
		UI: "exempt:the browser shows identity in the header and /settings; this is the machine summary"}, func(e *ops.Env, _ NoInput) (WhoamiOut, error) {
		u, err := loadUser(e, e.User.UserID)
		if err != nil {
			return WhoamiOut{}, err
		}
		q := `SELECT b.id,b.name,b.project_id,p.name FROM boards b JOIN projects p ON p.id=b.project_id WHERE b.is_template=0`
		var args []any
		if e.User.Role != "admin" {
			q += ` AND (b.id IN (SELECT board_id FROM board_memberships WHERE user_id=? OR group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?))
				OR b.project_id IN (SELECT project_id FROM project_managers WHERE user_id=?))`
			args = []any{e.User.UserID, e.User.UserID, e.User.UserID}
		}
		rows, err := e.DB.QueryContext(e.Ctx, q+` ORDER BY p.name, b.position, b.name LIMIT 1000`, args...)
		if err != nil {
			return WhoamiOut{}, err
		}
		defer rows.Close()
		out := WhoamiOut{User: u, Role: u.Role, IsAgent: u.IsAgent, Boards: []WhoamiBoard{}}
		for rows.Next() {
			var b WhoamiBoard
			if err := rows.Scan(&b.ID, &b.Name, &b.ProjectID, &b.ProjectName); err != nil {
				return out, err
			}
			out.Boards = append(out.Boards, b)
		}
		if err := rows.Err(); err != nil {
			return out, err
		}
		kept := out.Boards[:0]
		for _, b := range out.Boards {
			if l := authz.BoardLevel(e, b.ID); l >= authz.View {
				b.Role = levelName(l)
				kept = append(kept, b)
			}
		}
		out.Boards = kept
		return out, nil
	})
}
