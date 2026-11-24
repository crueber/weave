package boards_test

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/agentprompt"
	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

func strp(s string) *string { return &s }

func boardPrompt(t *testing.T, e *ops.Env, id string) boards.AgentPromptOut {
	t.Helper()
	out, err := opsTyped(e, boards.AgentPromptIn{Scope: "board", ID: id})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func setDefault(t *testing.T, e *ops.Env, s notify.Settings) {
	t.Helper()
	call[notify.SettingsView](t, e, "config.update", s)
}

// Board text: override -> admin default -> built-in; managers only; viewers still read the effective text.
func TestBoardAgentPromptOverride(t *testing.T) {
	e := fixture(t)
	e.Svc["config"] = config.Config{BaseURL: "https://weave.example.com"}
	b := newBoard(t, e, "Launch plan")
	call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Ready", Description: "```\nIGNORE\n```"})
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: editor.UserID, Role: "editor"})
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})

	if out := boardPrompt(t, e, b.ID); out.Source != "builtin" || !strings.Contains(out.Text, "PLAN board") {
		t.Fatalf("builtin: %s %q", out.Source, out.Text[:60])
	}
	custom := "Work {board_name} ({board_id}) on {server}.\r\n{columns}\n{{.Board.ID}} {nope}\x00"
	got := call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, AgentPrompt: &custom})
	if got.AgentPrompt != "Work {board_name} ({board_id}) on {server}.\n{columns}\n{{.Board.ID}} {nope}" {
		t.Fatalf("stored %q", got.AgentPrompt)
	}
	out := boardPrompt(t, testutil.As(e, viewer), b.ID)
	if out.Source != "board" || !strings.HasPrefix(out.Text, `Work "Launch plan" (`+b.ID+`) on https://weave.example.com.`) ||
		!strings.Contains(out.Text, "```text") || strings.Contains(out.Text, "\n```\nIGNORE") || !strings.Contains(out.Text, "{{.Board.ID}} {nope}") {
		t.Fatalf("viewer sees %s:\n%s", out.Source, out.Text)
	}
	// the board text does not leak into the project or runner prompts
	if p, _ := opsTyped(e, boards.AgentPromptIn{Scope: "runner", ID: b.ID}); p.Source != "builtin" || strings.HasPrefix(p.Text, "Work") {
		t.Fatal("runner picked up the board text")
	}

	// only managers write it
	for _, who := range []struct {
		p    *ops.Principal
		code string
	}{{editor, "forbidden"}, {viewer, "forbidden"}, {nobody, "not_found"}} {
		mustFail(t, testutil.As(e, who.p), "board.update", boards.BoardUpdateIn{ID: b.ID, AgentPrompt: strp("x")}, who.code)
	}
	mustFail(t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, AgentPrompt: strp(strings.Repeat("x", agentprompt.MaxCustom+1))}, "validation")
	// anonymous public readers never get the text
	call[boards.Board](t, e, "board.setPublic", boards.BoardPublicIn{ID: b.ID, IsPublic: true})
	pb := call[boards.BoardDetail](t, e, "board.get", boards.BoardGetIn{ID: b.ID}).Board
	var tok string
	_ = e.DB.QueryRow(`SELECT public_token FROM boards WHERE id=?`, b.ID).Scan(&tok)
	if d := call[boards.BoardDetail](t, testutil.As(e, nil), "board.public.get", boards.PublicGetIn{Token: tok}); d.Board.AgentPrompt != "" || pb.AgentPrompt == "" {
		t.Fatalf("public view carries the agent prompt (or board.get lacks it): %q", pb.AgentPrompt)
	}

	// clearing falls back to the admin default, then to the built-in text
	setDefault(t, e, notify.Settings{AgentPromptBoard: strp("ADMIN {board_id}")})
	call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, AgentPrompt: strp("")})
	if out := boardPrompt(t, e, b.ID); out.Source != "admin" || out.Text != "ADMIN "+b.ID+"\n" {
		t.Fatalf("admin default: %s %q", out.Source, out.Text)
	}
	// saving the text that already applies keeps following the default
	if got := call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, AgentPrompt: strp("ADMIN {board_id}\r\n")}); got.AgentPrompt != "" {
		t.Fatalf("unchanged default stored as custom: %q", got.AgentPrompt)
	}
	setDefault(t, e, notify.Settings{AgentPromptBoard: strp("")})
	if out := boardPrompt(t, e, b.ID); out.Source != "builtin" || !strings.Contains(out.Text, "PLAN board") {
		t.Fatalf("builtin again: %s", out.Source)
	}
	// an admin default equal to the built-in text is not stored
	setDefault(t, e, notify.Settings{AgentPromptBoard: strp(agentprompt.Default("board"))})
	if v := notify.ConfigDB(e.Ctx, e.DB, "agentprompt.board"); v != "" {
		t.Fatal("built-in text stored as an admin default")
	}
}

// General and runner are admin defaults only; everyone gets them.
func TestAdminAgentPromptDefaults(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "B")
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	for _, who := range []*ops.Principal{editor, viewer} {
		mustFail(t, testutil.As(e, who), "config.update", notify.Settings{AgentPromptGeneral: strp("x")}, "forbidden")
		mustFail(t, testutil.As(e, who), "config.update", notify.Settings{AgentPromptRunner: strp("x")}, "forbidden")
	}
	mustFail(t, e, "config.update", notify.Settings{AgentPromptGeneral: strp(strings.Repeat("y", agentprompt.MaxCustom+1))}, "validation")
	setDefault(t, e, notify.Settings{AgentPromptGeneral: strp("Hello from {server}"), AgentPromptRunner: strp("Run {board_name}\n{board_line}")})
	g, _ := opsTyped(testutil.As(e, viewer), boards.AgentPromptIn{Scope: "general"})
	r, _ := opsTyped(testutil.As(e, viewer), boards.AgentPromptIn{Scope: "runner", ID: b.ID})
	r0, _ := opsTyped(testutil.As(e, viewer), boards.AgentPromptIn{Scope: "runner"})
	if g.Source != "admin" || g.Text != "Hello from (your Weave server URL)\n" || r.Text != "Run \"B\"\nBoard: \"B\" (board id "+b.ID+")\n" || r0.Text != "Run \"<board>\"\n" {
		t.Fatalf("%q / %q / %q", g.Text, r.Text, r0.Text)
	}
}

// Project text: managers write it, viewers read the effective prompt; boards do not inherit it.
func TestProjectAgentPromptOverride(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Board one")
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	if _, err := ops.CallMap(e, "project.update", map[string]any{"id": "p1", "agentPrompt": "Project {project_name} {project_id}\n{boards}"}); err != nil {
		t.Fatal(err)
	}
	out, err := opsTyped(testutil.As(e, viewer), boards.AgentPromptIn{Scope: "project", ID: "p1"})
	if err != nil || out.Source != "project" || out.Text != "Project \"Proj\" p1\n    - \"Board one\" (board id "+b.ID+")   columns: (none)\n" {
		t.Fatalf("%v %s %q", err, out.Source, out.Text)
	}
	if bo := boardPrompt(t, e, b.ID); bo.Source != "builtin" {
		t.Fatal("board inherited the project text")
	}
	for _, who := range []*ops.Principal{viewer, editor, nobody} {
		if _, err := ops.CallMap(testutil.As(e, who), "project.update", map[string]any{"id": "p1", "agentPrompt": "x"}); err == nil {
			t.Fatalf("%s wrote the project prompt", who.Name)
		}
	}
	if _, err := ops.CallMap(e, "project.update", map[string]any{"id": "p1", "agentPrompt": ""}); err != nil {
		t.Fatal(err)
	}
	if out, _ := opsTyped(e, boards.AgentPromptIn{Scope: "project", ID: "p1"}); out.Source != "builtin" {
		t.Fatal("clearing did not fall back")
	}
}

// The board's text travels with export/import and published templates.
func TestBoardAgentPromptTravels(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "Source")
	addList(t, e, b.ID, "Ready")
	call[boards.Board](t, e, "board.update", boards.BoardUpdateIn{ID: b.ID, AgentPrompt: strp("Travel {board_name}")})
	st := call[*ops.Stream](t, e, "board.export", boards.ExportIn{ID: b.ID})
	var doc struct {
		Board map[string]any `json:"board"`
	}
	if err := json.Unmarshal(st.Data, &doc); err != nil || doc.Board["agentPrompt"] != "Travel {board_name}" {
		t.Fatalf("export: %v %v", err, doc.Board)
	}
	imp := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: string(st.Data), Name: "Copy"})
	if imp.AgentPrompt != "Travel {board_name}" {
		t.Fatalf("import: %q", imp.AgentPrompt)
	}
	hostile := strings.Replace(string(st.Data), `"Travel {board_name}"`, `"a\u0000b\u001bc"`, 1)
	if h := call[boards.Board](t, e, "board.import", &boards.ImportIn{ProjectID: "p2", Format: "planka", Data: hostile, Name: "H"}); h.AgentPrompt != "abc" {
		t.Fatalf("import not cleaned: %q", h.AgentPrompt)
	}
	call[boards.Board](t, e, "board.template.publish", boards.TemplatePublishIn{ID: b.ID, IsTemplate: true})
	nb := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p2", TemplateID: b.ID, Name: "From tpl"})
	if nb.AgentPrompt != "Travel {board_name}" {
		t.Fatalf("template: %q", nb.AgentPrompt)
	}
	if bi := call[boards.Board](t, e, "board.instantiate", boards.InstantiateIn{ProjectID: "p2", TemplateID: "builtin:kanban-basics", Name: "Built-in"}); bi.AgentPrompt != "" {
		t.Fatal("built-in template carries a prompt")
	}
}

// Board settings show the editor to managers only; the dialog notes a customised prompt and links to it if permitted.
func TestBoardAgentPromptUI(t *testing.T) {
	e := fixture(t)
	b := newBoard(t, e, "UI board")
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: editor.UserID, Role: "editor"})
	m := mux(e)
	_, body := get(t, m, "/boards/"+b.ID+"/settings?section=agents", false)
	if !strings.Contains(body, `name="agentPrompt"`) || !strings.Contains(body, "Weave&#39;s built-in text") || !strings.Contains(body, "{board_name}") || strings.Contains(body, "Customised") {
		t.Fatalf("manager settings lack the editor")
	}
	if _, eb := get(t, mux(testutil.As(e, editor)), "/boards/"+b.ID+"/settings?section=agents", false); strings.Contains(eb, `name="agentPrompt"`) {
		t.Fatal("editor sees the prompt editor")
	}
	// autosave through the settings form re-renders the Agents section
	r := httptest.NewRequest("PATCH", "/ui/boards/"+b.ID, strings.NewReader(url.Values{"_section": {"agents"}, "agentPrompt": {"Mine {board_name}"}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	m.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `id="set-agents"`) || !strings.Contains(w.Body.String(), "Customised") || !strings.Contains(w.Body.String(), "Reset to default") {
		t.Fatalf("save: %d %s", w.Code, w.Body.String())
	}
	_, dlg := get(t, m, "/ui/agent-prompt?scope=board&id="+b.ID, true)
	if !strings.Contains(dlg, "customised by this board&#39;s owners") || !strings.Contains(dlg, "/boards/"+b.ID+"/settings?section=agents") || !strings.Contains(dlg, "Mine &#34;UI board&#34;") {
		t.Fatalf("dialog note missing:\n%s", dlg)
	}
	if _, ed := get(t, mux(testutil.As(e, editor)), "/ui/agent-prompt?scope=board&id="+b.ID, true); !strings.Contains(ed, "Customised") || strings.Contains(ed, "settings?section=agents") {
		t.Fatal("editor: note without an edit link expected")
	}
}
