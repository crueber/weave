package server_test

import (
	"strings"
	"testing"
)

// No state-changing GETs: claiming a card is a POST with a JSON body.
func TestCardNextIsPostOnly(t *testing.T) {
	e := newEnv(t)
	bd := e.newBoard("Queue")
	c := e.card(bd.a, "first")
	auth := map[string]string{"Authorization": "Bearer " + e.admin}
	if code, _ := e.rawReq("GET", "/api/v1/lists/"+bd.a+"/next?claim=true", auth, nil); code != 405 {
		t.Fatalf("GET next?claim=true: %d, want 405", code)
	}
	var claimed string
	_ = e.d.QueryRow(`SELECT COALESCE(claimed_by,'') FROM cards WHERE id=?`, c).Scan(&claimed)
	if claimed != "" {
		t.Fatal("GET must not claim")
	}
	item := e.must(e.admin, "POST", "/lists/"+bd.a+"/next", map[string]any{"claim": true, "ttlSeconds": 60})
	if id(item) != c {
		t.Fatalf("claimed %v, want %s", item, c)
	}
	_ = e.d.QueryRow(`SELECT COALESCE(claimed_by,'') FROM cards WHERE id=?`, c).Scan(&claimed)
	if claimed == "" {
		t.Fatal("POST claim=true must claim")
	}
	// peek (no claim) is the same POST and changes nothing
	code, out := e.rawReq("POST", "/api/v1/lists/"+bd.a+"/next", map[string]string{"Authorization": "Bearer " + e.admin, "Content-Type": "application/json"}, strings.NewReader(`{}`))
	if code != 200 || !strings.Contains(out, `"item":null`) {
		t.Fatalf("peek after claim: %d %s", code, out)
	}
}
