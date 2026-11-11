// Package authz holds every permission rule. Domain packages must call these
// helpers rather than inventing their own checks.
//
// Rules: admin = full access. projectOwner may create projects. A project
// manager is editor on every board of the project. Board access comes from a
// direct membership or a user-group membership (role editor|viewer, can_comment
// overrides viewer commenting). Public boards give anonymous View. guest/worker
// roles are restricted: guests never see boards without a membership and cannot
// create projects; workers can edit cards they are members of when board role is viewer.
package authz

import (
	"github.com/crueber/weave/internal/ops"
)

type Level int

const (
	None Level = iota
	View
	Comment
	Edit
	Manage // project manager / admin
)

func BoardLevel(e *ops.Env, boardID string) Level {
	var projectID string
	var public bool
	if e.DB.QueryRowContext(e.Ctx, `SELECT project_id,is_public FROM boards WHERE id=?`, boardID).Scan(&projectID, &public) != nil {
		return None
	}
	if e.User == nil {
		if public {
			return View
		}
		return None
	}
	if e.User.Role == "admin" {
		return Manage
	}
	var n int
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM project_managers WHERE project_id=? AND user_id=?`, projectID, e.User.UserID).Scan(&n)
	if n > 0 {
		return Manage
	}
	best := None
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT role, can_comment FROM board_memberships WHERE board_id=? AND
		(user_id=? OR group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?))`, boardID, e.User.UserID, e.User.UserID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var role string
			var cc *bool
			_ = rows.Scan(&role, &cc)
			l := View
			if role == "editor" {
				l = Edit
			} else if cc != nil && *cc {
				l = Comment
			}
			if l > best {
				best = l
			}
		}
	}
	if best == None && public {
		best = View
	}
	return best
}

// IsBoardMember reports whether the caller has real access to the board (admin, project manager, or a direct/group
// membership), as opposed to merely being able to view it because it is public.
func IsBoardMember(e *ops.Env, boardID string) bool {
	if e.User == nil {
		return false
	}
	if e.User.Role == "admin" {
		return BoardLevel(e, boardID) != None
	}
	var n int
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM boards b WHERE b.id=? AND (
		EXISTS(SELECT 1 FROM project_managers pm WHERE pm.project_id=b.project_id AND pm.user_id=?) OR
		EXISTS(SELECT 1 FROM board_memberships m WHERE m.board_id=b.id AND (m.user_id=? OR m.group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?))))`,
		boardID, e.User.UserID, e.User.UserID, e.User.UserID).Scan(&n)
	return n > 0
}

// RequireBoard returns a not-found error (hiding existence) when below min.
func RequireBoard(e *ops.Env, boardID string, min Level) error {
	l := BoardLevel(e, boardID)
	if l == None {
		return ops.NotFound("board")
	}
	if l < min {
		return ops.Forbidden()
	}
	return nil
}

func BoardOfCard(e *ops.Env, cardID string) (string, error) {
	var b string
	if e.DB.QueryRowContext(e.Ctx, `SELECT board_id FROM cards WHERE id=?`, cardID).Scan(&b) != nil {
		return "", ops.NotFound("card")
	}
	return b, nil
}

func RequireCard(e *ops.Env, cardID string, min Level) (boardID string, err error) {
	if boardID, err = BoardOfCard(e, cardID); err != nil {
		return
	}
	return boardID, RequireBoard(e, boardID, min)
}

func ProjectLevel(e *ops.Env, projectID string) Level {
	if e.User == nil {
		return None
	}
	if e.User.Role == "admin" {
		return Manage
	}
	var n int
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM project_managers WHERE project_id=? AND user_id=?`, projectID, e.User.UserID).Scan(&n)
	if n > 0 {
		return Manage
	}
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM boards b JOIN board_memberships m ON m.board_id=b.id WHERE b.project_id=? AND
		(m.user_id=? OR m.group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?))`, projectID, e.User.UserID, e.User.UserID).Scan(&n)
	if n > 0 {
		return View
	}
	return None
}

func RequireProject(e *ops.Env, projectID string, min Level) error {
	l := ProjectLevel(e, projectID)
	if l == None {
		return ops.NotFound("project")
	}
	if l < min {
		return ops.Forbidden()
	}
	return nil
}

// CanCreateProject: admin or projectOwner.
func CanCreateProject(e *ops.Env) bool {
	return e.User != nil && (e.User.Role == "admin" || e.User.Role == "projectOwner")
}

// RoomAllowed decides whether the caller may subscribe to an SSE room.
func RoomAllowed(e *ops.Env, room string) bool {
	var kind, id string
	for i := 0; i < len(room); i++ {
		if room[i] == ':' {
			kind, id = room[:i], room[i+1:]
		}
	}
	switch kind {
	case "board":
		return BoardLevel(e, id) >= View
	case "project":
		return ProjectLevel(e, id) >= View
	case "user":
		return e.User != nil && e.User.UserID == id
	}
	return room == "global" && e.User != nil
}
