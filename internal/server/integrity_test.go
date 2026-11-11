package server_test

import (
	"strings"
	"testing"
)

func TestTextSizeCaps(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Caps")
	rep := func(n int) string { return strings.Repeat("é", n) } // multi-byte: caps count characters
	expect := func(what string, code int, method, path string, body map[string]any) {
		t.Helper()
		if got, out := e.api(e.admin, method, path, body); got != code {
			t.Fatalf("%s: %d %v, want %d", what, got, out, code)
		}
	}
	expect("card name 1000", 200, "POST", "/lists/"+bd.a+"/cards", map[string]any{"name": rep(1000)})
	expect("card name 1001", 422, "POST", "/lists/"+bd.a+"/cards", map[string]any{"name": rep(1001)})
	card := e.card(bd.a, "x")
	expect("card rename too long", 422, "PATCH", "/cards/"+card, map[string]any{"name": rep(1001)})
	expect("description 200KB", 200, "PATCH", "/cards/"+card, map[string]any{"description": strings.Repeat("a", 200<<10)})
	expect("description > 200KB", 422, "PATCH", "/cards/"+card, map[string]any{"description": strings.Repeat("a", 200<<10+1)})
	expect("create with long description", 422, "POST", "/lists/"+bd.a+"/cards", map[string]any{"name": "d", "description": strings.Repeat("a", 200<<10+1)})
	expect("list name 255", 200, "POST", "/boards/"+bd.id+"/lists", map[string]any{"name": rep(255)})
	expect("list name 256", 422, "POST", "/boards/"+bd.id+"/lists", map[string]any{"name": rep(256)})
	expect("list rename 256", 422, "PATCH", "/lists/"+bd.a, map[string]any{"name": rep(256)})
	expect("board name 256", 422, "POST", "/projects/"+bd.proj+"/boards", map[string]any{"name": rep(256)})
	expect("board rename 256", 422, "PATCH", "/boards/"+bd.id, map[string]any{"name": rep(256)})
	expect("label name 256", 422, "POST", "/boards/"+bd.id+"/labels", map[string]any{"name": rep(256), "color": "berry-red"})
	expect("label name 255", 200, "POST", "/boards/"+bd.id+"/labels", map[string]any{"name": rep(255), "color": "berry-red"})
}

func TestLabelGroupMustBelongToSameBoardOrProject(t *testing.T) {
	e := newEnv(t)
	b1, b2 := e.newBoard("One"), e.newBoard("Two") // separate projects
	foreign := id(e.must(e.admin, "POST", "/boards/"+b2.id+"/label-groups", map[string]any{"name": "Theirs"}))
	own := id(e.must(e.admin, "POST", "/boards/"+b1.id+"/label-groups", map[string]any{"name": "Mine"}))
	if code, _ := e.api(e.admin, "POST", "/boards/"+b1.id+"/labels", map[string]any{"name": "x", "color": "berry-red", "groupId": foreign}); code != 404 {
		t.Fatalf("label.create with a foreign group: %d, want 404", code)
	}
	l := e.must(e.admin, "POST", "/boards/"+b1.id+"/labels", map[string]any{"name": "ok", "color": "berry-red", "groupId": own})
	if l["groupId"] != own {
		t.Fatalf("own group not accepted: %v", l)
	}
	if code, _ := e.api(e.admin, "PATCH", "/labels/"+id(l), map[string]any{"groupId": foreign}); code != 404 {
		t.Fatalf("label.update with a foreign group: %d, want 404", code)
	}
	if code, _ := e.api(e.admin, "PATCH", "/labels/"+id(l), map[string]any{"groupId": ""}); code != 200 {
		t.Fatalf("ungrouping must still work: %d", code)
	}
}

func TestReenablingPublicBoardRotatesToken(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Pub")
	t1 := str(e.must(e.admin, "POST", "/boards/"+bd.id+"/public", map[string]any{"isPublic": true}), "publicToken")
	if t1 == "" {
		t.Fatal("no token")
	}
	// saving "enabled" again keeps the link stable
	if t1b := str(e.must(e.admin, "POST", "/boards/"+bd.id+"/public", map[string]any{"isPublic": true}), "publicToken"); t1b != t1 {
		t.Fatalf("token changed while staying public: %s -> %s", t1, t1b)
	}
	e.must(e.admin, "POST", "/boards/"+bd.id+"/public", map[string]any{"isPublic": false})
	t2 := str(e.must(e.admin, "POST", "/boards/"+bd.id+"/public", map[string]any{"isPublic": true}), "publicToken")
	if t2 == "" || t2 == t1 {
		t.Fatalf("re-enabling must issue a new token (old %q new %q)", t1, t2)
	}
	if code, _ := e.rawReq("GET", "/api/v1/public/boards/"+t1, nil, nil); code != 404 {
		t.Fatalf("old token still works: %d", code)
	}
	if code, _ := e.rawReq("GET", "/api/v1/public/boards/"+t2, nil, nil); code != 200 {
		t.Fatalf("new token: %d", code)
	}
}

func TestShareLinkUsesConfiguredBaseURL(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Share")
	c := e.card(bd.a, "shared")
	e.must(e.admin, "POST", "/cards/"+c+"/share", nil)
	_, body := e.rawReq("GET", "/ui/cards/"+c, map[string]string{"Authorization": "Bearer " + e.admin, "Host": "evil.example", "X-Forwarded-Proto": "https", "X-Forwarded-Host": "evil.example"}, nil)
	if strings.Contains(body, "evil.example") || !strings.Contains(body, "http://weave.test/share/") {
		t.Fatalf("share link not built from WEAVE_BASE_URL: %s", body)
	}
}
