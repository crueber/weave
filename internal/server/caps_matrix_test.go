package server_test

// Capability matrix: personas x pages x controls.
//
// For every row and persona the test renders the page as that persona, computes the authz capability struct
// (authz.Home/Project/Board/Card) and calls the real op over HTTP, then requires the three to agree:
//
//	control rendered (and not disabled)  ==  capability field  ==  op allowed
//
// so a button can never show for someone the op refuses, nor hide from someone it allows. When the op refuses a
// persona who cannot see the object at all, it must answer not_found (existence hiding), never forbidden.
//
// Adding a control: give it a stable `data-cap="..."` hook in the template, then add ONE row below (page, hook,
// capability field, op probe). Rows whose op destroys its target (delete, remove) get a fresh target per persona.
// Probes may be "validation after authz" calls: an input the op rejects as invalid (422) only after its permission
// check, so 422 counts as allowed (okCodes). Required-field checks run before authz, so never probe with those.

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
)

// capPersonas, in report order. admin has no membership anywhere (system administrator only).
var capPersonas = []string{"admin", "P", "O", "E", "C", "V", "W", "G", "AG", "X", "anon"}

// other: a second board the card is on (rows about a card on several boards).
type capTarget struct{ project, board, list, card, ownerMgr, other string }

type capSet struct {
	t     capTarget
	home  authz.HomeCaps
	proj  authz.ProjectCaps
	board authz.BoardCaps
	card  authz.CardCaps
}

type capWorld struct {
	e    *env
	keys map[string]string // persona -> API key ("" = anonymous)
	uid  map[string]string
	main capTarget // project 1 (boardCreation owners), board B, list, card (W is a card member)
	pub  capTarget // a public board in project 1 that nobody is a member of
	mem  capTarget // project 2: boardCreation members + project-wide labels, board B2 (list: a column of B2)
	// multi: board B3 of project 1 with the standard memberships (a card's second board): twoBoards places a fresh card
	// of board B on it as well, so every persona that sees the card sees both of its boards
	multi capTarget
	n     int
}

func (w *capWorld) must(key, method, path string, body any) map[string]any {
	w.e.t.Helper()
	return w.e.must(key, method, path, body)
}

// members gives a board the standard memberships: O owner, E editor, C commenter, V/W/G viewers, AG editor.
func (w *capWorld) members(board string, withAgent bool) {
	P := w.keys["P"]
	add := func(who, role string, cc bool) {
		body := map[string]any{"userId": w.uid[who], "role": role}
		if cc {
			body["canComment"] = true
		}
		w.must(P, "POST", "/boards/"+board+"/members", body)
	}
	add("O", "owner", false)
	add("E", "editor", false)
	add("C", "viewer", true)
	add("V", "viewer", false)
	add("W", "viewer", false)
	add("G", "viewer", false)
	if withAgent {
		w.must(w.keys["O"], "POST", "/agents/"+w.uid["AG"]+"/access", map[string]any{"boardId": board, "role": "editor"})
	}
}

// freshBoard is a new board in project 1 with the standard memberships (for rows whose op destroys the board).
func (w *capWorld) freshBoard() capTarget {
	w.n++
	b := id(w.must(w.keys["P"], "POST", "/projects/"+w.main.project+"/boards", map[string]any{"name": fmt.Sprintf("Fresh %d", w.n)}))
	w.members(b, true)
	return capTarget{project: w.main.project, board: b}
}

// trashedCard is a new card on board B, moved to the trash (only trashed cards, drafts and templates can be deleted).
// W is a member of it, so the worker rule applies to the worker persona.
func (w *capWorld) trashedCard() capTarget {
	P := w.keys["P"]
	t := w.main
	t.card = id(w.must(P, "POST", "/lists/"+w.main.list+"/cards", map[string]any{"name": "Doomed"}))
	w.must(P, "POST", "/cards/"+t.card+"/members", map[string]any{"userId": w.uid["W"]})
	w.must(P, "POST", "/cards/"+t.card+"/archive", map[string]any{"trash": true})
	return t
}

// archivedCard is a new card on board B moved to the archive. W is a member of it, so the archive restore row also
// proves that card.restore.bulk's board-Edit rule does not inherit the card worker rule.
func (w *capWorld) archivedCard() capTarget {
	P := w.keys["P"]
	t := w.main
	t.card = id(w.must(P, "POST", "/lists/"+w.main.list+"/cards", map[string]any{"name": "Archived for matrix"}))
	w.must(P, "POST", "/cards/"+t.card+"/members", map[string]any{"userId": w.uid["W"]})
	w.must(P, "POST", "/cards/"+t.card+"/archive", map[string]any{})
	return t
}

// twoBoards is a new card on board B (W is a member of it: the worker rule) that P also placed on board B3.
func (w *capWorld) twoBoards() capTarget {
	P := w.keys["P"]
	t := w.main
	t.card = id(w.must(P, "POST", "/lists/"+w.main.list+"/cards", map[string]any{"name": "On two boards"}))
	w.must(P, "POST", "/cards/"+t.card+"/members", map[string]any{"userId": w.uid["W"]})
	w.must(P, "POST", "/cards/"+t.card+"/placements", map[string]any{"listId": w.multi.list})
	t.other = w.multi.board
	return t
}

// freshProject is a new project (P owns it, Q2 is a second manager) with one board carrying the standard memberships.
func (w *capWorld) freshProject() capTarget {
	w.n++
	P := w.keys["P"]
	p := id(w.must(P, "POST", "/projects", map[string]any{"name": fmt.Sprintf("Fresh project %d", w.n)}))
	b := id(w.must(P, "POST", "/projects/"+p+"/boards", map[string]any{"name": "Board"}))
	w.members(b, true)
	w.must(P, "POST", "/projects/"+p+"/managers", map[string]any{"userId": w.uid["Q"]})
	var owner string
	_ = w.e.d.QueryRow(`SELECT COALESCE(owner_manager_id,'') FROM projects WHERE id=?`, p).Scan(&owner)
	return capTarget{project: p, board: b, ownerMgr: owner}
}

func newCapWorld(t *testing.T) *capWorld {
	t.Setenv("WEAVE_RATE_LIMIT", "0") // the matrix makes thousands of requests; throttling would only blur it
	e := newEnv(t)
	w := &capWorld{e: e, keys: map[string]string{"admin": e.admin, "anon": ""}, uid: map[string]string{}}
	for k, role := range map[string]string{"P": "boardUser", "O": "boardUser", "E": "boardUser", "C": "boardUser", "V": "boardUser",
		"W": "worker", "G": "guest", "X": "boardUser", "Q": "boardUser"} {
		w.keys[k] = e.newUser(strings.ToLower(k)+"-persona@x.io", role)
	}
	for k, v := range w.keys {
		if v != "" {
			w.uid[k] = e.userID(v)
		}
	}
	P := w.keys["P"]
	m := &w.main
	m.project = id(w.must(P, "POST", "/projects", map[string]any{"name": "Matrix"}))
	m.board = id(w.must(P, "POST", "/projects/"+m.project+"/boards", map[string]any{"name": "Board B"}))
	// The agent: created by the board owner O for board B (owner-created agents).
	w.members(m.board, false)
	ag := w.must(w.keys["O"], "POST", "/agents", map[string]any{"name": "Matrix agent", "boardIds": []string{m.board}, "boardRole": "editor"})
	if k, _ := ag["key"].(map[string]any); k != nil {
		w.keys["AG"], _ = k["key"].(string)
	}
	if w.keys["AG"] == "" {
		t.Fatalf("agent key missing: %v", ag)
	}
	w.uid["AG"] = id(ag)
	m.list = id(w.must(P, "POST", "/boards/"+m.board+"/lists", map[string]any{"name": "Todo"}))
	m.card = id(w.must(P, "POST", "/lists/"+m.list+"/cards", map[string]any{"name": "Card K"}))
	w.must(P, "POST", "/cards/"+m.card+"/members", map[string]any{"userId": w.uid["W"]})

	w.pub = capTarget{project: m.project}
	w.pub.board = id(w.must(P, "POST", "/projects/"+m.project+"/boards", map[string]any{"name": "Public board"}))
	w.pub.list = id(w.must(P, "POST", "/boards/"+w.pub.board+"/lists", map[string]any{"name": "Open"}))
	w.must(P, "POST", "/boards/"+w.pub.board+"/public", map[string]any{"isPublic": true})

	w.mem = capTarget{}
	w.mem.project = id(w.must(P, "POST", "/projects", map[string]any{"name": "Members create"}))
	w.must(P, "PATCH", "/projects/"+w.mem.project, map[string]any{"boardCreation": "members", "useProjectLabels": true})
	w.mem.board = id(w.must(P, "POST", "/projects/"+w.mem.project+"/boards", map[string]any{"name": "Board B2"}))
	w.members(w.mem.board, true)
	w.mem.list = id(w.must(P, "POST", "/boards/"+w.mem.board+"/lists", map[string]any{"name": "Inbox column"}))

	w.multi = capTarget{project: m.project}
	w.multi.board = id(w.must(P, "POST", "/projects/"+m.project+"/boards", map[string]any{"name": "Board B3"}))
	w.multi.list = id(w.must(P, "POST", "/boards/"+w.multi.board+"/lists", map[string]any{"name": "Elsewhere"}))
	w.members(w.multi.board, true)
	return w
}

// env builds an ops.Env for a persona from the stored account (the same Principal the API key authenticates as).
func (w *capWorld) env(persona string) *ops.Env {
	e := &ops.Env{Ctx: context.Background(), DB: w.e.d}
	if uid := w.uid[persona]; uid != "" && persona != "anon" {
		p := &ops.Principal{UserID: uid}
		_ = w.e.d.QueryRow(`SELECT role,is_agent,name FROM users WHERE id=?`, uid).Scan(&p.Role, &p.IsAgent, &p.Name)
		e.User = p
	}
	return e
}

func (w *capWorld) caps(persona string, t capTarget) capSet {
	e := w.env(persona)
	cs := capSet{t: t, home: authz.Home(e)}
	if t.project != "" {
		cs.proj = authz.Project(e, t.project)
	}
	if t.board != "" {
		cs.board = authz.Board(e, t.board)
	}
	if t.card != "" {
		cs.card = authz.CardPlacing(e, t.card) // Card plus CanPlace
	}
	return cs
}

// present reports whether the page carries the hook on an element that is not disabled.
func present(html, hook string) bool {
	re := regexp.MustCompile(`<[a-zA-Z]+[^>]*\bdata-cap="` + regexp.QuoteMeta(hook) + `"[^>]*>`)
	for _, tag := range re.FindAllString(html, -1) {
		if !regexp.MustCompile(`\sdisabled(\s|>|=)`).MatchString(tag) {
			return true
		}
	}
	return false
}

func (w *capWorld) page(persona, path string) (int, string) {
	req, _ := http.NewRequest("GET", w.e.ts.URL+path, nil)
	if k := w.keys[persona]; k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	res, err := sweepClient.Do(req)
	if err != nil {
		w.e.t.Fatal(err)
	}
	defer res.Body.Close()
	var b strings.Builder
	buf := make([]byte, 32<<10)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return res.StatusCode, b.String()
}

type capRow struct {
	name   string
	target func(w *capWorld) capTarget // the shared target, or a fresh one per persona (destructive ops)
	page   func(t capTarget) string
	hook   string
	cap    func(c capSet) bool
	// op is the probe: method, API path (under /api/v1, or a web path starting with "web:") and JSON body.
	op      func(w *capWorld, t capTarget) (string, string, any)
	okCodes []int
	// hidden: when the op refuses and this is false (the persona cannot see the object), it must say not_found.
	hidden func(c capSet) bool
}

func mainT(w *capWorld) capTarget  { return w.main }
func pubT(w *capWorld) capTarget   { return w.pub }
func memT(w *capWorld) capTarget   { return w.mem }
func boardPage(t capTarget) string { return "/boards/" + t.board }
func boardArchive(t capTarget) string {
	// The board shell lazy-loads #archive-view; request its fragment directly because the matrix does not run htmx.
	return "/ui/boards/" + t.board + "/archive?kind=archived"
}
func boardSettings(t capTarget) string {
	return "/boards/" + t.board + "/settings"
}
func projectPage(t capTarget) string     { return "/projects/" + t.project }
func projectSettings(t capTarget) string { return "/projects/" + t.project + "/settings" }
func boardSeen(c capSet) bool            { return c.board.CanView }
func projSeen(c capSet) bool             { return c.proj.CanView }

// Not rows (no data-cap, no op): the read-only views of a card on several boards (docs/DESIGN.md "Placements"): the
// tile's "also on N other boards" marker and its popover, the card sheet's board crumb switcher and the Boards row's
// list. They are gated by visibility alone (card.get's placements: only boards the caller can view; the tile count is
// authz.AlsoOn), which internal/domain/boards/placements_ui_test.go checks per viewer. Their write controls ("Add to
// board...", "Remove from this board") have rows below ("card: add to board", "card: remove from board").
var capRows = []capRow{
	// ---- home and layout
	{name: "home: New project", target: mainT, page: func(capTarget) string { return "/" }, hook: "home-create-project",
		cap: func(c capSet) bool { return c.home.CanCreateProject },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/projects", map[string]any{"name": "Probe"}
		}},
	{name: "layout: Admin console", target: mainT, page: func(capTarget) string { return "/" }, hook: "admin-console",
		cap: func(c capSet) bool { return c.home.AdminConsole },
		op:  func(w *capWorld, t capTarget) (string, string, any) { return "GET", "web:/admin", nil }},

	// ---- project page
	{name: "project: Project settings", target: mainT, page: projectPage, hook: "project-settings",
		cap: func(c capSet) bool { return c.proj.CanManage }, hidden: projSeen,
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "PATCH", "/projects/" + t.project, map[string]any{}
		}},
	{name: "project: Create board (owners policy)", target: mainT, page: projectPage, hook: "project-create-board",
		cap: func(c capSet) bool { return c.proj.CanCreateBoard }, hidden: projSeen,
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/projects/" + t.project + "/boards", map[string]any{"name": "Probe"}
		}},
	{name: "project: Create board (members policy)", target: memT, page: projectPage, hook: "project-create-board",
		cap: func(c capSet) bool { return c.proj.CanCreateBoard }, hidden: projSeen,
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/projects/" + t.project + "/boards", map[string]any{"name": "Probe"}
		}},
	{name: "project: Connect an agent", target: mainT, page: projectPage, hook: "project-connect-agent",
		cap: func(c capSet) bool { return c.proj.CanConnectAgent }, hidden: projSeen,
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "GET", "/agent/prompt?scope=project&id=" + t.project, nil
		}},

	// ---- project settings
	{name: "project settings: General", target: mainT, page: projectSettings, hook: "project-edit",
		cap: func(c capSet) bool { return c.proj.CanManage }, hidden: projSeen,
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "PATCH", "/projects/" + t.project, map[string]any{}
		}},
	{name: "project settings: Add manager", target: mainT, page: projectSettings, hook: "project-managers-add",
		cap: func(c capSet) bool { return c.proj.CanManage }, hidden: projSeen,
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/projects/" + t.project + "/managers", map[string]any{"userId": w.uid["G"]} // guests are refused after authz
		}, okCodes: []int{422}},
	{name: "project settings: Remove owning manager", target: func(w *capWorld) capTarget { return w.freshProject() }, page: projectSettings,
		hook: "project-owner-remove", hidden: projSeen,
		cap: func(c capSet) bool { return c.proj.CanRemoveOwnerManager },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "DELETE", "/projects/" + t.project + "/managers/" + t.ownerMgr, nil
		}},
	{name: "project settings: Delete project", target: func(w *capWorld) capTarget { return w.freshProject() }, page: projectSettings,
		hook: "project-delete", hidden: projSeen,
		cap: func(c capSet) bool { return c.proj.CanManage },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "DELETE", "/projects/" + t.project, nil
		}},

	// ---- board page header
	{name: "board: Settings link", target: mainT, page: boardPage, hook: "board-settings", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanOpenSettings },
		op:  func(w *capWorld, t capTarget) (string, string, any) { return "GET", "web:" + boardSettings(t), nil }},
	{name: "public board: Settings link", target: pubT, page: boardPage, hook: "board-settings", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanOpenSettings },
		op:  func(w *capWorld, t capTarget) (string, string, any) { return "GET", "web:" + boardSettings(t), nil }},
	{name: "board: Watch", target: mainT, page: boardPage, hook: "board-watch", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanWatch },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "PUT", "/boards/" + t.board + "/subscription", map[string]any{}
		}},
	{name: "public board: Watch", target: pubT, page: boardPage, hook: "board-watch", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanWatch },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "PUT", "/boards/" + t.board + "/subscription", map[string]any{}
		}},
	{name: "board: Connect an agent", target: mainT, page: boardPage, hook: "board-connect-agent", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanConnectAgent },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "GET", "/agent/prompt?scope=board&id=" + t.board, nil
		}},
	{name: "board: restore archived", target: func(w *capWorld) capTarget { return w.archivedCard() }, page: boardArchive,
		hook: "restoreArchived", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanRestoreArchived },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/boards/" + t.board + "/archive/restore", map[string]any{"cardIds": []string{t.card}}
		}},

	// ---- board settings
	{name: "board settings: General (edit)", target: mainT, page: boardSettings, hook: "board-edit", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanEdit && c.board.CanOpenSettings },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "PATCH", "/boards/" + t.board, map[string]any{"name": ""} // empty name: refused after authz
		}, okCodes: []int{422}},
	{name: "board settings: Background upload", target: mainT, page: boardSettings, hook: "board-background-upload", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanEdit && c.board.CanOpenSettings },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "PATCH", "/boards/" + t.board, map[string]any{"backgroundPattern": "!"} // invalid pattern: refused after authz (Edit)
		}, okCodes: []int{422}},
	{name: "board settings: Add member", target: mainT, page: boardSettings, hook: "board-members-add", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanManageMembers },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/boards/" + t.board + "/members", map[string]any{} // neither user nor group: refused after authz
		}, okCodes: []int{422}},
	{name: "board settings: Add label", target: mainT, page: boardSettings, hook: "board-labels-add", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanEditLabels },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/boards/" + t.board + "/labels", map[string]any{"name": "x", "color": "berry-red"}
		}},
	{name: "board settings: Add label (project labels)", target: memT, page: boardSettings, hook: "board-labels-add", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanEditLabels },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/boards/" + t.board + "/labels", map[string]any{"name": "x", "color": "berry-red"}
		}},
	{name: "board settings: Public link", target: mainT, page: boardSettings, hook: "board-share", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanShare },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/boards/" + t.board + "/public", map[string]any{"isPublic": false}
		}},
	{name: "board settings: Publish template", target: mainT, page: boardSettings, hook: "board-template-publish", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanPublishTemplate },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/boards/" + t.board + "/template", map[string]any{"isTemplate": false}
		}},
	{name: "board settings: Apply a template", target: mainT, page: boardSettings, hook: "board-apply-template", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanApplyTemplate },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/boards/" + t.board + "/template/apply", map[string]any{"templateId": "builtin:kanban-basics", "dryRun": true}
		}},
	{name: "board settings: Import row", target: mainT, page: boardSettings, hook: "board-import", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanCreateBoardHere && c.board.CanOpenSettings },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/projects/" + t.project + "/boards", map[string]any{"name": "Probe"}
		}},
	{name: "board settings: Agent accounts", target: mainT, page: boardSettings, hook: "board-agents", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanManageAgents },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/agents", map[string]any{"name": "Probe", "boardIds": []string{t.board}, "boardRole": "viewer"}
		}},
	{name: "board settings: Agent accounts fragment", target: mainT, page: func(t capTarget) string { return "/ui/boards/" + t.board + "/agents" },
		hook: "board-agent-create", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanManageAgents },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "GET", "/agents?boardId=" + t.board, nil
		}},
	{name: "board settings: Prompt text", target: mainT, page: boardSettings, hook: "board-prompt-edit", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanEditPrompt },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "PATCH", "/boards/" + t.board, map[string]any{"agentPrompt": ""}
		}},
	{name: "board settings: Webhooks", target: mainT, page: boardSettings, hook: "board-webhooks", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanManageWebhooks && c.board.CanOpenSettings },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/webhooks", map[string]any{"name": "p", "url": "https://" + sweepHookHost + "/x", "boardId": t.board, "events": []string{"bogus"}}
		}, okCodes: []int{422}},
	{name: "board settings: Webhooks fragment", target: mainT, page: func(t capTarget) string { return "/ui/boards/" + t.board + "/webhooks" },
		hook: "board-webhook-create", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanManageWebhooks },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "GET", "/webhooks?boardId=" + t.board, nil
		}},
	{name: "board settings: Notifications", target: mainT, page: boardSettings, hook: "board-notify", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanManageNotify },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "GET", "/notification-services?boardId=" + t.board, nil
		}},
	{name: "board settings: Delete board", target: func(w *capWorld) capTarget { return w.freshBoard() }, page: boardSettings,
		hook: "board-delete", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanDelete },
		op:  func(w *capWorld, t capTarget) (string, string, any) { return "DELETE", "/boards/" + t.board, nil }},

	// ---- column "..." menu (board page) and the column sheet's right rail
	{name: "column menu: Column settings", target: mainT, page: boardPage, hook: "column-settings", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanEdit },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "PATCH", "/lists/" + t.list, map[string]any{}
		}},
	{name: "column sheet: Destructive actions", target: mainT, page: func(t capTarget) string { return "/ui/lists/" + t.list }, hook: "column-destructive", hidden: boardSeen,
		cap: func(c capSet) bool { return c.board.CanEdit },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "PATCH", "/lists/" + t.list, map[string]any{}
		}},

	// ---- card sheet
	{name: "card: edit (worker rule)", target: mainT, page: func(t capTarget) string { return "/cards/" + t.card }, hook: "card-edit",
		hidden: func(c capSet) bool { return c.card.CanView },
		cap:    func(c capSet) bool { return c.card.CanEdit },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "PATCH", "/cards/" + t.card, map[string]any{}
		}},
	{name: "card: add checklist (board Edit, no worker rule)", target: mainT, page: func(t capTarget) string { return "/ui/cards/" + t.card + "/tasks" },
		hook: "card-add-checklist", hidden: func(c capSet) bool { return c.card.CanView },
		cap: func(c capSet) bool { return c.card.CanEditExtras },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/cards/" + t.card + "/task-lists", map[string]any{"name": "probe"}
		}},
	{name: "card: delete permanently (trashed card)", target: func(w *capWorld) capTarget { return w.trashedCard() },
		page: func(t capTarget) string { return "/cards/" + t.card }, hook: "card-delete", hidden: func(c capSet) bool { return c.card.CanView },
		cap: func(c capSet) bool { return c.card.CanEdit },
		op:  func(w *capWorld, t capTarget) (string, string, any) { return "DELETE", "/cards/" + t.card, nil }},
	// a card on two boards (B and B3): "Add to board..." probes card.place onto board B2 with a boardId that does not
	// match the column, which card.place refuses (422) only after both of its checks (edit the card; edit the target
	// board), so nothing is placed; every persona's memberships are the same on B, B2 and B3.
	{name: "card: add to board (card on two boards)", target: func(w *capWorld) capTarget { t := w.twoBoards(); t.list = w.mem.list; return t },
		page: func(t capTarget) string { return "/cards/" + t.card }, hook: "card-add-to-board", hidden: func(c capSet) bool { return c.card.CanView },
		cap: func(c capSet) bool { return c.card.CanPlace },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/cards/" + t.card + "/placements", map[string]any{"listId": t.list, "boardId": "not-that-board"}
		}, okCodes: []int{422}},
	{name: "card: remove from board (card on two boards)", target: func(w *capWorld) capTarget { return w.twoBoards() },
		page: func(t capTarget) string { return "/cards/" + t.card }, hook: "card-remove-from-board", hidden: func(c capSet) bool { return c.card.CanView },
		cap: func(c capSet) bool { return c.card.CanRemoveFromBoard(c.t.other) },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "DELETE", "/cards/" + t.card + "/placements/" + t.other, nil
		}},
	{name: "card: remove from board (tile quick edit)", target: func(w *capWorld) capTarget { return w.twoBoards() },
		page: func(t capTarget) string { return "/ui/cards/" + t.card + "/quick?board=" + t.board }, hook: "card-remove-from-board", hidden: func(c capSet) bool { return c.card.CanView },
		cap: func(c capSet) bool { return c.card.CanRemoveFromBoard(c.t.board) },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "DELETE", "/cards/" + t.card + "/placements/" + t.board, nil
		}},
	{name: "card: comment", target: mainT, page: func(t capTarget) string { return "/cards/" + t.card }, hook: "card-comment",
		hidden: func(c capSet) bool { return c.card.CanView },
		cap:    func(c capSet) bool { return c.card.CanComment },
		op: func(w *capWorld, t capTarget) (string, string, any) {
			return "POST", "/cards/" + t.card + "/comments", map[string]any{"text": "probe"}
		}},
}

func (w *capWorld) probe(persona, method, path string, body any) int {
	if web, ok := strings.CutPrefix(path, "web:"); ok {
		code, _ := w.page(persona, web)
		return code
	}
	code, _ := w.e.raw(w.keys[persona], method, "/api/v1"+path, body)
	return code
}

func TestCapabilityMatrix(t *testing.T) {
	w := newCapWorld(t)
	for _, r := range capRows {
		for _, p := range capPersonas {
			tg := r.target(w)
			code, html := w.page(p, r.page(tg))
			shown := code == 200 && present(html, r.hook)
			cs := w.caps(p, tg)
			want := r.cap(cs)
			m, path, body := r.op(w, tg)
			oc := w.probe(p, m, path, body)
			allowed := oc >= 200 && oc < 300
			for _, c := range r.okCodes {
				allowed = allowed || oc == c
			}
			if shown != want || want != allowed {
				t.Errorf("%-45s %-5s shown=%v cap=%v op=%v (%s %s -> %d, page %d)", r.name, p, shown, want, allowed, m, path, oc, code)
			}
			if !allowed && r.hidden != nil && !r.hidden(cs) && oc == http.StatusForbidden {
				t.Errorf("%-45s %-5s op answers 403 to someone who cannot see the object (want 404)", r.name, p)
			}
		}
	}
}

// adminRowAllowed lists the matrix rows where the admin persona (no membership anywhere) may hold the capability:
// system powers and what every signed-in person gets. Every other row is content and must be false for the admin.
var adminRowAllowed = map[string]string{
	"home: New project":     "any signed-in admin or boardUser may create a project of their own (it then owns it)",
	"layout: Admin console": "system administration",
	"public board: Watch":   "a public board is readable by everyone; any signed-in person may watch it",
}

// TestCapabilityMatrixAdminHasNoContentControls: the admin role carries no content access, so the admin persona,
// who is a member of nothing, holds no content capability on any page of the matrix.
func TestCapabilityMatrixAdminHasNoContentControls(t *testing.T) {
	w := newCapWorld(t)
	for _, r := range capRows {
		tg := r.target(w)
		if r.cap(w.caps("admin", tg)) {
			if _, ok := adminRowAllowed[r.name]; !ok {
				t.Errorf("%s: the admin persona (no membership) holds this content capability", r.name)
			}
		}
	}
	cs := w.caps("admin", w.main)
	if cs.proj.CanView || cs.board.CanView || cs.card.CanView || cs.board.IsMember || cs.proj.Level != authz.None || cs.board.Level != authz.None {
		t.Errorf("admin persona sees the main project/board/card: %+v", cs)
	}
	if !cs.home.AdminConsole || !cs.home.CanCreateProject {
		t.Errorf("admin persona lost a system capability: %+v", cs.home)
	}
}

// TestCapabilityMatrixManagerRows covers the per-row rule of project.manager.remove: a second manager's row is
// removable by any manager, the owning manager's row only by the owner themself (while their account is active),
// the last manager never, and an admin who is not a manager does not get the settings page at all.
func TestCapabilityMatrixManagerRows(t *testing.T) {
	w := newCapWorld(t)
	if code, _ := w.page("admin", projectSettings(w.freshProject())); code != 404 {
		t.Errorf("admin (not a manager) project settings: %d, want 404", code)
	}
	for _, p := range []string{"P", "Q"} {
		tg := w.freshProject()
		_, html := w.page(p, projectSettings(tg))
		if n := strings.Count(html, `data-cap="project-manager-remove"`); n != 1 { // Q's row
			t.Errorf("%s: %d removable (non-owner) manager rows, want 1", p, n)
		}
		owner := strings.Contains(html, `data-cap="project-owner-remove"`)
		if want := authz.Project(w.env(p), tg.project).CanRemoveOwnerManager; owner != want {
			t.Errorf("%s: owner row removable=%v, want %v", p, owner, want)
		}
	}
	// a project with a single manager offers no remove button at all (the op refuses the last manager)
	tg := capTarget{project: id(w.must(w.keys["P"], "POST", "/projects", map[string]any{"name": "Solo"}))}
	if _, html := w.page("P", projectSettings(tg)); strings.Contains(html, `-remove"`) {
		t.Error("the last manager must not be removable")
	}
}

// TestCapabilityMatrixNoForeignEmails: an owner's board settings list people to invite by name, never by e-mail
// (only admins and the person themself see addresses).
func TestCapabilityMatrixNoForeignEmails(t *testing.T) {
	w := newCapWorld(t)
	for _, p := range []string{"O", "P"} {
		code, html := w.page(p, boardSettings(w.main))
		if code != 200 || !present(html, "board-members-add") {
			t.Fatalf("%s: settings %d without the member picker", p, code)
		}
		if !strings.Contains(html, "x-persona") { // the outsider is offered as a candidate (by name)
			t.Errorf("%s: the outsider is missing from the people picker", p)
		}
		self := strings.ToLower(p) + "-persona@x.io"
		for _, m := range regexp.MustCompile(`[a-z0-9.-]+@x\.io`).FindAllString(html, -1) {
			if m != self {
				t.Errorf("%s: board settings expose the e-mail address %s", p, m)
			}
		}
	}
}
