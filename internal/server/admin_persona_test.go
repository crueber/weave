package server_test

// The admin persona: a system administrator with no membership anywhere.
//
// The admin role is system administration (users, groups, instance settings, global webhooks, agent ACCOUNTS,
// project recovery) and carries no content access. These tests seed the sweep world (owned by user A), give the
// admin stale per-user rows on A's content (as an upgraded instance would have: watches, a card membership, a
// favourite, old notifications) and then require:
//
//   - every non-system op answers the admin exactly like the plain outsider B: not_found where B gets not_found
//     (never forbidden, never data), and no seeded content in any response;
//   - every web page and SSE room of A's content is closed to the admin, and the admin console carries no content;
//   - the admin still performs every system function.
//
// adminDiffOK lists the ops where the admin may legitimately get a different answer than B, with the reason.

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// adminAccountMarkers are seeded names the admin may see through SYSTEM functions: the agent ACCOUNT (agent
// account control lists every agent by name, never its boards) and the user group (group administration). They are
// removed from a response before the content check; everything else carrying the CANARY marker is content.
var adminAccountMarkers = []string{"AgentCANARY", "GroupCANARY"}

var adminDiffOK = map[string]string{
	"apikey.revoke":           "user administration: admins revoke any person's API key",
	"user.avatar.delete":      "user administration: admins remove any person's avatar",
	"user.avatar.update":      "user administration: admins replace any person's avatar (the sweep's request then fails validation)",
	"user.update":             "user administration: admins edit any person's profile (account data, not content)",
	"user.preferences.update": "user administration: admins edit any person's preferences (account data, not content)",
	"agent.update":            "agent account control: admins rename and (de)activate every agent account",
	"agent.rotateKey":         "agent account control: admins rotate every agent's key",
	"agent.delete":            "agent account control: admins delete every agent account",
	"agent.list":              "agent account control: admins list every agent account (board access only on boards they manage)",
}

// seedAdminStale gives the admin per-user rows on A's content, as an instance upgraded from the admin bypass would
// have them. None of them may reveal anything once the admin is no longer a member.
func seedAdminStale(t *testing.T, w *world) {
	t.Helper()
	I, a := w.ids, w.uid["admin"]
	now := db.Now()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO card_subscriptions(card_id,user_id) VALUES(?,?)`, []any{I["card"], a}},
		{`INSERT INTO board_subscriptions(board_id,user_id) VALUES(?,?)`, []any{I["board"], a}},
		{`INSERT INTO list_subscriptions(list_id,user_id,created_at) VALUES(?,?,?)`, []any{I["list"], a, now}},
		{`INSERT INTO card_memberships(card_id,user_id) VALUES(?,?)`, []any{I["card"], a}},
		{`INSERT INTO project_favorites(project_id,user_id) VALUES(?,?)`, []any{I["project"], a}},
		{`INSERT INTO notifications(id,user_id,creator_user_id,board_id,card_id,type,data,created_at) VALUES('n-admin-stale',?,?,?,?,'commentCard','{"card":{"name":"NotifCANARY"},"text":"NotifCANARY"}',?)`,
			[]any{a, w.uid["A"], I["board"], I["card"], now}},
	} {
		if _, err := w.e.d.Exec(q.sql, q.args...); err != nil {
			t.Fatalf("stale seed %s: %v", q.sql, err)
		}
	}
}

// contentLeak reports whether a response to the admin carries seeded content (account markers removed).
func contentLeak(body string) bool {
	body = strings.ReplaceAll(body, `"query":"`+canary+`"`, "")
	for _, m := range adminAccountMarkers {
		body = strings.ReplaceAll(body, m, "")
	}
	return strings.Contains(body, canary)
}

func collect(t *testing.T, caller string, stale bool) map[string]result {
	w := newWorld(t)
	if stale {
		seedAdminStale(t, w)
	}
	out := map[string]result{}
	for _, o := range ops.All() {
		// system writes (user deletion, group changes, ...) would change the world for the ops after them; they are
		// covered by TestAdminPersonaSystemFunctions
		if skipOp(o) || (o.Auth == ops.AuthAdmin && o.Method != "GET") {
			continue
		}
		out[o.Name] = w.call(o, caller)
	}
	return out
}

// TestAdminPersonaOpsMatchOutsider: for every op that is not a system op, the admin without membership gets the
// answer the plain outsider B gets (status), never forbidden where B gets not_found, and never content.
func TestAdminPersonaOpsMatchOutsider(t *testing.T) {
	admin, outsider := collect(t, "admin", true), collect(t, "B", false)
	var bad []string
	for _, o := range ops.All() {
		if skipOp(o) {
			continue
		}
		a, b := admin[o.Name], outsider[o.Name]
		if _, public := publicOps[o.Name]; public || a.op == "" {
			continue // public views (share token, public board) are the same for everyone
		}
		if contentLeak(a.body) && o.Name != "user.list" && o.Name != "group.get" && o.Name != "group.list" {
			bad = append(bad, fmt.Sprintf("%s %s leaked content to the admin: %.160s", o.Name, a.path, strings.ReplaceAll(a.body, "\n", " ")))
		}
		if o.Auth == ops.AuthAdmin {
			continue // system ops: covered by TestAdminPersonaSystemFunctions
		}
		if _, ok := adminDiffOK[o.Name]; ok {
			continue
		}
		if b.status == http.StatusNotFound && a.status == http.StatusForbidden {
			bad = append(bad, fmt.Sprintf("%s %s: admin gets 403 where an outsider gets 404 (existence must stay hidden)", o.Name, a.path))
			continue
		}
		if a.status != b.status {
			bad = append(bad, fmt.Sprintf("%s %s: admin %d, outsider %d (%.120s)", o.Name, a.path, a.status, b.status, strings.ReplaceAll(a.body, "\n", " ")))
		}
	}
	sort.Strings(bad)
	for _, x := range bad {
		t.Error(x)
	}
}

// TestAdminPersonaWebRoutes: the admin without membership gets no page of A's content (not found), no content in
// any GET route (the admin console included), and no SSE room of A's board or project.
func TestAdminPersonaWebRoutes(t *testing.T) {
	w := newWorld(t)
	seedAdminStale(t, w)
	key := w.keys["admin"]
	for _, p := range []string{"/boards/" + w.ids["board"], "/projects/" + w.ids["project"], "/cards/" + w.ids["card"],
		"/boards/" + w.ids["board"] + "/settings", "/projects/" + w.ids["project"] + "/settings", "/export/boards/" + w.ids["board"],
		"/export/cards/" + w.ids["card"], "/ui/boards/" + w.ids["board"] + "/webhooks", "/ui/boards/" + w.ids["board"] + "/agents",
		"/ui/webhooks/" + w.ids["webhook"] + "/deliveries", "/admin/webhooks/" + w.ids["webhook"] + "/deliveries"} {
		code, b := w.e.rawPatient(key, "GET", p, nil)
		if code != http.StatusNotFound || contentLeak(string(b)) {
			t.Errorf("admin GET %s -> %d (want 404, no content)", p, code)
		}
	}
	var ids []string
	for _, k := range []string{"card", "board", "list", "project", "attachment", "comment", "task", "image", "cfgroup", "userA", "session", "ns", "member", "webhook", "agent", "tplboard"} {
		ids = append(ids, w.ids[k])
	}
	for _, rp := range web.RoutePatterns() {
		meth, pat, _ := strings.Cut(rp[0], " ")
		if meth != "GET" || strings.HasPrefix(pat, "/share/") || strings.HasPrefix(pat, "/public/") || pat == "/admin/backup" {
			continue
		}
		for _, cand := range ids {
			p := strings.ReplaceAll(pat, "{$}", "")
			for _, ph := range []string{"id", "boardId", "cardId", "listId", "projectId", "imageId"} {
				p = strings.ReplaceAll(p, "{"+ph+"}", cand)
			}
			p = strings.NewReplacer("{token}", "nope", "{name}", w.ids["attname"], "{variant}", "original", "{tab}", "general", "{userId}", w.ids["userA"]).Replace(p)
			if strings.Contains(p, "{") || (strings.HasSuffix(p, "/") && strings.Contains(p, "/ui/cards/")) {
				continue
			}
			code, b := w.e.rawPatient(key, "GET", p, nil)
			if cand == w.ids["pubboard"] {
				continue
			}
			if code == 200 && contentLeak(string(b)) {
				t.Errorf("admin GET %s leaked content (route %s)", p, rp[0])
			}
			if !strings.Contains(rp[0], "{") {
				break
			}
		}
	}
	// the admin console: no links into anyone's content
	for _, p := range []string{"/admin", "/admin/users", "/admin/agents", "/admin/groups", "/admin/orphans", "/admin/audit", "/admin/webhooks", "/admin/config", "/admin/sso", "/admin/agent-prompts", "/", "/dashboard", "/notifications", "/search?q=CANARY", "/settings"} {
		code, b := w.e.rawPatient(key, "GET", p, nil)
		if code != 200 {
			t.Errorf("admin GET %s -> %d", p, code)
		}
		for _, link := range []string{`href="/boards/` + w.ids["board"], `href="/projects/` + w.ids["project"], `href="/cards/` + w.ids["card"]} {
			if strings.Contains(string(b), link) {
				t.Errorf("admin GET %s links to A's content (%s)", p, link)
			}
		}
		if strings.HasPrefix(p, "/search") { // the page echoes the query; only results count
			for _, m := range []string{"ProjCANARY", "BoardCANARY", "CardCANARY", "DescCANARY", "CommentCANARY", "TaskCANARY", "NotifCANARY"} {
				if strings.Contains(string(b), m) {
					t.Errorf("admin search shows %s", m)
				}
			}
			continue
		}
		if contentLeak(string(b)) {
			t.Errorf("admin GET %s shows content", p)
		}
	}
	// a full-page navigation to A's board gets the styled not-found page in the layout; htmx and fragments a toast
	if code, b := w.e.rawPatient(key, "GET", "/boards/"+w.ids["board"], nil); code != 404 || !strings.Contains(string(b), "Board not found") || !strings.Contains(string(b), `class="web-error"`) {
		t.Errorf("not-found page: %d", code)
	}
	if code, b := w.e.rawPatient(key, "GET", "/ui/boards/"+w.ids["board"]+"/webhooks", nil); code != 404 || strings.Contains(string(b), `class="web-error"`) {
		t.Errorf("a fragment must get the toast, not the page: %d", code)
	}
	// the stale notification about A's card: not listed, not counted, and not readable by id
	if code, b := w.e.rawPatient(key, "POST", "/api/v1/notifications/n-admin-stale/read", nil); code != http.StatusNotFound || contentLeak(string(b)) {
		t.Errorf("admin notification.read of a stale notification -> %d %s", code, b)
	}
	if _, b := w.e.rawPatient(key, "GET", "/api/v1/notifications/unread-count", nil); !strings.Contains(string(b), `"count":0`) {
		t.Errorf("admin unread count includes a notification about a board they cannot see: %s", b)
	}
	// SSE: envelope stream filters and legacy rooms
	if s := w.e.open(key, "boards="+w.ids["board"], nil); s.status != http.StatusNotFound {
		t.Errorf("admin events boards=<A's board> -> %d, want 404", s.status)
	}
	if s := w.e.open(key, "lists="+w.ids["list"], nil); s.status != http.StatusNotFound {
		t.Errorf("admin events lists=<A's list> -> %d, want 404", s.status)
	}
}

// TestAdminPersonaSystemFunctions: the admin without any membership still runs the instance.
func TestAdminPersonaSystemFunctions(t *testing.T) {
	w := newWorld(t)
	e, key := w.e, w.keys["admin"]
	ok := func(method, path string, body any) map[string]any {
		t.Helper()
		code, out := e.api(key, method, path, body)
		if code != 200 {
			t.Errorf("admin %s %s -> %d %v", method, path, code, out)
		}
		it, _ := out["item"].(map[string]any)
		return it
	}
	// users and sessions
	ok("GET", "/users", nil)
	ok("POST", "/users/"+w.uid["B"]+"/sessions/revoke-all", nil)
	ok("POST", "/users/"+w.uid["B"]+"/deactivate", nil)
	ok("POST", "/users/"+w.uid["B"]+"/activate", nil)
	// groups
	g := ok("POST", "/user-groups", map[string]any{"name": "Ops team"})
	ok("POST", "/user-groups/"+id(g)+"/members", map[string]any{"userId": w.uid["B"]})
	ok("DELETE", "/user-groups/"+id(g), nil)
	// instance settings, prompt defaults, storage, stats, reindex, backup
	ok("GET", "/config/settings", nil)
	ok("PATCH", "/config/settings", map[string]any{"branding.name": "Weave test"})
	ok("GET", "/system/storage", nil)
	ok("GET", "/admin/stats", nil)
	ok("POST", "/search/reindex", nil)
	if code, _ := e.raw(key, "GET", "/api/v1/admin/backup", nil); code != 200 {
		t.Errorf("admin backup -> %d", code)
	}
	ok("POST", "/agent/prompt/preview", map[string]any{"kind": "prompt", "name": "general"})
	// global webhooks
	hk := ok("POST", "/webhooks", map[string]any{"name": "global", "url": "https://" + sweepHookHost + "/g"})
	ok("DELETE", "/webhooks/"+id(hk), nil)
	// agent ACCOUNT control: list, rename, deactivate/activate, rotate, delete; no board names of A's boards
	code, b := e.raw(key, "GET", "/api/v1/agents", nil)
	if code != 200 || !strings.Contains(string(b), "AgentCANARY") || strings.Contains(string(b), "BoardCANARY") || !strings.Contains(string(b), `"boardCount":1`) {
		t.Errorf("admin agent.list: %d %s", code, b)
	}
	ag := w.ids["agent"]
	ok("PATCH", "/agents/"+ag, map[string]any{"isDeactivated": true})
	ok("PATCH", "/agents/"+ag, map[string]any{"isDeactivated": false, "name": "Renamed agent"})
	ok("POST", "/agents/"+ag+"/key", map[string]any{})
	// ... but no board grant or revoke on a board the admin does not manage (not found, the board stays hidden)
	for _, rv := range []bool{false, true} {
		if code, _ := e.api(key, "POST", "/agents/"+ag+"/access", map[string]any{"boardId": w.ids["board"], "revoke": rv}); code != 404 {
			t.Errorf("admin agent.access revoke=%v on A's board -> %d, want 404", rv, code)
		}
	}
	ok("DELETE", "/agents/"+ag, nil)
	// orphan recovery: list and audit log
	ok("GET", "/projects/orphans", nil)
	ok("GET", "/projects/recoveries", nil)
	// the admin audit log (agent key rotations, group adds, relinks): readable, and the rotation above is in it
	if code, out := e.api(key, "GET", "/audit?action=agent.rotateKey", nil); code != 200 || !strings.Contains(fmt.Sprint(out), "agent.rotateKey") {
		t.Errorf("admin audit.list -> %d %v", code, out)
	}
	// the admin's own project: full content access as its owner
	p := ok("POST", "/projects", map[string]any{"name": "Admin's own"})
	bd := ok("POST", "/projects/"+id(p)+"/boards", map[string]any{"name": "Admin board"})
	ok("GET", "/boards/"+id(bd), nil)
	code, out := e.api(key, "GET", "/projects", nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Errorf("admin project.list: %d %v (want exactly the admin's own project)", code, out)
	}
}

// TestAdminPersonaMemberSeesExactlyThat: an admin invited to one board sees that board (and its project) and
// nothing else of A's.
func TestAdminPersonaMemberSeesExactlyThat(t *testing.T) {
	w := newWorld(t)
	e, key := w.e, w.keys["admin"]
	e.must(w.keys["A"], "POST", "/boards/"+w.ids["pubboard"]+"/members", map[string]any{"userId": w.uid["admin"], "role": "viewer"})
	if code, _ := e.api(key, "GET", "/boards/"+w.ids["pubboard"], nil); code != 200 {
		t.Errorf("member admin: board.get -> %d", code)
	}
	if code, _ := e.api(key, "GET", "/boards/"+w.ids["board"], nil); code != 404 {
		t.Errorf("member admin: another board -> %d, want 404", code)
	}
	if code, _ := e.api(key, "PATCH", "/boards/"+w.ids["pubboard"], map[string]any{"name": "x"}); code != 403 {
		t.Errorf("viewer admin: board.update -> %d, want 403", code)
	}
	code, out := e.api(key, "GET", "/projects", nil)
	items, _ := out["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Errorf("member admin: project.list %d %v", code, out)
	}
	_, who := e.api(key, "GET", "/auth/whoami", nil)
	it, _ := who["item"].(map[string]any)
	bs, _ := it["boards"].([]any)
	if len(bs) != 1 {
		t.Errorf("member admin: whoami boards %v", bs)
	}
}
