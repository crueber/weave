package auth

import (
	"crypto/subtle"
	"database/sql"
	"strings"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// LoginOut is returned by the OIDC callback. When PendingTerms is true, Token is a
// short-lived pending token that is only accepted by auth.terms.accept; otherwise it
// is a session token (cookie value).
type LoginOut struct {
	Token        string `json:"token"`
	ExpiresAt    string `json:"expiresAt"`
	PendingTerms bool   `json:"pendingTerms,omitempty"`
}

type TermsOut struct {
	Enabled   bool   `json:"enabled"`
	Text      string `json:"text"`
	Signature string `json:"signature" desc:"hash identifying this version of the terms"`
}

type TermsAcceptIn struct {
	PendingToken string `json:"pendingToken" desc:"pending token from login (omit when already signed in)"`
	Signature    string `json:"signature" required:"true" desc:"signature from auth.terms.get"`
}

type TermsSetIn struct {
	Text string `json:"text" desc:"terms of service text; empty disables the requirement"`
}

type SessionInfo struct {
	ID         string  `json:"id"`
	RemoteAddr string  `json:"remoteAddr"`
	UserAgent  string  `json:"userAgent"`
	CreatedAt  string  `json:"createdAt"`
	LastUsedAt *string `json:"lastUsedAt"`
	ExpiresAt  string  `json:"expiresAt"`
	Current    bool    `json:"current"`
}
type SessionList struct {
	Items []SessionInfo `json:"items"`
}
type SessionIDIn struct {
	ID string `path:"id"`
}
type OKOut struct {
	OK bool `json:"ok"`
}

func termsHash(text string) string { return sha("terms:" + text)[:16] }

func currentTerms(e *ops.Env) TermsOut {
	t := strings.TrimSpace(GetConfig(e, "terms.text"))
	if t == "" {
		return TermsOut{}
	}
	return TermsOut{Enabled: true, Text: t, Signature: termsHash(t)}
}

func sessionTTL(e *ops.Env) time.Duration {
	h := cfgOf(e).SessionHours
	if h <= 0 {
		h = 168
	}
	return time.Duration(h) * time.Hour
}

func newSession(e *ops.Env, userID string, pendingTerms bool) (LoginOut, error) {
	tok := db.Token(32)
	ttl := sessionTTL(e)
	if pendingTerms {
		ttl = pendingTTL
	}
	exp := time.Now().UTC().Add(ttl).Format(time.RFC3339)
	_, err := e.DB.ExecContext(e.Ctx, `INSERT INTO sessions(id,user_id,token_hash,remote_addr,user_agent,pending_totp,pending_terms,created_at,expires_at) VALUES(?,?,?,?,?,0,?,?,?)`,
		db.NewID(), userID, sha(tok), ClientIP(e), userAgent(e), b2i(pendingTerms), db.Now(), exp)
	if err != nil {
		return LoginOut{}, err
	}
	// opportunistic cleanup
	_, _ = e.DB.ExecContext(e.Ctx, `DELETE FROM sessions WHERE expires_at<?`, db.Now())
	return LoginOut{Token: tok, ExpiresAt: exp, PendingTerms: pendingTerms}, nil
}

type acct struct {
	ID, Email, Name string
	Deactivated     bool
	TermsSig        sql.NullString
}

func termsPending(e *ops.Env, a acct) bool {
	t := currentTerms(e)
	return t.Enabled && a.TermsSig.String != t.Signature
}

// completeLogin issues the final session, or a pending-terms session.
func completeLogin(e *ops.Env, a acct) (LoginOut, error) {
	return newSession(e, a.ID, termsPending(e, a))
}

func init() {

	ops.Register(ops.Op{Name: "auth.logout", Summary: "End the current session", Method: "POST", Path: "/auth/logout", Auth: ops.AuthPublic, UI: "POST /logout"},
		func(e *ops.Env, _ NoInput) (OKOut, error) {
			if e.Req != nil {
				if tok, explicit := requestToken(e.Req); tok != "" && !explicit {
					_, _ = e.DB.ExecContext(e.Ctx, `DELETE FROM sessions WHERE token_hash=?`, sha(tok))
				}
			}
			return OKOut{true}, nil
		})

	ops.Register(ops.Op{Name: "auth.me", Summary: "The authenticated user", Method: "GET", Path: "/auth/me", UI: "GET /settings"},
		func(e *ops.Env, _ NoInput) (*User, error) { return loadUser(e, e.User.UserID) })

	ops.Register(ops.Op{Name: "auth.terms.get", Summary: "Current terms of service (public)", Method: "GET", Path: "/auth/terms", Auth: ops.AuthPublic, UI: "GET /auth/oidc/callback (terms step)"},
		func(e *ops.Env, _ NoInput) (TermsOut, error) { return currentTerms(e), nil })

	ops.Register(ops.Op{Name: "auth.terms.set", Summary: "Set the terms of service text (admin); empty disables acceptance", Method: "PUT", Path: "/auth/terms", Auth: ops.AuthAdmin, UI: "POST /ui/admin/sso/terms"},
		func(e *ops.Env, in TermsSetIn) (TermsOut, error) {
			if err := SetConfig(e, "terms.text", strings.TrimSpace(in.Text)); err != nil {
				return TermsOut{}, err
			}
			return currentTerms(e), nil
		})

	ops.Register(ops.Op{Name: "auth.terms.accept", Summary: "Accept the current terms (completes a pending login or records acceptance while signed in)", Method: "POST", Path: "/auth/terms/accept", Auth: ops.AuthPublic, UI: "POST /login/terms"},
		func(e *ops.Env, in TermsAcceptIn) (LoginOut, error) {
			t := currentTerms(e)
			if t.Enabled && subtle.ConstantTimeCompare([]byte(in.Signature), []byte(t.Signature)) != 1 {
				return LoginOut{}, ops.Conflict("terms changed; reload and accept again")
			}
			sig := in.Signature
			if in.PendingToken != "" {
				sid, uid, pterms, err := pendingSession(e, in.PendingToken)
				if err != nil || !pterms {
					return LoginOut{}, &ops.Error{Status: 401, Code: "unauthorized", Msg: "login expired; start again"}
				}
				_, _ = e.DB.ExecContext(e.Ctx, `UPDATE users SET terms_accepted_at=?,terms_signature=? WHERE id=?`, db.Now(), sig, uid)
				_, _ = e.DB.ExecContext(e.Ctx, `DELETE FROM sessions WHERE id=?`, sid)
				return newSession(e, uid, false)
			}
			if e.User == nil {
				return LoginOut{}, ops.Unauthorized()
			}
			_, err := e.DB.ExecContext(e.Ctx, `UPDATE users SET terms_accepted_at=?,terms_signature=? WHERE id=?`, db.Now(), sig, e.User.UserID)
			return LoginOut{}, err
		})

	ops.Register(ops.Op{Name: "session.list", Summary: "List your active sessions", Method: "GET", Path: "/sessions", UI: "GET /settings (Sessions)"},
		func(e *ops.Env, _ NoInput) (SessionList, error) {
			cur := currentSessionID(e)
			rows, err := e.DB.QueryContext(e.Ctx, `SELECT id,COALESCE(remote_addr,''),COALESCE(user_agent,''),created_at,last_used_at,expires_at FROM sessions
				WHERE user_id=? AND pending_totp=0 AND pending_terms=0 AND expires_at>? ORDER BY created_at DESC`, e.User.UserID, db.Now())
			if err != nil {
				return SessionList{}, err
			}
			defer rows.Close()
			out := SessionList{Items: []SessionInfo{}}
			for rows.Next() {
				var s SessionInfo
				if err := rows.Scan(&s.ID, &s.RemoteAddr, &s.UserAgent, &s.CreatedAt, &s.LastUsedAt, &s.ExpiresAt); err != nil {
					return out, err
				}
				s.Current = s.ID == cur
				out.Items = append(out.Items, s)
			}
			return out, rows.Err()
		})

	ops.Register(ops.Op{Name: "session.revoke", Summary: "Revoke one of your sessions", Method: "DELETE", Path: "/sessions/{id}", UI: "POST /ui/settings/sessions/{id}/revoke"},
		func(e *ops.Env, in SessionIDIn) (OKOut, error) {
			r, err := e.DB.ExecContext(e.Ctx, `DELETE FROM sessions WHERE id=? AND user_id=?`, in.ID, e.User.UserID)
			if err != nil {
				return OKOut{}, err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				return OKOut{}, ops.NotFound("session")
			}
			return OKOut{true}, nil
		})

	ops.Register(ops.Op{Name: "session.revoke_others", Summary: "Revoke all your sessions except the current one", Method: "POST", Path: "/sessions/revoke-others", UI: "POST /ui/settings/sessions/revoke-others"},
		func(e *ops.Env, _ NoInput) (OKOut, error) {
			_, err := e.DB.ExecContext(e.Ctx, `DELETE FROM sessions WHERE user_id=? AND id<>?`, e.User.UserID, currentSessionID(e))
			return OKOut{true}, err
		})
}

// pendingSession validates a pending-terms token and returns the session id and user id.
func pendingSession(e *ops.Env, tok string) (sid, uid string, pendingTerms bool, err error) {
	if tok == "" {
		return "", "", false, ops.Unauthorized()
	}
	err = e.DB.QueryRowContext(e.Ctx, `SELECT s.id,s.user_id,s.pending_terms FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=? AND s.pending_terms=1 AND s.expires_at>? AND u.is_deactivated=0`, sha(tok), db.Now()).Scan(&sid, &uid, &pendingTerms)
	return
}
