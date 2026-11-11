package server_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"testing"
)

func TestAgentLifecycle(t *testing.T) {
	e := newEnv(t)
	b1, b2 := e.newBoard("One"), e.newBoard("Two")
	human := e.newUser("human@x.io", "boardUser")

	// only admins manage agents
	if code, _ := e.api(human, "POST", "/agents", map[string]any{"name": "nope"}); code != 403 {
		t.Fatalf("non-admin create: %d", code)
	}
	if code, _ := e.api(human, "GET", "/agents", nil); code != 403 {
		t.Fatalf("non-admin list: %d", code)
	}

	a := e.must(e.admin, "POST", "/agents", map[string]any{"name": "Reviewer", "boardIds": []string{b1.id}, "role": "worker"})
	if a["isAgent"] != true || a["role"] != "worker" {
		t.Fatalf("agent %v", a)
	}
	key := str(a["key"].(map[string]any), "key")
	if len(key) < 20 || key[:3] != "wk_" {
		t.Fatalf("key %q", key)
	}
	agentID := id(a)

	// the key is shown once
	_, list := e.api(e.admin, "GET", "/agents", nil)
	items := list["items"].([]any)
	if len(items) != 1 || items[0].(map[string]any)["key"] != nil {
		t.Fatalf("agent.list must not expose keys: %v", items)
	}

	// whoami: user, role, isAgent and boards with their role
	_, w := e.api(key, "GET", "/auth/whoami", nil)
	me := w["item"].(map[string]any)
	if me["isAgent"] != true || me["role"] != "worker" || me["user"].(map[string]any)["name"] != "Reviewer" {
		t.Fatalf("whoami %v", me)
	}
	boards := me["boards"].([]any)
	if len(boards) != 1 || boards[0].(map[string]any)["id"] != b1.id || boards[0].(map[string]any)["role"] != "editor" {
		t.Fatalf("whoami boards %v", boards)
	}

	// board access: grant viewer on b2 (-> commenter), then revoke
	acc := e.must(e.admin, "POST", "/agents/"+agentID+"/access", map[string]any{"boardId": b2.id, "role": "commenter"})
	if bs := acc["boards"].([]any); len(bs) != 2 {
		t.Fatalf("after grant %v", bs)
	}
	_, w = e.api(key, "GET", "/auth/whoami", nil)
	for _, b := range w["item"].(map[string]any)["boards"].([]any) {
		if bb := b.(map[string]any); bb["id"] == b2.id && bb["role"] != "commenter" {
			t.Fatalf("role on b2 = %v", bb["role"])
		}
	}
	if code, _ := e.api(key, "POST", "/lists/"+b2.a+"/cards", map[string]any{"name": "x"}); code != 403 {
		t.Fatalf("commenter must not create cards: %d", code)
	}
	e.must(e.admin, "POST", "/agents/"+agentID+"/access", map[string]any{"boardId": b2.id, "role": "viewer"})
	e.must(e.admin, "POST", "/agents/"+agentID+"/access", map[string]any{"boardId": b2.id, "revoke": true})
	if code, _ := e.api(key, "GET", "/boards/"+b2.id+"/tree", nil); code != 404 {
		t.Fatalf("revoked agent sees board: %d", code)
	}
	if code, _ := e.api(e.admin, "POST", "/agents/"+agentID+"/access", map[string]any{"boardId": "nope"}); code != 404 {
		t.Fatalf("grant on unknown board: %d", code)
	}

	// agents appear in user.list with isAgent
	_, ul := e.api(e.admin, "GET", "/users", nil)
	found := false
	for _, u := range ul["item"].(map[string]any)["items"].([]any) {
		if um := u.(map[string]any); um["id"] == agentID {
			found = um["isAgent"] == true
		}
	}
	if !found {
		t.Fatal("agent missing from user.list or not flagged isAgent")
	}

	// no UI login for agents: a session cookie cannot be minted for them and the key is not a cookie
	if _, err := e.d.Exec(`INSERT INTO sessions(id,user_id,token_hash,created_at,expires_at) VALUES('s1',?,?,?,?)`, agentID, sha("tok"), "2026-01-01T00:00:00Z", "2099-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/api/v1/auth/me", nil)
	req.AddCookie(&http.Cookie{Name: "weave_session", Value: "tok"})
	if res, _ := http.DefaultClient.Do(req); res.StatusCode != 401 {
		t.Fatalf("session of an agent must not authenticate: %d", res.StatusCode)
	}

	// rotate: old key dies, new key works
	r := e.must(e.admin, "POST", "/agents/"+agentID+"/key", nil)
	key2 := str(r["key"].(map[string]any), "key")
	if key2 == key {
		t.Fatal("rotate returned the same key")
	}
	if code, _ := e.api(key, "GET", "/auth/whoami", nil); code != 401 {
		t.Fatalf("old key still valid: %d", code)
	}
	if code, _ := e.api(key2, "GET", "/auth/whoami", nil); code != 200 {
		t.Fatalf("new key rejected: %d", code)
	}

	// deactivate, rename, delete (claims released)
	e.must(e.admin, "PATCH", "/agents/"+agentID, map[string]any{"name": "Reviewer 2"})
	c := e.card(b1.a, "held")
	e.must(key2, "POST", "/cards/"+c+"/claim", map[string]any{})
	s := e.open(e.admin, "boards="+b1.id, nil)
	e.must(e.admin, "PATCH", "/agents/"+agentID, map[string]any{"isDeactivated": true})
	if code, _ := e.api(key2, "GET", "/auth/whoami", nil); code != 401 {
		t.Fatalf("deactivated agent authenticates: %d", code)
	}
	e.must(e.admin, "DELETE", "/agents/"+agentID, nil)
	ev := s.events(1)[0]
	if str(ev, "type") != "card.released" || ev["data"].(map[string]any)["reason"] != "agent_deleted" {
		t.Fatalf("delete event %v", ev)
	}
	if code, _ := e.api(e.admin, "DELETE", "/agents/"+agentID, nil); code != 404 {
		t.Fatalf("second delete: %d", code)
	}
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
