package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func hstsHeader(t *testing.T, baseURL string) string {
	t.Helper()
	h := withHSTS(baseURL, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/", nil))
	return w.Header().Get("Strict-Transport-Security")
}

func TestHSTS(t *testing.T) {
	t.Setenv("WEAVE_HSTS", "")
	t.Setenv("WEAVE_HSTS_MAX_AGE", "")
	if got := hstsHeader(t, "https://weave.example.com"); got != "max-age=15552000" {
		t.Errorf("https default = %q", got)
	}
	if got := hstsHeader(t, "http://localhost:8080"); got != "" {
		t.Errorf("http base URL must not send HSTS, got %q", got)
	}
	if got := hstsHeader(t, ""); got != "" {
		t.Errorf("empty base URL must not send HSTS, got %q", got)
	}
	t.Setenv("WEAVE_HSTS_MAX_AGE", "86400")
	if got := hstsHeader(t, "https://weave.example.com"); got != "max-age=86400" {
		t.Errorf("override = %q", got)
	}
	t.Setenv("WEAVE_HSTS_MAX_AGE", "junk")
	if got := hstsHeader(t, "https://weave.example.com"); got != "max-age=15552000" {
		t.Errorf("bad override must fall back, got %q", got)
	}
	t.Setenv("WEAVE_HSTS", "off")
	if got := hstsHeader(t, "https://weave.example.com"); got != "" {
		t.Errorf("WEAVE_HSTS=off = %q", got)
	}
}
