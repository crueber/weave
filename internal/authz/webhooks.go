package authz

import (
	"errors"

	"github.com/crueber/weave/internal/ops"
)

// Webhook scopes (webhooks.project_id / board_id):
//
//   - global (neither set) and project-scoped webhooks: system administrators only (SystemAdmin). They are instance
//     integrations, configured in the admin console; the console names only the projects the admin can see.
//   - board-scoped webhooks: Manage on that board (a board owner or a project manager), for everyone: an
//     administrator manages a board's webhooks only as that board's owner or project manager. Every manager of the
//     board sees and manages every webhook of the board, whoever created it, so no webhook keeps delivering a
//     board's events out of sight of the people who run the board. A change by a non-admin puts the webhook under
//     the strict destination policy (webhooks.strict_dest, notify/webhook_ssrf.go).
//
// A board-scoped webhook never also carries a project scope when a non-admin manages it. Agents never manage
// webhooks. Missing access answers not_found so a webhook's existence does not leak.

// canManageBoardWebhooks is the pure predicate for board-scoped webhooks (used by Board caps).
func canManageBoardWebhooks(e *ops.Env, boardLevel Level) bool {
	return e.User != nil && !e.User.IsAgent && boardLevel >= Manage
}

// RequireWebhookScope checks that the caller may create, see or manage a webhook with this scope.
func RequireWebhookScope(e *ops.Env, projectID, boardID string) error {
	if e.User == nil || e.User.IsAgent {
		return ops.NotFound("webhook")
	}
	admin := SystemAdmin(e)
	if boardID == "" {
		if admin {
			return nil
		}
		return ops.NotFound("webhook")
	}
	if projectID != "" && !admin {
		return ops.Forbidden()
	}
	if err := RequireBoard(e, boardID, Manage); err != nil {
		var oe *ops.Error
		if errors.As(err, &oe) && oe.Status == 404 {
			return ops.NotFound("webhook")
		}
		return err
	}
	return nil
}

// RequireWebhook checks that the caller may see and manage an existing webhook: global and project webhooks need a
// system administrator, board webhooks Manage on their board (see the rules above).
func RequireWebhook(e *ops.Env, projectID, boardID string) error {
	if boardID == "" {
		if SystemAdmin(e) {
			return nil
		}
		return ops.NotFound("webhook")
	}
	if e.User == nil || e.User.IsAgent || RequireBoard(e, boardID, Manage) != nil {
		return ops.NotFound("webhook") // a board member who cannot manage it learns nothing about it either
	}
	return nil
}
