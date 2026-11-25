package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/weave/internal/domain/auth"
)

func (e *env) status(method, path string, hdr map[string]string) (int, http.Header) {
	e.t.Helper()
	code, h, _ := e.rawReqH(method, path, hdr, nil)
	return code, h
}

func openStream(t *testing.T, e *env, key, query string) (int, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", e.ts.URL+"/events"+query, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		b := make([]byte, 400)
		n, _ := res.Body.Read(b)
		res.Body.Close()
		cancel()
		if res.StatusCode == 429 && !strings.Contains(string(b[:n]), "WEAVE_MAX_STREAMS_PER_USER") {
			t.Errorf("429 body should explain the limit: %s", b[:n])
		}
		return res.StatusCode, func() {}
	}
	return 200, func() { cancel(); res.Body.Close() }
}

func TestStreamCapPerUser(t *testing.T) {
	t.Setenv("WEAVE_MAX_STREAMS_PER_USER", "2")
	e := newEnv(t)
	other, err := auth.MintKey(context.Background(), e.d, "other@x.io", "admin")
	if err != nil {
		t.Fatal(err)
	}
	c1, close1 := openStream(t, e, e.admin, "?rooms=")
	c2, close2 := openStream(t, e, e.admin, "?heartbeat=5") // envelope mode counts too
	defer close2()
	if c1 != 200 || c2 != 200 {
		t.Fatalf("first two streams: %d %d", c1, c2)
	}
	if c, _ := openStream(t, e, e.admin, "?rooms="); c != 429 {
		t.Fatalf("third legacy stream: %d, want 429", c)
	}
	if c, _ := openStream(t, e, e.admin, "?heartbeat=5"); c != 429 {
		t.Fatalf("third envelope stream: %d, want 429", c)
	}
	// another user is unaffected
	if c, cl := openStream(t, e, other.Key, "?rooms="); c != 200 {
		t.Fatalf("other user: %d", c)
	} else {
		cl()
	}
	// the slot is released when a stream disconnects
	close1()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, cl := openStream(t, e, e.admin, "?rooms=")
		if c == 200 {
			cl()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream slot not released after disconnect")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDefaultStreamCapIsGenerous(t *testing.T) {
	e := newEnv(t)
	var closers []func()
	defer func() {
		for _, c := range closers {
			c()
		}
	}()
	for i := 0; i < 20; i++ {
		c, cl := openStream(t, e, e.admin, "?rooms=")
		if c != 200 {
			t.Fatalf("stream %d refused (%d) under the default cap of 20", i+1, c)
		}
		closers = append(closers, cl)
	}
	if c, _ := openStream(t, e, e.admin, "?rooms="); c != 429 {
		t.Fatalf("21st stream: %d, want 429", c)
	}
}

func TestDefaultRateLimitAllowsUIBursts(t *testing.T) {
	e := newEnv(t)
	bearer := map[string]string{"Authorization": "Bearer " + e.admin}
	// a bulk paste of 100 cards plus a page's worth of polling: well inside the burst of 200
	for i := 0; i < 180; i++ {
		if code, _ := e.status("GET", "/api/v1/auth/me", bearer); code != 200 {
			t.Fatalf("request %d: %d (burst must not trip default limits)", i+1, code)
		}
	}
	// static files and health checks are never throttled
	for i := 0; i < 600; i++ {
		if code, _ := e.status("GET", "/healthz", nil); code != 200 {
			t.Fatalf("healthz %d: %d", i, code)
		}
		if code, _ := e.status("GET", "/static/weave.js", nil); code != 200 {
			t.Fatalf("static %d: %d", i, code)
		}
	}
}

func TestRateLimitThrottlesRunawayClients(t *testing.T) {
	e := newEnv(t)
	other, _ := auth.MintKey(context.Background(), e.d, "other@x.io", "admin")
	bearer := map[string]string{"Authorization": "Bearer " + e.admin}
	var limited int
	var h http.Header
	for i := 0; i < 600; i++ {
		code, hh := e.status("GET", "/api/v1/auth/me", bearer)
		if code == 429 {
			limited++
			h = hh
		}
	}
	if limited == 0 {
		t.Fatal("600 immediate requests were never throttled")
	}
	if h.Get("Retry-After") == "" {
		t.Error("429 without Retry-After")
	}
	var code int
	var body string
	for i := 0; i < 100 && code != 429; i++ { // refill is 20/s: keep hammering until the bucket is empty again
		code, _, body = e.rawReqH("GET", "/api/v1/auth/me", bearer, nil)
	}
	if code != 429 || !strings.Contains(body, "rate_limited") {
		t.Errorf("throttled response: %d %s", code, body)
	}
	// htmx callers get a toast fragment aimed at #toasts
	var hh http.Header
	for i := 0; i < 100 && hh.Get("HX-Retarget") == ""; i++ {
		_, hh, body = e.rawReqH("GET", "/ui/notifications/badge", map[string]string{"Authorization": "Bearer " + e.admin, "HX-Request": "true"}, nil)
	}
	if hh.Get("HX-Retarget") != "#toasts" || !strings.Contains(body, "toast") {
		t.Errorf("htmx 429: %v %s", hh, body)
	}
	// another principal has its own bucket
	if code, _ := e.status("GET", "/api/v1/auth/me", map[string]string{"Authorization": "Bearer " + other.Key}); code != 200 {
		t.Errorf("other principal throttled: %d", code)
	}
	// tokens come back with time
	time.Sleep(1200 * time.Millisecond)
	if code, _ := e.status("GET", "/api/v1/auth/me", bearer); code != 200 {
		t.Errorf("no refill after waiting: %d", code)
	}
}

func TestRateLimitOff(t *testing.T) {
	t.Setenv("WEAVE_RATE_LIMIT", "0")
	e := newEnv(t)
	for i := 0; i < 500; i++ {
		if code, _ := e.status("GET", "/api/v1/auth/me", map[string]string{"Authorization": "Bearer " + e.admin}); code != 200 {
			t.Fatalf("request %d: %d with WEAVE_RATE_LIMIT=0", i, code)
		}
	}
}

func TestClassifyArchiveBulkHeavy(t *testing.T) {
	t.Setenv("WEAVE_RATE_LIMIT", "0")
	t.Setenv("WEAVE_RATE_LIMIT_HEAVY", "0.000001") // keep the burst deterministic: effectively no refill
	for _, tt := range []struct {
		name string
		path string
	}{
		{"api restore", "/api/v1/boards/nope/archive/restore"},
		{"api delete", "/api/v1/boards/nope/archive/delete"},
		{"ui restore", "/ui/boards/nope/archive/restore"},
		{"ui delete", "/ui/boards/nope/archive/delete"},
		{"unprefixed restore", "/boards/nope/archive/restore"},
		{"unprefixed delete", "/boards/nope/archive/delete"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			bearer := map[string]string{"Authorization": "Bearer " + e.admin}
			for i := 1; i <= 16; i++ {
				code, _ := e.status(http.MethodPost, tt.path, bearer)
				if i < 16 && code == http.StatusTooManyRequests {
					t.Fatalf("request %d hit the heavy burst of 15 early", i)
				}
				if i == 16 && code != http.StatusTooManyRequests {
					t.Fatalf("request %d: %d, want the heavy bucket to throttle at 16", i, code)
				}
			}
		})
	}
}

func TestClassifyArchiveBulkNonHeavy(t *testing.T) {
	t.Setenv("WEAVE_RATE_LIMIT", "0")
	t.Setenv("WEAVE_RATE_LIMIT_HEAVY", "0.000001")
	e := newEnv(t)
	bearer := map[string]string{"Authorization": "Bearer " + e.admin}
	// Drain the heavy bucket once; every route below must still reach its handler.
	for i := 0; i < 15; i++ {
		if code, _ := e.status(http.MethodPost, "/api/v1/boards/nope/archive/restore", bearer); code == http.StatusTooManyRequests {
			t.Fatalf("heavy bucket drained early at request %d", i+1)
		}
	}
	if code, _ := e.status(http.MethodPost, "/api/v1/boards/nope/archive/restore", bearer); code != http.StatusTooManyRequests {
		t.Fatalf("heavy bucket not exhausted: %d", code)
	}
	for _, tt := range []struct {
		name   string
		method string
		path   string
	}{
		{"api archive list", http.MethodGet, "/api/v1/boards/nope/archive"},
		{"ui archive list", http.MethodGet, "/ui/boards/nope/archive"},
		{"unprefixed archive list", http.MethodGet, "/boards/nope/archive"},
		{"ordinary api read", http.MethodGet, "/api/v1/auth/me"},
		{"ordinary ui read", http.MethodGet, "/ui/notifications/badge"},
		{"single card archive", http.MethodPost, "/api/v1/cards/nope/archive"},
		{"single card restore", http.MethodPost, "/api/v1/cards/nope/restore"},
		{"archive list wrong method", http.MethodPost, "/api/v1/boards/nope/archive"},
		{"restore wrong method", http.MethodGet, "/api/v1/boards/nope/archive/restore"},
		{"delete wrong method", http.MethodGet, "/ui/boards/nope/archive/delete"},
		{"restore extra segment", http.MethodPost, "/api/v1/boards/nope/archive/restore/extra"},
		{"delete extra segment", http.MethodPost, "/ui/boards/nope/archive/delete/extra"},
		{"restore suffix", http.MethodPost, "/api/v1/boards/nope/archive/restore-extra"},
		{"delete suffix", http.MethodPost, "/ui/boards/nope/archive/delete-extra"},
		{"extra board segment", http.MethodPost, "/api/v1/boards/nope/extra/archive/restore"},
		{"missing board id", http.MethodPost, "/api/v1/boards//archive/delete"},
		{"unrelated prefix", http.MethodPost, "/api/v1/other/boards/nope/archive/restore"},
		{"unrelated action", http.MethodPost, "/api/v1/boards/nope/archive/purge"},
		{"trailing slash", http.MethodPost, "/ui/boards/nope/archive/restore/"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if code, _ := e.status(tt.method, tt.path, bearer); code == http.StatusTooManyRequests {
				t.Errorf("%s %s used the exhausted heavy bucket", tt.method, tt.path)
			}
		})
	}
}

func TestStrictBuckets(t *testing.T) {
	e := newEnv(t)
	bearer := map[string]string{"Authorization": "Bearer " + e.admin}
	firstLimited := func(method, path string, hdr map[string]string, max int) int {
		for i := 1; i <= max; i++ {
			if code, _ := e.status(method, path, hdr); code == 429 {
				return i
			}
		}
		return 0
	}
	// sign-in: 20 per minute per IP, anonymous
	if n := firstLimited("GET", "/auth/oidc/login", nil, 30); n != 21 {
		t.Errorf("login throttled at request %d, want 21", n)
	}
	// search: burst of 30
	if n := firstLimited("GET", "/api/v1/search?q=abc", bearer, 60); n < 31 || n > 36 {
		t.Errorf("search throttled at request %d, want ~31", n)
	}
	// exports: burst of 15 (unknown board still passes through the bucket first)
	if n := firstLimited("GET", "/api/v1/boards/nope/export", bearer, 40); n < 16 || n > 20 {
		t.Errorf("export throttled at request %d, want ~16", n)
	}
	// ordinary requests of the same principal still work (own bucket is far from empty)
	if code, _ := e.status("GET", "/api/v1/auth/me", bearer); code != 200 {
		t.Errorf("normal request after strict throttling: %d", code)
	}
}

func TestStrictBucketsTunable(t *testing.T) {
	t.Setenv("WEAVE_RATE_LIMIT_LOGIN", "0")
	e := newEnv(t)
	for i := 0; i < 60; i++ {
		if code, _ := e.status("GET", "/auth/oidc/login", nil); code == 429 {
			t.Fatalf("login limited at %d despite WEAVE_RATE_LIMIT_LOGIN=0", i)
		}
	}
}
