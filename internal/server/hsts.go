package server

import (
	"net/http"
	"net/url"
	"os"
	"strconv"
)

// defaultHSTSMaxAge is 180 days. No includeSubDomains / preload: those are hard-to-undo
// commitments an operator should opt into at the reverse proxy, not something an app imposes.
const defaultHSTSMaxAge = 15552000

// hstsValue returns the Strict-Transport-Security value for a deployment, or "" when HSTS must
// not be sent: only when WEAVE_BASE_URL is https (a dev setup on plain http must never pin
// itself), and never when WEAVE_HSTS=off. WEAVE_HSTS_MAX_AGE (seconds) overrides the default.
func hstsValue(baseURL string) string {
	if u, err := url.Parse(baseURL); err != nil || u.Scheme != "https" {
		return ""
	}
	if v := os.Getenv("WEAVE_HSTS"); v == "off" || v == "false" || v == "0" {
		return ""
	}
	age := defaultHSTSMaxAge
	if v := os.Getenv("WEAVE_HSTS_MAX_AGE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			age = n
		}
	}
	return "max-age=" + strconv.Itoa(age)
}

// withHSTS adds Strict-Transport-Security to every response when enabled (see hstsValue).
func withHSTS(baseURL string, next http.Handler) http.Handler {
	v := hstsValue(baseURL)
	if v == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", v)
		next.ServeHTTP(w, r)
	})
}
