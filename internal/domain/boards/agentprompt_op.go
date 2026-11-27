package boards

import (
	"strings"
	"unicode/utf8"

	"github.com/crueber/weave/internal/agentprompt"
	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/domain/notify"
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
	Scope  string            `json:"scope"`
	Title  string            `json:"title"`
	Text   string            `json:"text" desc:"the prompt, plain text; never contains an API key (it references $WEAVE_TOKEN)"`
	Vars   map[string]string `json:"vars" desc:"the ids and names the prompt was built from"`
	Source string            `json:"source" desc:"where the text came from: builtin (Weave's own), admin (the admin default), project or board (that object's own text); column and work-order prompts are always builtin"`
	// CustomBlocks names the shared blocks this prompt uses whose text an admin replaced.
	CustomBlocks []string `json:"customBlocks,omitempty" desc:"shared blocks used by this prompt whose text an admin customised (instance, connect, install, install_skill, keynote, concepts, questionnaire, etiquette, etiquette_compact, security, readmore)"`
}

// Prompt text sources (AgentPromptOut.Source).
const (
	SourceBuiltin = "builtin"
	SourceAdmin   = "admin"
)

// promptText resolves the text of an editable scope: the object's own text (scope project or board), else the
// admin default, else the built-in text (""). There is no board -> project fallback.
func promptText(e *ops.Env, scope, own string) (text, source string) {
	if strings.TrimSpace(own) != "" {
		return own, scope
	}
	if t, admin := notify.AgentPromptDefault(e, scope); admin {
		return t, SourceAdmin
	}
	return "", SourceBuiltin
}

func init() {
	ops.Register(ops.Op{Name: "agent.prompt", Summary: "Render the copy-paste prompt that connects an AI agent to Weave (scope general, project, board, list, runner or workorder); no key is ever included",
		Method: "GET", Path: "/agent/prompt", UI: "GET /ui/agent-prompt", Tag: "agent"}, agentPromptOp)
	ops.Register(ops.Op{Name: "agent.prompt.preview", Summary: "Admin: render an editable agent prompt or a shared block with sample data (the saved text, or text you pass), to check a customisation before or after saving it",
		Method: "POST", Path: "/agent/prompt/preview", Auth: ops.AuthAdmin, UI: "POST /ui/admin/agent-prompts/preview", Tag: "agent"}, agentPromptPreviewOp)
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

// promptBoardFrom builds an agentprompt.Board from an already-loaded Board (caller has checked View).
func promptBoardFrom(e *ops.Env, b Board) (agentprompt.Board, error) {
	lists, err := listsOf(e, b.ID, true)
	if err != nil {
		return agentprompt.Board{}, err
	}
	pb := agentprompt.Board{ID: b.ID, Name: b.Name}
	for _, l := range lists {
		pb.Columns = append(pb.Columns, promptColumn(l))
	}
	return pb, nil
}

// PromptContext is the chain of ids the dialog needs to decide which tabs to show.
type PromptContext struct{ ProjectID, BoardID, ListID string }

// resolvePromptChain resolves scope/id to its ids and already-loaded Board/List, authorizing View on the way.
func resolvePromptChain(e *ops.Env, scope, id string) (pc PromptContext, board Board, list List, err error) {
	switch scope {
	case agentprompt.ScopeProject:
		if id == "" {
			return pc, board, list, ops.Invalid("id (project) is required")
		}
		if err := authz.RequireProject(e, id, authz.View); err != nil {
			return pc, board, list, err
		}
		pc.ProjectID = id
	case agentprompt.ScopeBoard, agentprompt.ScopeRunner:
		if id == "" {
			return pc, board, list, nil
		}
		if err := authz.RequireBoard(e, id, authz.View); err != nil {
			return pc, board, list, err
		}
		board, err = GetBoard(e.Ctx, e.DB, id)
		if err != nil {
			return pc, board, list, err
		}
		pc.BoardID, pc.ProjectID = board.ID, board.ProjectID
	case agentprompt.ScopeList, agentprompt.ScopeWorkOrder:
		if id == "" {
			return pc, board, list, ops.Invalid("id (list) is required")
		}
		list, err = requireList(e, id, authz.View)
		if err != nil {
			return pc, board, list, err
		}
		board, err = GetBoard(e.Ctx, e.DB, list.BoardID)
		if err != nil {
			return pc, board, list, err
		}
		pc = PromptContext{ProjectID: board.ProjectID, BoardID: board.ID, ListID: list.ID}
	}
	return pc, board, list, nil
}

// promptChain resolves scope/id to its project, board and list ids, authorizing View on the way.
func promptChain(e *ops.Env, scope, id string) (PromptContext, error) {
	pc, _, _, err := resolvePromptChain(e, scope, id)
	return pc, err
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
	d := agentprompt.Data{Scope: in.Scope, BaseURL: agentBaseURL(e), InstallBase: agentInstallBase(e), Compact: in.Compact, Blocks: notify.AgentBlocks(e)}
	if in.Install != nil && !*in.Install {
		d.NoInstall = true
	}
	vars := map[string]string{"baseUrl": agentBaseURL(e)}
	source := SourceBuiltin
	switch in.Scope {
	case agentprompt.ScopeGeneral:
		// instance text (admin default or built-in): no object data
		d.Custom, source = promptText(e, in.Scope, "")
	case agentprompt.ScopeProject:
		pc, _, _, err := resolvePromptChain(e, in.Scope, in.ID)
		if err != nil {
			return AgentPromptOut{}, err
		}
		var own string
		if err := e.DB.QueryRowContext(e.Ctx, `SELECT name,COALESCE(agent_prompt,'') FROM projects WHERE id=?`, pc.ProjectID).Scan(&d.ProjectName, &own); err != nil {
			return AgentPromptOut{}, ops.NotFound("project")
		}
		d.Custom, source = promptText(e, in.Scope, own)
		d.ProjectID = pc.ProjectID
		bs, err := ops.Typed[[]Board](e, "board.list", BoardListIn{ProjectID: pc.ProjectID})
		if err != nil {
			return AgentPromptOut{}, err
		}
		if len(bs) > 0 {
			byBoard := map[string][]agentprompt.Column{}
			for i := 0; i < len(bs); i += archiveChunk {
				chunk := bs[i:min(i+archiveChunk, len(bs))]
				inClause := "?" + strings.Repeat(",?", len(chunk)-1)
				args := make([]any, len(chunk))
				for k, b := range chunk {
					args[k] = b.ID
				}
				rows, err := e.DB.QueryContext(e.Ctx, `SELECT `+listCols+` FROM lists WHERE board_id IN (`+inClause+`) AND type IN ('active','closed') ORDER BY position IS NULL, position, id`, args...)
				if err != nil {
					return AgentPromptOut{}, err
				}
				for rows.Next() {
					l, err := scanList(rows)
					if err != nil {
						rows.Close()
						return AgentPromptOut{}, err
					}
					byBoard[l.BoardID] = append(byBoard[l.BoardID], promptColumn(l))
				}
				if err := rows.Close(); err != nil {
					return AgentPromptOut{}, err
				}
				if err := rows.Err(); err != nil {
					return AgentPromptOut{}, err
				}
			}
			for _, b := range bs { // board.list only returns boards the caller can see
				d.Boards = append(d.Boards, agentprompt.Board{ID: b.ID, Name: b.Name, Columns: byBoard[b.ID]})
			}
		}
		vars["projectId"], vars["project"] = d.ProjectID, d.ProjectName
	case agentprompt.ScopeBoard:
		if in.ID == "" {
			return AgentPromptOut{}, ops.Invalid("id (board) is required")
		}
		_, b, _, err := resolvePromptChain(e, in.Scope, in.ID)
		if err != nil {
			return AgentPromptOut{}, err
		}
		pb, err := promptBoardFrom(e, b)
		if err != nil {
			return AgentPromptOut{}, err
		}
		d.Board = pb
		d.Custom, source = promptText(e, in.Scope, b.AgentPrompt)
		vars["boardId"], vars["board"] = pb.ID, pb.Name
	case agentprompt.ScopeRunner:
		_, b, _, err := resolvePromptChain(e, in.Scope, in.ID)
		if err != nil {
			return AgentPromptOut{}, err
		}
		d.Custom, source = promptText(e, in.Scope, "") // admin default only; no per-board runner text
		if in.ID != "" {
			pb, err := promptBoardFrom(e, b)
			if err != nil {
				return AgentPromptOut{}, err
			}
			d.Board, d.HasBoard = pb, true
			vars["boardId"], vars["board"] = pb.ID, pb.Name
		}
	case agentprompt.ScopeList, agentprompt.ScopeWorkOrder:
		pc, b, l, err := resolvePromptChain(e, in.Scope, in.ID)
		if err != nil {
			return AgentPromptOut{}, err
		}
		pb, err := promptBoardFrom(e, b)
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
		if err := authz.RequireCardOn(e, in.CardID, pc.BoardID, authz.View); err != nil {
			return AgentPromptOut{}, err
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
	out := AgentPromptOut{Scope: in.Scope, Title: agentprompt.Title(d), Text: text, Vars: vars, Source: source}
	for _, n := range agentprompt.BlocksUsed(d) {
		if d.Blocks[n] != "" {
			out.CustomBlocks = append(out.CustomBlocks, n)
		}
	}
	return out, nil
}

type AgentPromptPreviewIn struct {
	Kind    string  `json:"kind" required:"true" desc:"prompt | block"`
	Name    string  `json:"name" required:"true" desc:"the prompt scope (general, project, board, runner) or the block name (instance, connect, install, install_skill, keynote, concepts, questionnaire, etiquette, etiquette_compact, security, readmore)"`
	Text    *string `json:"text" desc:"text to preview instead of the saved one (plain text with {tokens}, max 20000 characters); absent or empty = the text that applies now"`
	Compact bool    `json:"compact" desc:"prompt previews: render the compact variant"`
}

type AgentPromptPreviewOut struct {
	Kind   string `json:"kind"`
	Name   string `json:"name"`
	Text   string `json:"text" desc:"the rendered text"`
	Sample string `json:"sample" desc:"what the sample data is"`
}

// agentPromptPreviewOp renders a prompt or block with the example data of the generated skill (project "Product",
// board "Launch plan"), this server's URLs and the saved block texts, so an admin sees the effect of a text.
func agentPromptPreviewOp(e *ops.Env, in AgentPromptPreviewIn) (AgentPromptPreviewOut, error) {
	text := ""
	if in.Text != nil {
		t, err := agentprompt.Normalize(*in.Text, "")
		if err != nil {
			return AgentPromptPreviewOut{}, ops.Invalid("text: " + err.Error())
		}
		text = t
	}
	out := AgentPromptPreviewOut{Kind: in.Kind, Name: in.Name,
		Sample: `sample data: project "Product", board "Launch plan" with four columns; this server's URLs and the saved shared blocks`}
	sample := func(scope string) agentprompt.Data {
		for _, d := range agentprompt.Examples() {
			if d.Scope == scope {
				d.BaseURL, d.InstallBase, d.Blocks, d.Compact = agentBaseURL(e), agentInstallBase(e), notify.AgentBlocks(e), in.Compact
				return d
			}
		}
		return agentprompt.Data{}
	}
	switch in.Kind {
	case "prompt":
		if !agentprompt.Editable(in.Name) {
			return out, ops.Invalid("name must be one of general, project, board, runner")
		}
		d := sample(in.Name)
		if text == "" {
			text, _ = notify.AgentPromptDefault(e, in.Name)
		}
		d.Custom = text
		r, err := agentprompt.Render(d)
		if err != nil {
			return out, err
		}
		out.Text = r
	case "block":
		if !agentprompt.IsBlock(in.Name) {
			return out, ops.Invalid("unknown block " + in.Name)
		}
		out.Text = agentprompt.RenderBlock(in.Name, sample(agentprompt.ScopeBoard), text)
		out.Sample = "sample data: this server's URLs and the saved shared blocks"
	default:
		return out, ops.Invalid("kind must be prompt or block")
	}
	return out, nil
}

func agentPromptOp(e *ops.Env, in AgentPromptIn) (AgentPromptOut, error) {
	if e.User == nil {
		return AgentPromptOut{}, ops.Forbidden()
	}
	return BuildPrompt(e, in)
}
