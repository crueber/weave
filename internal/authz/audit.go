package authz

import "github.com/crueber/weave/internal/ops"

// The admin audit log (`audit.list`, Admin console > Audit log): who rotated an agent's key, added a person to a group
// or unlinked someone's SSO identity, and when. It is system administration: it names people, agents and groups, never
// a project, board or card, and only a human administrator reads it (an agent account with the admin role does not).

// AuditLogAdmin reports whether the caller may read the admin audit log.
func AuditLogAdmin(e *ops.Env) bool { return SystemAdmin(e) }

// RequireAuditLog is AuditLogAdmin as an error: anonymous callers are unauthorized, everyone else who is not a human
// admin is forbidden (the op is AuthAdmin, so the route itself is no secret).
func RequireAuditLog(e *ops.Env) error {
	if e.User == nil {
		return ops.Unauthorized()
	}
	if !AuditLogAdmin(e) {
		return ops.Forbidden()
	}
	return nil
}
