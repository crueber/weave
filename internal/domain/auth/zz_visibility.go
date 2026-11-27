package auth

import (
	"github.com/crueber/weave/internal/ops"
)

// Directory visibility. Guests and workers (agents) only learn about accounts they work with:
// users and groups are visible to them when they are themselves or share at least one board
// (directly, through a group, or as project manager). Admins and board users
// keep seeing everyone. The names shown in activity, comments and notifications are not
// affected: they are attached to content the caller can already read.

// directoryRestricted reports whether the caller may only see accounts that share a board.
func directoryRestricted(e *ops.Env) bool {
	return e.User != nil && (e.User.Role == "guest" || e.User.Role == "worker")
}

// sharedBoardsSQL selects the ids of boards the user is a member of (user or group membership)
// or manages; takes the user id three times.
const sharedBoardsSQL = `SELECT board_id FROM board_memberships WHERE user_id=? OR group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?)
	UNION SELECT b.id FROM boards b JOIN project_managers pm ON pm.project_id=b.project_id WHERE pm.user_id=?`

// visibleUsersSQL is a sub-select of user ids the restricted caller may see, with its arguments.
func visibleUsersSQL(e *ops.Env) (string, []any) {
	me := e.User.UserID
	q := `WITH sb(board_id) AS (` + sharedBoardsSQL + `)
	SELECT ? UNION SELECT user_id FROM board_memberships WHERE user_id IS NOT NULL AND board_id IN (SELECT board_id FROM sb)
	UNION SELECT gm.user_id FROM board_memberships bm JOIN user_group_members gm ON gm.group_id=bm.group_id WHERE bm.board_id IN (SELECT board_id FROM sb)
	UNION SELECT pm.user_id FROM boards b JOIN project_managers pm ON pm.project_id=b.project_id WHERE b.id IN (SELECT board_id FROM sb)`
	return q, []any{me, me, me, me}
}

func visibleUserIDs(e *ops.Env) map[string]bool {
	out := map[string]bool{e.User.UserID: true}
	q, args := visibleUsersSQL(e)
	rows, err := e.DB.QueryContext(e.Ctx, q, args...)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}

// visibleGroupIDs: groups the caller belongs to or that hold a membership on a board they share.
func visibleGroupIDs(e *ops.Env) map[string]bool {
	out := map[string]bool{}
	me := e.User.UserID
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT group_id FROM user_group_members WHERE user_id=?
		UNION SELECT group_id FROM board_memberships WHERE group_id IS NOT NULL AND board_id IN (`+sharedBoardsSQL+`)`, me, me, me, me)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out
}
