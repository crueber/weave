package auth

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/server"
	"github.com/crueber/weave/internal/testutil"
)

func newEnv(t *testing.T) *ops.Env {
	e := testutil.Env(t, nil)
	e.Svc["config"] = config.Config{DataDir: t.TempDir(), SessionHours: 1}
	return e
}

func mkUser(t *testing.T, e *ops.Env, email, role string) *ops.Principal {
	t.Helper()
	id := db.NewID()
	if _, err := e.DB.Exec(`INSERT INTO users(id,email,name,role,created_at) VALUES(?,?,?,?,?)`, id, email, email, role, db.Now()); err != nil {
		t.Fatal(err)
	}
	return &ops.Principal{UserID: id, Role: role, Name: email}
}

func call[T any](t *testing.T, e *ops.Env, name string, in any) T {
	t.Helper()
	v, err := ops.Typed[T](e, name, in)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return v
}

func wantErr(t *testing.T, e *ops.Env, name string, in any, status int) {
	t.Helper()
	_, err := ops.Typed[any](e, name, in)
	oe, ok := err.(*ops.Error)
	if !ok || oe.Status != status {
		t.Fatalf("%s: want status %d, got %v", name, status, err)
	}
}

func reqEnv(e *ops.Env, hdr func(*http.Request)) *ops.Env {
	r := httptest.NewRequest("GET", "/", nil)
	hdr(r)
	c := *e
	c.Req = r
	c.User = ops.Authenticate(&c, r)
	return &c
}

func TestSessionsAndAuthenticate(t *testing.T) {
	e := newEnv(t)
	u := mkUser(t, e, "a@x.io", "boardUser")
	out, err := newSession(e, u.UserID, false)
	if err != nil || out.Token == "" {
		t.Fatalf("session %+v %v", out, err)
	}
	var stored string
	_ = e.DB.QueryRow(`SELECT token_hash FROM sessions`).Scan(&stored)
	if stored == out.Token || stored != sha(out.Token) {
		t.Fatal("session token must be stored hashed")
	}
	// Bearer, cookie
	for name, f := range map[string]func(*http.Request){
		"bearer": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+out.Token) },
		"cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: CookieName, Value: out.Token}) },
	} {
		if c := reqEnv(e, f); c.User == nil || c.User.UserID != u.UserID {
			t.Fatalf("%s auth failed", name)
		}
	}
	if reqEnv(e, func(r *http.Request) { r.Header.Set("Authorization", "Bearer bogus") }).User != nil {
		t.Fatal("bogus token accepted")
	}
	// sessions list marks current; logout kills it
	me := reqEnv(e, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+out.Token) })
	l := call[SessionList](t, me, "session.list", NoInput{})
	if len(l.Items) != 1 || !l.Items[0].Current {
		t.Fatalf("sessions %+v", l)
	}
	call[OKOut](t, me, "auth.logout", NoInput{})
	if reqEnv(e, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+out.Token) }).User != nil {
		t.Fatal("session survives logout")
	}
	// expiry
	out, _ = newSession(e, u.UserID, false)
	_, _ = e.DB.Exec(`UPDATE sessions SET expires_at='2000-01-01T00:00:00Z'`)
	if reqEnv(e, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+out.Token) }).User != nil {
		t.Fatal("expired session accepted")
	}
	// deactivated
	out, _ = newSession(e, u.UserID, false)
	_, _ = e.DB.Exec(`UPDATE users SET is_deactivated=1`)
	if reqEnv(e, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+out.Token) }).User != nil {
		t.Fatal("deactivated user authenticates")
	}
}

// Authentication is OIDC-only: no credential-based ops may exist.
func TestNoLocalAuthOps(t *testing.T) {
	for _, n := range []string{"auth.login", "auth.login.totp", "user.create", "user.password.update", "user.email.update", "user.username.update",
		"totp.setup", "totp.enable", "totp.disable", "totp.recovery.regenerate", "device.list", "device.revoke"} {
		if ops.Get(n) != nil {
			t.Errorf("local-auth op %s still registered", n)
		}
	}
}

func TestAPIKey(t *testing.T) {
	e := newEnv(t)
	u := mkUser(t, e, "a@x.io", "boardUser")
	as := testutil.As(e, u)
	k := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	var h string
	_ = e.DB.QueryRow(`SELECT api_key_hash FROM users`).Scan(&h)
	if h == k.Key || h != sha(k.Key) {
		t.Fatal("api key must be hashed")
	}
	for _, f := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set("X-API-Key", k.Key) },
		func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+k.Key) },
	} {
		if reqEnv(e, f).User == nil {
			t.Fatal("api key rejected")
		}
	}
	k2 := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	if reqEnv(e, func(r *http.Request) { r.Header.Set("X-API-Key", k.Key) }).User != nil {
		t.Fatal("rotated key still valid")
	}
	call[OKOut](t, as, "apikey.revoke", IDIn{ID: "me"})
	if reqEnv(e, func(r *http.Request) { r.Header.Set("X-API-Key", k2.Key) }).User != nil {
		t.Fatal("revoked key valid")
	}
	other := mkUser(t, e, "b@x.io", "boardUser")
	wantErr(t, as, "apikey.generate", APIKeyGenIn{ID: other.UserID}, 403)
}

func TestUsersRules(t *testing.T) {
	e := newEnv(t)
	admin := testutil.As(e, mkUser(t, e, "root@x.io", "admin"))
	up := mkUser(t, e, "new@x.io", "boardUser") // (guests and workers see only board mates: see visibility_test.go)
	me := testutil.As(e, up)
	// profile: only phone/organization are editable (name, email and role belong to the IdP)
	ph := "555-1234"
	got := call[*User](t, me, "user.update", UpdateIn{ID: "me", Phone: &ph})
	if got.Phone == nil || *got.Phone != ph {
		t.Fatal("phone update")
	}
	wantErr(t, me, "user.update", UpdateIn{ID: admin.User.UserID, Phone: &ph}, 403)
	// other users' private details are hidden
	g := call[*User](t, me, "user.get", IDIn{ID: admin.User.UserID})
	if g.Email != "" {
		t.Fatal("email leaked to non-admin")
	}
	l := call[UserList](t, me, "user.list", ListIn{})
	for _, x := range l.Items {
		if x.ID != up.UserID && x.Email != "" {
			t.Fatal("email leaked in list")
		}
	}
	// prefs
	th, ed, to := "dark", "markdown", 30
	b := true
	p := call[*User](t, me, "user.preferences.update", PrefsIn{ID: "me", Theme: &th, DefaultEditorMode: &ed, AutoLogoutMinutes: &to, SubscribeOwnCards: &b})
	if *p.Theme != "dark" || p.DefaultEditorMode != "markdown" || *p.AutoLogoutMinutes != 30 || !p.SubscribeOwnCards {
		t.Fatalf("prefs %+v", p)
	}
	bad := "neon"
	wantErr(t, me, "user.preferences.update", PrefsIn{ID: "me", Theme: &bad}, 422)
	// last admin guards
	self := admin.User.UserID
	wantErr(t, admin, "user.deactivate", IDIn{ID: self}, 409)
	wantErr(t, admin, "user.delete", IDIn{ID: self}, 409)
	// deactivate blocks sessions and keys; activate / delete
	call[APIKeyOut](t, me, "apikey.generate", APIKeyGenIn{ID: "me"})
	d := call[*User](t, admin, "user.deactivate", IDIn{ID: up.UserID})
	if !d.IsDeactivated {
		t.Fatal("deactivate")
	}
	call[*User](t, admin, "user.activate", IDIn{ID: up.UserID})
	call[*User](t, admin, "user.delete", IDIn{ID: up.UserID})
	wantErr(t, admin, "user.get", IDIn{ID: up.UserID}, 404)
}

func TestAvatar(t *testing.T) {
	e := newEnv(t)
	u := mkUser(t, e, "a@x.io", "boardUser")
	as := testutil.As(e, u)
	img := image.NewRGBA(image.Rect(0, 0, 400, 300))
	for x := 0; x < 400; x++ {
		img.Set(x, 5, color.RGBA{255, 0, 0, 255})
	}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	got := call[*User](t, as, "user.avatar.update", AvatarIn{ID: "me", File: &ops.File{Name: "a.png", Data: buf.Bytes()}})
	if got.AvatarURL == "" {
		t.Fatal("no avatar url")
	}
	s := call[*ops.Stream](t, as, "user.avatar.get", IDIn{ID: u.UserID})
	cfg, err := png.DecodeConfig(bytes.NewReader(s.Data))
	if err != nil || cfg.Width != avatarSize || cfg.Height != avatarSize {
		t.Fatalf("avatar not normalised: %v %+v", err, cfg)
	}
	wantErr(t, as, "user.avatar.update", AvatarIn{ID: "me", File: &ops.File{Name: "x.txt", Data: []byte("not an image")}}, 422)
	call[*User](t, as, "user.avatar.delete", IDIn{ID: "me"})
	wantErr(t, as, "user.avatar.get", IDIn{ID: u.UserID}, 404)
}

func TestGroups(t *testing.T) {
	e := newEnv(t)
	admin := testutil.As(e, mkUser(t, e, "root@x.io", "admin"))
	u := mkUser(t, e, "a@x.io", "boardUser")
	g := call[*Group](t, admin, "group.create", GroupCreateIn{Name: "Devs"})
	wantErr(t, admin, "group.create", GroupCreateIn{Name: "Devs"}, 409)
	g = call[*Group](t, admin, "group.member.add", GroupMemberAddIn{ID: g.ID, UserID: u.UserID})
	if len(g.Members) != 1 || g.Members[0].ID != u.UserID {
		t.Fatalf("members %+v", g.Members)
	}
	g = call[*Group](t, admin, "group.update", GroupUpdateIn{ID: g.ID, Name: "Engineers"})
	if g.Name != "Engineers" {
		t.Fatal("rename")
	}
	wantErr(t, testutil.As(e, u), "group.create", GroupCreateIn{Name: "X"}, 403)
	if l := call[GroupList](t, testutil.As(e, u), "group.list", NoInput{}); len(l.Items) != 1 {
		t.Fatal("list")
	}
	call[*Group](t, admin, "group.member.remove", GroupMemberIn{ID: g.ID, UserID: u.UserID})
	wantErr(t, admin, "group.member.remove", GroupMemberIn{ID: g.ID, UserID: u.UserID}, 404)
	call[OKOut](t, admin, "group.delete", GroupIDIn{ID: g.ID})
	wantErr(t, admin, "group.get", GroupIDIn{ID: g.ID}, 404)
}

func TestTermsFlow(t *testing.T) {
	e := newEnv(t)
	admin := testutil.As(e, mkUser(t, e, "root@x.io", "admin"))
	if call[TermsOut](t, e, "auth.terms.get", NoInput{}).Enabled {
		t.Fatal("terms on by default")
	}
	terms := call[TermsOut](t, admin, "auth.terms.set", TermsSetIn{Text: "Be nice."})
	login := func() LoginOut {
		out, err := OIDCLogin(e, "iss|a", map[string]any{"email": "a@x.io", "groups": []any{"weave-users"}})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	p := login()
	if !p.PendingTerms {
		t.Fatal("expected pending terms")
	}
	if reqEnv(e, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+p.Token) }).User != nil {
		t.Fatal("pending-terms token authenticates")
	}
	wantErr(t, e, "auth.terms.accept", TermsAcceptIn{PendingToken: p.Token, Signature: "stale"}, 409)
	f := call[LoginOut](t, e, "auth.terms.accept", TermsAcceptIn{PendingToken: p.Token, Signature: terms.Signature})
	if f.PendingTerms || f.Token == "" {
		t.Fatal("accept should complete login")
	}
	if login().PendingTerms {
		t.Fatal("accepted terms asked again")
	}
	call[TermsOut](t, admin, "auth.terms.set", TermsSetIn{Text: "Be nicer."})
	if !login().PendingTerms {
		t.Fatal("new terms version must be re-accepted")
	}
}

func TestOIDCProvisioning(t *testing.T) {
	e := newEnv(t)
	admin := mkUser(t, e, "root@x.io", "admin")
	ae := testutil.As(e, admin)
	out, err := OIDCLogin(e, "iss|sub1", map[string]any{"email": "Sso@X.io", "name": "Sso User", "preferred_username": "sso", "groups": []any{"weave-admins"}})
	if err != nil || out.Token == "" {
		t.Fatalf("login %v", err)
	}
	var role string
	var sso bool
	_ = e.DB.QueryRow(`SELECT role,is_sso FROM users WHERE email='sso@x.io'`).Scan(&role, &sso)
	if role != "admin" || !sso {
		t.Fatalf("provisioned %s %v", role, sso)
	}
	// existing local user is linked by email
	mkUser(t, e, "local@x.io", "boardUser")
	if _, err := OIDCLogin(e, "iss|sub2", map[string]any{"email": "local@x.io"}); err != nil {
		t.Fatal(err)
	}
	var sub string
	_ = e.DB.QueryRow(`SELECT oidc_sub FROM users WHERE email='local@x.io'`).Scan(&sub)
	if sub != "iss|sub2" {
		t.Fatal("not linked")
	}
	// unverified email rejected, auto-provision off blocks new users, deactivated blocked
	if _, err := OIDCLogin(e, "iss|s3", map[string]any{"email": "u@x.io", "email_verified": false}); err == nil {
		t.Fatal("unverified email accepted")
	}
	f := false
	call[OIDCSettings](t, ae, "auth.oidc.settings.update", OIDCSettingsIn{AutoProvision: &f})
	if _, err := OIDCLogin(e, "iss|s4", map[string]any{"email": "new@x.io"}); err == nil {
		t.Fatal("provisioned despite autoProvision=false")
	}
	_, _ = e.DB.Exec(`UPDATE users SET is_deactivated=1 WHERE email='local@x.io'`)
	if _, err := OIDCLogin(e, "iss|sub2", map[string]any{"email": "local@x.io"}); err == nil {
		t.Fatal("deactivated SSO user logged in")
	}
	// state signing
	st := newState(e)
	if !checkState(e, st) || checkState(e, st+"x") || checkState(e, "garbage") {
		t.Fatal("state validation")
	}
	s := call[OIDCSettings](t, ae, "auth.oidc.settings.get", NoInput{})
	if s.HasSecret {
		t.Fatal("no secret set")
	}
}

func ptr[T any](v T) *T { return &v }

func TestPresence(t *testing.T) {
	e := newEnv(t)
	u := mkUser(t, e, "a@x.io", "boardUser")
	as := testutil.As(e, u)
	wantErr(t, as, "presence.set", PresenceIn{State: "dancing"}, 422)
	call[OKOut](t, as, "presence.set", PresenceIn{State: "online"})
	l := call[PresenceList](t, as, "presence.list", NoInput{})
	if len(l.Items) != 1 || l.Items[0].State != "online" {
		t.Fatalf("%+v", l)
	}
	_, _ = e.DB.Exec(`UPDATE users SET last_seen_at='2000-01-01T00:00:00Z'`)
	if l := call[PresenceList](t, as, "presence.list", NoInput{}); len(l.Items) != 0 {
		t.Fatal("stale presence not expired")
	}
}

func TestGetConfigEnvOverride(t *testing.T) {
	e := newEnv(t)
	_ = SetConfig(e, "oidc.clientId", "db")
	if GetConfig(e, "oidc.clientId") != "db" {
		t.Fatal("db value")
	}
	t.Setenv("WEAVE_OIDC_CLIENT_ID", "env")
	if GetConfig(e, "oidc.clientId") != "env" {
		t.Fatal("env override")
	}
}

func TestOIDCGroups(t *testing.T) {
	e := newEnv(t)
	admin := mkUser(t, e, "root@x.io", "admin")
	ae := testutil.As(e, admin)
	role := func(email string) string {
		var r string
		_ = e.DB.QueryRow(`SELECT role FROM users WHERE email=?`, email).Scan(&r)
		return r
	}
	login := func(sub, email string, groups ...any) error {
		_, err := OIDCLogin(e, sub, map[string]any{"email": email, "groups": groups})
		return err
	}
	// highest-ranked role group wins
	if err := login("s1", "a@x.io", "weave-guests", "weave-project-owners", "weave-users"); err != nil || role("a@x.io") != "projectOwner" {
		t.Fatalf("owner: %v %s", err, role("a@x.io"))
	}
	// IdP owns roles: promote and demote on later logins
	_ = login("s1", "a@x.io", "weave-admins")
	if role("a@x.io") != "admin" {
		t.Fatalf("promote: %s", role("a@x.io"))
	}
	_ = login("s1", "a@x.io", "weave-workers")
	if role("a@x.io") != "worker" {
		t.Fatalf("demote: %s", role("a@x.io"))
	}
	// no matching group -> boardUser by default
	_ = login("s2", "b@x.io", "unrelated")
	if role("b@x.io") != "boardUser" {
		t.Fatalf("default: %s", role("b@x.io"))
	}
	// the last admin is never demoted by an IdP change
	_ = login("s3", "only@x.io", "weave-admins")
	_, _ = e.DB.Exec(`UPDATE users SET role='boardUser' WHERE email='root@x.io'`)
	_ = login("s3", "only@x.io", "unrelated")
	if role("only@x.io") != "admin" {
		t.Fatalf("last admin demoted: %s", role("only@x.io"))
	}
	// requireGroup denies users outside the role groups
	tr := true
	call[OIDCSettings](t, ae, "auth.oidc.settings.update", OIDCSettingsIn{RequireGroup: &tr})
	if err := login("s4", "c@x.io", "unrelated"); err == nil {
		t.Fatal("requireGroup let an ungrouped user in")
	}
	if err := login("s4", "c@x.io", "weave-users"); err != nil {
		t.Fatal(err)
	}
	// team groups sync into Weave user groups (created, added, removed)
	_ = login("s5", "d@x.io", "weave-users", "weave-team-design", "weave-team-ops")
	members := func(email string) string {
		var n string
		_ = e.DB.QueryRow(`SELECT COALESCE(group_concat(g.name),'') FROM user_group_members m JOIN user_groups g ON g.id=m.group_id JOIN users u ON u.id=m.user_id WHERE u.email=? ORDER BY g.name`, email).Scan(&n)
		return n
	}
	if got := members("d@x.io"); got != "design,ops" && got != "ops,design" {
		t.Fatalf("teams %q", got)
	}
	_ = login("s5", "d@x.io", "weave-users", "weave-team-ops")
	if got := members("d@x.io"); got != "ops" {
		t.Fatalf("team removal %q", got)
	}
}

func TestMintKey(t *testing.T) {
	e := newEnv(t)
	out, err := MintKey(e.Ctx, e.DB, "Boss@X.io", "admin")
	if err != nil || !strings.HasPrefix(out.Key, "wk_") {
		t.Fatalf("mint: %+v %v", out, err)
	}
	c := reqEnv(e, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+out.Key) })
	if c.User == nil || c.User.Role != "admin" {
		t.Fatalf("minted key does not authenticate as admin: %+v", c.User)
	}
	// re-minting rotates the key and may change the role
	out2, err := MintKey(e.Ctx, e.DB, "boss@x.io", "projectOwner")
	if err != nil || out2.Key == out.Key {
		t.Fatalf("remint: %v", err)
	}
	if reqEnv(e, func(r *http.Request) { r.Header.Set("X-API-Key", out.Key) }).User != nil {
		t.Fatal("old key still valid")
	}
	if c := reqEnv(e, func(r *http.Request) { r.Header.Set("X-API-Key", out2.Key) }); c.User == nil || c.User.Role != "projectOwner" {
		t.Fatalf("role not updated: %+v", c.User)
	}
	if _, err := MintKey(e.Ctx, e.DB, "bad", ""); err == nil {
		t.Fatal("invalid email accepted")
	}
	if _, err := MintKey(e.Ctx, e.DB, "x@x.io", "king"); err == nil {
		t.Fatal("invalid role accepted")
	}
}

// The server must refuse to start unless OIDC is fully configured.
func TestValidateOIDCHook(t *testing.T) {
	e := newEnv(t)
	s := &server.Server{Cfg: config.Config{BaseURL: "https://w.example.com", SecretKey: "k"}, DB: e.DB, Hub: e.Hub}
	err := validateOIDCHook(s)
	if err == nil || !strings.Contains(err.Error(), "WEAVE_OIDC_ISSUER") || !strings.Contains(err.Error(), "https://w.example.com/auth/oidc/callback") {
		t.Fatalf("expected a clear missing-OIDC error, got %v", err)
	}
	for k, v := range map[string]string{"oidc.issuer": "https://idp.example.com", "oidc.clientId": "id", "oidc.clientSecret": "sec"} {
		_ = SetConfig(e, k, v)
	}
	if err := validateOIDCHook(s); err != nil {
		t.Fatalf("fully configured: %v", err)
	}
	s.Cfg.SecretKey = ""
	if err := validateOIDCHook(s); err == nil || !strings.Contains(err.Error(), "WEAVE_SECRET_KEY") {
		t.Fatalf("missing secret key must fail: %v", err)
	}
	// environment alone is enough
	e2 := newEnv(t)
	t.Setenv("WEAVE_OIDC_ISSUER", "https://idp.example.com")
	t.Setenv("WEAVE_OIDC_CLIENT_ID", "id")
	t.Setenv("WEAVE_OIDC_CLIENT_SECRET", "sec")
	if err := validateOIDCHook(&server.Server{Cfg: config.Config{SecretKey: "k"}, DB: e2.DB, Hub: e2.Hub}); err != nil {
		t.Fatalf("env config: %v", err)
	}
}
