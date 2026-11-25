package authz

import "github.com/crueber/weave/internal/ops"

// Capabilities: what the caller may do on a page, computed from the SAME predicates the ops enforce.
//
// The rule for every page: a button, link, form or section is shown iff the op behind it would allow the caller.
// UI code asks these structs and never re-derives a permission (no `Role == "admin"`, no `Level >= X` in handlers or
// templates for anything listed here). Each field names the op(s) whose check it mirrors; when an op's rule changes,
// change the predicate here (or the shared helper both call) and the capability matrix test
// (internal/server/caps_matrix_test.go) proves the page and the op still agree.
//
// System powers (the admin console, global webhooks, project recovery) are separate fields from content access:
// the admin role carries NO content access (BoardLevel/ProjectLevel know nothing about it), so an administrator's
// content capabilities are exactly those of a person with the same memberships.

// SystemAdmin reports whether the caller is a human system administrator: the admin console, instance settings,
// global/project webhooks, user and group administration, agent ACCOUNT control and project recovery. It never
// implies content access; content access always comes from BoardLevel/ProjectLevel (membership and ownership).
// It is the one place the role name "admin" is turned into a permission: every caller is listed, with its reason,
// in the allowlist of TestNoAdminContentBypass (internal/authz/nobypass_test.go).
func SystemAdmin(e *ops.Env) bool {
	return e.User != nil && !e.User.IsAgent && e.User.Role == "admin"
}

// HomeCaps is what the home page (project list) and the account menu may offer.
type HomeCaps struct {
	SignedIn         bool
	CanCreateProject bool // project.create (CanCreateProject)
	AdminConsole     bool // the /admin pages (AuthAdmin ops; SystemAdmin)
	Restricted       bool // guest or worker: a restricted directory, never creates projects or boards
}

// Home computes HomeCaps for the caller.
func Home(e *ops.Env) HomeCaps {
	h := HomeCaps{SignedIn: e.User != nil, CanCreateProject: CanCreateProject(e), AdminConsole: SystemAdmin(e)}
	h.Restricted = e.User != nil && (e.User.Role == "guest" || e.User.Role == "worker")
	return h
}

// ProjectCaps is what a project page and its settings may offer.
type ProjectCaps struct {
	ProjectID string
	Level     Level
	CanView   bool // project.get, project.favorite, agent.prompt (scope project)
	// CanManage gates the settings page as a whole and every project.* write: project.update (General, Background,
	// Agents prompt text), project.delete, project.manager.add/remove, project base custom fields, background images,
	// project labels, board.move (board order).
	CanManage             bool
	CanCreateBoard        bool // board.create / board.instantiate / board.import and the template gallery (CanCreateBoard)
	CanFavorite           bool // project.favorite / unfavorite
	CanConnectAgent       bool // agent.prompt with scope project
	CanRemoveOwnerManager bool // project.manager.remove of the owning manager (CanRemoveOwningManager)
}

// Project computes ProjectCaps for the caller.
func Project(e *ops.Env, projectID string) ProjectCaps {
	l := ProjectLevel(e, projectID)
	pc := ProjectCaps{ProjectID: projectID, Level: l, CanView: l >= View, CanManage: l >= Manage}
	pc.CanFavorite = pc.CanView && e.User != nil
	pc.CanConnectAgent = pc.CanView && e.User != nil
	pc.CanCreateBoard = pc.CanView && CanCreateBoard(e, projectID)
	pc.CanRemoveOwnerManager = pc.CanManage && CanRemoveOwningManager(e, projectID)
	return pc
}

// CanRemoveOwningManager is the extra rule of project.manager.remove for the project's owning manager (its
// creator, projects.owner_manager_id): the owning manager may step down themself, and a co-manager may remove the
// owning manager only once that account no longer counts (deactivated, deleted, an agent or a guest: the same
// "active manager" rule as project recovery). While the owner is active, nobody else (an administrator included)
// can push them out of their own project. The caller still needs Manage on the project, and the last manager can
// never be removed. An ownerless project (no active manager at all) goes through project recovery instead.
func CanRemoveOwningManager(e *ops.Env, projectID string) bool {
	if e.User == nil {
		return false
	}
	var uid string
	var deleted, deactivated, agent bool
	var role string
	if e.DB.QueryRowContext(e.Ctx, `SELECT u.id,u.deleted_at IS NOT NULL,u.is_deactivated,u.is_agent,u.role FROM projects p
		JOIN project_managers m ON m.id=p.owner_manager_id JOIN users u ON u.id=m.user_id WHERE p.id=?`, projectID).
		Scan(&uid, &deleted, &deactivated, &agent, &role) != nil {
		return true // no owning manager recorded (or its account is gone): nothing to protect
	}
	if uid == e.User.UserID {
		return true
	}
	return deleted || deactivated || agent || role == "guest"
}

// ProjectLevels is ProjectLevel for many projects in a constant number of queries (same rules; unknown or invisible
// ids are absent). The home page uses it so its "My projects" grouping follows the ops.
func ProjectLevels(e *ops.Env, projectIDs []string) map[string]Level {
	out := map[string]Level{}
	if len(projectIDs) == 0 || e.User == nil {
		return out
	}
	in, args := placeholders(projectIDs)
	margs := append(append([]any{}, args...), e.User.UserID)
	if rows, err := e.DB.QueryContext(e.Ctx, `SELECT project_id FROM project_managers WHERE project_id IN (`+in+`) AND user_id=?`, margs...); err == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				out[id] = Manage
			}
		}
		rows.Close()
	}
	vargs := append(append([]any{}, args...), e.User.UserID, e.User.UserID)
	if rows, err := e.DB.QueryContext(e.Ctx, `SELECT DISTINCT b.project_id FROM boards b JOIN board_memberships m ON m.board_id=b.id WHERE b.project_id IN (`+in+`) AND
		(m.user_id=? OR m.group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?))`, vargs...); err == nil {
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil && out[id] < View {
				out[id] = View
			}
		}
		rows.Close()
	}
	return out
}

// BoardCaps is what a board page, its settings and its column menus may offer.
type BoardCaps struct {
	BoardID, ProjectID string
	Level              Level
	Public             bool // the board has a public link (anonymous View)
	CanView            bool // board.get, list.list, card.get, board.export
	CanComment         bool // comment.create
	CanEdit            bool // board.update (except agentPrompt), list.*, card.* (see CardCaps for the worker rule)
	CanManage          bool // the Manage-only board ops listed below
	CanRestoreArchived bool // card.restore.bulk (RequireBoard Edit; no worker rule)
	// IsMember: a real member (project manager, direct or group membership), not someone who merely sees a
	// public board. The settings page, the Inbox and draft cards are for members only.
	IsMember bool
	IsOwner  bool // a direct user membership with role owner (Manage on this board only)
	// Settings page and its sections.
	CanOpenSettings     bool // GET /boards/{id}/settings (IsBoardMember)
	CanEditLabels       bool // label.create / labelgroup.create and editing the listed labels (RequireBoardLabels)
	CanManageMembers    bool // board.member.add/update/remove
	CanShare            bool // board.setPublic (public link)
	CanPublishTemplate  bool // board.template.publish
	CanApplyTemplate    bool // board.template.apply (board Manage)
	CanManageAgents     bool // agent.create for this board, agent.access grants on it
	CanManageWebhooks   bool // webhook.create/update/delete with this board's scope (RequireWebhookScope)
	CanManageNotify     bool // board notification services (notification-service.* with boardId)
	CanEditPrompt       bool // board.update agentPrompt
	CanDelete           bool // board.delete
	CanReorder          bool // board.move (project Manage: the order belongs to the project)
	CanCreateBoardHere  bool // board.create/import in the board's project (the "Import" row of Templates & export)
	CanWatch            bool // board.subscribe / list.subscribe (signed in, View)
	CanConnectAgent     bool // agent.prompt with scope board/list (signed in, View)
	CanSeeInbox         bool // the Inbox column and draft.* (members; the board must also have its Inbox on)
	CanUseSavedFilters  bool // filter.create/update/delete (signed in, View; filters are per person)
	CanRemoveSelfMember bool // board.member.remove of one's own membership (any member may leave)
}

// Board computes BoardCaps for the caller.
func Board(e *ops.Env, boardID string) BoardCaps {
	bc := BoardCaps{BoardID: boardID}
	var useProjectLabels bool
	if e.DB.QueryRowContext(e.Ctx, `SELECT b.project_id,b.is_public,COALESCE(p.use_project_labels,0) FROM boards b LEFT JOIN projects p ON p.id=b.project_id WHERE b.id=?`, boardID).
		Scan(&bc.ProjectID, &bc.Public, &useProjectLabels) != nil {
		return bc
	}
	l := BoardLevel(e, boardID)
	bc.Level = l
	bc.CanView, bc.CanComment, bc.CanEdit, bc.CanManage = l >= View, l >= Comment, l >= Edit, l >= Manage
	bc.CanRestoreArchived = l >= Edit
	if !bc.CanView {
		return bc
	}
	signedIn := e.User != nil
	bc.IsMember = IsBoardMember(e, boardID)
	if signedIn {
		var n int
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM board_memberships WHERE board_id=? AND user_id=? AND role=?`, boardID, e.User.UserID, RoleOwner).Scan(&n)
		bc.IsOwner = n > 0
	}
	bc.CanOpenSettings = bc.IsMember
	bc.CanEditLabels = bc.CanEdit && (!useProjectLabels || ProjectLevel(e, bc.ProjectID) >= Manage)
	bc.CanManageMembers, bc.CanShare, bc.CanPublishTemplate = bc.CanManage, bc.CanManage, bc.CanManage
	bc.CanApplyTemplate = bc.CanManage
	bc.CanManageAgents = bc.CanManage && signedIn && !e.User.IsAgent
	bc.CanManageWebhooks = RequireWebhookScope(e, "", boardID) == nil
	bc.CanManageNotify, bc.CanEditPrompt, bc.CanDelete = bc.CanManage, bc.CanManage, bc.CanManage
	bc.CanReorder = bc.CanManage && ProjectLevel(e, bc.ProjectID) >= Manage
	bc.CanCreateBoardHere = signedIn && CanCreateBoard(e, bc.ProjectID)
	bc.CanWatch, bc.CanConnectAgent, bc.CanUseSavedFilters = signedIn, signedIn, signedIn
	bc.CanSeeInbox = signedIn && bc.IsMember
	bc.CanRemoveSelfMember = signedIn && bc.IsMember
	return bc
}

// CanEditBoardLabels is the rule of label.create and labelgroup.create on a board: Edit on the board, and when its
// project uses project-wide labels (projects.use_project_labels), Manage on the project (the labels are the
// project's). Existing labels follow the same split: a board label needs board Edit, a project label project Manage.
func CanEditBoardLabels(e *ops.Env, boardID string) bool {
	return RequireBoardLabels(e, boardID) == nil
}

// RequireBoardLabels is CanEditBoardLabels as an error (not_found without View, forbidden otherwise).
func RequireBoardLabels(e *ops.Env, boardID string) error {
	if err := RequireBoard(e, boardID, Edit); err != nil {
		return err
	}
	var pid string
	var use bool
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT b.project_id,COALESCE(p.use_project_labels,0) FROM boards b JOIN projects p ON p.id=b.project_id WHERE b.id=?`, boardID).Scan(&pid, &use)
	if use {
		return RequireProject(e, pid, Manage)
	}
	return nil
}

// CardCaps is what the card sheet and a card's tile may offer.
type CardCaps struct {
	CardID, BoardID string
	Level           Level // the caller's level on the card's board
	CanView         bool  // card.get
	CanComment      bool  // comment.create
	// CanEdit is the card's own fields (name, description, dates, labels, members, move, archive...: the cards domain
	// ops, RequireCardEdit), which includes the worker rule.
	CanEdit bool
	// CanEditExtras is board Edit: tasks, attachments, custom fields and time tracking (the cardextras ops) and claims
	// (card.claim|release|renew), which do not follow the worker rule.
	CanEditExtras bool
	CanManage     bool
	// CanPlace is card.place ("Add to board..."): the card may be edited (CanEdit) and there is at least one board to
	// put it on (PlaceTargets). Only CardPlacing fills it: Card leaves it false, since it costs a query over the
	// caller's boards and Card runs per tile.
	CanPlace bool
	// boards: the viewable placements and the caller's level on each (CanRemoveFromBoard).
	boards map[string]Level
}

// CanRemoveFromBoard is card.unplace ("Remove from board") on boardID as the UI offers it: the op's own rule
// (RequireCardOn Edit: the card is placed there and the caller edits that board) and the card stays on another board
// the caller can SEE. The op counts every placement, hidden ones included; the UI deliberately counts only visible
// ones, so it never reveals (by offering the action) that the card is on a board the caller cannot see. For a caller
// who sees two or more placements the two agree.
func (c CardCaps) CanRemoveFromBoard(boardID string) bool {
	return c.boards[boardID] >= Edit && len(c.boards) > 1
}

// CardPlacing is Card plus CanPlace (the card sheet's Boards row and the "Add to board..." picker).
func CardPlacing(e *ops.Env, cardID string) CardCaps {
	cc := Card(e, cardID)
	cc.CanPlace = cc.CanEdit && len(PlaceTargets(e, cardID)) > 0
	return cc
}

// Card computes CardCaps for the caller (through CardAccessFor: the context board is the home when viewable, the
// level the maximum over viewable placements). BoardID is "" when the caller cannot view the card.
func Card(e *ops.Env, cardID string) CardCaps {
	cc := CardCaps{CardID: cardID}
	a, _ := cardAccess(e, cardID, "")
	if a.Level == None {
		return cc
	}
	l := a.Level
	cc.BoardID, cc.Level, cc.boards = a.BoardID, l, a.Boards
	cc.CanView, cc.CanComment, cc.CanEditExtras, cc.CanManage = l >= View, l >= Comment, l >= Edit, l >= Manage
	cc.CanEdit = cardEditable(a)
	return cc
}

// cardEditable: Edit on a placement the caller can view; or a worker who is a member of the card and can view it
// (workers do the work on cards they are assigned to even with a viewer membership; CardAccess.WorkerMember).
func cardEditable(a CardAccess) bool {
	return a.Level >= Edit || (a.Level >= View && a.WorkerMember)
}

// RequireCardEdit is the rule of every cards-domain write on an existing card (see CardCaps.CanEdit): not_found when
// the caller cannot see the card, forbidden when they see it but may not edit it. It returns the context board (the
// card's home board when viewable).
func RequireCardEdit(e *ops.Env, cardID string) (string, error) {
	a, _ := cardAccess(e, cardID, "")
	if a.Level == None {
		return "", ops.NotFound("card")
	}
	if !cardEditable(a) {
		return "", ops.Forbidden()
	}
	return a.BoardID, nil
}
