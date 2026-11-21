package boards_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/agentprompt"
	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/domain/boards"
	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/testutil"
)

// Every block has exactly one config key in notify.Settings (agentblock.<name>).
func TestAgentBlockSettingsCoverEveryBlock(t *testing.T) {
	var keys []string
	st := reflect.TypeOf(notify.Settings{})
	for i := 0; i < st.NumField(); i++ {
		if k, ok := strings.CutPrefix(st.Field(i).Tag.Get("json"), "agentblock."); ok {
			keys = append(keys, k)
		}
	}
	if !slices.Equal(keys, agentprompt.BlockNames()) {
		t.Fatalf("Settings agentblock keys %v, blocks %v", keys, agentprompt.BlockNames())
	}
}

// An admin block text reaches every scope that uses the block (column prompt included), never the work order; the
// output names it; Reset (empty) and text equal to the built-in fall back; only admins write blocks.
func TestAgentBlockOverride(t *testing.T) {
	e := fixture(t)
	e.Svc["config"] = config.Config{BaseURL: "https://weave.example.com"}
	b := newBoard(t, e, "Launch plan")
	l := call[boards.List](t, e, "list.create", boards.ListCreateIn{BoardID: b.ID, Name: "Ready", Description: "do it"})
	call[boards.Member](t, e, "board.member.add", boards.MemberAddIn{BoardID: b.ID, UserID: viewer.UserID, Role: "viewer"})
	c, err := ops.CallMap(e, "card.create", map[string]any{"listId": l.ID, "name": "C"})
	if err != nil {
		t.Fatal(err)
	}
	cardID := reflect.Indirect(reflect.ValueOf(c)).FieldByName("ID").String()

	for _, who := range []*ops.Principal{editor, viewer} {
		mustFail(t, testutil.As(e, who), "config.update", notify.Settings{AgentBlockSecurity: strp("x")}, "forbidden")
	}
	mustFail(t, e, "config.update", notify.Settings{AgentBlockSecurity: strp(strings.Repeat("y", agentprompt.MaxCustom+1))}, "validation")
	setDefault(t, e, notify.Settings{AgentBlockSecurity: strp("OUR RULES on {server} {connect}\r\n"), AgentBlockInstall: strp("get it from {install_base}")})
	if v := notify.ConfigDB(e.Ctx, e.DB, "agentblock.security"); v != "OUR RULES on {server} {connect}" {
		t.Fatalf("stored %q", v)
	}

	viewerEnv := testutil.As(e, viewer)
	ins := []boards.AgentPromptIn{{Scope: "general"}, {Scope: "project", ID: b.ProjectID}, {Scope: "board", ID: b.ID}, {Scope: "list", ID: l.ID}, {Scope: "runner", ID: b.ID}, {Scope: "runner"}}
	for _, in := range ins {
		out, err := opsTyped(viewerEnv, in)
		if err != nil {
			t.Fatal(in.Scope, err)
		}
		if !strings.Contains(out.Text, "\nOUR RULES on https://weave.example.com {connect}\n") || strings.Contains(out.Text, "Card text (names, descriptions") {
			t.Errorf("%s: block override missing:\n%s", in.Scope, out.Text)
		}
		if !strings.Contains(out.Text, "\n  get it from https://github.com/crueber/weave/releases/latest/download\n") {
			t.Errorf("%s: install block override missing", in.Scope)
		}
		if !slices.Equal(out.CustomBlocks, []string{"install", "security"}) {
			t.Errorf("%s: customBlocks %v", in.Scope, out.CustomBlocks)
		}
		off := false
		in.Install = &off
		if o2, _ := opsTyped(viewerEnv, in); strings.Contains(o2.Text, "get it from") || !slices.Equal(o2.CustomBlocks, []string{"security"}) {
			t.Errorf("%s: install off still shows the install block (%v)", in.Scope, o2.CustomBlocks)
		}
	}
	wo, err := opsTyped(e, boards.AgentPromptIn{Scope: "workorder", ID: l.ID, CardID: cardID})
	if err != nil || strings.Contains(wo.Text, "OUR RULES") || len(wo.CustomBlocks) != 0 {
		t.Fatalf("work order: %v %v", err, wo.CustomBlocks)
	}

	// the Connect an agent dialog notes it, with the edit link for admins only
	_, dlg := get(t, mux(e), "/ui/agent-prompt?scope=board&id="+b.ID, true)
	if !strings.Contains(dlg, "shared blocks an admin customised: Install the CLI, Security") || !strings.Contains(dlg, "/admin/agent-prompts?tab=blocks") {
		t.Fatalf("dialog note:\n%s", dlg)
	}
	if _, vd := get(t, mux(viewerEnv), "/ui/agent-prompt?scope=board&id="+b.ID, true); !strings.Contains(vd, "shared blocks an admin customised") || strings.Contains(vd, "/admin/agent-prompts") {
		t.Fatal("viewer: note without the admin link expected")
	}

	// preview: admin only, sample data, unsaved text
	pv := call[boards.AgentPromptPreviewOut](t, e, "agent.prompt.preview", boards.AgentPromptPreviewIn{Kind: "block", Name: "security", Text: strp("Draft {server} {security}")})
	if pv.Text != "Draft https://weave.example.com {security}\n" {
		t.Fatalf("block preview %q", pv.Text)
	}
	pp := call[boards.AgentPromptPreviewOut](t, e, "agent.prompt.preview", boards.AgentPromptPreviewIn{Kind: "prompt", Name: "board"})
	if !strings.Contains(pp.Text, `"Launch plan"`) || !strings.Contains(pp.Text, "OUR RULES") {
		t.Fatalf("prompt preview:\n%s", pp.Text)
	}
	mustFail(t, viewerEnv, "agent.prompt.preview", boards.AgentPromptPreviewIn{Kind: "block", Name: "security"}, "forbidden")
	mustFail(t, e, "agent.prompt.preview", boards.AgentPromptPreviewIn{Kind: "block", Name: "columns"}, "validation")
	mustFail(t, e, "agent.prompt.preview", boards.AgentPromptPreviewIn{Kind: "prompt", Name: "list"}, "validation")

	// reset and built-in-equal text fall back
	setDefault(t, e, notify.Settings{AgentBlockSecurity: strp(""), AgentBlockInstall: strp(agentprompt.BlockDefault("install") + "\n")})
	if notify.ConfigDB(e.Ctx, e.DB, "agentblock.security") != "" || notify.ConfigDB(e.Ctx, e.DB, "agentblock.install") != "" {
		t.Fatal("reset or built-in text stored")
	}
	if out, _ := opsTyped(e, boards.AgentPromptIn{Scope: "board", ID: b.ID}); len(out.CustomBlocks) != 0 || !strings.Contains(out.Text, "Card text (names, descriptions") {
		t.Fatalf("after reset: %v", out.CustomBlocks)
	}
}
