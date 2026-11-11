package auth

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

func claimsFor(email string, groups ...any) map[string]any {
	return map[string]any{"email": email, "groups": groups}
}

func TestOIDCSubjectMismatchIsDenied(t *testing.T) {
	e := newEnv(t)
	if _, err := OIDCLogin(e, "https://idp|alice-sub", claimsFor("alice@x.io", "weave-admins")); err != nil {
		t.Fatal(err)
	}
	var aliceID string
	_ = e.DB.QueryRow(`SELECT id FROM users WHERE email='alice@x.io'`).Scan(&aliceID)

	// the same subject can sign in again and again
	for i := 0; i < 2; i++ {
		if _, err := OIDCLogin(e, "https://idp|alice-sub", claimsFor("alice@x.io", "weave-admins")); err != nil {
			t.Fatalf("repeat login: %v", err)
		}
	}

	// a different identity presenting alice's e-mail is refused, with a clear message, and gets no session
	out, err := OIDCLogin(e, "https://idp|ATTACKER", claimsFor("alice@x.io", "weave-users"))
	oe, ok := err.(*ops.Error)
	if !ok || oe.Status != 403 || !strings.Contains(oe.Msg, "linked to a different identity") || out.Token != "" {
		t.Fatalf("takeover must be denied with 403: out=%+v err=%v", out, err)
	}
	var sub, role string
	_ = e.DB.QueryRow(`SELECT oidc_sub,role FROM users WHERE id=?`, aliceID).Scan(&sub, &role)
	if sub != "https://idp|alice-sub" || role != "admin" {
		t.Fatalf("account must be untouched: sub=%q role=%q", sub, role)
	}
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	if n != 1 {
		t.Fatalf("denied login must not provision a second user, got %d", n)
	}
}

func TestOIDCFirstLoginBindsPrecreatedUser(t *testing.T) {
	e := newEnv(t)
	pre := mkUser(t, e, "pre@x.io", "boardUser") // pre-created: no oidc_sub yet
	if _, err := OIDCLogin(e, "https://idp|pre-sub", claimsFor("pre@x.io", "weave-users")); err != nil {
		t.Fatalf("first login of a pre-created user: %v", err)
	}
	var id, sub string
	_ = e.DB.QueryRow(`SELECT id,oidc_sub FROM users WHERE email='pre@x.io'`).Scan(&id, &sub)
	if id != pre.UserID || sub != "https://idp|pre-sub" {
		t.Fatalf("bind: id=%s sub=%q", id, sub)
	}
	if _, err := OIDCLogin(e, "https://idp|someone-else", claimsFor("pre@x.io", "weave-users")); err == nil {
		t.Fatal("second identity must not rebind")
	}
}

func TestOIDCRelinkThenLogin(t *testing.T) {
	e := newEnv(t)
	admin := mkUser(t, e, "root@x.io", "admin")
	as := testutil.As(e, admin)
	if _, err := OIDCLogin(e, "https://idp|old", claimsFor("bob@x.io", "weave-users")); err != nil {
		t.Fatal(err)
	}
	var bobID string
	_ = e.DB.QueryRow(`SELECT id FROM users WHERE email='bob@x.io'`).Scan(&bobID)
	if _, err := OIDCLogin(e, "https://idp|new", claimsFor("bob@x.io", "weave-users")); err == nil {
		t.Fatal("must be denied before relink")
	}
	// only admins may relink
	wantErr(t, testutil.As(e, mkUser(t, e, "mallory@x.io", "boardUser")), "user.relink", IDIn{ID: bobID}, 403)
	call[*User](t, as, "user.relink", IDIn{ID: bobID})
	var sessions int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id=?`, bobID).Scan(&sessions)
	if sessions != 0 {
		t.Fatal("relink must end the user's sessions")
	}
	if _, err := OIDCLogin(e, "https://idp|new", claimsFor("bob@x.io", "weave-users")); err != nil {
		t.Fatalf("after relink the new identity binds: %v", err)
	}
	if _, err := OIDCLogin(e, "https://idp|old", claimsFor("bob@x.io", "weave-users")); err == nil {
		t.Fatal("identity is bound again; the old one must now be denied")
	}
}

func TestBreakGlassRelinkByEmail(t *testing.T) {
	e := newEnv(t)
	if _, err := OIDCLogin(e, "https://idp|old", claimsFor("carol@x.io", "weave-users")); err != nil {
		t.Fatal(err)
	}
	if err := RelinkByEmail(e.Ctx, e.DB, "nobody@x.io"); err == nil {
		t.Fatal("unknown user must be an error")
	}
	if err := RelinkByEmail(e.Ctx, e.DB, "carol@x.io"); err != nil {
		t.Fatal(err)
	}
	if _, err := OIDCLogin(e, "https://idp|new", claimsFor("carol@x.io", "weave-users")); err != nil {
		t.Fatalf("login after host relink: %v", err)
	}
}

// Several identities racing for one unbound account: exactly one wins.
func TestOIDCBindIsAtomic(t *testing.T) {
	e := newEnv(t)
	mkUser(t, e, "race@x.io", "boardUser")
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sub := "https://idp|racer-" + string(rune('a'+i))
			if _, err := OIDCLogin(e, sub, claimsFor("race@x.io", "weave-users")); err == nil {
				atomic.AddInt32(&wins, 1)
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("exactly one identity may bind the account, got %d", wins)
	}
}

func TestSignOutEverywhere(t *testing.T) {
	e := newEnv(t)
	admin := testutil.As(e, mkUser(t, e, "root@x.io", "admin"))
	u := mkUser(t, e, "u@x.io", "boardUser")
	for i := 0; i < 3; i++ {
		if _, err := newSession(e, u.UserID, false); err != nil {
			t.Fatal(err)
		}
	}
	wantErr(t, testutil.As(e, u), "user.sessions.revokeAll", IDIn{ID: u.UserID}, 403)
	call[OKOut](t, admin, "user.sessions.revokeAll", IDIn{ID: u.UserID})
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id=?`, u.UserID).Scan(&n)
	if n != 0 {
		t.Fatalf("sessions left: %d", n)
	}
}

func TestDefaultSessionLifetimeIsOneDay(t *testing.T) {
	t.Setenv("WEAVE_SESSION_HOURS", "")
	if h := config.Load().SessionHours; h != 24 {
		t.Fatalf("config default = %d, want 24", h)
	}
	e := newEnv(t)
	e.Svc["config"] = config.Config{DataDir: t.TempDir()} // unset -> sessionTTL fallback
	if d := sessionTTL(e); d != 24*time.Hour {
		t.Fatalf("fallback ttl = %v", d)
	}
	t.Setenv("WEAVE_SESSION_HOURS", "48")
	if h := config.Load().SessionHours; h != 48 {
		t.Fatalf("override = %d", h)
	}
}

func TestAPIKeyExpiry(t *testing.T) {
	e := newEnv(t)
	u := mkUser(t, e, "k@x.io", "boardUser")
	as := testutil.As(e, u)
	hdr := func(k string) *ops.Env {
		return reqEnv(e, func(r *http.Request) { r.Header.Set("X-API-Key", k) })
	}

	forever := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	if forever.ExpiresAt != nil {
		t.Fatal("default must be no expiry")
	}
	k := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me", ExpiresInDays: 30})
	if k.ExpiresAt == nil {
		t.Fatal("expiresAt missing")
	}
	exp, _ := time.Parse(time.RFC3339, *k.ExpiresAt)
	if d := time.Until(exp); d < 29*24*time.Hour || d > 31*24*time.Hour {
		t.Fatalf("expiry = %v from now", d)
	}
	if hdr(k.Key).User == nil {
		t.Fatal("fresh key must work")
	}
	me := call[*User](t, as, "auth.me", NoInput{})
	if me.APIKeyExpiresAt == nil || me.APIKeyExpired {
		t.Fatalf("user output: %+v", me)
	}
	for _, bad := range []int{-1, 3651} {
		wantErr(t, as, "apikey.generate", APIKeyGenIn{ID: "me", ExpiresInDays: bad}, 422)
	}

	past := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	if _, err := e.DB.Exec(`UPDATE users SET api_key_expires_at=? WHERE id=?`, past, u.UserID); err != nil {
		t.Fatal(err)
	}
	got := hdr(k.Key)
	if got.User != nil || !strings.Contains(got.AuthMsg, "expired") {
		t.Fatalf("expired key must be rejected with a clear message: user=%v msg=%q", got.User, got.AuthMsg)
	}
	me = call[*User](t, testutil.As(e, u), "auth.me", NoInput{})
	if !me.APIKeyExpired {
		t.Fatal("expired key must be flagged in user output")
	}
	// rotating clears the expiry
	k2 := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	if hdr(k2.Key).User == nil {
		t.Fatal("new key must work")
	}
}

func TestAPIKeyExpiryOverHTTP401(t *testing.T) {
	c, _ := newClient(t)
	u := mkUser(t, c.e, "h@x.io", "boardUser")
	k := call[APIKeyOut](t, testutil.As(c.e, u), "apikey.generate", APIKeyGenIn{ID: "me", ExpiresInDays: 1})
	if _, err := c.e.DB.Exec(`UPDATE users SET api_key_expires_at=?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	w := c.do("GET", "/api/v1/auth/me", nil, map[string]string{"X-API-Key": k.Key})
	if w.Code != 401 || !strings.Contains(w.Body.String(), "API key expired") {
		t.Fatalf("expired key: %d %s", w.Code, w.Body.String())
	}
}

func TestAgentKeyExpiry(t *testing.T) {
	e := newEnv(t)
	admin := testutil.As(e, mkUser(t, e, "root@x.io", "admin"))
	a := call[AgentOut](t, admin, "agent.create", AgentCreateIn{Name: "bot", ExpiresInDays: 90})
	if a.APIKeyExpiresAt == nil || a.APIKeyExpired || a.Key == nil || a.Key.ExpiresAt == nil {
		t.Fatalf("agent.create expiry: %+v", a)
	}
	wantErr(t, admin, "agent.create", AgentCreateIn{Name: "bad", ExpiresInDays: 9999}, 422)
	if _, err := e.DB.Exec(`UPDATE users SET api_key_expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), a.ID); err != nil {
		t.Fatal(err)
	}
	if reqEnv(e, func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+a.Key.Key) }).User != nil {
		t.Fatal("expired agent key must be rejected")
	}
	list := call[[]AgentOut](t, admin, "agent.list", NoInput{})
	if len(list) != 1 || !list[0].APIKeyExpired {
		t.Fatalf("agent.list must flag the expired key: %+v", list)
	}
	r := call[AgentOut](t, admin, "agent.rotateKey", AgentRotateIn{ID: a.ID})
	if r.APIKeyExpiresAt != nil || r.APIKeyExpired {
		t.Fatalf("rotate without expiry clears it: %+v", r)
	}
	r = call[AgentOut](t, admin, "agent.rotateKey", AgentRotateIn{ID: a.ID, ExpiresInDays: 365})
	if r.APIKeyExpiresAt == nil {
		t.Fatal("rotateKey expiresInDays")
	}
}

func TestMaxIdleMinutesBoundsAutoLogout(t *testing.T) {
	c, idp := newClient(t)
	c.signIn(idp, userClaims("idle2@x.io", "weave-users"))
	age := func(d time.Duration) {
		if _, err := c.e.DB.Exec(`UPDATE sessions SET last_used_at=?`, time.Now().UTC().Add(-d).Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	// no bound configured, user has no auto-logout: a day-old idle session still works
	age(24 * time.Hour)
	if w := c.do("GET", "/settings", nil, nil); w.Code != 200 {
		t.Fatalf("unbounded: %d", w.Code)
	}
	// instance bound of 30 minutes (stored setting) applies even if the user allows 600
	if err := SetConfig(c.e, "session.maxIdleMinutes", "30"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.e.DB.Exec(`UPDATE users SET auto_logout_minutes=600`); err != nil {
		t.Fatal(err)
	}
	age(10 * time.Minute)
	if w := c.do("GET", "/settings", nil, nil); w.Code != 200 {
		t.Fatalf("inside bound: %d", w.Code)
	}
	age(31 * time.Minute)
	if w := c.do("GET", "/settings", nil, nil); w.Code != 303 {
		t.Fatalf("beyond instance bound must log out: %d", w.Code)
	}
	// a stricter per-user value still wins over a looser instance bound
	c.signIn(idp, userClaims("idle2@x.io", "weave-users"))
	if _, err := c.e.DB.Exec(`UPDATE users SET auto_logout_minutes=5`); err != nil {
		t.Fatal(err)
	}
	age(6 * time.Minute)
	if w := c.do("GET", "/settings", nil, nil); w.Code != 303 {
		t.Fatalf("per-user limit must still apply: %d", w.Code)
	}
	// the environment variable overrides the stored value
	c.signIn(idp, userClaims("idle2@x.io", "weave-users"))
	if _, err := c.e.DB.Exec(`UPDATE users SET auto_logout_minutes=NULL`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_SESSION_MAX_IDLE_MINUTES", "2")
	age(3 * time.Minute)
	if w := c.do("GET", "/settings", nil, nil); w.Code != 303 {
		t.Fatalf("env bound: %d", w.Code)
	}
}
