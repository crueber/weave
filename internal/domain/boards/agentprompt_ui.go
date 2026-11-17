package boards

import (
	"github.com/crueber/weave/internal/agentprompt"
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
			IsAdmin: c.Env.User != nil && c.Env.User.Role == "admin"}
		add := func(label, scope, id string) {
			d.Tabs = append(d.Tabs, APTab{Label: label, Scope: scope, ID: id, Active: scope == in.Scope})
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
