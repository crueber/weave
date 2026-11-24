package boards

import (
	"database/sql"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

func projectEvent(name, projectID, boardID string) realtime.Event {
	return realtime.Event{Name: name, Room: "project:" + projectID, Data: map[string]any{"id": boardID, "projectId": projectID}}
}

// Member is a board membership for a user or a user group.
type Member struct {
	ID         string  `json:"id"`
	BoardID    string  `json:"boardId"`
	UserID     *string `json:"userId,omitempty"`
	GroupID    *string `json:"groupId,omitempty"`
	Role       string  `json:"role"`
	CanComment *bool   `json:"canComment,omitempty"`
	Name       string  `json:"name"`
	Email      string  `json:"email,omitempty" desc:"admins and the member themself only"`
	IsAgent    bool    `json:"isAgent"`
	CreatedAt  string  `json:"createdAt"`
}

func membersOf(e *ops.Env, boardID string) ([]Member, error) {
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT m.id,m.board_id,m.user_id,m.group_id,m.role,m.can_comment,COALESCE(u.name,g.name,''),COALESCE(u.email,''),COALESCE(u.is_agent,0),m.created_at
		FROM board_memberships m LEFT JOIN users u ON u.id=m.user_id LEFT JOIN user_groups g ON g.id=m.group_id WHERE m.board_id=? ORDER BY m.created_at,m.id`, boardID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Member{}
	for rows.Next() {
		var m Member
		if err := rows.Scan(&m.ID, &m.BoardID, &m.UserID, &m.GroupID, &m.Role, &m.CanComment, &m.Name, &m.Email, &m.IsAgent, &m.CreatedAt); err != nil {
			return nil, err
		}
		redactMember(e, &m)
		out = append(out, m)
	}
	return out, rows.Err()
}

func getMember(e *ops.Env, id string) (Member, error) {
	var m Member
	err := e.DB.QueryRowContext(e.Ctx, `SELECT m.id,m.board_id,m.user_id,m.group_id,m.role,m.can_comment,COALESCE(u.name,g.name,''),COALESCE(u.email,''),COALESCE(u.is_agent,0),m.created_at
		FROM board_memberships m LEFT JOIN users u ON u.id=m.user_id LEFT JOIN user_groups g ON g.id=m.group_id WHERE m.id=?`, id).
		Scan(&m.ID, &m.BoardID, &m.UserID, &m.GroupID, &m.Role, &m.CanComment, &m.Name, &m.Email, &m.IsAgent, &m.CreatedAt)
	if err == sql.ErrNoRows {
		return m, ops.NotFound("membership")
	}
	redactMember(e, &m)
	return m, err
}

// redactMember keeps e-mail addresses for admins and the member themself, as the user directory does (user.list):
// board owners and viewers are ordinary users and see names only.
func redactMember(e *ops.Env, m *Member) {
	if e.User != nil && (e.User.Role == "admin" || (m.UserID != nil && *m.UserID == e.User.UserID)) {
		return
	}
	m.Email = ""
}

type MemberListIn struct {
	BoardID string `path:"boardId"`
}
type MemberAddIn struct {
	BoardID    string `path:"boardId"`
	UserID     string `json:"userId" desc:"user to add (exclusive with groupId)"`
	GroupID    string `json:"groupId" desc:"user group to add (exclusive with userId)"`
	Role       string `json:"role" desc:"editor (default) | viewer | owner (people only, not groups, guests or agents: manages this board, not the project)"`
	CanComment *bool  `json:"canComment" desc:"viewers only: may comment"`
}
type MemberUpdateIn struct {
	ID         string `path:"memberId"`
	Role       string `json:"role" desc:"editor | viewer | owner (people only, not groups, guests or agents: manages this board, not the project)"`
	CanComment *bool  `json:"canComment" desc:"viewers only: may comment"`
}
type MemberRefIn struct {
	ID string `path:"memberId"`
}

// validMemberRole accepts the stored membership roles (a commenter is a viewer with canComment).
func validMemberRole(r string) bool {
	return r == authz.RoleOwner || r == authz.RoleEditor || r == authz.RoleViewer
}

// checkOwnerTarget: the owner role is for people only (never a group, an agent account or a guest) and for active accounts.
func checkOwnerTarget(e *ops.Env, userID string) error {
	if userID == "" {
		return ops.Invalid("only a person can be a board owner, not a group")
	}
	var role string
	var agent, deact bool
	if e.DB.QueryRowContext(e.Ctx, `SELECT role,is_agent,is_deactivated FROM users WHERE id=? AND deleted_at IS NULL`, userID).Scan(&role, &agent, &deact) != nil {
		return ops.NotFound("user")
	}
	if agent || role == "guest" || deact {
		return ops.Invalid("guests, agent accounts and deactivated users cannot be board owners")
	}
	return nil
}

// checkOwnerLeaves is the last-owner rule: an owner membership may be removed or downgraded only when another owner
// remains on the board or the caller manages the project (a project manager or admin). Every project keeps at
// least one manager, so a board is never left without someone who can manage it; the rule stops an owner from
// locking themselves out by accident.
func checkOwnerLeaves(e *ops.Env, m Member) error {
	if m.Role != authz.RoleOwner {
		return nil
	}
	var pid string
	var others int
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT project_id,(SELECT COUNT(*) FROM board_memberships WHERE board_id=? AND role=? AND id<>?) FROM boards WHERE id=?`,
		m.BoardID, authz.RoleOwner, m.ID, m.BoardID).Scan(&pid, &others)
	if others > 0 || authz.ProjectLevel(e, pid) >= authz.Manage {
		return nil
	}
	return ops.Conflict("a board needs an owner: make someone else an owner first, or ask a project manager")
}

func registerMemberOps() {
	ops.Register(ops.Op{Name: "board.member.list", Summary: "List board memberships (users and groups)", Method: "GET", Path: "/boards/{boardId}/members",
		UI: ui("GET", "/boards/{boardId}/members")}, func(e *ops.Env, in MemberListIn) ([]Member, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
			return nil, err
		}
		return membersOf(e, in.BoardID)
	})
	ops.Register(ops.Op{Name: "board.member.add", Summary: "Add a user or user group to a board (project manager or board owner)", Method: "POST", Path: "/boards/{boardId}/members",
		UI: ui("POST", "/boards/{boardId}/members")}, func(e *ops.Env, in MemberAddIn) (Member, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.Manage); err != nil {
			return Member{}, err
		}
		if (in.UserID == "") == (in.GroupID == "") {
			return Member{}, ops.Invalid("exactly one of userId or groupId is required")
		}
		role := in.Role
		if role == "" {
			role = authz.RoleEditor
		}
		if !validMemberRole(role) {
			return Member{}, ops.Invalid("role must be owner, editor or viewer")
		}
		if role == authz.RoleOwner {
			if err := checkOwnerTarget(e, in.UserID); err != nil {
				return Member{}, err
			}
		}
		var cc any
		if role == authz.RoleViewer && in.CanComment != nil {
			cc = boolInt(*in.CanComment)
		}
		var n int
		if in.UserID != "" {
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM users WHERE id=? AND deleted_at IS NULL`, in.UserID).Scan(&n)
		} else {
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM user_groups WHERE id=?`, in.GroupID).Scan(&n)
		}
		if n == 0 {
			return Member{}, ops.NotFound("user or group")
		}
		id := db.NewID()
		_, err := e.DB.ExecContext(e.Ctx, `INSERT INTO board_memberships(id,board_id,user_id,group_id,role,can_comment,created_at) VALUES(?,?,?,?,?,?,?)`,
			id, in.BoardID, nullStr(in.UserID), nullStr(in.GroupID), role, cc, db.Now())
		if db.IsUnique(err) {
			return Member{}, ops.Conflict("already a member of this board")
		}
		if err != nil {
			return Member{}, err
		}
		emit(e, "boardMembershipCreate", "boardMembership.created", in.BoardID, map[string]any{"id": id, "userId": in.UserID, "groupId": in.GroupID, "role": role})
		return getMember(e, id)
	})
	ops.Register(ops.Op{Name: "board.member.update", Summary: "Change a membership's role / comment permission (project manager or board owner; the last owner cannot be downgraded except by a project manager)", Method: "PATCH", Path: "/board-members/{memberId}",
		UI: ui("PATCH", "/board-members/{memberId}")}, func(e *ops.Env, in MemberUpdateIn) (Member, error) {
		m, err := getMember(e, in.ID)
		if err != nil {
			return m, err
		}
		if err := authz.RequireBoard(e, m.BoardID, authz.Manage); err != nil {
			return m, err
		}
		role := m.Role
		if in.Role != "" {
			if !validMemberRole(in.Role) {
				return m, ops.Invalid("role must be owner, editor or viewer")
			}
			role = in.Role
		}
		if role == authz.RoleOwner && m.Role != authz.RoleOwner {
			uid := ""
			if m.UserID != nil {
				uid = *m.UserID
			}
			if err := checkOwnerTarget(e, uid); err != nil {
				return m, err
			}
		}
		if role != authz.RoleOwner {
			if err := checkOwnerLeaves(e, m); err != nil {
				return m, err
			}
		}
		var cc any
		if role == authz.RoleViewer {
			if in.CanComment != nil {
				cc = boolInt(*in.CanComment)
			} else if m.CanComment != nil {
				cc = boolInt(*m.CanComment)
			}
		}
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE board_memberships SET role=?,can_comment=? WHERE id=?`, role, cc, in.ID); err != nil {
			return m, err
		}
		emit(e, "boardMembershipUpdate", "boardMembership.updated", m.BoardID, map[string]any{"id": in.ID, "role": role})
		return getMember(e, in.ID)
	})
	ops.Register(ops.Op{Name: "board.member.remove", Summary: "Remove a membership (project manager, board owner, or the member themself; the last owner can leave only when a project manager removes them)", Method: "DELETE", Path: "/board-members/{memberId}",
		UI: ui("DELETE", "/board-members/{memberId}")}, func(e *ops.Env, in MemberRefIn) (Member, error) {
		m, err := getMember(e, in.ID)
		if err != nil {
			return m, err
		}
		self := e.User != nil && m.UserID != nil && *m.UserID == e.User.UserID
		if !self {
			if err := authz.RequireBoard(e, m.BoardID, authz.Manage); err != nil {
				return m, err
			}
		}
		if err := checkOwnerLeaves(e, m); err != nil {
			return m, err
		}
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(e.Ctx, `DELETE FROM board_memberships WHERE id=?`, in.ID); err != nil {
				return err
			}
			if m.UserID != nil {
				// the user leaves the cards placed on this board, except a card also placed on another board they are a
				// member of (directly or through a group): they still work on it there
				_, err := tx.ExecContext(e.Ctx, `DELETE FROM card_memberships WHERE user_id=?1 AND card_id IN (SELECT card_id FROM card_placements WHERE board_id=?2)
					AND card_id NOT IN (SELECT p.card_id FROM card_placements p WHERE p.board_id<>?2 AND (
						p.board_id IN (SELECT board_id FROM board_memberships WHERE user_id=?1) OR
						p.board_id IN (SELECT bm.board_id FROM board_memberships bm JOIN user_group_members gm ON gm.group_id=bm.group_id WHERE gm.user_id=?1)))`, *m.UserID, m.BoardID)
				return err
			}
			return nil
		})
		if err != nil {
			return m, err
		}
		emit(e, "boardMembershipDelete", "boardMembership.deleted", m.BoardID, map[string]any{"id": in.ID})
		return m, nil
	})
}
