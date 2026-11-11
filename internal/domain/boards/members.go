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
	Email      string  `json:"email,omitempty"`
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
	return m, err
}

type MemberListIn struct {
	BoardID string `path:"boardId"`
}
type MemberAddIn struct {
	BoardID    string `path:"boardId"`
	UserID     string `json:"userId" desc:"user to add (exclusive with groupId)"`
	GroupID    string `json:"groupId" desc:"user group to add (exclusive with userId)"`
	Role       string `json:"role" desc:"editor (default) | viewer"`
	CanComment *bool  `json:"canComment" desc:"viewers only: may comment"`
}
type MemberUpdateIn struct {
	ID         string `path:"memberId"`
	Role       string `json:"role" desc:"editor | viewer"`
	CanComment *bool  `json:"canComment" desc:"viewers only: may comment"`
}
type MemberRefIn struct {
	ID string `path:"memberId"`
}

func registerMemberOps() {
	ops.Register(ops.Op{Name: "board.member.list", Summary: "List board memberships (users and groups)", Method: "GET", Path: "/boards/{boardId}/members",
		UI: ui("GET", "/boards/{boardId}/members")}, func(e *ops.Env, in MemberListIn) ([]Member, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.View); err != nil {
			return nil, err
		}
		return membersOf(e, in.BoardID)
	})
	ops.Register(ops.Op{Name: "board.member.add", Summary: "Add a user or user group to a board (project manager)", Method: "POST", Path: "/boards/{boardId}/members",
		UI: ui("POST", "/boards/{boardId}/members")}, func(e *ops.Env, in MemberAddIn) (Member, error) {
		if err := authz.RequireBoard(e, in.BoardID, authz.Manage); err != nil {
			return Member{}, err
		}
		if (in.UserID == "") == (in.GroupID == "") {
			return Member{}, ops.Invalid("exactly one of userId or groupId is required")
		}
		role := in.Role
		if role == "" {
			role = "editor"
		}
		if role != "editor" && role != "viewer" {
			return Member{}, ops.Invalid("role must be editor or viewer")
		}
		var cc any
		if role == "viewer" && in.CanComment != nil {
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
		emit(e, "boardMembershipCreate", "boardMembership.created", in.BoardID, map[string]any{"id": id, "userId": in.UserID, "groupId": in.GroupID})
		return getMember(e, id)
	})
	ops.Register(ops.Op{Name: "board.member.update", Summary: "Change a membership's role / comment permission (project manager)", Method: "PATCH", Path: "/board-members/{memberId}",
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
			if in.Role != "editor" && in.Role != "viewer" {
				return m, ops.Invalid("role must be editor or viewer")
			}
			role = in.Role
		}
		var cc any
		if role == "viewer" {
			if in.CanComment != nil {
				cc = boolInt(*in.CanComment)
			} else if m.CanComment != nil {
				cc = boolInt(*m.CanComment)
			}
		}
		if _, err := e.DB.ExecContext(e.Ctx, `UPDATE board_memberships SET role=?,can_comment=? WHERE id=?`, role, cc, in.ID); err != nil {
			return m, err
		}
		emit(e, "boardMembershipUpdate", "boardMembership.updated", m.BoardID, map[string]any{"id": in.ID})
		return getMember(e, in.ID)
	})
	ops.Register(ops.Op{Name: "board.member.remove", Summary: "Remove a membership (project manager, or the member themself)", Method: "DELETE", Path: "/board-members/{memberId}",
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
		err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(e.Ctx, `DELETE FROM board_memberships WHERE id=?`, in.ID); err != nil {
				return err
			}
			if m.UserID != nil {
				_, err := tx.ExecContext(e.Ctx, `DELETE FROM card_memberships WHERE user_id=? AND card_id IN (SELECT id FROM cards WHERE board_id=?)`, *m.UserID, m.BoardID)
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
