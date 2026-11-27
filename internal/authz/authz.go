// Package authz holds every permission rule. Domain packages must call these
// helpers rather than inventing their own checks.
//
// Rules: content access comes ONLY from ownership and membership. The admin role is
// system administration (users, groups, instance settings, recovery: SystemAdmin) and
// gives no access to any project, board or card by itself; an admin sees exactly what
// they manage or are a member of, like everyone else (TestNoAdminContentBypass guards
// this). Any signed-in human with role admin or boardUser may create a project and
// becomes its manager (CanCreateProject; workers, guests and agent accounts may not). A project manager manages every board of the project.
// Board access comes from a direct membership or a user-group membership (role
// owner|editor|viewer, can_comment overrides viewer commenting). A board owner (a
// user membership with role owner) manages THAT board only: not the project and not
// its other boards. Any membership on a board of a project gives View on the
// project. Who may create boards in a project is the project's boardCreation setting
// (CanCreateBoard): owners = project managers only, members = also any project member
// who may create projects; a creator who is not a project manager becomes the new
// board's owner. Public boards give anonymous View. guest/worker roles are restricted:
// guests never see boards without a membership; neither may create projects or
// boards; workers can edit cards they are members of when board role is viewer.
package authz

import (
	"strings"

	"github.com/crueber/weave/internal/ops"
)

type Level int

const (
	None Level = iota
	View
	Comment
	Edit
	Manage // project manager, or the owner of that one board
)

// Board membership roles (board_memberships.role). A commenter is a viewer with can_comment.
const (
	RoleOwner  = "owner"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// MembershipLevel maps one board membership row to the level it grants: owner = Manage (that board only),
// editor = Edit, viewer = View (Comment with can_comment).
func MembershipLevel(role string, canComment *bool) Level {
	switch role {
	case RoleOwner:
		return Manage
	case RoleEditor:
		return Edit
	}
	if canComment != nil && *canComment {
		return Comment
	}
	return View
}

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
			if l := MembershipLevel(role, cc); l > best {
				best = l
			}
		}
	}
	if best == None && public {
		best = View
	}
	return best
}

// idChunk bounds the number of ids bound in a single SQLite IN (...) clause.
const idChunk = 500

func dedupeIDs(ids []string) []string {
	if len(ids) <= 1 {
		return ids
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; !ok {
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out
}

// BoardLevels is BoardLevel for many boards in a constant number of queries (same rules; unknown ids are absent).
// Pages that summarise many boards at once (the project page tiles, board.stats) use it so the cost does not grow
// with the number of boards.
func BoardLevels(e *ops.Env, boardIDs []string) map[string]Level {
	out := map[string]Level{}
	boardIDs = dedupeIDs(boardIDs)
	if len(boardIDs) == 0 {
		return out
	}
	type row struct {
		project string
		public  bool
	}
	boards := make(map[string]row, len(boardIDs))
	for i := 0; i < len(boardIDs); i += idChunk {
		in, args := placeholders(boardIDs[i:min(i+idChunk, len(boardIDs))])
		rows, err := e.DB.QueryContext(e.Ctx, `SELECT id,project_id,is_public FROM boards WHERE id IN (`+in+`)`, args...)
		if err != nil {
			return out
		}
		for rows.Next() {
			var id string
			var r row
			if rows.Scan(&id, &r.project, &r.public) == nil {
				boards[id] = r
			}
		}
		rows.Close()
	}
	if len(boards) == 0 {
		return out
	}
	if e.User == nil {
		for id, r := range boards {
			if r.public {
				out[id] = View
			}
		}
		return out
	}
	managed := map[string]bool{}
	if rows, err := e.DB.QueryContext(e.Ctx, `SELECT project_id FROM project_managers WHERE user_id=?`, e.User.UserID); err == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				managed[p] = true
			}
		}
		rows.Close()
	}
	var unmanaged []string
	for id, r := range boards {
		if managed[r.project] {
			out[id] = Manage
		} else {
			unmanaged = append(unmanaged, id)
		}
	}
	best := map[string]Level{}
	for i := 0; i < len(unmanaged); i += idChunk {
		in, args := placeholders(unmanaged[i:min(i+idChunk, len(unmanaged))])
		margs := append(append([]any{}, args...), e.User.UserID, e.User.UserID)
		if rows, err := e.DB.QueryContext(e.Ctx, `SELECT board_id, role, can_comment FROM board_memberships WHERE board_id IN (`+in+`) AND
			(user_id=? OR group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?))`, margs...); err == nil {
			for rows.Next() {
				var id, role string
				var cc *bool
				if rows.Scan(&id, &role, &cc) != nil {
					continue
				}
				if l := MembershipLevel(role, cc); l > best[id] {
					best[id] = l
				}
			}
			rows.Close()
		}
	}
	for _, id := range unmanaged {
		r := boards[id]
		switch {
		case best[id] != None:
			out[id] = best[id]
		case r.public:
			out[id] = View
		}
	}
	return out
}

// ViewersAmong returns the users among userIDs who can view the board (BoardLevel >= View for each of them), in one
// query per 500 users. Same rules as BoardLevel, applied to the stored account: the user must exist, not
// be deleted and not be deactivated; then a manager of the board's project, a direct or group board membership,
// or a public board. The notification planner uses it to drop watchers who lost access.
func ViewersAmong(e *ops.Env, boardID string, userIDs []string) map[string]bool {
	out := map[string]bool{}
	userIDs = dedupeIDs(userIDs)
	if len(userIDs) == 0 {
		return out
	}
	for i := 0; i < len(userIDs); i += idChunk {
		in, args := placeholders(userIDs[i:min(i+idChunk, len(userIDs))])
		q := `SELECT u.id FROM users u JOIN boards b ON b.id=? WHERE u.id IN (` + in + `) AND u.deleted_at IS NULL AND u.is_deactivated=0 AND (
			b.is_public=1 OR
			EXISTS(SELECT 1 FROM project_managers pm WHERE pm.project_id=b.project_id AND pm.user_id=u.id) OR
			EXISTS(SELECT 1 FROM board_memberships m WHERE m.board_id=b.id AND
				(m.user_id=u.id OR m.group_id IN (SELECT group_id FROM user_group_members g WHERE g.user_id=u.id))))`
		rows, err := e.DB.QueryContext(e.Ctx, q, append([]any{boardID}, args...)...)
		if err != nil {
			return out
		}
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				out[id] = true
			}
		}
		rows.Close()
	}
	return out
}

func placeholders(ids []string) (string, []any) {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return "?" + strings.Repeat(",?", len(ids)-1), args
}

// IsBoardMember reports whether the caller has real access to the board (project manager, or a direct/group
// membership), as opposed to merely being able to view it because it is public.
func IsBoardMember(e *ops.Env, boardID string) bool {
	if e.User == nil {
		return false
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

// BoardOfCard returns the card's HOME board (cards.board_id, the mirror of the home placement; see placements.go).
// It is not an access check and, once a card is on several boards, not the only board the card is on: access goes
// through CardAccessFor / RequireCard / RequireCardOn.
func BoardOfCard(e *ops.Env, cardID string) (string, error) {
	var b string
	if e.DB.QueryRowContext(e.Ctx, `SELECT board_id FROM cards WHERE id=?`, cardID).Scan(&b) != nil {
		return "", ops.NotFound("card")
	}
	return b, nil
}

// RequireCard checks the caller's level on the card (the maximum over the placements they can view, CardAccessFor)
// and returns the context board (the home when viewable). Errors are those of the single-board check it replaced:
// "card not found" for a missing card, "board not found" (not_found) when no placement is viewable, forbidden below min.
func RequireCard(e *ops.Env, cardID string, min Level) (boardID string, err error) {
	a, exists := cardAccess(e, cardID, "")
	switch {
	case !exists:
		return "", ops.NotFound("card")
	case a.Level == None:
		return "", ops.NotFound("board")
	case a.Level < min:
		return a.BoardID, ops.Forbidden()
	}
	return a.BoardID, nil
}

func ProjectLevel(e *ops.Env, projectID string) Level {
	if e.User == nil {
		return None
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

// CanCreateProject: a signed-in human (never an agent/service account) with role admin or boardUser. Workers and
// guests may not. The creator becomes the project's manager.
func CanCreateProject(e *ops.Env) bool {
	return e.User != nil && !e.User.IsAgent && (SystemAdmin(e) || e.User.Role == "boardUser")
}

// Project board-creation policies (projects.board_creation).
const (
	BoardCreationOwners  = "owners"  // project managers only; the default
	BoardCreationMembers = "members" // also any project member who may create projects
)

// CanCreateBoardWith evaluates CanCreateBoard from an already-known ProjectLevel and projects.board_creation value.
func CanCreateBoardWith(e *ops.Env, level Level, policy string) bool {
	if level >= Manage {
		return true
	}
	return level >= View && CanCreateProject(e) && policy == BoardCreationMembers
}

func canCreateBoardAt(e *ops.Env, projectID string, level Level) bool {
	if level >= Manage {
		return true
	}
	if level < View || !CanCreateProject(e) {
		return false
	}
	var policy string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COALESCE(board_creation,'owners') FROM projects WHERE id=?`, projectID).Scan(&policy)
	return policy == BoardCreationMembers
}

// CanCreateBoard is the one rule for every path that creates a board in a project (blank board, template,
// import) and for the buttons and gallery that lead there: project Manage (a project manager) always; with
// the project's boardCreation set to members, also any project member (ProjectLevel >= View, i.e. a board
// membership in the project) who may create projects (CanCreateProject: a human admin/boardUser; never workers,
// guests or agents).
func CanCreateBoard(e *ops.Env, projectID string) bool {
	return canCreateBoardAt(e, projectID, ProjectLevel(e, projectID))
}

// RequireCreateBoard is CanCreateBoard as an error: not_found when the caller cannot see the project at all
// (hiding its existence), forbidden when they can see it but may not create boards there.
func RequireCreateBoard(e *ops.Env, projectID string) error {
	l := ProjectLevel(e, projectID)
	if l == None {
		return ops.NotFound("project")
	}
	if !canCreateBoardAt(e, projectID, l) {
		return ops.Forbidden()
	}
	return nil
}

// RoomAllowed decides whether the caller may subscribe to an SSE room.
func RoomAllowed(e *ops.Env, room string) bool {
	kind, id, ok := strings.Cut(room, ":")
	if ok {
		switch kind {
		case "board":
			return BoardLevel(e, id) >= View
		case "project":
			return ProjectLevel(e, id) >= View
		case "user":
			return e.User != nil && e.User.UserID == id
		}
	}
	return room == "global" && e.User != nil
}

// TemplateVisible reports whether the caller may see and start boards from a published board template: everyone who
// can already see the board (its project members); the publisher; and, depending on the publisher's choice
// (boards.template_visibility), the members of one user group or every signed-in user.
func TemplateVisible(e *ops.Env, boardID string) bool {
	if e.User == nil {
		return false
	}
	var tpl bool
	var vis string
	var group, publisher *string
	if e.DB.QueryRowContext(e.Ctx, `SELECT is_template,template_visibility,template_group_id,template_publisher_id FROM boards WHERE id=?`, boardID).
		Scan(&tpl, &vis, &group, &publisher) != nil || !tpl {
		return false
	}
	if BoardLevel(e, boardID) >= View || (publisher != nil && *publisher == e.User.UserID) {
		return true
	}
	switch vis {
	case "public":
		return true
	case "group":
		var n int
		if group != nil {
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM user_group_members WHERE group_id=? AND user_id=?`, *group, e.User.UserID).Scan(&n)
		}
		return n > 0
	}
	return false
}
