package boards

import (
	"reflect"
	"strings"

	"github.com/crueber/weave/internal/agentprompt"
	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// APTab is one tab of the "Connect an agent" dialog.
type APTab struct {
	Label, Scope, ID string
	Active           bool
}

// APData feeds the dialog fragment (boards_agentprompt.html).
type APData struct {
	Out       AgentPromptOut
	Scope, ID string
	Tabs      []APTab
	NoInstall bool
	Compact   bool
	IsAdmin   bool
	BaseURL   string
	// Context carried across tabs (project, board, column ids the dialog was opened from).
	PID, BID, LID string
	// A customised prompt: who customised it, and where the reader may edit it ("" = not permitted).
	CustomBy, EditURL string
	// Shared blocks of this prompt that an admin customised (titles, comma separated), and where admins edit them.
	CustomBlocks, BlocksURL string
	// Board and Runner tabs, for someone who manages the board: where agent accounts for it are created, and how
	// many agent accounts already have access ("Create an agent for this board" shows when there is none).
	AgentsURL  string
	AgentCount int
}

func init() {
	// "Connect an agent": one dialog, five tabs (About Weave, Project, Board, Column, Runner); only the ones
	// that apply to where it was opened. The text is rendered by the agent.prompt op (single source of truth);
	// the toggles just refetch with query parameters, nothing is templated in the browser.
	web.Route("GET /ui/agent-prompt", func(c *web.Ctx) error {
		q := c.R.URL.Query()
		in := AgentPromptIn{Scope: q.Get("scope"), ID: q.Get("id"), Compact: q.Get("compact") != ""}
		if in.Scope == "" {
			in.Scope = agentprompt.ScopeGeneral
		}
		noInstall := q.Get("noinstall") != ""
		if noInstall {
			off := false
			in.Install = &off
		}
		out, err := ops.Typed[AgentPromptOut](c.Env, "agent.prompt", in)
		if err != nil {
			return err
		}
		pc, err := promptChain(c.Env, in.Scope, in.ID)
		if err != nil {
			return err
		}
		// The tabs of the context the dialog was opened from survive switching to a tab that carries no context of its
		// own (About, Runner without a board): the ids ride along as pid/bid/lid and are re-checked by promptChain.
		for _, x := range []struct{ param, scope string }{{"lid", agentprompt.ScopeList}, {"bid", agentprompt.ScopeBoard}, {"pid", agentprompt.ScopeProject}} {
			if id := q.Get(x.param); id != "" {
				if o, err := promptChain(c.Env, x.scope, id); err == nil {
					if pc.ListID == "" {
						pc.ListID = o.ListID
					}
					if pc.BoardID == "" {
						pc.BoardID = o.BoardID
					}
					if pc.ProjectID == "" {
						pc.ProjectID = o.ProjectID
					}
				}
			}
		}
		d := APData{Out: out, PID: pc.ProjectID, BID: pc.BoardID, LID: pc.ListID, Scope: in.Scope, ID: in.ID, NoInstall: noInstall, Compact: in.Compact, BaseURL: agentBaseURL(c.Env),
			IsAdmin: authz.SystemAdmin(c.Env)}
		add := func(label, scope, id string) {
			d.Tabs = append(d.Tabs, APTab{Label: label, Scope: scope, ID: id, Active: scope == in.Scope})
		}
		switch out.Source {
		case agentprompt.ScopeBoard:
			d.CustomBy = "this board's owners"
			if authz.Board(c.Env, pc.BoardID).CanEditPrompt {
				d.EditURL = "/boards/" + pc.BoardID + "/settings?section=agents"
			}
		case agentprompt.ScopeProject:
			d.CustomBy = "this project's managers"
			if authz.Project(c.Env, pc.ProjectID).CanManage {
				d.EditURL = "/projects/" + pc.ProjectID + "/settings?section=agents"
			}
		case SourceAdmin:
			d.CustomBy = "an admin"
			if d.IsAdmin {
				d.EditURL = "/admin/agent-prompts?tab=prompts&open=" + in.Scope + "#ap-" + in.Scope
			}
		}
		if len(out.CustomBlocks) > 0 {
			var titles []string
			for _, n := range out.CustomBlocks {
				if b, ok := agentprompt.Block(n); ok {
					titles = append(titles, b.Title)
				}
			}
			d.CustomBlocks = strings.Join(titles, ", ")
			if d.IsAdmin {
				d.BlocksURL = "/admin/agent-prompts?tab=blocks&open=" + out.CustomBlocks[0] + "#ab-" + out.CustomBlocks[0]
			}
		}
		if pc.BoardID != "" && (in.Scope == agentprompt.ScopeBoard || in.Scope == agentprompt.ScopeRunner) && authz.Board(c.Env, pc.BoardID).CanManageAgents {
			if out, err := ops.CallMap(c.Env, "agent.list", map[string]any{"boardId": pc.BoardID}); err == nil {
				if v := reflect.ValueOf(out); v.Kind() == reflect.Slice {
					d.AgentCount = v.Len()
				}
				d.AgentsURL = "/boards/" + pc.BoardID + "/settings?section=agents#set-agents"
			}
		}
		add("About Weave", agentprompt.ScopeGeneral, "")
		if pc.ProjectID != "" {
			add("Project", agentprompt.ScopeProject, pc.ProjectID)
		}
		if pc.BoardID != "" {
			add("Board", agentprompt.ScopeBoard, pc.BoardID)
		}
		if pc.ListID != "" {
			add("Column", agentprompt.ScopeList, pc.ListID)
		}
		add("Runner", agentprompt.ScopeRunner, pc.BoardID)
		return c.RenderBlock("boards_agentprompt.html", "boards_agentprompt", d)
	})
}
