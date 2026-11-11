package auth

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/server"
)

const (
	CookieName   = "weave_session"
	apiKeyPrefix = "wk_"
	pendingTTL   = 10 * time.Minute // pending-terms sessions
)

// Roles is the allowed role set.
var Roles = []string{"admin", "projectOwner", "boardUser", "worker", "guest"}

func validRole(r string) bool {
	for _, x := range Roles {
		if x == r {
			return true
		}
	}
	return false
}

func init() {
	ops.Authenticate = authenticate
	server.Hooks = append(server.Hooks, validateOIDCHook)
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func cfgOf(e *ops.Env) config.Config { c, _ := e.Svc["config"].(config.Config); return c }

func dataDir(e *ops.Env) string {
	if d := cfgOf(e).DataDir; d != "" {
		return d
	}
	return "./data"
}

// envName maps "oidc.clientId" -> WEAVE_OIDC_CLIENT_ID.
func envName(key string) string {
	var b strings.Builder
	b.WriteString("WEAVE_")
	for i, r := range key {
		switch {
		case r == '.':
			b.WriteByte('_')
		case r >= 'A' && r <= 'Z':
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(r)
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// GetConfig returns a setting: environment first, then the config table.
func GetConfig(e *ops.Env, key string) string {
	if v := os.Getenv(envName(key)); v != "" {
		return v
	}
	var v string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT value FROM config WHERE key=?`, key).Scan(&v)
	return v
}

func envSet(key string) bool { return os.Getenv(envName(key)) != "" }

// SetConfig upserts a config row.
func SetConfig(e *ops.Env, key, value string) error {
	_, err := e.DB.ExecContext(e.Ctx, `INSERT INTO config(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func secretKey(e *ops.Env) []byte {
	s := cfgOf(e).SecretKey
	if s == "" {
		s = GetConfig(e, "auth.secret")
		if s == "" {
			_, _ = e.DB.ExecContext(e.Ctx, `INSERT OR IGNORE INTO config(key,value) VALUES('auth.secret',?)`, db.Token(32))
			s = GetConfig(e, "auth.secret")
		}
	}
	k := sha256.Sum256([]byte("weave-auth:" + s))
	return k[:]
}

func seal(key []byte, plain string) string {
	blk, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(blk)
	n := make([]byte, g.NonceSize())
	_, _ = rand.Read(n)
	return base64.StdEncoding.EncodeToString(g.Seal(n, n, []byte(plain), nil))
}

func unseal(key []byte, s string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	blk, _ := aes.NewCipher(key)
	g, _ := cipher.NewGCM(blk)
	if err != nil || len(raw) < g.NonceSize() {
		return "", errors.New("bad ciphertext")
	}
	b, err := g.Open(nil, raw[:g.NonceSize()], raw[g.NonceSize():], nil)
	return string(b), err
}

// ClientIP returns the caller address (X-Forwarded-For only when WEAVE_TRUST_PROXY=true).
func ClientIP(e *ops.Env) string {
	if e.Req == nil {
		return ""
	}
	if cfgOf(e).TrustProxy {
		if f := e.Req.Header.Get("X-Forwarded-For"); f != "" {
			return strings.TrimSpace(strings.Split(f, ",")[0])
		}
	}
	h, _, err := net.SplitHostPort(e.Req.RemoteAddr)
	if err != nil {
		return e.Req.RemoteAddr
	}
	return h
}

func userAgent(e *ops.Env) string {
	if e.Req == nil {
		return ""
	}
	ua := e.Req.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	return ua
}

// requestToken extracts the presented credential from a request.
func requestToken(r *http.Request) (tok string, explicitKey bool) {
	if k := r.Header.Get("X-API-Key"); k != "" {
		return k, true
	}
	if h := r.Header.Get("Authorization"); len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:]), false
	}
	if c, err := r.Cookie(CookieName); err == nil {
		return c.Value, false
	}
	return "", false
}

func currentSessionID(e *ops.Env) string {
	if e.Req == nil {
		return ""
	}
	tok, explicit := requestToken(e.Req)
	if tok == "" || explicit || strings.HasPrefix(tok, apiKeyPrefix) {
		return ""
	}
	var id string
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT id FROM sessions WHERE token_hash=?`, sha(tok)).Scan(&id)
	return id
}

func authenticate(e *ops.Env, r *http.Request) *ops.Principal {
	tok, explicit := requestToken(r)
	if tok == "" {
		return nil
	}
	ctx := r.Context()
	if explicit || strings.HasPrefix(tok, apiKeyPrefix) {
		var p ops.Principal
		var keyExp sql.NullString
		err := e.DB.QueryRowContext(ctx, `SELECT id,role,name,is_agent,api_key_expires_at FROM users WHERE api_key_hash=? AND is_deactivated=0 AND deleted_at IS NULL`, sha(tok)).Scan(&p.UserID, &p.Role, &p.Name, &p.IsAgent, &keyExp)
		if err != nil {
			return nil
		}
		if keyExp.Valid && keyExpired(&keyExp.String) {
			e.AuthMsg = "API key expired; generate a new one (Settings, or ask an administrator for an agent key)"
			return nil
		}
		if p.IsAgent && !explicit && !strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
			return nil // agents use keys in headers only, never a browser (cookie) session
		}
		return &p
	}
	var p ops.Principal
	var sid string
	var lastUsed sql.NullString
	var created string
	var autoLogout sql.NullInt64
	err := e.DB.QueryRowContext(ctx, `SELECT s.id,u.id,u.role,u.name,s.last_used_at,s.created_at,u.auto_logout_minutes FROM sessions s JOIN users u ON u.id=s.user_id
		WHERE s.token_hash=? AND s.pending_totp=0 AND s.pending_terms=0 AND s.expires_at>? AND u.is_deactivated=0 AND u.is_agent=0 AND u.deleted_at IS NULL`, sha(tok), db.Now()).
		Scan(&sid, &p.UserID, &p.Role, &p.Name, &lastUsed, &created, &autoLogout)
	if err != nil {
		return nil
	}
	now := time.Now().UTC()
	seen := created
	if lastUsed.Valid {
		seen = lastUsed.String
	}
	t, _ := time.Parse(time.RFC3339, seen)
	idle := time.Duration(0) // the stricter of the user's auto-logout and the instance-wide maximum
	if autoLogout.Valid && autoLogout.Int64 > 0 {
		idle = time.Duration(autoLogout.Int64) * time.Minute
	}
	if m := maxIdleMinutes(e); m > 0 && (idle == 0 || time.Duration(m)*time.Minute < idle) {
		idle = time.Duration(m) * time.Minute
	}
	if idle > 0 && now.Sub(t) > idle {
		_, _ = e.DB.ExecContext(ctx, `DELETE FROM sessions WHERE id=?`, sid)
		return nil
	}
	// only real interactions keep a session alive: heartbeats and polling must not defeat auto-logout
	if now.Sub(t) > time.Minute && !isBackground(r) {
		_, _ = e.DB.ExecContext(ctx, `UPDATE sessions SET last_used_at=? WHERE id=?`, db.Now(), sid)
	}
	return &p
}

// maxIdleMinutes is the admin-set upper bound on session idleness (session.maxIdleMinutes /
// WEAVE_SESSION_MAX_IDLE_MINUTES); 0 or unset = no instance-wide bound.
func maxIdleMinutes(e *ops.Env) int {
	n, _ := strconv.Atoi(strings.TrimSpace(GetConfig(e, "session.maxIdleMinutes")))
	if n < 0 {
		return 0
	}
	return n
}

// keyExpired reports whether an api_key_expires_at timestamp (nil = never) is in the past.
func keyExpired(exp *string) bool {
	if exp == nil || *exp == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, *exp)
	return err == nil && !time.Now().UTC().Before(t)
}

// BackgroundHeader marks requests the page makes on its own (polling, heartbeats); they are
// authenticated normally but never refresh sessions.last_used_at.
const BackgroundHeader = "X-Weave-Background"

func isBackground(r *http.Request) bool {
	switch p := r.URL.Path; {
	case p == "/ui/presence", p == "/events", p == "/api/v1/presence":
		return true
	case strings.HasPrefix(p, "/ui/cards/") && (strings.HasSuffix(p, "/editing") || strings.HasSuffix(p, "/editing/touch")):
		return true
	}
	return r.Header.Get(BackgroundHeader) != ""
}

// validateOIDCHook refuses to start unless OIDC is fully configured: authentication
// is OIDC-only, so without it nobody could sign in.
func validateOIDCHook(s *server.Server) error {
	e := &ops.Env{Ctx: context.Background(), DB: s.DB, Hub: s.Hub, Svc: map[string]any{"config": s.Cfg}}
	var missing []string
	for _, k := range []struct{ key, env string }{{"oidc.issuer", "WEAVE_OIDC_ISSUER"}, {"oidc.clientId", "WEAVE_OIDC_CLIENT_ID"}, {"oidc.clientSecret", "WEAVE_OIDC_CLIENT_SECRET"}} {
		if GetConfig(e, k.key) == "" {
			missing = append(missing, k.env)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("OIDC is required (Weave has no local accounts): set %s. Redirect URI to register at the identity provider: %s",
			strings.Join(missing, ", "), redirectURL(e))
	}
	if s.Cfg.SecretKey == "" {
		return errors.New("WEAVE_SECRET_KEY is required (signs OIDC state); generate one with: openssl rand -hex 32")
	}
	return nil
}
