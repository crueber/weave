package boards

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/crueber/weave/internal/agentprompt"
	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/ops"
)

// The "Connect an agent" prompts (see internal/agentprompt for the wording). One op serves the browser
// dialog, the REST API and `weave agent prompt`. Prompts never contain a key.

type AgentPromptIn struct {
	Scope    string `query:"scope" required:"true" desc:"general | project | board | list | runner | workorder"`
	ID       string `query:"id" desc:"project id (scope project), board id (board, runner: optional) or list id (list, workorder)"`
	CardID   string `query:"cardId" desc:"workorder only: the card (must be on the list's board)"`
	CardFile string `query:"cardFile" desc:"workorder only: path of the card JSON file the runner wrote (single line, max 512 chars)"`
	Install  *bool  `query:"install" desc:"include the install steps for the CLI and skill (default true)"`
	Compact  bool   `query:"compact" desc:"shorter etiquette section"`
}

type AgentPromptOut struct {
	Scope string            `json:"scope"`
	Title string            `json:"title"`
	Text  string            `json:"text" desc:"the prompt, plain text; never contains an API key (it references $WEAVE_TOKEN)"`
	Vars  map[string]string `json:"vars" desc:"the ids and names the prompt was built from"`
}

func init() {
	ops.Register(ops.Op{Name: "agent.prompt", Summary: "Render the copy-paste prompt that connects an AI agent to Weave (scope general, project, board, list, runner or workorder); no key is ever included",
		Method: "GET", Path: "/agent/prompt", UI: "GET /ui/agent-prompt", Tag: "agent"}, agentPromptOp)
}

// agentInstallBase returns the download base for install.sh/install.ps1 (config or the published
// release page).
func agentInstallBase(e *ops.Env) string {
	base := ""
	if c, ok := e.Svc["config"].(config.Config); ok {
		base = c.AgentInstallURL
	}
	if base == "" {
		base = "https://github.com/crueber/weave/releases/latest/download"
	}
	return strings.TrimRight(base, "/")
}

// agentBaseURL returns the public URL of this server (no trailing slash).
func agentBaseURL(e *ops.Env) string {
	if c, ok := e.Svc["config"].(config.Config); ok {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return ""
}

func listName(l List) string {
	if l.Name == nil || strings.TrimSpace(*l.Name) == "" {
		return l.Type
	}
	return *l.Name
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func promptColumn(l List) agentprompt.Column {
	c := agentprompt.Column{ID: l.ID, Name: listName(l), Brief: deref(l.Description)}
	if l.WIPLimit != nil {
		c.Limit = *l.WIPLimit
	}
	return c
}

// promptBoard loads a board with its finite columns (caller has checked View).
func promptBoard(e *ops.Env, id string) (agentprompt.Board, Board, error) {
	b, err := GetBoard(e.Ctx, e.DB, id)
	if err != nil {
		return agentprompt.Board{}, b, err
	}
	lists, err := listsOf(e, id, true)
	if err != nil {
		return agentprompt.Board{}, b, err
	}
	pb := agentprompt.Board{ID: b.ID, Name: b.Name}
	for _, l := range lists {
		pb.Columns = append(pb.Columns, promptColumn(l))
	}
	return pb, b, nil
}

// PromptContext is the chain of ids the dialog needs to decide which tabs to show.
type PromptContext struct{ ProjectID, BoardID, ListID string }

// promptChain resolves scope/id to its project, board and list ids, authorizing View on the way.
func promptChain(e *ops.Env, scope, id string) (PromptContext, error) {
	var pc PromptContext
	switch scope {
	case agentprompt.ScopeProject:
		if id == "" {
			return pc, ops.Invalid("id (project) is required")
		}
		if err := authz.RequireProject(e, id, authz.View); err != nil {
			return pc, err
		}
		pc.ProjectID = id
	case agentprompt.ScopeBoard, agentprompt.ScopeRunner:
		if id == "" {
			return pc, nil
		}
		if err := authz.RequireBoard(e, id, authz.View); err != nil {
			return pc, err
		}
		b, err := GetBoard(e.Ctx, e.DB, id)
		if err != nil {
			return pc, err
		}
		pc.BoardID, pc.ProjectID = b.ID, b.ProjectID
	case agentprompt.ScopeList, agentprompt.ScopeWorkOrder:
		if id == "" {
			return pc, ops.Invalid("id (list) is required")
		}
		l, err := requireList(e, id, authz.View)
		if err != nil {
			return pc, err
		}
		b, err := GetBoard(e.Ctx, e.DB, l.BoardID)
		if err != nil {
			return pc, err
		}
		pc = PromptContext{ProjectID: b.ProjectID, BoardID: b.ID, ListID: l.ID}
	}
	return pc, nil
}

func validCardFile(s string) error {
	if utf8.RuneCountInString(s) > 512 {
		return ops.Invalid("cardFile is longer than 512 characters")
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return ops.Invalid("cardFile must be a single line without control characters")
		}
	}
	return nil
}

// BuildPrompt authorizes and renders one prompt; shared by the op and the UI dialog.
func BuildPrompt(e *ops.Env, in AgentPromptIn) (AgentPromptOut, error) {
	d := agentprompt.Data{Scope: in.Scope, BaseURL: agentBaseURL(e), InstallBase: agentInstallBase(e), Compact: in.Compact}
	if in.Install != nil && !*in.Install {
		d.NoInstall = true
	}
	vars := map[string]string{"baseUrl": agentBaseURL(e)}
	switch in.Scope {
	case agentprompt.ScopeGeneral:
		// static instance text: no object data
	case agentprompt.ScopeProject:
		pc, err := promptChain(e, in.Scope, in.ID)
		if err != nil {
			return AgentPromptOut{}, err
		}
		if err := e.DB.QueryRowContext(e.Ctx, `SELECT name FROM projects WHERE id=?`, pc.ProjectID).Scan(&d.ProjectName); err != nil {
			return AgentPromptOut{}, ops.NotFound("project")
		}
		d.ProjectID = pc.ProjectID
		bs, err := ops.Typed[[]Board](e, "board.list", BoardListIn{ProjectID: pc.ProjectID})
		if err != nil {
			return AgentPromptOut{}, err
		}
		for _, b := range bs { // board.list only returns boards the caller can see
			pb, _, err := promptBoard(e, b.ID)
			if err != nil {
				return AgentPromptOut{}, err
			}
			d.Boards = append(d.Boards, pb)
		}
		vars["projectId"], vars["project"] = d.ProjectID, d.ProjectName
	case agentprompt.ScopeBoard:
		if in.ID == "" {
			return AgentPromptOut{}, ops.Invalid("id (board) is required")
		}
		if _, err := promptChain(e, in.Scope, in.ID); err != nil {
			return AgentPromptOut{}, err
		}
		pb, _, err := promptBoard(e, in.ID)
		if err != nil {
			return AgentPromptOut{}, err
		}
		d.Board = pb
		vars["boardId"], vars["board"] = pb.ID, pb.Name
	case agentprompt.ScopeRunner:
		if _, err := promptChain(e, in.Scope, in.ID); err != nil {
			return AgentPromptOut{}, err
		}
		if in.ID != "" {
			pb, _, err := promptBoard(e, in.ID)
			if err != nil {
				return AgentPromptOut{}, err
			}
			d.Board, d.HasBoard = pb, true
			vars["boardId"], vars["board"] = pb.ID, pb.Name
		}
	case agentprompt.ScopeList, agentprompt.ScopeWorkOrder:
		pc, err := promptChain(e, in.Scope, in.ID)
		if err != nil {
			return AgentPromptOut{}, err
		}
		pb, _, err := promptBoard(e, pc.BoardID)
		if err != nil {
			return AgentPromptOut{}, err
		}
		l, err := GetList(e.Ctx, e.DB, pc.ListID)
		if err != nil {
			return AgentPromptOut{}, err
		}
		d.List = promptColumn(l)
		vars["boardId"], vars["board"], vars["listId"], vars["list"] = pb.ID, pb.Name, d.List.ID, d.List.Name
		if in.Scope == agentprompt.ScopeList {
			for _, c := range pb.Columns {
				if c.ID != l.ID {
					d.Board.Columns = append(d.Board.Columns, c)
				}
			}
			d.Board.ID, d.Board.Name = pb.ID, pb.Name
			break
		}
		// work order: the card must belong to this board; card text is never read into the prompt
		if in.CardID == "" {
			return AgentPromptOut{}, ops.Invalid("cardId is required for a work order")
		}
		if err := validCardFile(in.CardFile); err != nil {
			return AgentPromptOut{}, err
		}
		got, err := ops.CallMap(e, "card.get", map[string]any{"id": in.CardID})
		if err != nil {
			return AgentPromptOut{}, err
		}
		var card struct {
			BoardID string `json:"boardId"`
		}
		raw, _ := json.Marshal(got)
		if json.Unmarshal(raw, &card) != nil || card.BoardID != pc.BoardID {
			return AgentPromptOut{}, ops.NotFound("card")
		}
		d.CardID = in.CardID
		d.CardFile = in.CardFile
		if d.CardFile == "" {
			d.CardFile = "$WEAVE_CARD_FILE"
		}
		d.BoardFile, d.WorkOrderFile, d.OutDir = "$WEAVE_BOARD_FILE", "$WEAVE_WORKORDER_FILE", "$WEAVE_OUT_DIR"
		vars["cardId"] = in.CardID
	default:
		return AgentPromptOut{}, ops.Invalid("scope must be one of general, project, board, list, runner, workorder")
	}
	text, err := agentprompt.Render(d)
	if err != nil {
		return AgentPromptOut{}, err
	}
	return AgentPromptOut{Scope: in.Scope, Title: agentprompt.Title(d), Text: text, Vars: vars}, nil
}

func agentPromptOp(e *ops.Env, in AgentPromptIn) (AgentPromptOut, error) {
	if e.User == nil {
		return AgentPromptOut{}, ops.Forbidden()
	}
	return BuildPrompt(e, in)
}
