package auth

import (
	"net/url"
	"regexp"
	"strings"
	"testing"
)

func keyUIClient(t *testing.T, email string) (*client, *fakeIdP) {
	t.Helper()
	c, idp := newClient(t)
	if w := c.signIn(idp, userClaims(email, "weave-users")); w.Code != 303 {
		t.Fatalf("sign in: %d", w.Code)
	}
	return c, idp
}

func TestAPIKeysSectionUI(t *testing.T) {
	c, _ := keyUIClient(t, "keys@example.com")
	hx := map[string]string{"HX-Request": "true"}
	section := func() string { return c.do("GET", "/settings?section=apikey", nil, nil).Body.String() }

	if b := section(); !strings.Contains(b, "No API keys yet.") || strings.Contains(b, "Revoke all") {
		t.Fatalf("empty state: %s", b)
	}

	w := c.do("POST", "/ui/settings/apikey", url.Values{"name": {"work laptop"}}, hx)
	full := regexp.MustCompile(`wk_[A-Za-z0-9_-]{40,}`).FindString(w.Body.String())
	if full == "" || !strings.Contains(w.Body.String(), "will not be shown again") || !strings.Contains(w.Body.String(), "work laptop") {
		t.Fatalf("create: %s", w.Body.String())
	}
	if strings.Count(w.Body.String(), full) != 1 {
		t.Fatal("full key must appear exactly once (the copy box)")
	}
	if b := section(); strings.Contains(b, full) || !strings.Contains(b, "work laptop") || !strings.Contains(b, "Never expires") || !strings.Contains(b, "Last used: never") {
		t.Fatalf("list after create must not contain the key: %s", b)
	}
	c.do("POST", "/ui/settings/apikey", url.Values{"name": {"home desktop"}, "expiresInDays": {"30"}}, hx)

	b := section()
	ids := regexp.MustCompile(`data-key-id="([^"]+)"`).FindAllStringSubmatch(b, -1)
	if len(ids) != 2 || !strings.Contains(b, "home desktop") || !strings.Contains(b, "Expires 20") || !strings.Contains(b, "Revoke all") {
		t.Fatalf("two keys: %s", b)
	}
	for _, id := range ids {
		if strings.Count(b, `/ui/settings/apikey/revoke?keyId=`+id[1]) != 2 { // hx-post + action
			t.Fatalf("one revoke form per row (%s)", id[1])
		}
	}
	if strings.Count(b, "as-revoke") != 2 {
		t.Fatal("expected one Revoke button per key")
	}

	w = c.do("POST", "/ui/settings/apikey/revoke?keyId="+ids[0][1], url.Values{}, hx)
	left := regexp.MustCompile(`data-key-id="([^"]+)"`).FindAllStringSubmatch(w.Body.String(), -1)
	if len(left) != 1 || left[0][1] != ids[1][1] || strings.Contains(w.Body.String(), "Revoke all") {
		t.Fatalf("revoke one: %s", w.Body.String())
	}
	c.do("POST", "/ui/settings/apikey/revoke?all=true", url.Values{}, hx)
	if b := section(); !strings.Contains(b, "No API keys yet.") {
		t.Fatalf("revoke all: %s", b)
	}
}

func TestAPIKeysSectionIsPrivate(t *testing.T) {
	a, a2idp := keyUIClient(t, "a@example.com")
	hx := map[string]string{"HX-Request": "true"}
	a.do("POST", "/ui/settings/apikey", url.Values{"name": {"secret-name-of-a"}}, hx)
	if !strings.Contains(a.do("GET", "/settings?section=apikey", nil, nil).Body.String(), "secret-name-of-a") {
		t.Fatal("owner should see own key")
	}
	b := &client{t: t, e: a.e, h: a.h} // same server, its own cookies
	idp := a2idp
	if w := b.signIn(idp, userClaims("b@example.com", "weave-users")); w.Code != 303 {
		t.Fatalf("sign in b: %d", w.Code)
	}
	if body := b.do("GET", "/settings?section=apikey", nil, nil).Body.String(); strings.Contains(body, "secret-name-of-a") || !strings.Contains(body, "No API keys yet.") {
		t.Fatalf("another user's keys leaked: %s", body)
	}
}

// Revoking through the UI route binds ?keyId= and ?all=true from the query; another user's key id deletes nothing.
func TestAPIKeyRevokeUIIsolation(t *testing.T) {
	a, idp := keyUIClient(t, "a@example.com")
	hx := map[string]string{"HX-Request": "true"}
	b := &client{t: t, e: a.e, h: a.h}
	if w := b.signIn(idp, userClaims("b@example.com", "weave-users")); w.Code != 303 {
		t.Fatalf("sign in b: %d", w.Code)
	}
	b.do("POST", "/ui/settings/apikey", url.Values{"name": {"bobs"}}, hx)
	var bobKey string
	if err := a.e.DB.QueryRow(`SELECT k.id FROM api_keys k JOIN users u ON u.id=k.user_id WHERE u.email='b@example.com'`).Scan(&bobKey); err != nil {
		t.Fatal(err)
	}
	a.do("POST", "/ui/settings/apikey", url.Values{"name": {"alices"}}, hx)
	count := func() (n int) {
		_ = a.e.DB.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n)
		return
	}
	if count() != 2 {
		t.Fatalf("want 2 keys, got %d", count())
	}
	w := a.do("POST", "/ui/settings/apikey/revoke?keyId="+bobKey, url.Values{}, hx)
	if w.Code < 400 && !strings.Contains(strings.ToLower(w.Body.String()), "not found") {
		t.Fatalf("revoking another user's key must fail: %d %s", w.Code, w.Body.String())
	}
	if count() != 2 {
		t.Fatal("another user's key was deleted")
	}
	// neither keyId nor all: refused, nothing deleted
	a.do("POST", "/ui/settings/apikey/revoke", url.Values{}, hx)
	if count() != 2 {
		t.Fatal("a revoke without keyId or all deleted keys")
	}
	// all=true only takes the caller's keys
	a.do("POST", "/ui/settings/apikey/revoke?all=true", url.Values{}, hx)
	if count() != 1 {
		t.Fatalf("all=true must delete exactly the caller's keys, %d left", count())
	}
}
