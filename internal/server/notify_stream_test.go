package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/server"
)

// nextNamed returns the next frame called name, skipping comments and other events.
func (s *stream) nextNamed(name string, d time.Duration) (frame, bool) {
	deadline := time.Now().Add(d)
	for {
		f, ok := s.next(time.Until(deadline))
		if !ok {
			return frame{}, false
		}
		if f.Event == name {
			return f, true
		}
	}
}

// quietOf fails if a frame called name arrives within d.
func (s *stream) quietOf(name string, d time.Duration) {
	s.t.Helper()
	if f, ok := s.nextNamed(name, d); ok {
		s.t.Fatalf("unexpected %s frame: %s", name, f.Data)
	}
}

// The global page script opens `GET /events` with no rooms (or only a board room): the implicit
// user room must deliver the caller's own notifications, and nobody else's, even when another
// user asks for that room by name.
func TestNotificationStreamIsPerUser(t *testing.T) {
	e := newEnv(t)
	keyA, keyB := e.newUser("ann@x.io", "boardUser"), e.newUser("bea@x.io", "boardUser")
	a, b := e.userID(keyA), e.userID(keyB)
	bd := e.newBoard("Live bell")
	for _, u := range []string{a, b} {
		e.must(e.admin, "POST", "/boards/"+bd.id+"/members", map[string]any{"userId": u})
	}
	c := e.card(bd.a, "Ring the bell")

	sa := e.open(keyA, "", nil)
	sb := e.open(keyB, "", nil)
	sbSnoop := e.open(keyB, "rooms=user:"+a, nil) // not allowed: silently dropped by authz.RoomAllowed
	sbBoard := e.open(keyB, "rooms=board:"+bd.id, nil)
	for _, s := range []*stream{sa, sb, sbSnoop, sbBoard} {
		if s.status != 200 {
			t.Fatalf("stream status %d", s.status)
		}
		if f, ok := s.next(3 * time.Second); !ok || f.Event != "comment" { // ": ok" — subscribed
			t.Fatalf("stream not ready: %+v", f)
		}
	}

	e.must(e.admin, "POST", "/cards/"+c+"/members", map[string]any{"userId": a}) // addMemberToCard for ann only

	f, ok := sa.nextNamed("notification", 5*time.Second)
	if !ok {
		t.Fatal("ann's stream did not deliver her notification")
	}
	var ev struct {
		Name string `json:"name"`
		Room string `json:"room"`
		Data struct {
			Action string         `json:"action"`
			Item   map[string]any `json:"item"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(f.Data), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Name != "notification" || ev.Room != "user:"+a || ev.Data.Action != "create" {
		t.Fatalf("frame = %+v", ev)
	}
	it := ev.Data.Item
	if it["userId"] != a || it["type"] != "addMemberToCard" || it["cardId"] != c || it["isRead"] != false {
		t.Fatalf("item = %v", it)
	}
	// the payload is ann's own notification row and nothing more: no credentials, e-mail or service URLs
	for k := range it {
		switch k {
		case "id", "userId", "creatorUserId", "creatorName", "boardId", "boardName", "cardId", "cardName", "commentId",
			"actionId", "type", "data", "isRead", "createdAt", "summary", "reason":
		default:
			t.Errorf("unexpected field %q in notification payload", k)
		}
	}
	if strings.Contains(f.Data, "@x.io") || strings.Contains(f.Data, "wk_") {
		t.Fatalf("payload leaks an e-mail or key: %s", f.Data)
	}

	sb.quietOf("notification", 500*time.Millisecond)
	sbSnoop.quietOf("notification", 200*time.Millisecond)
	sbBoard.quietOf("notification", 200*time.Millisecond)

	// marking it read reaches ann's other tabs as a non-create notification event (the bell refetches the count)
	sa2 := e.open(keyA, "rooms=board:"+bd.id, nil)
	sa2.next(3 * time.Second)
	e.must(keyA, "POST", "/notifications/"+str(it, "id")+"/read", nil)
	f, ok = sa2.nextNamed("notification", 5*time.Second)
	if !ok || !strings.Contains(f.Data, `"action":"update"`) {
		t.Fatalf("read event = %+v %v", f, ok)
	}
	sb.quietOf("notification", 200*time.Millisecond)
}

// A plain `rooms=` stream must not outlive its credentials: idle auto-logout counts only real
// interactions, so an open tab's stream (background traffic) ends once the session is idle too long.
func TestLegacyStreamEndsWithItsSession(t *testing.T) {
	defer server.SetLegacyRecheck(150 * time.Millisecond)()
	e := newEnv(t)
	keyA := e.newUser("ann@x.io", "boardUser")
	a := e.userID(keyA)

	tok := "sess-token-for-ann"
	sum := sha256.Sum256([]byte(tok))
	now := time.Now().UTC()
	if _, err := e.d.Exec(`UPDATE users SET auto_logout_minutes=5 WHERE id=?`, a); err != nil {
		t.Fatal(err)
	}
	seen := now.Format(time.RFC3339)
	if _, err := e.d.Exec(`INSERT INTO sessions(id,user_id,token_hash,created_at,expires_at,last_used_at) VALUES(?,?,?,?,?,?)`,
		db.NewID(), a, hex.EncodeToString(sum[:]), seen, now.Add(24*time.Hour).Format(time.RFC3339), seen); err != nil {
		t.Fatal(err)
	}
	s := e.open("", "", map[string]string{"Cookie": "weave_session=" + tok})
	if s.status != 200 {
		t.Fatalf("cookie stream status %d", s.status)
	}
	// alive across several re-checks, and the re-checks never count as activity
	time.Sleep(600 * time.Millisecond)
	var last string
	_ = e.d.QueryRow(`SELECT last_used_at FROM sessions WHERE user_id=?`, a).Scan(&last)
	if last != seen {
		t.Fatalf("stream refreshed last_used_at: %s -> %s", seen, last)
	}
	// nor do the live bell's badge refetches (polling while the tab is parked must not defeat auto-logout)
	req, _ := http.NewRequest("GET", e.ts.URL+"/ui/notifications/badge", nil)
	req.Header.Set("Cookie", "weave_session="+tok)
	req.Header.Set("HX-Request", "true")
	seen = now.Add(-2 * time.Minute).Format(time.RFC3339)
	if _, err := e.d.Exec(`UPDATE sessions SET last_used_at=? WHERE user_id=?`, seen, a); err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	_ = e.d.QueryRow(`SELECT last_used_at FROM sessions WHERE user_id=?`, a).Scan(&last)
	if res.StatusCode != 200 || last != seen {
		t.Fatalf("badge refetch: %d, last_used_at %s -> %s", res.StatusCode, seen, last)
	}
	drain := func(d time.Duration) bool { // true when the server closed the stream
		deadline := time.Now().Add(d)
		for time.Now().Before(deadline) {
			select {
			case _, ok := <-s.frames:
				if !ok {
					return true
				}
			case <-time.After(50 * time.Millisecond):
			}
		}
		return false
	}
	if drain(300 * time.Millisecond) {
		t.Fatal("stream ended while the session was valid")
	}
	// the user walks away: the session is now idle past auto-logout
	if _, err := e.d.Exec(`UPDATE sessions SET last_used_at=? WHERE user_id=?`, now.Add(-10*time.Minute).Format(time.RFC3339), a); err != nil {
		t.Fatal(err)
	}
	if !drain(3 * time.Second) {
		t.Fatal("stream outlived an idle-expired session")
	}

	// deactivating a key holder ends a Bearer stream the same way
	k := e.open(keyA, "", nil)
	k.next(3 * time.Second)
	if _, err := e.d.Exec(`UPDATE users SET is_deactivated=1 WHERE id=?`, a); err != nil {
		t.Fatal(err)
	}
	s = k
	if !drain(3 * time.Second) {
		t.Fatal("stream outlived a deactivated user")
	}
}
