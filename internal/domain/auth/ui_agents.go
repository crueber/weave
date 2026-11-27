package auth

import (
	"strconv"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// Two pages manage agent accounts through the same agent.* ops (which decide who may do what, internal/authz):
// the admin console (/admin/agents: every agent ACCOUNT, for account management; board access shows only for boards
// the admin manages, otherwise just a count) and Board settings > Agents
// (/ui/boards/{boardId}/agents: the agents with access to that board, for its managers; create an agent for it).

type agentBoardOpt struct{ ID, Name string }

type agentsData struct {
	Agents     []AgentOut
	Boards     []agentBoardOpt
	Error      string
	NewKey     string
	NewKeyName string
}

func renderAgents(c *web.Ctx, errm string, key *APIKeyOut, keyName string) error {
	l, err := ops.Typed[[]AgentOut](c.Env, "agent.list", AgentListIn{})
	if err != nil {
		return err
	}
	d := agentsData{Agents: l, Error: errm}
	if key != nil {
		d.NewKey, d.NewKeyName = key.Key, keyName
	}
	// board choices: only the boards the admin manages themself ("Project / Board"); the admin role shows no other
	// board, so this page is account management (name, creator, status, key, how many boards) and nothing more
	uid := c.Env.User.UserID
	if rows, err := c.Env.DB.QueryContext(c.Env.Ctx, `SELECT b.id,p.name||' / '||b.name FROM boards b JOIN projects p ON p.id=b.project_id
		WHERE b.is_template=0 AND (
			b.project_id IN (SELECT project_id FROM project_managers WHERE user_id=?) OR
			b.id IN (SELECT board_id FROM board_memberships WHERE role='owner' AND (user_id=? OR group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?))))
		ORDER BY p.name,b.position,b.name LIMIT 500`, uid, uid, uid); err == nil {
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
		out, err := callAgentOp(c, op, bind)
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

func callAgentOp(c *web.Ctx, op string, bind func(*web.Ctx) (any, error)) (any, error) {
	var in any
	var err error
	if bind != nil {
		in, err = bind(c)
	} else {
		in, err = c.Bind(op)
	}
	if err != nil {
		return nil, err
	}
	return ops.Call(c.Env, op, in)
}

// ---- Board settings > Agents ------------------------------------------------

type boardAgentsData struct {
	BoardID    string
	Agents     []AgentOut // agents with access to this board
	Mine       []AgentOut // agents the caller may manage that have no access here yet (to add)
	Error      string
	Msg        string
	NewKey     string
	NewKeyName string
}

// RoleOn returns the agent's access on one board (for the template).
func (a AgentOut) RoleOn(boardID string) string {
	for _, b := range a.Boards {
		if b.BoardID == boardID {
			return b.Role
		}
	}
	return ""
}

// Hidden counts the boards the agent has access to that the caller does not manage (shown as a number only).
func (a AgentOut) Hidden() int { return a.BoardCount - len(a.Boards) }

// OtherBoards counts the boards besides boardID that the caller can see this agent on.
func (a AgentOut) OtherBoards(boardID string) int {
	n := 0
	for _, b := range a.Boards {
		if b.BoardID != boardID {
			n++
		}
	}
	return n
}

func atoiOr0(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func renderBoardAgents(c *web.Ctx, boardID string, d boardAgentsData) error {
	if err := noAgents(c.Env); err != nil {
		return err
	}
	if err := authz.RequireBoard(c.Env, boardID, authz.Manage); err != nil {
		return err
	}
	all, err := ops.Typed[[]AgentOut](c.Env, "agent.list", AgentListIn{})
	if err != nil {
		return err
	}
	d.BoardID = boardID
	d.Agents = []AgentOut{}
	for _, a := range all {
		if a.RoleOn(boardID) != "" {
			d.Agents = append(d.Agents, a)
		} else if a.CanManage && !a.IsDeactivated {
			d.Mine = append(d.Mine, a)
		}
	}
	return c.RenderBlock("auth_board_agents.html", "auth_board_agents", d)
}

// boardAgentsAction runs an agent op from the board fragment and re-renders it (a new key is shown once).
func boardAgentsAction(op string, board func(*web.Ctx) string, bind func(*web.Ctx) (any, error), done string) func(*web.Ctx) error {
	return func(c *web.Ctx) error {
		_ = c.R.ParseForm()
		b := board(c)
		out, err := callAgentOp(c, op, bind)
		if err != nil {
			m, err2 := errMsg(err)
			if err2 != nil {
				return err2
			}
			return renderBoardAgents(c, b, boardAgentsData{Error: m})
		}
		d := boardAgentsData{Msg: done}
		if a, ok := out.(AgentOut); ok && a.Key != nil {
			d.NewKey, d.NewKeyName, d.Msg = a.Key.Key, a.Name, ""
		}
		return renderBoardAgents(c, b, d)
	}
}

func init() {
	web.Route("GET /admin/agents", adminOnly(func(c *web.Ctx) error { return renderAgents(c, "", nil, "") }))
	web.Route("POST /ui/admin/agents", agentsAction("agent.create", func(c *web.Ctx) (any, error) {
		_ = c.R.ParseForm()
		f := c.R.Form
		// repeated checkbox values cannot go through the generic binder
		return &AgentCreateIn{Name: f.Get("name"), Role: f.Get("role"), BoardRole: f.Get("boardRole"), BoardIDs: f["boardIds"], ExpiresInDays: atoiOr0(f.Get("expiresInDays"))}, nil
	}))
	web.Route("POST /ui/admin/agents/{id}/update", agentsAction("agent.update", nil))
	web.Route("POST /ui/admin/agents/{id}/key", agentsAction("agent.rotateKey", nil))
	web.Route("POST /ui/admin/agents/{id}/delete", agentsAction("agent.delete", nil))
	web.Route("POST /ui/admin/agents/{id}/access", agentsAction("agent.access", nil))

	pathBoard := func(c *web.Ctx) string { return c.R.PathValue("boardId") }
	formBoard := func(c *web.Ctx) string { return c.R.Form.Get("_board") }
	web.Route("GET /ui/boards/{boardId}/agents", func(c *web.Ctx) error { return renderBoardAgents(c, c.R.PathValue("boardId"), boardAgentsData{}) })
	web.Route("POST /ui/boards/{boardId}/agents", boardAgentsAction("agent.create", pathBoard, func(c *web.Ctx) (any, error) {
		f := c.R.Form
		return &AgentCreateIn{Name: f.Get("name"), BoardRole: f.Get("boardRole"), BoardIDs: []string{c.R.PathValue("boardId")}, ExpiresInDays: atoiOr0(f.Get("expiresInDays"))}, nil
	}, ""))
	web.Route("POST /ui/boards/{boardId}/agents/grant", boardAgentsAction("agent.access", pathBoard, func(c *web.Ctx) (any, error) {
		f := c.R.Form
		return &AgentAccessIn{ID: f.Get("id"), BoardID: c.R.PathValue("boardId"), Role: f.Get("role")}, nil
	}, "Access granted."))
	web.Route("POST /ui/agents/{id}/update", boardAgentsAction("agent.update", formBoard, nil, "Agent updated."))
	web.Route("POST /ui/agents/{id}/key", boardAgentsAction("agent.rotateKey", formBoard, nil, ""))
	web.Route("POST /ui/agents/{id}/delete", boardAgentsAction("agent.delete", formBoard, nil, "Agent deleted."))
	web.Route("POST /ui/agents/{id}/access", boardAgentsAction("agent.access", formBoard, nil, "Access updated."))
}
