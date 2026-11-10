package server_test

import (
	"regexp"
	"strings"
	"testing"
)

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	bearer := map[string]string{"Authorization": "Bearer " + e.admin}
	_, h, _ := e.rawReqH("GET", "/healthz", nil, nil)
	for k, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
		"Permissions-Policy":     "camera=(), microphone=(), geolocation=(), payment=()",
	} {
		if got := h.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self';", "style-src 'self' 'unsafe-inline'", "img-src 'self' data:", "connect-src 'self'",
		"form-action 'self'", "base-uri 'none'", "object-src 'none'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe-eval") || regexp.MustCompile(`script-src[^;]*unsafe-inline`).MatchString(csp) {
		t.Errorf("CSP must not allow inline scripts or eval: %q", csp)
	}
	if h.Get("Content-Security-Policy-Report-Only") != "" {
		t.Error("enforce mode sends no report-only header")
	}
	if h.Get("Strict-Transport-Security") != "" {
		t.Error("HSTS is not enabled")
	}
	// API responses carry the headers too
	_, h, _ = e.rawReqH("GET", "/api/v1/auth/me", bearer, nil)
	if h.Get("X-Frame-Options") != "DENY" {
		t.Error("headers missing on /api")
	}
	// authenticated HTML pages: Cache-Control private (not no-store)
	_, h, body := e.rawReqH("GET", "/settings", map[string]string{"Authorization": "Bearer " + e.admin}, nil)
	if ct := h.Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("settings: %q %.80s", ct, body)
	}
	if cc := h.Get("Cache-Control"); cc != "private" {
		t.Errorf("HTML Cache-Control = %q, want private", cc)
	}
}

func TestPublicPagesReferrerPolicy(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/share/nope", "/public/nope"} {
		_, h, _ := e.rawReqH("GET", p, nil, nil)
		if got := h.Get("Referrer-Policy"); got != "no-referrer" {
			t.Errorf("%s Referrer-Policy = %q", p, got)
		}
		if h.Get("Cache-Control") == "private" {
			t.Errorf("%s: public pages are not marked private", p)
		}
	}
}

func TestLoginRedirectEscapesNext(t *testing.T) {
	e := newEnv(t)
	code, h, _ := e.rawReqH("GET", "/settings?a=b&next=//evil.example", nil, nil)
	if code != 303 {
		t.Fatalf("anon: %d", code)
	}
	if loc := h.Get("Location"); loc != "/login?next=%2Fsettings%3Fa%3Db%26next%3D%2F%2Fevil.example" {
		t.Fatalf("Location not escaped: %s", loc)
	}
}

func TestCSRFProtection(t *testing.T) {
	e := newEnv(t)
	json := "application/json"
	post := func(hdr map[string]string) int {
		h := map[string]string{"Authorization": "Bearer " + e.admin, "Content-Type": json}
		for k, v := range hdr {
			h[k] = v
		}
		code, _ := e.rawReq("POST", "/api/v1/projects", h, strings.NewReader(`{"name":"p"}`))
		return code
	}
	if c := post(nil); c != 200 { // CLI / agents: no Origin, no Sec-Fetch-*
		t.Fatalf("bearer client without browser headers: %d", c)
	}
	if c := post(map[string]string{"X-API-Key": e.admin}); c != 200 {
		t.Fatalf("X-API-Key client: %d", c)
	}
	if c := post(map[string]string{"Sec-Fetch-Site": "cross-site"}); c != 403 {
		t.Fatalf("cross-site browser POST: %d, want 403", c)
	}
	if c := post(map[string]string{"Origin": "https://evil.example"}); c != 403 {
		t.Fatalf("foreign Origin POST: %d, want 403", c)
	}
	if c := post(map[string]string{"Sec-Fetch-Site": "same-origin"}); c != 200 {
		t.Fatalf("same-origin POST: %d", c)
	}
	// the configured WEAVE_BASE_URL origin is trusted (reverse proxies that rewrite Host)
	if c := post(map[string]string{"Origin": "http://weave.test"}); c != 200 {
		t.Fatalf("Origin == BaseURL: %d", c)
	}
	// safe methods are never blocked
	if code, _ := e.rawReq("GET", "/api/v1/auth/me", map[string]string{"Authorization": "Bearer " + e.admin, "Sec-Fetch-Site": "cross-site"}, nil); code != 200 {
		t.Fatalf("cross-site GET: %d", code)
	}
}

func TestCSPModes(t *testing.T) {
	t.Setenv("WEAVE_CSP", "report-only")
	e := newEnv(t)
	_, h, _ := e.rawReqH("GET", "/healthz", nil, nil)
	if h.Get("Content-Security-Policy") != "frame-ancestors 'none'" || !strings.Contains(h.Get("Content-Security-Policy-Report-Only"), "script-src 'self'") {
		t.Errorf("report-only: %q / %q", h.Get("Content-Security-Policy"), h.Get("Content-Security-Policy-Report-Only"))
	}
	t.Setenv("WEAVE_CSP", "off")
	e = newEnv(t)
	_, h, _ = e.rawReqH("GET", "/healthz", nil, nil)
	if h.Get("Content-Security-Policy") != "frame-ancestors 'none'" || h.Get("Content-Security-Policy-Report-Only") != "" {
		t.Errorf("off: %q", h.Get("Content-Security-Policy"))
	}
}

// Share pages without their own policy get the global one.
func TestSharePageKeepsGlobalPolicy(t *testing.T) {
	e := newEnv(t)
	_, h, _ := e.rawReqH("GET", "/share/nope", nil, nil)
	if got := h.Get("Content-Security-Policy"); !strings.Contains(got, "script-src 'self'") {
		t.Errorf("share page lost the global policy: %q", got)
	}
}
