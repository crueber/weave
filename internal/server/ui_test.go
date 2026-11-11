package server_test

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// page performs a browser-style request (form body, htmx header optional) with a key.
func (e *env) page(key, method, path string, form url.Values, htmx bool) (int, string) {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, body)
	req.Header.Set("Authorization", "Bearer "+key)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if htmx {
		req.Header.Set("HX-Request", "true")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func TestAgentsAdminPage(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Admin UI")
	human := e.newUser("h@x.io", "boardUser")
	if code, _ := e.page(human, "GET", "/admin/agents", nil, false); code != 404 {
		t.Fatalf("non-admin page: %d", code)
	}
	code, html := e.page(e.admin, "GET", "/admin/agents", nil, false)
	if code != 200 || !strings.Contains(html, "New agent") || !strings.Contains(html, "Admin UI") {
		t.Fatalf("page %d", code)
	}
	code, html = e.page(e.admin, "POST", "/ui/admin/agents", url.Values{"name": {"UI Bot"}, "boardRole": {"editor"}, "boardIds": {bd.id}}, true)
	if code != 200 || !strings.Contains(html, "UI Bot") || !strings.Contains(html, `id="agent-key"`) || !strings.Contains(html, "wk_") || !strings.Contains(html, "Copy") {
		t.Fatalf("create: %d %s", code, html)
	}
	if !strings.Contains(html, "Admin UI") || !strings.Contains(html, "editor") {
		t.Fatalf("board access not shown: %s", html)
	}
	// the key is not shown again
	if _, html = e.page(e.admin, "GET", "/admin/agents", nil, false); strings.Contains(html, `id="agent-key"`) {
		t.Fatal("key shown twice")
	}
	_, list := e.api(e.admin, "GET", "/agents", nil)
	agentID := id(list["items"].([]any)[0].(map[string]any))
	// grant/revoke, rotate, delete through the UI
	code, html = e.page(e.admin, "POST", "/ui/admin/agents/"+agentID+"/access", url.Values{"boardId": {bd.id}, "revoke": {"true"}}, true)
	if code != 200 || !strings.Contains(html, "No board access yet") {
		t.Fatalf("revoke: %d", code)
	}
	if _, html = e.page(e.admin, "POST", "/ui/admin/agents/"+agentID+"/key", url.Values{}, true); !strings.Contains(html, `id="agent-key"`) {
		t.Fatal("rotate must reveal the new key")
	}
	if _, html = e.page(e.admin, "POST", "/ui/admin/agents/"+agentID+"/delete", url.Values{}, true); !strings.Contains(html, "No agents yet") {
		t.Fatalf("delete: %s", html)
	}
	// the users page flags agents with the robot badge
	e.must(e.admin, "POST", "/agents", map[string]any{"name": "Badge Bot"})
	if _, html = e.page(e.admin, "GET", "/admin/users", nil, false); !strings.Contains(html, "badge-agent") {
		t.Fatal("agent badge missing on /admin/users")
	}
}

func TestBoardPageShowsListenersClaimsAndAgents(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Visible")
	a := e.must(e.admin, "POST", "/agents", map[string]any{"name": "Robo", "boardIds": []string{bd.id}})
	key := str(a["key"].(map[string]any), "key")
	c := e.card(bd.a, "claimed card")
	e.must(key, "POST", "/cards/"+c+"/claim", map[string]any{"ttlSeconds": 300, "note": "working"})

	s := e.open(key, "lists="+bd.a+"&agent=robo-1", nil)
	s.next(2 * time.Second) // ready
	for i := 0; i < 40; i++ {
		_, out := e.api(e.admin, "GET", "/lists/"+bd.a+"/listeners", nil)
		if len(out["items"].([]any)) == 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	code, html := e.page(e.admin, "GET", "/boards/"+bd.id, nil, false)
	if code != 200 {
		t.Fatalf("board page %d", code)
	}
	for _, want := range []string{`class="boards-listeners"`, "robo-1", "Claimed by Robo until", `boards-claim`, "boards-avatar avatar is-agent", `id="i-robot"`} {
		if !strings.Contains(html, want) {
			t.Errorf("board page lacks %q", want)
		}
	}
	// card sheet claim row and release
	code, frag := e.page(e.admin, "GET", "/ui/cards/"+c+"/claim", nil, true)
	if code != 200 || !strings.Contains(frag, "Claimed by") || !strings.Contains(frag, "Robo") || !strings.Contains(frag, "working") || !strings.Contains(frag, "/release") {
		t.Fatalf("claim fragment: %d %s", code, frag)
	}
	code, frag = e.page(e.admin, "POST", "/ui/cards/"+c+"/release", url.Values{}, true)
	if code != 200 || strings.Contains(frag, "Claimed by") {
		t.Fatalf("release: %d %s", code, frag)
	}
	_, html = e.page(e.admin, "GET", "/boards/"+bd.id, nil, false)
	if strings.Contains(html, "Claimed by Robo") {
		t.Error("tile still shows the released claim")
	}
	// the board settings member table flags the agent
	if _, html = e.page(e.admin, "GET", "/boards/"+bd.id+"/settings", nil, false); !strings.Contains(html, "AI agent") {
		t.Error("board settings does not flag the agent member")
	}
	// listener indicator goes away when the stream closes
	s.cancel()
	for i := 0; i < 60; i++ {
		_, html = e.page(e.admin, "GET", "/boards/"+bd.id, nil, false)
		if !strings.Contains(html, `class="boards-listeners"`) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("listener indicator still shown after disconnect")
}
