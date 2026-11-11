package auth

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/config"
)

func TestEmailVerifiedForms(t *testing.T) {
	for v, want := range map[any]bool{false: true, "false": true, "FALSE": true, " false ": true, true: false, "true": false, nil: false, "": false, float64(0): false} {
		if got := emailUnverified(v); got != want {
			t.Errorf("emailUnverified(%#v) = %v, want %v", v, got, want)
		}
	}
	c, idp := newClient(t)
	cl := userClaims("s@x.io", "weave-users")
	cl["email_verified"] = "false"
	if w := c.signIn(idp, cl); !strings.HasPrefix(w.Header().Get("Location"), "/login?error=") {
		t.Fatalf(`email_verified "false" must be rejected: %d %s`, w.Code, w.Header().Get("Location"))
	}
	// an absent claim is still tolerated (not every IdP sends it)
	cl = userClaims("absent@x.io", "weave-users")
	delete(cl, "email_verified")
	if w := c.signIn(idp, cl); w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("absent email_verified must be tolerated: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestUserinfoMergedOnlyForSameSubject(t *testing.T) {
	c, idp := newClient(t)
	// ID token has no email/groups: they would come from userinfo
	idp.mu.Lock()
	idp.userinfo = map[string]any{"sub": "someone-else", "email": "victim@x.io", "groups": []string{"weave-admins"}}
	idp.mu.Unlock()
	w := c.signIn(idp, map[string]any{"sub": "s1", "name": "N"})
	if !strings.HasPrefix(w.Header().Get("Location"), "/login?error=") {
		t.Fatalf("userinfo for another subject must be ignored: %d %s", w.Code, w.Header().Get("Location"))
	}
	idp.mu.Lock()
	idp.userinfo = map[string]any{"sub": "s1", "email": "ok@x.io", "groups": []string{"weave-admins"}}
	idp.mu.Unlock()
	w = c.signIn(idp, map[string]any{"sub": "s1", "name": "N"})
	if w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("userinfo for the same subject must be merged: %d %s", w.Code, w.Header().Get("Location"))
	}
}

func TestCookiesSecureFromBaseURLNotForwardedHeader(t *testing.T) {
	c, idp := newClient(t)
	// http base URL: a spoofed X-Forwarded-Proto must not flip Secure
	w := c.do("GET", "/auth/oidc/login?next=/boards/x", nil, map[string]string{"X-Forwarded-Proto": "https"})
	if w.Code != 303 {
		t.Fatalf("start: %d", w.Code)
	}
	for _, ck := range w.Result().Cookies() {
		if ck.Secure {
			t.Errorf("cookie %s Secure under http BaseURL", ck.Name)
		}
	}
	// https base URL: every cookie is Secure + HttpOnly + Lax, including the state, PKCE and next cookies
	cfg := cfgOf(c.e)
	https := config.Config{DataDir: cfg.DataDir, BaseURL: "https://weave.test", SecretKey: cfg.SecretKey, SessionHours: cfg.SessionHours}
	c.e.Svc["config"] = https
	w = c.do("GET", "/auth/oidc/login?next=/boards/x", nil, nil)
	names := map[string]bool{}
	for _, ck := range w.Result().Cookies() {
		names[ck.Name] = true
		if !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
			t.Errorf("cookie %s: Secure=%v HttpOnly=%v SameSite=%v", ck.Name, ck.Secure, ck.HttpOnly, ck.SameSite)
		}
	}
	for _, n := range []string{oidcStateCk, oidcVerifierCk, oidcNextCk} {
		if !names[n] {
			t.Errorf("missing cookie %s", n)
		}
	}
	_ = idp
}

func TestLogoutOnlyOnPost(t *testing.T) {
	c, idp := newClient(t)
	c.signIn(idp, userClaims("root@x.io", "weave-admins"))
	if w := c.do("GET", "/logout", nil, nil); w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("GET /logout: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := c.do("GET", "/settings", nil, nil); w.Code != 200 {
		t.Fatalf("GET /logout must not end the session: %d", w.Code)
	}
	if w := c.do("POST", "/logout", nil, nil); w.Code != 303 {
		t.Fatalf("POST /logout: %d", w.Code)
	}
	if w := c.do("GET", "/settings", nil, nil); w.Code != 303 {
		t.Fatal("POST /logout must end the session")
	}
}
