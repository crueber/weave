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

// RelinkIdentity clears the stored identity of the user (by id, or by e-mail when id is empty)
// and ends their sessions. It reports whether a user matched.
func RelinkIdentity(ctx context.Context, d *db.DB, userID, email string) (bool, error) {
	var r sql.Result
	var err error
	if userID != "" {
		r, err = d.ExecContext(ctx, `UPDATE users SET oidc_sub=NULL,updated_at=? WHERE id=? AND is_agent=0 AND deleted_at IS NULL`, db.Now(), userID)
	} else {
		r, err = d.ExecContext(ctx, `UPDATE users SET oidc_sub=NULL,updated_at=? WHERE email=? AND is_agent=0 AND deleted_at IS NULL`, db.Now(), email)
	}
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	if n > 0 { // the old identity must not keep a live session either
		if userID != "" {
			_, _ = d.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=?`, userID)
		} else {
			_, _ = d.ExecContext(ctx, `DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE email=?)`, email)
		}
	}
	return n > 0, nil
}

// RelinkByEmail is the break-glass path behind `weave admin relink` (direct DB access).
func RelinkByEmail(ctx context.Context, d *db.DB, email string) error {
	email, err := cleanEmail(email)
	if err != nil {
		return err
	}
	ok, err := RelinkIdentity(ctx, d, "", email)
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
			if _, err := RelinkIdentity(e.Ctx, e.DB, u.ID, ""); err != nil {
				return nil, err
			}
			log.Printf("auth: AUDIT SSO identity of user %s (%s) unlinked by admin %s", u.ID, u.Email, e.User.UserID)
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
