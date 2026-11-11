package auth

import (
	"github.com/crueber/weave/internal/ops"
)

// Directory visibility. Guests and workers (agents) only learn about accounts they work with:
// users and groups are visible to them when they are themselves or share at least one board
// (directly, through a group, or as project manager). Admins, project owners and board users
// keep seeing everyone. The names shown in activity, comments and notifications are not
// affected: they are attached to content the caller can already read.

// directoryRestricted reports whether the caller may only see accounts that share a board.
func directoryRestricted(e *ops.Env) bool {
	return e.User != nil && (e.User.Role == "guest" || e.User.Role == "worker")
}

// sharedBoardsSQL selects the ids of boards the user is a member of (user or group membership)
// or manages; %s placeholders take the user id four times.
const sharedBoardsSQL = `SELECT board_id FROM board_memberships WHERE user_id=? OR group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?)
	UNION SELECT b.id FROM boards b JOIN project_managers pm ON pm.project_id=b.project_id WHERE pm.user_id=?`

// visibleUsersSQL is a sub-select of user ids the restricted caller may see, with its arguments.
func visibleUsersSQL(e *ops.Env) (string, []any) {
	me := e.User.UserID
	q := `SELECT ? UNION SELECT user_id FROM board_memberships WHERE user_id IS NOT NULL AND board_id IN (` + sharedBoardsSQL + `)
	UNION SELECT gm.user_id FROM board_memberships bm JOIN user_group_members gm ON gm.group_id=bm.group_id WHERE bm.board_id IN (` + sharedBoardsSQL + `)
	UNION SELECT pm.user_id FROM boards b JOIN project_managers pm ON pm.project_id=b.project_id WHERE b.id IN (` + sharedBoardsSQL + `)`
	return q, []any{me, me, me, me, me, me, me, me, me, me}
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

func init() {
	// Wrap the group ops after they are registered (this file sorts last, so its init runs last).
	if o := ops.Get("group.list"); o != nil {
		inner := o.Handler
		o.Handler = func(e *ops.Env, in any) (any, error) {
			out, err := inner(e, in)
			if err != nil || !directoryRestricted(e) {
				return out, err
			}
			l, ok := out.(GroupList)
			if !ok {
				return out, err
			}
			return restrictGroupList(e, l), nil
		}
	}
	if o := ops.Get("group.get"); o != nil {
		inner := o.Handler
		o.Handler = func(e *ops.Env, in any) (any, error) {
			out, err := inner(e, in)
			if err != nil || !directoryRestricted(e) {
				return out, err
			}
			g, ok := out.(*Group)
			if !ok || g == nil {
				return out, err
			}
			if !visibleGroupIDs(e)[g.ID] {
				return nil, ops.NotFound("group")
			}
			return restrictGroup(g, visibleUserIDs(e)), nil
		}
	}
}

func restrictGroupList(e *ops.Env, l GroupList) GroupList {
	groups, users := visibleGroupIDs(e), visibleUserIDs(e)
	out := GroupList{Items: []*Group{}}
	for _, g := range l.Items {
		if groups[g.ID] {
			out.Items = append(out.Items, restrictGroup(g, users))
		}
	}
	return out
}

// restrictGroup returns a copy of g listing only the members the caller may see.
func restrictGroup(g *Group, users map[string]bool) *Group {
	c := *g
	c.Members, c.MemberIDs = []*User{}, []string{}
	for _, m := range g.Members {
		if users[m.ID] {
			c.Members = append(c.Members, m)
			c.MemberIDs = append(c.MemberIDs, m.ID)
		}
	}
	return &c
}
