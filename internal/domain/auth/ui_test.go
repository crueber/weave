package auth

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
	"github.com/crueber/weave/internal/server"
)

type client struct {
	t       *testing.T
	e       *ops.Env
	h       http.Handler
	cookies []*http.Cookie
}

func (c *client) do(method, path string, form url.Values, hdr map[string]string) *httptest.ResponseRecorder {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, path, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	for _, ck := range c.cookies {
		r.AddCookie(ck)
	}
	w := httptest.NewRecorder()
	c.h.ServeHTTP(w, r)
	for _, ck := range w.Result().Cookies() {
		kept := c.cookies[:0:0]
		for _, old := range c.cookies {
			if old.Name != ck.Name {
				kept = append(kept, old)
			}
		}
		c.cookies = kept
		if ck.MaxAge >= 0 && ck.Value != "" {
			c.cookies = append(c.cookies, ck)
		}
	}
	return w
}

// ---- fake identity provider (discovery, JWKS, token endpoint; RS256 id_tokens) ----

type fakeIdP struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	nonce  string
	claims map[string]any // claims for the next id_token (sub/email/name/groups…)
	// challenge is the PKCE S256 code_challenge from the last authorize request; the token
	// endpoint rejects exchanges whose code_verifier does not match it.
	challenge string
	userinfo  map[string]any // served by /userinfo (nil = 404)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func newFakeIdP(t *testing.T) *fakeIdP {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeIdP{key: key, claims: map[string]any{}}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize", "token_endpoint": f.srv.URL + "/token",
			"jwks_uri": f.srv.URL + "/jwks", "userinfo_endpoint": f.srv.URL + "/userinfo", "id_token_signing_alg_values_supported": []string{"RS256"}, "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{"kty": "RSA", "alg": "RS256", "use": "sig", "kid": "k1",
			"n": b64(key.N.Bytes()), "e": b64(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.userinfo == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(f.userinfo)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = r.ParseForm()
		vsum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
		if f.challenge == "" || b64(vsum[:]) != f.challenge {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"invalid_grant","error_description":"PKCE verification failed"}`))
			return
		}
		claims := map[string]any{"iss": f.srv.URL, "aud": "weave-client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": f.nonce}
		for k, v := range f.claims {
			claims[k] = v
		}
		hb, _ := json.Marshal(map[string]string{"alg": "RS256", "typ": "JWT", "kid": "k1"})
		pb, _ := json.Marshal(claims)
		signing := b64(hb) + "." + b64(pb)
		sum := sha256.Sum256([]byte(signing))
		sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": signing + "." + b64(sig)})
	})
	return f
}

// newClient builds the full HTTP handler wired to a fake IdP (OIDC is the only way in).
func newClient(t *testing.T) (*client, *fakeIdP) {
	idp := newFakeIdP(t)
	e := newEnv(t)
	cfg := config.Config{DataDir: t.TempDir(), BaseURL: "http://weave.test", SecretKey: "test-secret-key-test-secret-key-12", SessionHours: 1}
	e.Svc["config"] = cfg
	for k, v := range map[string]string{"oidc.issuer": idp.srv.URL, "oidc.clientId": "weave-client", "oidc.clientSecret": "s3cret"} {
		if err := SetConfig(e, k, v); err != nil {
			t.Fatal(err)
		}
	}
	s := &server.Server{Cfg: cfg, DB: e.DB, Hub: realtime.New(), Svc: e.Svc}
	h, err := s.Handler()
	if err != nil {
		t.Fatal(err)
	}
	return &client{t: t, e: e, h: h}, idp
}

// signIn drives the real browser flow: /login -> /auth/oidc/login -> IdP -> callback.
func (c *client) signIn(idp *fakeIdP, claims map[string]any) *httptest.ResponseRecorder {
	c.t.Helper()
	idp.mu.Lock()
	idp.claims = claims
	idp.mu.Unlock()
	w := c.do("GET", "/auth/oidc/login", nil, nil)
	if w.Code != 303 {
		c.t.Fatalf("oidc start: %d %s", w.Code, w.Body.String())
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if !strings.HasPrefix(loc.String(), idp.srv.URL+"/authorize") {
		c.t.Fatalf("not redirected to the IdP: %s", loc)
	}
	q := loc.Query()
	idp.mu.Lock()
	idp.nonce = q.Get("nonce")
	idp.challenge = q.Get("code_challenge")
	if q.Get("code_challenge_method") != "S256" || idp.challenge == "" {
		c.t.Fatalf("authorize request lacks PKCE S256: %v", q)
	}
	idp.mu.Unlock()
	if q.Get("client_id") != "weave-client" || !strings.Contains(q.Get("scope"), "groups") || q.Get("redirect_uri") != "http://weave.test/auth/oidc/callback" {
		c.t.Fatalf("authorize params: %v", q)
	}
	return c.do("GET", "/auth/oidc/callback?code=abc&state="+url.QueryEscape(q.Get("state")), nil, nil)
}

func userClaims(email string, groups ...string) map[string]any {
	g := make([]any, len(groups))
	for i, x := range groups {
		g[i] = x
	}
	return map[string]any{"sub": "sub-" + email, "email": email, "email_verified": true, "name": "Test " + email, "groups": g}
}

func TestOIDCBrowserFlow(t *testing.T) {
	c, idp := newClient(t)
	hx := map[string]string{"HX-Request": "true"}

	// anonymous: protected pages go to /login, which goes straight to the IdP
	if w := c.do("GET", "/settings", nil, nil); w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/login") {
		t.Fatalf("anon /settings: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := c.do("GET", "/login", nil, nil); w.Code != 303 || w.Header().Get("Location") != "/auth/oidc/login" {
		t.Fatalf("login should redirect to SSO: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := c.do("GET", "/login?error=nope", nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "Sign in with SSO") || strings.Contains(w.Body.String(), "password") {
		t.Fatalf("login page must offer only SSO: %d %s", w.Code, w.Body.String())
	}
	if w := c.do("POST", "/login", url.Values{"emailOrUsername": {"x"}, "password": {"y"}}, nil); w.Code < 400 {
		t.Fatalf("password login must not exist: %d", w.Code)
	}

	// sign in as an admin via the IdP group
	w := c.signIn(idp, userClaims("root@x.io", "weave-admins"))
	if w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("callback: %d %s %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	var ck *http.Cookie
	for _, x := range c.cookies {
		if x.Name == CookieName {
			ck = x
		}
	}
	if ck == nil || !ck.HttpOnly || ck.SameSite != http.SameSiteLaxMode {
		t.Fatalf("session cookie %+v", ck)
	}
	for _, p := range []string{"/settings", "/admin/users", "/admin/groups", "/admin/sso"} {
		if w := c.do("GET", p, nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "<main") {
			t.Fatalf("%s: %d %s", p, w.Code, w.Body.String())
		}
	}
	body := c.do("GET", "/settings", nil, nil).Body.String()
	if strings.Contains(strings.ToLower(body), "password") || strings.Contains(body, "Two-factor") {
		t.Fatal("settings must not offer local credentials")
	}
	if w := c.do("POST", "/ui/settings/preferences", url.Values{"theme": {"dark"}}, hx); !strings.Contains(w.Body.String(), "Preferences saved") {
		t.Fatalf("prefs: %s", w.Body.String())
	}

	// API key (how the CLI authenticates) works on the REST API
	w = c.do("POST", "/ui/settings/apikey", url.Values{}, hx)
	m := strings.Index(w.Body.String(), "wk_")
	if m < 0 {
		t.Fatalf("api key not shown: %s", w.Body.String())
	}
	key := w.Body.String()[m : m+3+48]
	rec := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	r.Header.Set("X-API-Key", key)
	c.h.ServeHTTP(rec, r)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "root@x.io") || !strings.Contains(rec.Body.String(), `"role":"admin"`) {
		t.Fatalf("api key auth: %d %s", rec.Code, rec.Body.String())
	}

	// logout shows the SSO button without bouncing straight back to the IdP
	if w := c.do("POST", "/logout", nil, nil); w.Code != 303 || w.Header().Get("Location") != "/login?loggedout=1" {
		t.Fatalf("logout: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := c.do("GET", "/login?loggedout=1", nil, nil); w.Code != 200 || !strings.Contains(w.Body.String(), "signed out") {
		t.Fatalf("logged-out page: %d", w.Code)
	}
	if w := c.do("GET", "/settings", nil, nil); w.Code != 303 {
		t.Fatal("session survives logout")
	}
}

func TestOIDCRolesAndAdminPages(t *testing.T) {
	c, idp := newClient(t)
	c.signIn(idp, userClaims("owner@x.io", "weave-project-owners"))
	var role string
	_ = c.e.DB.QueryRow(`SELECT role FROM users WHERE email='owner@x.io'`).Scan(&role)
	if role != "projectOwner" {
		t.Fatalf("role %q", role)
	}
	if w := c.do("GET", "/admin/users", nil, nil); w.Code != 404 {
		t.Fatalf("non-admins must not see admin users: %d", w.Code)
	}
	if w := c.do("POST", "/ui/admin/users/x/delete", nil, nil); w.Code != 404 {
		t.Fatalf("non-admins must not delete users: %d", w.Code)
	}
	if w := c.do("GET", "/admin/groups", nil, nil); w.Code != 200 {
		t.Fatalf("groups page readable: %d", w.Code)
	}
}

func TestOIDCCallbackRejections(t *testing.T) {
	c, idp := newClient(t)
	// state cookie missing / wrong
	if w := c.do("GET", "/auth/oidc/callback?code=abc&state=forged", nil, nil); w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/login?error=") {
		t.Fatalf("forged state: %d %s", w.Code, w.Header().Get("Location"))
	}
	// IdP says the email is unverified
	cl := userClaims("u@x.io", "weave-users")
	cl["email_verified"] = false
	if w := c.signIn(idp, cl); !strings.HasPrefix(w.Header().Get("Location"), "/login?error=") {
		t.Fatalf("unverified email must be rejected: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := c.do("GET", "/settings", nil, nil); w.Code != 303 {
		t.Fatal("rejected login created a session")
	}
}
