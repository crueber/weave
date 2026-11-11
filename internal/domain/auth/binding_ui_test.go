package auth

import (
	"net/url"
	"strings"
	"testing"
)

func TestAdminUIRelinkSignOutAndKeyExpiryForm(t *testing.T) {
	c, idp := newClient(t)
	if w := c.signIn(idp, userClaims("boss@x.io", "weave-admins")); w.Code != 303 {
		t.Fatalf("admin sign-in: %d", w.Code)
	}
	if _, err := OIDCLogin(c.e, "sub-bob", claimsFor("bob@x.io", "weave-users")); err != nil {
		t.Fatal(err)
	}
	var bobID string
	_ = c.e.DB.QueryRow(`SELECT id FROM users WHERE email='bob@x.io'`).Scan(&bobID)
	hx := map[string]string{"HX-Request": "true"}

	w := c.do("GET", "/admin/users", nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Relink SSO identity") || !strings.Contains(w.Body.String(), "Sign out everywhere") {
		t.Fatalf("users page: %d", w.Code)
	}
	if w := c.do("POST", "/ui/admin/users/"+bobID+"/sessions/revoke-all", url.Values{}, hx); w.Code != 200 || !strings.Contains(w.Body.String(), "signed out everywhere") {
		t.Fatalf("revoke-all: %d %s", w.Code, w.Body.String())
	}
	if w := c.do("POST", "/ui/admin/users/"+bobID+"/relink", url.Values{}, hx); w.Code != 200 || !strings.Contains(w.Body.String(), "unlinked") {
		t.Fatalf("relink: %d %s", w.Code, w.Body.String())
	}
	var sub *string
	_ = c.e.DB.QueryRow(`SELECT oidc_sub FROM users WHERE id=?`, bobID).Scan(&sub)
	if sub != nil {
		t.Fatalf("oidc_sub must be cleared, got %q", *sub)
	}

	// settings: the expiry select posts expiresInDays
	if w := c.do("GET", "/settings", nil, nil); !strings.Contains(w.Body.String(), `name="expiresInDays"`) {
		t.Fatal("settings page lacks the key-expiry select")
	}
	if w := c.do("POST", "/ui/settings/apikey", url.Values{"expiresInDays": {"30"}}, hx); w.Code != 200 {
		t.Fatalf("create expiring key: %d %s", w.Code, w.Body.String())
	}
	var exp *string
	_ = c.e.DB.QueryRow(`SELECT api_key_expires_at FROM users WHERE email='boss@x.io'`).Scan(&exp)
	if exp == nil {
		t.Fatal("expiry not stored")
	}
}
