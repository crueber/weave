package auth

import (
	"context"
	"database/sql"
	"log"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// An account is bound to its first SSO identity (issuer|sub). RelinkIdentity is the only way
// to detach it: the next SSO login whose e-mail matches then binds the new identity.

// RelinkIdentity clears the stored identity of the user (by id, or by e-mail when id is empty), ends their sessions
// and writes the audit row (actor: the caller, nil for the host command; source ui|api|host), all in one transaction.
// It reports the audit entry and whether a user matched.
func RelinkIdentity(ctx context.Context, d *db.DB, userID, email string, actor *ops.Principal, source string) (AuditEntry, bool, error) {
	au := AuditEntry{Action: AuditUserRelink, TargetKind: "user", Source: source}
	found := false
	err := d.Tx(ctx, func(tx *sql.Tx) error {
		q, arg := `SELECT id,name,email FROM users WHERE id=? AND is_agent=0 AND deleted_at IS NULL`, userID
		if userID == "" {
			q, arg = `SELECT id,name,email FROM users WHERE email=? AND is_agent=0 AND deleted_at IS NULL`, email
		}
		if err := tx.QueryRowContext(ctx, q, arg).Scan(&au.TargetID, &au.TargetName, &au.TargetEmail); err != nil {
			if err == sql.ErrNoRows {
				return nil
			}
			return err
		}
		found = true
		if _, err := tx.ExecContext(ctx, `UPDATE users SET oidc_sub=NULL,updated_at=? WHERE id=?`, db.Now(), au.TargetID); err != nil {
			return err
		}
		// the old identity must not keep a live session either
		r, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, au.TargetID)
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		au.Details = map[string]any{"sessionsEnded": n}
		auditActor(ctx, tx, actor, &au)
		return writeAudit(ctx, tx, &au)
	})
	if err != nil || !found {
		return AuditEntry{}, false, err
	}
	logAudit(au)
	return au, true, nil
}

// RelinkByEmail is the break-glass path behind `weave admin relink` (direct DB access; audited with source host).
func RelinkByEmail(ctx context.Context, d *db.DB, email string) error {
	email, err := cleanEmail(email)
	if err != nil {
		return err
	}
	_, ok, err := RelinkIdentity(ctx, d, "", email, nil, "host")
	if err != nil {
		return err
	}
	if !ok {
		return ops.NotFound("user")
	}
	log.Printf("auth: AUDIT SSO identity unlinked for %s via host command; their next SSO login with this e-mail binds the identity it presents", email)
	return nil
}

func init() {
	ops.Register(ops.Op{Name: "user.relink", Summary: "Unlink a user's SSO identity so the next SSO login with their e-mail binds a (new) identity; ends their sessions", Method: "POST", Path: "/users/{id}/relink", Auth: ops.AuthAdmin, UI: "POST /ui/admin/users/{id}/relink"},
		func(e *ops.Env, in IDIn) (*User, error) {
			u, err := loadUser(e, in.ID)
			if err != nil {
				return nil, err
			}
			if u.IsAgent {
				return nil, ops.Conflict("agents have no SSO identity")
			}
			if _, _, err := RelinkIdentity(e.Ctx, e.DB, u.ID, "", e.User, auditSource(e)); err != nil {
				return nil, err
			}
			ops.Emit(e, ops.Event{Type: "userRelink", Name: "user.relinked", Room: "global", Data: map[string]any{"item": map[string]any{"userId": u.ID, "by": e.User.UserID}}})
			return loadUser(e, u.ID)
		})

	ops.Register(ops.Op{Name: "user.sessions.revokeAll", Summary: "Sign a user out everywhere: revoke all of their sessions", Method: "POST", Path: "/users/{id}/sessions/revoke-all", Auth: ops.AuthAdmin, UI: "POST /ui/admin/users/{id}/sessions/revoke-all"},
		func(e *ops.Env, in IDIn) (OKOut, error) {
			u, err := loadUser(e, in.ID)
			if err != nil {
				return OKOut{}, err
			}
			if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM sessions WHERE user_id=?`, u.ID); err != nil {
				return OKOut{}, err
			}
			log.Printf("auth: AUDIT all sessions of user %s (%s) revoked by admin %s", u.ID, u.Email, e.User.UserID)
			return OKOut{true}, nil
		})
}
