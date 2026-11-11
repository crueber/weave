package auth

import (
	"net/url"
	"testing"
	"time"
)

func TestBackgroundRequestsDoNotRefreshSessions(t *testing.T) {
	c, idp := newClient(t)
	c.signIn(idp, userClaims("idle@x.io", "weave-users"))
	if _, err := c.e.DB.Exec(`UPDATE users SET auto_logout_minutes=5 WHERE email='idle@x.io'`); err != nil {
		t.Fatal(err)
	}
	setAge := func(d time.Duration) string {
		ts := time.Now().UTC().Add(-d).Format(time.RFC3339)
		if _, err := c.e.DB.Exec(`UPDATE sessions SET last_used_at=?`, ts); err != nil {
			t.Fatal(err)
		}
		return ts
	}
	lastUsed := func() string {
		var s string
		_ = c.e.DB.QueryRow(`SELECT last_used_at FROM sessions`).Scan(&s)
		return s
	}
	hx := map[string]string{"HX-Request": "true"}

	// the 60s presence heartbeat, the SSE re-auth path and header-marked polls leave last_used_at alone
	ts := setAge(3 * time.Minute)
	if w := c.do("POST", "/ui/presence", url.Values{"state": {"online"}}, hx); w.Code >= 400 {
		t.Fatalf("presence: %d", w.Code)
	}
	if w := c.do("GET", "/settings", nil, map[string]string{BackgroundHeader: "1"}); w.Code != 200 {
		t.Fatalf("background GET: %d", w.Code)
	}
	if got := lastUsed(); got != ts {
		t.Fatalf("background requests refreshed last_used_at: %s -> %s", ts, got)
	}

	// a real interaction does refresh it
	if w := c.do("GET", "/settings", nil, nil); w.Code != 200 {
		t.Fatalf("settings: %d", w.Code)
	}
	if got := lastUsed(); got == ts {
		t.Fatal("real interaction must refresh last_used_at")
	}

	// heartbeats alone cannot keep an idle session alive past auto_logout_minutes
	setAge(4 * time.Minute)
	for i := 0; i < 3; i++ {
		c.do("POST", "/ui/presence", url.Values{"state": {"online"}}, hx)
	}
	setAge(6 * time.Minute) // time passes; only heartbeats happened
	if w := c.do("GET", "/settings", nil, nil); w.Code != 303 {
		t.Fatalf("idle session must be logged out: %d", w.Code)
	}
}
