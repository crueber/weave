package server_test

import (
	"net/http"
	"testing"
	"time"
)

// waitClosed waits for the server to end the stream.
func (s *stream) waitClosed(d time.Duration) bool {
	deadline := time.After(d)
	for {
		select {
		case _, ok := <-s.frames:
			if !ok {
				return true
			}
		case <-deadline:
			return false
		}
	}
}

func TestOpenStreamEndsWhenKeyRotatedOrAccessRevoked(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Revoke")
	for _, how := range []string{"rotate", "deactivate", "revoke", "delete"} {
		a := e.must(e.admin, "POST", "/agents", map[string]any{"name": "Leaky " + how, "boardIds": []string{bd.id}})
		key, aid := str(a["key"].(map[string]any), "key"), id(a)
		s := e.open(key, "lists="+bd.a+"&heartbeat=1", nil)
		for i := 0; i < 40; i++ { // registered as a listener
			if _, o := e.api(e.admin, "GET", "/lists/"+bd.a+"/listeners", nil); len(o["items"].([]any)) >= 1 {
				break
			}
			time.Sleep(25 * time.Millisecond)
		}
		switch how {
		case "rotate":
			e.must(e.admin, "POST", "/agents/"+aid+"/key", nil)
		case "deactivate":
			e.must(e.admin, "PATCH", "/agents/"+aid, map[string]any{"isDeactivated": true})
		case "revoke":
			e.must(e.admin, "POST", "/agents/"+aid+"/access", map[string]any{"boardId": bd.id, "revoke": true})
		case "delete":
			e.must(e.admin, "DELETE", "/agents/"+aid, nil)
		}
		if !s.waitClosed(8 * time.Second) {
			t.Fatalf("%s: stream still open after credentials/access were revoked", how)
		}
		for i := 0; ; i++ {
			_, o := e.api(e.admin, "GET", "/lists/"+bd.a+"/listeners", nil)
			if len(o["items"].([]any)) == 0 {
				break
			}
			if i > 40 {
				t.Fatalf("%s: listener still registered after the stream ended", how)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func TestAgentKeyInSessionCookieIsRejected(t *testing.T) {
	e := newEnv(t)
	a := e.must(e.admin, "POST", "/agents", map[string]any{"name": "Cookie Bot"})
	key := str(a["key"].(map[string]any), "key")
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: "weave_session", Value: key})
	res, err := http.DefaultClient.Do(req)
	if err != nil || res.StatusCode != 401 {
		t.Fatalf("agent key as session cookie must be rejected: %v %v", err, res.StatusCode)
	}
	// headers still work
	if code, _ := e.api(key, "GET", "/auth/me", nil); code != 200 {
		t.Fatalf("bearer: %d", code)
	}
	req, _ = http.NewRequest("GET", e.ts.URL+"/api/v1/auth/me", nil)
	req.Header.Set("X-API-Key", key)
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 200 {
		t.Fatalf("X-API-Key: %d", res.StatusCode)
	}
}

func TestCrossBoardMoveDoesNotLeakDestinationCard(t *testing.T) {
	e := newEnv(t)
	src, dst := e.newBoard("Source"), e.newBoard("Destination")
	viewer := e.newUser("srcviewer@x.io", "boardUser")
	_, me := e.api(viewer, "GET", "/auth/me", nil)
	e.must(e.admin, "POST", "/boards/"+src.id+"/members", map[string]any{"userId": id(me["item"].(map[string]any)), "role": "viewer"})
	c := e.card(src.a, "traveller")
	s := e.open(viewer, "lists="+src.a, nil)
	e.must(e.admin, "POST", "/cards/"+c+"/move", map[string]any{"listId": dst.a})
	ev := s.events(1)[0]
	if str(ev, "type") != "card.deleted" || ev["data"].(map[string]any)["movedToBoardId"] != dst.id || str(ev, "listId") != src.a {
		t.Fatalf("source-side event %v", ev)
	}
	if ev["card"] != nil {
		t.Fatalf("source viewer received the destination board's card: %v", ev["card"])
	}
	s.expectQuiet() // the card.moved belongs to the destination board
}
