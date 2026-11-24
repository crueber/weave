package authz

import "github.com/crueber/weave/internal/ops"

// Project recovery (ownerless projects, `project.orphans|reassign|recovery.list`, Admin console > Orphaned projects).
//
// It is the narrow, audited way for an administrator to give a project whose managers are all gone (deactivated,
// deleted, or never a person who may manage) a new manager. It deliberately does NOT go through ProjectLevel: it
// works although the admin role carries no content access, and it never shows the admin the project's content (the
// listing carries no board or card names): the admin only names a new manager, and every change is audited. The
// admin may name themself, which is the one way an admin gains content access without being invited, and only for
// a project nobody can manage any more. Rules: a signed-in human admin (never an agent account); the target project must have no active manager at the time of the change (checked inside the write
// transaction by the projects domain); the target person must be eligible to manage (projects.CanBeManager).

// ProjectRecoveryAdmin reports whether the caller may use the project recovery path.
func ProjectRecoveryAdmin(e *ops.Env) bool { return SystemAdmin(e) }

// RequireProjectRecovery is ProjectRecoveryAdmin as an error: anonymous callers are unauthorized, everyone else
// who is not a human admin is forbidden (the ops are AuthAdmin, so the route itself is no secret).
func RequireProjectRecovery(e *ops.Env) error {
	if e.User == nil {
		return ops.Unauthorized()
	}
	if !ProjectRecoveryAdmin(e) {
		return ops.Forbidden()
	}
	return nil
}
