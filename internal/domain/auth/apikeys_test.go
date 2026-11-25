package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

func keyUser(e *ops.Env, key string) *ops.Principal {
	return reqEnv(e, func(r *http.Request) { r.Header.Set("X-API-Key", key) }).User
}

func lastUsed(t *testing.T, e *ops.Env, id string) (s *string) {
	t.Helper()
	if err := e.DB.QueryRow(`SELECT last_used_at FROM api_keys WHERE id=?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return
}

// setMaxKeys overrides WEAVE_MAX_API_KEYS for one test (the value is parsed once, so the parse is reset around it).
func setMaxKeys(t *testing.T, v string) {
	t.Helper()
	maxKeysEnv = func() string { return v }
	maxKeysOnce, maxKeysVal = sync.Once{}, defaultAPIKeysPerUser
	t.Cleanup(func() {
		maxKeysEnv = func() string { return os.Getenv("WEAVE_MAX_API_KEYS") }
		maxKeysOnce, maxKeysVal = sync.Once{}, defaultAPIKeysPerUser
	})
}

func TestManyKeysIndependent(t *testing.T) {
	e := newEnv(t)
	u := mkUser(t, e, "a@example.com", "boardUser")
	as := testutil.As(e, u)
	k1 := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me", Name: "  work laptop "})
	k2 := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	k3 := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: u.UserID, Name: "ci", ExpiresInDays: 30})
	if k1.Name != "work laptop" || !strings.HasPrefix(k2.Name, "Key 20") || k3.ExpiresAt == nil || k1.ID == k2.ID {
		t.Fatalf("outputs: %+v %+v %+v", k1, k2, k3)
	}
	for _, k := range []APIKeyOut{k1, k2, k3} {
		if keyUser(e, k.Key) == nil {
			t.Fatalf("key %s must authenticate", k.Name)
		}
	}
	if n := call[*User](t, as, "auth.me", NoInput{}).APIKeyCount; n != 3 {
		t.Fatalf("apiKeyCount %d", n)
	}
	// revoke one by id: the others keep working
	call[OKOut](t, as, "apikey.revoke", APIKeyRevokeIn{ID: "me", KeyID: k2.ID})
	if keyUser(e, k2.Key) != nil || keyUser(e, k1.Key) == nil || keyUser(e, k3.Key) == nil {
		t.Fatal("revoking one key must leave the others working")
	}
	// neither or both selectors => 422, nothing deleted
	wantErr(t, as, "apikey.revoke", APIKeyRevokeIn{ID: "me"}, 422)
	wantErr(t, as, "apikey.revoke", APIKeyRevokeIn{ID: "me", KeyID: k1.ID, All: true}, 422)
	if keyUser(e, k1.Key) == nil {
		t.Fatal("a refused revoke deleted a key")
	}
	wantErr(t, as, "apikey.revoke", APIKeyRevokeIn{ID: "me", KeyID: "nope"}, 404)
	// revoke all
	call[OKOut](t, as, "apikey.revoke", APIKeyRevokeIn{ID: "me", All: true})
	if keyUser(e, k1.Key) != nil || keyUser(e, k3.Key) != nil {
		t.Fatal("revoke all left a key")
	}
	if l := call[[]APIKeyInfo](t, as, "apikey.list", APIKeyListIn{ID: "me"}); len(l) != 0 {
		t.Fatalf("list after revoke all: %+v", l)
	}
	// the owner revoking their own keys writes no audit row
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM admin_audit WHERE action='apikey.revoke'`).Scan(&n)
	if n != 0 {
		t.Fatalf("owner revoke audited %d rows", n)
	}
	wantErr(t, as, "apikey.generate", APIKeyGenIn{ID: "me", Name: strings.Repeat("x", 81)}, 422)
}

func TestAPIKeyCap(t *testing.T) {
	e := newEnv(t)
	as := testutil.As(e, mkUser(t, e, "a@example.com", "boardUser"))
	other := testutil.As(e, mkUser(t, e, "b@example.com", "boardUser"))
	setMaxKeys(t, "2")
	k1 := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	wantErr(t, as, "apikey.generate", APIKeyGenIn{ID: "me"}, 409)
	if keyUser(e, k1.Key) == nil {
		t.Fatal("a refused generate must not touch existing keys")
	}
	call[APIKeyOut](t, other, "apikey.generate", APIKeyGenIn{ID: "me"}) // the cap is per user
	call[OKOut](t, as, "apikey.revoke", APIKeyRevokeIn{ID: "me", KeyID: k1.ID})
	call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"}) // room again
	setMaxKeys(t, "0")
	for i := 0; i < 5; i++ {
		call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	}
	for _, v := range []string{"", "-3", "banana"} {
		setMaxKeys(t, v)
		if maxAPIKeys() != 50 {
			t.Fatalf("%q: default cap is 50", v)
		}
	}
}

func TestExpiredKeysDoNotCountTowardCap(t *testing.T) {
	e := newEnv(t)
	as := testutil.As(e, mkUser(t, e, "a@example.com", "boardUser"))
	setMaxKeys(t, "2")
	call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	old := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	wantErr(t, as, "apikey.generate", APIKeyGenIn{ID: "me"}, 409)
	if _, err := e.DB.Exec(`UPDATE api_keys SET expires_at=? WHERE id=?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), old.ID); err != nil {
		t.Fatal(err)
	}
	call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"}) // the expired one no longer counts
}

func TestKeyNameSanitised(t *testing.T) {
	for in, want := range map[string]string{
		"  work\x00 lap\ttop\n ":               "work laptop",
		"evil\u202etxt.exe\u2066x\u200f\u200e": "eviltxt.exex",
		"caf\u00e9 \u202d":                     "caf\u00e9",
	} {
		if got, err := keyName(in); err != nil || got != want {
			t.Fatalf("keyName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if got, _ := keyName("\u202e\x01 "); !strings.HasPrefix(got, "Key 20") {
		t.Fatalf("only control characters should give the default name, got %q", got)
	}
	if _, err := keyName(strings.Repeat("\u202e", 5) + strings.Repeat("x", 81)); err == nil {
		t.Fatal("the 80-rune limit applies after stripping")
	}
	if got, err := keyName(strings.Repeat("\u202e", 100) + strings.Repeat("x", 80)); err != nil || len(got) != 80 {
		t.Fatalf("stripped characters must not count toward the limit: %q %v", got, err)
	}
}

func TestAdminRevokeAgentKeyAudit(t *testing.T) {
	e := newEnv(t)
	admin := testutil.As(e, mkUser(t, e, "root@example.com", "admin"))
	ag := call[AgentOut](t, admin, "agent.create", AgentCreateIn{Name: "bot"})
	call[OKOut](t, admin, "apikey.revoke", APIKeyRevokeIn{ID: ag.ID, All: true})
	var kind, details string
	if err := e.DB.QueryRow(`SELECT target_kind, COALESCE(details,'') FROM admin_audit WHERE action='apikey.revoke'`).Scan(&kind, &details); err != nil {
		t.Fatalf("no audit row: %v", err)
	}
	if kind != "agent" {
		t.Fatalf("target kind %q, want agent", kind)
	}
	if strings.Contains(strings.ToLower(details), "hash") || regexp.MustCompile(`wk_[A-Za-z0-9_-]{20,}|[0-9a-f]{64}`).MatchString(details) {
		t.Fatalf("audit details carry key material: %s", details)
	}
}

func TestAPIKeyListHidesSecrets(t *testing.T) {
	e := newEnv(t)
	as := testutil.As(e, mkUser(t, e, "a@example.com", "boardUser"))
	k := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me", Name: "laptop", ExpiresInDays: 5})
	l := call[[]APIKeyInfo](t, as, "apikey.list", APIKeyListIn{ID: "me"})
	b, _ := json.Marshal(l)
	if len(l) != 1 || l[0].ID != k.ID || l[0].Name != "laptop" || l[0].Prefix != k.Prefix || l[0].ExpiresAt == nil || l[0].LastUsedAt != nil {
		t.Fatalf("list: %s", b)
	}
	if strings.Contains(string(b), k.Key) || strings.Contains(string(b), sha(k.Key)) || strings.Contains(strings.ToLower(string(b)), "hash") {
		t.Fatalf("list leaks the key or its hash: %s", b)
	}
	// the user output and every other op carry no key either
	me, _ := json.Marshal(call[*User](t, as, "auth.me", NoInput{}))
	if strings.Contains(string(me), k.Key) || strings.Contains(string(me), sha(k.Key)) {
		t.Fatal("auth.me leaks the key")
	}
	// expired keys are flagged
	if _, err := e.DB.Exec(`UPDATE api_keys SET expires_at=?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	if l = call[[]APIKeyInfo](t, as, "apikey.list", APIKeyListIn{ID: "me"}); !l[0].Expired {
		t.Fatal("expired not flagged")
	}
}

func TestAPIKeyOwnership(t *testing.T) {
	e := newEnv(t)
	alice, bob, root := mkUser(t, e, "a@example.com", "boardUser"), mkUser(t, e, "b@example.com", "boardUser"), mkUser(t, e, "root@example.com", "admin")
	as, bs, rs := testutil.As(e, alice), testutil.As(e, bob), testutil.As(e, root)
	ak := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me", Name: "alice laptop"})
	bk := call[APIKeyOut](t, bs, "apikey.generate", APIKeyGenIn{ID: "me", Name: "bob laptop"})
	// nobody lists or adds keys for someone else, admins included
	for _, who := range []*ops.Env{bs, rs} {
		wantErr(t, who, "apikey.list", APIKeyListIn{ID: alice.UserID}, 403)
		wantErr(t, who, "apikey.generate", APIKeyGenIn{ID: alice.UserID}, 403)
	}
	// a keyId of another user, addressed through your own id, deletes nothing
	wantErr(t, bs, "apikey.revoke", APIKeyRevokeIn{ID: "me", KeyID: ak.ID}, 404)
	wantErr(t, rs, "apikey.revoke", APIKeyRevokeIn{ID: bob.UserID, KeyID: ak.ID}, 404)
	if keyUser(e, ak.Key) == nil {
		t.Fatal("alice's key was revoked through another user's id")
	}
	// another user cannot revoke yours
	wantErr(t, bs, "apikey.revoke", APIKeyRevokeIn{ID: alice.UserID, All: true}, 403)
	// an admin may revoke, and it is audited (names and prefixes only)
	call[OKOut](t, rs, "apikey.revoke", APIKeyRevokeIn{ID: alice.UserID, KeyID: ak.ID})
	if keyUser(e, ak.Key) != nil || keyUser(e, bk.Key) == nil {
		t.Fatal("admin revoke hit the wrong key")
	}
	var action, actor, target, details, source string
	if err := e.DB.QueryRow(`SELECT action,actor_id,target_id,details,source FROM admin_audit WHERE action='apikey.revoke'`).Scan(&action, &actor, &target, &details, &source); err != nil {
		t.Fatalf("no audit row: %v", err)
	}
	if actor != root.UserID || target != alice.UserID || source != "api" || !strings.Contains(details, ak.Prefix) {
		t.Fatalf("audit row: %s %s %s %s", actor, target, details, source)
	}
	if strings.Contains(details, ak.Key) || strings.Contains(details, sha(ak.Key)) {
		t.Fatal("audit row holds a secret")
	}
	// admin revoke of nothing (no keys left) writes no second row
	call[OKOut](t, rs, "apikey.revoke", APIKeyRevokeIn{ID: alice.UserID, All: true})
	var n int
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM admin_audit WHERE action='apikey.revoke'`).Scan(&n)
	if n != 1 {
		t.Fatalf("%d audit rows", n)
	}
	// the admin's own keys are theirs to manage without an audit row
	rk := call[APIKeyOut](t, rs, "apikey.generate", APIKeyGenIn{ID: "me"})
	call[OKOut](t, rs, "apikey.revoke", APIKeyRevokeIn{ID: "me", KeyID: rk.ID})
	_ = e.DB.QueryRow(`SELECT COUNT(*) FROM admin_audit WHERE action='apikey.revoke'`).Scan(&n)
	if n != 1 {
		t.Fatal("self revoke was audited")
	}
	// the audit page lists it
	log := call[AuditPage](t, rs, "audit.list", AuditListIn{Action: "apikey.revoke"})
	if len(log.Items) != 1 || !strings.Contains(auditView(log.Items[0]).What, "Revoked all") && !strings.Contains(auditView(log.Items[0]).What, "Revoked an API key") {
		t.Fatalf("audit.list: %+v", log.Items)
	}
}

func TestAgentKeysStaySingle(t *testing.T) {
	e := newEnv(t)
	admin := testutil.As(e, mkUser(t, e, "root@example.com", "admin"))
	a := call[AgentOut](t, admin, "agent.create", AgentCreateIn{Name: "bot", ExpiresInDays: 30})
	count := func() (n int) {
		_ = e.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE user_id=?`, a.ID).Scan(&n)
		return
	}
	if count() != 1 || a.Key == nil || keyUser(e, a.Key.Key) == nil || a.KeyPrefix != a.Key.Prefix {
		t.Fatalf("create: %+v count=%d", a, count())
	}
	r := call[AgentOut](t, admin, "agent.rotateKey", AgentRotateIn{ID: a.ID})
	if count() != 1 || keyUser(e, a.Key.Key) != nil || keyUser(e, r.Key.Key) == nil || r.KeyPrefix != r.Key.Prefix {
		t.Fatalf("rotate must leave exactly one working key: count=%d", count())
	}
	// an agent cannot add keys to itself (nor can it list/generate for anyone else)
	self := testutil.As(e, &ops.Principal{UserID: a.ID, Role: "boardUser", Name: "bot", IsAgent: true})
	wantErr(t, self, "apikey.generate", APIKeyGenIn{ID: "me"}, 403)
	wantErr(t, self, "apikey.generate", APIKeyGenIn{ID: a.ID}, 403)
	if count() != 1 {
		t.Fatal("agent minted a sibling key")
	}
	_, err := ops.Typed[any](self, "apikey.generate", APIKeyGenIn{ID: "me"})
	if err == nil || !strings.Contains(err.Error(), "agent.rotateKey") {
		t.Fatalf("refusal must say how: %v", err)
	}
}

func TestDeactivatedUsersKeysStop(t *testing.T) {
	e := newEnv(t)
	admin := testutil.As(e, mkUser(t, e, "root@example.com", "admin"))
	u := mkUser(t, e, "a@example.com", "boardUser")
	as := testutil.As(e, u)
	k1 := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	k2 := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	call[*User](t, admin, "user.deactivate", IDIn{ID: u.UserID})
	if keyUser(e, k1.Key) != nil || keyUser(e, k2.Key) != nil {
		t.Fatal("a deactivated user's keys must stop working")
	}
	call[*User](t, admin, "user.activate", IDIn{ID: u.UserID})
	if keyUser(e, k1.Key) == nil {
		t.Fatal("reactivated user's keys work again")
	}
	call[*User](t, admin, "user.delete", IDIn{ID: u.UserID})
	if keyUser(e, k1.Key) != nil || keyUser(e, k2.Key) != nil {
		t.Fatal("a deleted user's keys must stop working")
	}
}

func TestLastUsedThrottle(t *testing.T) {
	e := newEnv(t)
	as := testutil.As(e, mkUser(t, e, "a@example.com", "boardUser"))
	k := call[APIKeyOut](t, as, "apikey.generate", APIKeyGenIn{ID: "me"})
	if lastUsed(t, e, k.ID) != nil {
		t.Fatal("a new key has never been used")
	}
	if keyUser(e, k.Key) == nil {
		t.Fatal("key rejected")
	}
	first := lastUsed(t, e, k.ID)
	if first == nil {
		t.Fatal("first use not recorded")
	}
	// within 10 minutes: no write
	recent := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	_, _ = e.DB.Exec(`UPDATE api_keys SET last_used_at=?`, recent)
	keyUser(e, k.Key)
	if got := lastUsed(t, e, k.ID); got == nil || *got != recent {
		t.Fatalf("written again inside the throttle window: %v", got)
	}
	l := call[[]APIKeyInfo](t, as, "apikey.list", APIKeyListIn{ID: "me"})
	if l[0].LastUsedAt == nil || *l[0].LastUsedAt != recent {
		t.Fatal("list must show last use")
	}
	// older than 10 minutes: refreshed
	old := time.Now().UTC().Add(-11 * time.Minute).Format(time.RFC3339)
	_, _ = e.DB.Exec(`UPDATE api_keys SET last_used_at=?`, old)
	keyUser(e, k.Key)
	if got := lastUsed(t, e, k.ID); got == nil || *got == old {
		t.Fatal("not refreshed after the window")
	}
	// background requests count for keys too (an agent that only listens on /events shows a real last use), throttled
	for _, f := range []func(*http.Request){
		func(r *http.Request) { r.Header.Set(BackgroundHeader, "1") },
		func(r *http.Request) { r.URL.Path = "/events" },
		func(r *http.Request) { r.URL.Path = "/api/v1/presence" },
	} {
		_, _ = e.DB.Exec(`UPDATE api_keys SET last_used_at=NULL`)
		c := *e
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("X-API-Key", k.Key)
		f(r)
		c.Req = r
		if ops.Authenticate(&c, r) == nil {
			t.Fatal("background request must still authenticate")
		}
		if lastUsed(t, e, k.ID) == nil {
			t.Fatal("a background request must record the key's last use")
		}
		_, _ = e.DB.Exec(`UPDATE api_keys SET last_used_at=?`, recent)
		ops.Authenticate(&c, r)
		if got := lastUsed(t, e, k.ID); got == nil || *got != recent {
			t.Fatal("background requests are throttled like any other")
		}
	}
	// an expired key is rejected and not touched; a failing write does not fail the request
	_, _ = e.DB.Exec(`UPDATE api_keys SET last_used_at=NULL`)
	_, _ = e.DB.Exec(`UPDATE api_keys SET expires_at=?`, time.Now().UTC().Add(-time.Hour).Format(time.RFC3339))
	if keyUser(e, k.Key) != nil || lastUsed(t, e, k.ID) != nil {
		t.Fatal("expired key must be rejected without being touched")
	}
	_, _ = e.DB.Exec(`UPDATE api_keys SET expires_at=NULL`)
	_, _ = e.DB.Exec(`CREATE TRIGGER no_touch BEFORE UPDATE OF last_used_at ON api_keys BEGIN SELECT RAISE(ABORT,'read-only'); END`)
	if keyUser(e, k.Key) == nil {
		t.Fatal("a failed last_used write must not fail the request")
	}
}

// The upgrade path: the old one-key columns are copied by migration 0901's backfill (the statement is read from the
// migration file itself) and every old key keeps authenticating, expiry included.
func TestBackfilledKeysAuthenticate(t *testing.T) {
	e := newEnv(t)
	sqlText, err := os.ReadFile("../../db/migrations/0901_api_keys.sql")
	if err != nil {
		t.Fatal(err)
	}
	backfill := string(sqlText)[strings.Index(string(sqlText), "INSERT INTO api_keys"):]
	u := mkUser(t, e, "a@example.com", "boardUser")
	x := mkUser(t, e, "x@example.com", "boardUser")
	admin := testutil.As(e, mkUser(t, e, "root@example.com", "admin"))
	ag := call[AgentOut](t, admin, "agent.create", AgentCreateIn{Name: "bot"})
	soon := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339)
	gone := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	legacy := func(id, key string, exp any) {
		if _, err := e.DB.Exec(`UPDATE users SET api_key_hash=?,api_key_prefix=?,api_key_created_at=?,api_key_expires_at=? WHERE id=?`, sha(key), key[:8], db.Now(), exp, id); err != nil {
			t.Fatal(err)
		}
	}
	legacy(u.UserID, "wk_legacyuserkey", nil)
	legacy(x.UserID, "wk_legacyexpiring", soon)
	legacy(ag.ID, "wk_legacyagentkey", nil)
	expired := mkUser(t, e, "old@example.com", "boardUser")
	legacy(expired.UserID, "wk_legacyexpired", gone)
	if _, err := e.DB.Exec(`DELETE FROM api_keys`); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DB.Exec(backfill); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"wk_legacyuserkey", "wk_legacyexpiring", "wk_legacyagentkey"} {
		if keyUser(e, k) == nil {
			t.Fatalf("legacy key %s no longer authenticates after the backfill", k)
		}
	}
	c := reqEnv(e, func(r *http.Request) { r.Header.Set("X-API-Key", "wk_legacyexpired") })
	if c.User != nil || !strings.Contains(c.AuthMsg, "expired") {
		t.Fatal("an already expired legacy key must stay expired")
	}
	me := call[*User](t, testutil.As(e, x), "auth.me", NoInput{})
	if me.APIKeyExpiresAt == nil || *me.APIKeyExpiresAt != soon || me.APIKeyCount != 1 {
		t.Fatalf("expiry not preserved: %+v", me)
	}
	// a backfilled key coexists with a new one
	nk := call[APIKeyOut](t, testutil.As(e, x), "apikey.generate", APIKeyGenIn{ID: "me"})
	if keyUser(e, nk.Key) == nil || keyUser(e, "wk_legacyexpiring") == nil {
		t.Fatal("old and new key must both work")
	}
}

func TestSettingsPageKeysUI(t *testing.T) {
	c, idp := newClient(t)
	if w := c.signIn(idp, userClaims("ui@example.com", "weave-users")); w.Code != 303 {
		t.Fatalf("sign-in: %d", w.Code)
	}
	var uid string
	_ = c.e.DB.QueryRow(`SELECT id FROM users WHERE email='ui@example.com'`).Scan(&uid)
	u := &ops.Principal{UserID: uid}
	hx := map[string]string{"HX-Request": "true"}
	if w := c.do("POST", "/ui/settings/apikey", url.Values{"name": {"desk"}}, hx); w.Code != 200 || !strings.Contains(w.Body.String(), "wk_") {
		t.Fatalf("create: %d", w.Code)
	}
	if w := c.do("POST", "/ui/settings/apikey", url.Values{"name": {"laptop"}}, hx); w.Code != 200 {
		t.Fatalf("create 2: %d", w.Code)
	}
	var n int
	_ = c.e.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE user_id=?`, u.UserID).Scan(&n)
	if n != 2 {
		t.Fatalf("%d keys, want 2 (Generate adds)", n)
	}
	if w := c.do("POST", "/ui/settings/apikey/revoke?all=true", url.Values{}, hx); w.Code != 200 {
		t.Fatalf("revoke all: %d", w.Code)
	}
	_ = c.e.DB.QueryRow(`SELECT COUNT(*) FROM api_keys WHERE user_id=?`, u.UserID).Scan(&n)
	if n != 0 {
		t.Fatalf("%d keys left", n)
	}
}
