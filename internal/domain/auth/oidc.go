package auth

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

type OIDCInfo struct {
	Enabled  bool   `json:"enabled"`
	LoginURL string `json:"loginUrl,omitempty"`
}
type OIDCAuthorizeOut struct {
	URL   string `json:"url" desc:"redirect the browser here"`
	State string `json:"state"`
	// Verifier is the PKCE code verifier (S256). The browser flow keeps it in a cookie; API
	// callers pass it back as code_verifier on the callback.
	Verifier string `json:"verifier"`
}
type OIDCCallbackIn struct {
	Code     string `query:"code" required:"true"`
	State    string `query:"state" required:"true"`
	Verifier string `query:"code_verifier" desc:"PKCE verifier returned by auth.oidc.authorize"`
}
type OIDCSettings struct {
	Issuer        string   `json:"issuer"`
	ClientID      string   `json:"clientId"`
	HasSecret     bool     `json:"hasSecret"`
	Scopes        string   `json:"scopes"`
	EmailClaim    string   `json:"emailClaim"`
	NameClaim     string   `json:"nameClaim"`
	UsernameClaim string   `json:"usernameClaim"`
	GroupsClaim   string   `json:"groupsClaim" desc:"ID-token claim holding the user's groups (default groups)"`
	AdminGroup    string   `json:"adminGroup" desc:"group granting the admin role (default weave-admins)"`
	OwnerGroup    string   `json:"ownerGroup" desc:"group granting projectOwner (default weave-project-owners)"`
	UserGroup     string   `json:"userGroup" desc:"group granting boardUser (default weave-users)"`
	WorkerGroup   string   `json:"workerGroup" desc:"group granting worker (default weave-workers)"`
	GuestGroup    string   `json:"guestGroup" desc:"group granting guest (default weave-guests)"`
	RequireGroup  bool     `json:"requireGroup" desc:"deny sign-in unless the user is in one of the role groups"`
	TeamPrefix    string   `json:"teamPrefix" desc:"groups with this prefix sync into Weave user groups (default weave-team-)"`
	AutoProvision bool     `json:"autoProvision"`
	RedirectURL   string   `json:"redirectUrl"`
	FromEnv       []string `json:"fromEnv" desc:"keys currently overridden by WEAVE_OIDC_* environment variables"`
}
type OIDCSettingsIn struct {
	Issuer        *string `json:"issuer"`
	ClientID      *string `json:"clientId"`
	ClientSecret  *string `json:"clientSecret" desc:"write-only"`
	Scopes        *string `json:"scopes" desc:"space separated, default \"openid profile email groups\""`
	EmailClaim    *string `json:"emailClaim"`
	NameClaim     *string `json:"nameClaim"`
	UsernameClaim *string `json:"usernameClaim"`
	GroupsClaim   *string `json:"groupsClaim"`
	AdminGroup    *string `json:"adminGroup"`
	OwnerGroup    *string `json:"ownerGroup"`
	UserGroup     *string `json:"userGroup"`
	WorkerGroup   *string `json:"workerGroup"`
	GuestGroup    *string `json:"guestGroup"`
	RequireGroup  *bool   `json:"requireGroup"`
	TeamPrefix    *string `json:"teamPrefix"`
	AutoProvision *bool   `json:"autoProvision"`
}

func cfgBool(e *ops.Env, key string, def bool) bool {
	v := strings.ToLower(GetConfig(e, key))
	if v == "" {
		return def
	}
	return v == "true" || v == "1" || v == "yes"
}

func cfgDefault(e *ops.Env, key, def string) string {
	if v := GetConfig(e, key); v != "" {
		return v
	}
	return def
}

func oidcEnabled(e *ops.Env) bool {
	return GetConfig(e, "oidc.issuer") != "" && GetConfig(e, "oidc.clientId") != "" && GetConfig(e, "oidc.clientSecret") != ""
}

func redirectURL(e *ops.Env) string {
	return strings.TrimRight(cfgOf(e).BaseURL, "/") + "/auth/oidc/callback"
}

var (
	provMu    sync.Mutex
	providers = map[string]*oidc.Provider{}
)

func provider(ctx context.Context, issuer string) (*oidc.Provider, error) {
	provMu.Lock()
	defer provMu.Unlock()
	if p, ok := providers[issuer]; ok {
		return p, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	p, err := oidc.NewProvider(cctx, issuer)
	if err != nil {
		return nil, err
	}
	providers[issuer] = p
	return p, nil
}

func oauthConfig(e *ops.Env, p *oidc.Provider) *oauth2.Config {
	return &oauth2.Config{ClientID: GetConfig(e, "oidc.clientId"), ClientSecret: GetConfig(e, "oidc.clientSecret"), Endpoint: p.Endpoint(), RedirectURL: redirectURL(e),
		Scopes: strings.Fields(cfgDefault(e, "oidc.scopes", "openid profile email groups"))}
}

func mac(e *ops.Env, s string) string {
	h := hmac.New(sha256.New, secretKey(e))
	h.Write([]byte(s))
	return hex.EncodeToString(h.Sum(nil))
}

// newState returns a signed, expiring state value; the nonce is derived from it.
func newState(e *ops.Env) string {
	body := base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(time.Now().Unix(), 10) + "." + db.Token(12)))
	return body + "." + mac(e, "state:"+body)
}

func checkState(e *ops.Env, state string) bool {
	body, sig, ok := strings.Cut(state, ".")
	if !ok || !hmac.Equal([]byte(sig), []byte(mac(e, "state:"+body))) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(body)
	if err != nil {
		return false
	}
	ts, _, _ := strings.Cut(string(raw), ".")
	n, err := strconv.ParseInt(ts, 10, 64)
	return err == nil && time.Since(time.Unix(n, 0)) < 10*time.Minute
}

func claimString(c map[string]any, key string) string {
	if key == "" {
		return ""
	}
	s, _ := c[key].(string)
	return strings.TrimSpace(s)
}

// claimStrings returns a claim as a string list (accepts a string or an array).
func claimStrings(c map[string]any, key string) []string {
	switch v := c[key].(type) {
	case string:
		return []string{v}
	case []any:
		var out []string
		for _, x := range v {
			if t, ok := x.(string); ok {
				out = append(out, t)
			}
		}
		return out
	}
	return nil
}

// roleRank orders Weave roles; the highest-ranked matching group wins.
var roleRank = map[string]int{"guest": 1, "worker": 2, "boardUser": 3, "projectOwner": 4, "admin": 5}

// groupAccess derives the Weave role and team-group names from the IdP groups claim.
// ok=false means sign-in must be denied (requireGroup set and no role group matched).
func groupAccess(e *ops.Env, claims map[string]any) (role string, teams []string, ok bool) {
	groups := claimStrings(claims, cfgDefault(e, "oidc.groupsClaim", "groups"))
	byGroup := map[string]string{
		cfgDefault(e, "oidc.adminGroup", "weave-admins"):         "admin",
		cfgDefault(e, "oidc.ownerGroup", "weave-project-owners"): "projectOwner",
		cfgDefault(e, "oidc.userGroup", "weave-users"):           "boardUser",
		cfgDefault(e, "oidc.workerGroup", "weave-workers"):       "worker",
		cfgDefault(e, "oidc.guestGroup", "weave-guests"):         "guest",
	}
	prefix := cfgDefault(e, "oidc.teamPrefix", "weave-team-")
	seen := map[string]bool{}
	for _, g := range groups {
		if r, hit := byGroup[g]; hit && roleRank[r] > roleRank[role] {
			role = r
		}
		if prefix != "" && strings.HasPrefix(g, prefix) && len(g) > len(prefix) && !seen[g] {
			seen[g] = true
			teams = append(teams, strings.TrimPrefix(g, prefix))
		}
	}
	if role == "" {
		if cfgBool(e, "oidc.requireGroup", false) {
			return "", nil, false
		}
		role = "boardUser"
	}
	return role, teams, true
}

// syncTeams makes the user's membership of OIDC-managed groups match the claim.
// Groups created here are marked source='oidc'; local groups are only added to, never removed from.
func syncTeams(e *ops.Env, userID string, teams []string) {
	want := map[string]bool{}
	for _, name := range teams {
		var id string
		err := e.DB.QueryRowContext(e.Ctx, `SELECT id FROM user_groups WHERE name=? COLLATE NOCASE`, name).Scan(&id)
		if err == sql.ErrNoRows {
			id = db.NewID()
			if _, err = e.DB.ExecContext(e.Ctx, `INSERT INTO user_groups(id,name,source,created_at) VALUES(?,?, 'oidc', ?)`, id, name, db.Now()); err != nil {
				continue
			}
		} else if err != nil {
			continue
		}
		want[id] = true
		_, _ = e.DB.ExecContext(e.Ctx, `INSERT OR IGNORE INTO user_group_members(group_id,user_id) VALUES(?,?)`, id, userID)
	}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT g.id FROM user_group_members m JOIN user_groups g ON g.id=m.group_id WHERE m.user_id=? AND g.source='oidc'`, userID)
	if err != nil {
		return
	}
	var drop []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil && !want[id] {
			drop = append(drop, id)
		}
	}
	rows.Close()
	for _, id := range drop {
		_, _ = e.DB.ExecContext(e.Ctx, `DELETE FROM user_group_members WHERE group_id=? AND user_id=?`, id, userID)
	}
}

// OIDCLogin maps verified ID-token claims to a local user (provisioning one when
// allowed) and issues a session. Exported for tests and alternative flows.
func OIDCLogin(e *ops.Env, subject string, claims map[string]any) (LoginOut, error) {
	email := strings.ToLower(claimString(claims, cfgDefault(e, "oidc.emailClaim", "email")))
	if email == "" || subject == "" {
		return LoginOut{}, ops.Invalid("identity provider did not return an email address")
	}
	if emailUnverified(claims["email_verified"]) {
		return LoginOut{}, ops.Forbidden()
	}
	name := claimString(claims, cfgDefault(e, "oidc.nameClaim", "name"))
	hasName := name != ""
	if name == "" {
		name, _, _ = strings.Cut(email, "@")
	}
	username := claimString(claims, cfgDefault(e, "oidc.usernameClaim", "preferred_username"))
	role, teams, allowed := groupAccess(e, claims)
	if !allowed {
		return LoginOut{}, ops.Forbidden()
	}

	var a acct
	const cols = `SELECT id,email,name,is_deactivated,terms_signature FROM users WHERE deleted_at IS NULL AND is_agent=0 AND `
	// 1. the stable identity (issuer|sub) always wins
	err := e.DB.QueryRowContext(e.Ctx, cols+`oidc_sub=? LIMIT 1`, subject).Scan(&a.ID, &a.Email, &a.Name, &a.Deactivated, &a.TermsSig)
	if err == sql.ErrNoRows {
		// 2. otherwise link by e-mail, but only an account that is not yet bound to an identity
		var bound sql.NullString
		err = e.DB.QueryRowContext(e.Ctx, `SELECT id,email,name,is_deactivated,terms_signature,oidc_sub FROM users WHERE deleted_at IS NULL AND is_agent=0 AND email=? LIMIT 1`, email).
			Scan(&a.ID, &a.Email, &a.Name, &a.Deactivated, &a.TermsSig, &bound)
		if err == nil {
			if bound.Valid && bound.String != "" {
				return LoginOut{}, deniedRebind(a.ID, email, subject)
			}
			if a.Deactivated {
				return LoginOut{}, ops.Forbidden()
			}
			r, uerr := e.DB.ExecContext(e.Ctx, `UPDATE users SET oidc_sub=?,is_sso=1 WHERE id=? AND oidc_sub IS NULL`, subject, a.ID)
			if uerr != nil {
				return LoginOut{}, uerr
			}
			if n, _ := r.RowsAffected(); n != 1 { // lost a race with another login: the account is bound now
				return LoginOut{}, deniedRebind(a.ID, email, subject)
			}
		}
	}
	switch {
	case err == sql.ErrNoRows:
		if !cfgBool(e, "oidc.autoProvision", true) {
			return LoginOut{}, ops.Forbidden()
		}
		var un any
		if v, _ := cleanUsername(username); v != nil {
			var n int
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM users WHERE username=?`, v).Scan(&n)
			if n == 0 {
				un = v
			}
		}
		a.ID = db.NewID()
		if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO users(id,email,username,name,role,is_sso,is_email_verified,oidc_sub,created_at) VALUES(?,?,?,?,?,1,1,?,?)`,
			a.ID, email, un, name, role, subject, db.Now()); err != nil {
			return LoginOut{}, err
		}
	case err != nil:
		return LoginOut{}, err
	default:
		if a.Deactivated {
			return LoginOut{}, ops.Forbidden()
		}
		if hasName && name != a.Name { // the identity provider owns the display name
			_, _ = e.DB.ExecContext(e.Ctx, `UPDATE users SET name=?,updated_at=? WHERE id=?`, name, db.Now(), a.ID)
		}
		// roles are owned by the identity provider: apply on every login (never demote the last admin)
		var cur string
		_ = e.DB.QueryRowContext(e.Ctx, `SELECT role FROM users WHERE id=?`, a.ID).Scan(&cur)
		if cur != role && !(cur == "admin" && countOtherAdmins(e, a.ID) == 0) {
			_, _ = e.DB.ExecContext(e.Ctx, `UPDATE users SET role=? WHERE id=?`, role, a.ID)
		}
	}
	syncTeams(e, a.ID, teams)
	return completeLogin(e, a)
}

// deniedRebind refuses a sign-in whose e-mail matches an account that is already linked to a
// different identity (reassigned mailbox, second tenant, IdP-side e-mail change). An admin
// must run user.relink (or `weave admin relink`) before the new identity can take the account.
func deniedRebind(userID, email, subject string) error {
	log.Printf("auth: AUDIT oidc login denied: %s matches user %s which is linked to a different identity (presented %q); relink required", email, userID, subject)
	return &ops.Error{Status: 403, Code: "forbidden", Msg: "This account is linked to a different identity. Ask an administrator to relink it."}
}

// emailUnverified is true only when the provider explicitly says the address is NOT verified
// (bool false or the string "false", as some IdPs send); an absent claim is tolerated.
func emailUnverified(v any) bool {
	switch x := v.(type) {
	case bool:
		return !x
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "false")
	}
	return false
}

func settingsOut(e *ops.Env) OIDCSettings {
	s := OIDCSettings{Issuer: GetConfig(e, "oidc.issuer"), ClientID: GetConfig(e, "oidc.clientId"),
		HasSecret: GetConfig(e, "oidc.clientSecret") != "", Scopes: cfgDefault(e, "oidc.scopes", "openid profile email groups"),
		EmailClaim: cfgDefault(e, "oidc.emailClaim", "email"), NameClaim: cfgDefault(e, "oidc.nameClaim", "name"),
		UsernameClaim: cfgDefault(e, "oidc.usernameClaim", "preferred_username"), GroupsClaim: cfgDefault(e, "oidc.groupsClaim", "groups"), AdminGroup: cfgDefault(e, "oidc.adminGroup", "weave-admins"),
		OwnerGroup: cfgDefault(e, "oidc.ownerGroup", "weave-project-owners"), UserGroup: cfgDefault(e, "oidc.userGroup", "weave-users"),
		WorkerGroup: cfgDefault(e, "oidc.workerGroup", "weave-workers"), GuestGroup: cfgDefault(e, "oidc.guestGroup", "weave-guests"),
		RequireGroup: cfgBool(e, "oidc.requireGroup", false), TeamPrefix: cfgDefault(e, "oidc.teamPrefix", "weave-team-"),
		AutoProvision: cfgBool(e, "oidc.autoProvision", true), RedirectURL: redirectURL(e), FromEnv: []string{}}
	for _, k := range []string{"issuer", "clientId", "clientSecret", "scopes", "emailClaim", "nameClaim", "usernameClaim", "groupsClaim", "adminGroup", "ownerGroup", "userGroup", "workerGroup", "guestGroup", "requireGroup", "teamPrefix", "autoProvision"} {
		if envSet("oidc." + k) {
			s.FromEnv = append(s.FromEnv, k)
		}
	}
	return s
}

func init() {
	ops.Register(ops.Op{Name: "auth.oidc.info", Summary: "Whether single sign-on is available", Method: "GET", Path: "/auth/oidc", Auth: ops.AuthPublic, UI: "GET /login (SSO button)"},
		func(e *ops.Env, _ NoInput) (OIDCInfo, error) {
			if !oidcEnabled(e) {
				return OIDCInfo{}, nil
			}
			return OIDCInfo{Enabled: true, LoginURL: "/auth/oidc/login"}, nil
		})

	ops.Register(ops.Op{Name: "auth.oidc.authorize", Summary: "Build the identity-provider authorization URL (browser flow)", Method: "GET", Path: "/auth/oidc/authorize", Auth: ops.AuthPublic, UI: "GET /auth/oidc/login"},
		func(e *ops.Env, _ NoInput) (OIDCAuthorizeOut, error) {
			if !oidcEnabled(e) {
				return OIDCAuthorizeOut{}, ops.NotFound("sso")
			}
			p, err := provider(e.Ctx, GetConfig(e, "oidc.issuer"))
			if err != nil {
				return OIDCAuthorizeOut{}, ops.Conflict("identity provider unreachable: " + err.Error())
			}
			st := newState(e)
			verifier := oauth2.GenerateVerifier()
			return OIDCAuthorizeOut{URL: oauthConfig(e, p).AuthCodeURL(st, oidc.Nonce(mac(e, "nonce:"+st)), oauth2.S256ChallengeOption(verifier)), State: st, Verifier: verifier}, nil
		})

	ops.Register(ops.Op{Name: "auth.oidc.callback", Summary: "Complete SSO: exchange the code, provision/link the user and return a session token", Method: "GET", Path: "/auth/oidc/callback", Auth: ops.AuthPublic, UI: "GET /auth/oidc/callback"},
		func(e *ops.Env, in OIDCCallbackIn) (LoginOut, error) {
			if !oidcEnabled(e) {
				return LoginOut{}, ops.NotFound("sso")
			}
			if !checkState(e, in.State) {
				return LoginOut{}, ops.Invalid("invalid or expired state")
			}
			p, err := provider(e.Ctx, GetConfig(e, "oidc.issuer"))
			if err != nil {
				return LoginOut{}, ops.Conflict("identity provider unreachable")
			}
			oc := oauthConfig(e, p)
			var xopts []oauth2.AuthCodeOption
			if in.Verifier != "" {
				xopts = append(xopts, oauth2.VerifierOption(in.Verifier))
			}
			tok, err := oc.Exchange(e.Ctx, in.Code, xopts...)
			if err != nil {
				return LoginOut{}, ops.Invalid("code exchange failed")
			}
			raw, _ := tok.Extra("id_token").(string)
			idt, err := p.Verifier(&oidc.Config{ClientID: oc.ClientID}).Verify(e.Ctx, raw)
			if err != nil {
				return LoginOut{}, ops.Invalid("invalid id_token")
			}
			if idt.Nonce != mac(e, "nonce:"+in.State) {
				return LoginOut{}, ops.Invalid("nonce mismatch")
			}
			claims := map[string]any{}
			if err := idt.Claims(&claims); err != nil {
				return LoginOut{}, err
			}
			// supplement with userinfo when the id_token lacks the email or the groups claim
			if _, hasGroups := claims[cfgDefault(e, "oidc.groupsClaim", "groups")]; !hasGroups || claimString(claims, cfgDefault(e, "oidc.emailClaim", "email")) == "" {
				// userinfo is only trusted when it describes the same subject as the ID token
				if ui, err := p.UserInfo(e.Ctx, oauth2.StaticTokenSource(tok)); err == nil && ui.Subject == idt.Subject {
					extra := map[string]any{}
					if ui.Claims(&extra) == nil {
						for k, v := range extra {
							if _, ok := claims[k]; !ok {
								claims[k] = v
							}
						}
					}
				}
			}
			return OIDCLogin(e, idt.Issuer+"|"+idt.Subject, claims)
		})

	ops.Register(ops.Op{Name: "auth.oidc.settings.get", Summary: "Read the SSO configuration (admin)", Method: "GET", Path: "/auth/oidc/settings", Auth: ops.AuthAdmin, UI: "GET /admin/sso"},
		func(e *ops.Env, _ NoInput) (OIDCSettings, error) { return settingsOut(e), nil })

	ops.Register(ops.Op{Name: "auth.oidc.settings.update", Summary: "Update the SSO configuration stored in the database (admin; env vars still override)", Method: "PUT", Path: "/auth/oidc/settings", Auth: ops.AuthAdmin, UI: "POST /ui/admin/sso"},
		func(e *ops.Env, in OIDCSettingsIn) (OIDCSettings, error) {
			set := func(k string, v *string) error {
				if v == nil {
					return nil
				}
				return SetConfig(e, "oidc."+k, strings.TrimSpace(*v))
			}
			setB := func(k string, v *bool) error {
				if v == nil {
					return nil
				}
				return SetConfig(e, "oidc."+k, fmt.Sprint(*v))
			}
			if in.Issuer != nil && *in.Issuer != "" && !strings.HasPrefix(*in.Issuer, "http") {
				return OIDCSettings{}, ops.Invalid("issuer must be an http(s) URL")
			}
			if in.ClientSecret != nil && *in.ClientSecret == "" {
				in.ClientSecret = nil // blank keeps the stored secret
			}
			for _, err := range []error{set("issuer", in.Issuer), set("clientId", in.ClientID), set("clientSecret", in.ClientSecret), set("scopes", in.Scopes),
				set("emailClaim", in.EmailClaim), set("nameClaim", in.NameClaim), set("usernameClaim", in.UsernameClaim), set("groupsClaim", in.GroupsClaim), set("adminGroup", in.AdminGroup), set("ownerGroup", in.OwnerGroup), set("userGroup", in.UserGroup),
				set("workerGroup", in.WorkerGroup), set("guestGroup", in.GuestGroup), setB("requireGroup", in.RequireGroup), set("teamPrefix", in.TeamPrefix),
				setB("autoProvision", in.AutoProvision)} {
				if err != nil {
					return OIDCSettings{}, err
				}
			}
			provMu.Lock()
			providers = map[string]*oidc.Provider{}
			provMu.Unlock()
			return settingsOut(e), nil
		})
}
