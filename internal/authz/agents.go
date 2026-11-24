package authz

import "github.com/crueber/weave/internal/ops"

// AI agent accounts (users.is_agent=1) are service accounts created by a person. The rules:
//
//   - Creating one, and granting it access to a board, needs Manage on that board (RequireBoard(e, id, Manage));
//     the grant itself is a normal board membership (editor | viewer | commenter), never project-wide.
//   - The agent's creator (users.created_by) may manage the account: rename, deactivate, rotate its key, delete it
//     and revoke its access to any board.
//   - A caller who manages every board the agent holds a membership on (at least one) may manage it too, so a
//     project manager can clean up agents on their boards.
//   - System administrators manage every agent ACCOUNT (rename, deactivate, rotate its key, delete it), but grants
//     and revokes still need Manage on the board (or, for a revoke, being the creator), and they see the agent's
//     board access only on boards they manage, like everyone.
//   - Agents never manage agents.

// agentFacts loads the creator and the boards an agent holds a membership on. ok is false for unknown or deleted
// agent ids (and for human accounts).
func agentFacts(e *ops.Env, agentID string) (createdBy string, boards []string, ok bool) {
	var cb *string
	if e.DB.QueryRowContext(e.Ctx, `SELECT created_by FROM users WHERE id=? AND is_agent=1 AND deleted_at IS NULL`, agentID).Scan(&cb) != nil {
		return "", nil, false
	}
	if cb != nil {
		createdBy = *cb
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT board_id FROM board_memberships WHERE user_id=?`, agentID)
	if err != nil {
		return createdBy, nil, true
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		if rows.Scan(&b) == nil {
			boards = append(boards, b)
		}
	}
	return createdBy, boards, true
}

// CanManageAgent reports whether the caller may manage an agent account (see the rules above).
func CanManageAgent(e *ops.Env, agentID string) bool {
	if e.User == nil || e.User.IsAgent {
		return false
	}
	createdBy, boards, ok := agentFacts(e, agentID)
	if !ok {
		return false
	}
	if SystemAdmin(e) || (createdBy != "" && createdBy == e.User.UserID) {
		return true
	}
	if len(boards) == 0 {
		return false
	}
	lv := BoardLevels(e, boards)
	for _, b := range boards {
		if lv[b] < Manage {
			return false
		}
	}
	return true
}

// CanSeeAgent reports whether the caller may see an agent account in agent.list: system administrators, the creator, and anyone who
// manages at least one board the agent holds a membership on.
func CanSeeAgent(e *ops.Env, agentID string) bool {
	if e.User == nil || e.User.IsAgent {
		return false
	}
	createdBy, boards, ok := agentFacts(e, agentID)
	if !ok {
		return false
	}
	if SystemAdmin(e) || (createdBy != "" && createdBy == e.User.UserID) {
		return true
	}
	for _, l := range BoardLevels(e, boards) {
		if l >= Manage {
			return true
		}
	}
	return false
}

// RequireAgentManage is CanManageAgent as an error: not_found when the caller may not even see the agent,
// forbidden when they see it but may not manage it.
func RequireAgentManage(e *ops.Env, agentID string) error {
	if CanManageAgent(e, agentID) {
		return nil
	}
	if CanSeeAgent(e, agentID) {
		return ops.Forbidden()
	}
	return ops.NotFound("agent")
}
