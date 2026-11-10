package auth

import (
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

type agentBoardOpt struct{ ID, Name string }

type agentsData struct {
	Agents     []AgentOut
	Boards     []agentBoardOpt
	Error      string
	NewKey     string
	NewKeyName string
}

func renderAgents(c *web.Ctx, errm string, key *APIKeyOut, keyName string) error {
	l, err := ops.Typed[[]AgentOut](c.Env, "agent.list", NoInput{})
	if err != nil {
		return err
	}
	d := agentsData{Agents: l, Error: errm}
	if key != nil {
		d.NewKey, d.NewKeyName = key.Key, keyName
	}
	// board choices: every board (admins see all), "Project / Board"
	if rows, err := c.Env.DB.QueryContext(c.Env.Ctx, `SELECT b.id,p.name||' / '||b.name FROM boards b JOIN projects p ON p.id=b.project_id WHERE b.is_template=0 ORDER BY p.name,b.position,b.name LIMIT 500`); err == nil {
		for rows.Next() {
			var o agentBoardOpt
			if rows.Scan(&o.ID, &o.Name) == nil {
				d.Boards = append(d.Boards, o)
			}
		}
		rows.Close()
	}
	if c.IsHTMX() && c.R.Method != "GET" {
		return c.RenderBlock("auth_admin_agents.html", "agents-body", d)
	}
	return c.Render("auth_admin_agents.html", d)
}

// agentsAction runs an agent op from a form and re-renders the page; ops returning a
// key (create, rotateKey) show it once.
func agentsAction(op string, bind func(*web.Ctx) (any, error)) func(*web.Ctx) error {
	return adminOnly(func(c *web.Ctx) error {
		var in any
		var err error
		if bind != nil {
			in, err = bind(c)
		} else {
			in, err = c.Bind(op)
		}
		var out any
		if err == nil {
			out, err = ops.Call(c.Env, op, in)
		}
		if err != nil {
			m, err2 := errMsg(err)
			if err2 != nil {
				return err2
			}
			return renderAgents(c, m, nil, "")
		}
		if a, ok := out.(AgentOut); ok && a.Key != nil {
			return renderAgents(c, "", a.Key, a.Name)
		}
		return renderAgents(c, "", nil, "")
	})
}

func init() {
	web.Route("GET /admin/agents", adminOnly(func(c *web.Ctx) error { return renderAgents(c, "", nil, "") }))
	web.Route("POST /ui/admin/agents", agentsAction("agent.create", func(c *web.Ctx) (any, error) {
		_ = c.R.ParseForm()
		f := c.R.Form
		// repeated checkbox values cannot go through the generic binder
		return &AgentCreateIn{Name: f.Get("name"), Role: f.Get("role"), BoardRole: f.Get("boardRole"), BoardIDs: f["boardIds"]}, nil
	}))
	web.Route("POST /ui/admin/agents/{id}/update", agentsAction("agent.update", nil))
	web.Route("POST /ui/admin/agents/{id}/key", agentsAction("agent.rotateKey", nil))
	web.Route("POST /ui/admin/agents/{id}/delete", agentsAction("agent.delete", nil))
	web.Route("POST /ui/admin/agents/{id}/access", agentsAction("agent.access", nil))
}
