// Package agentprompt renders the copy-paste prompts Weave hands to AI agents (general, project, board,
// column, runner and work-order scopes). It is a leaf package: plain data in, text out, no database, no
// server. The same texts feed the UI dialog, the `agent.prompt` op, the CLI and the generated skill
// (references/prompts.md), so there is exactly one source of truth for the wording.
//
// The general, project, board and runner prompts are plain text with {token}s (defaults/*.txt, see custom.go)
// and can be customised, and so can the shared blocks they are built from (blocks/*.txt, see blocks.go). The
// column prompt and the work order are Go templates (templates/*.tmpl) that only ever see Data and the
// pre-rendered block strings, never run user-written text as a template.
package agentprompt

import (
	"embed"
	"fmt"
	"regexp"
	"strings"
	"text/template"
	"unicode/utf8"
)

// Scopes understood by Render.
const (
	ScopeGeneral   = "general"
	ScopeProject   = "project"
	ScopeBoard     = "board"
	ScopeList      = "list"
	ScopeRunner    = "runner"
	ScopeWorkOrder = "workorder"
)

// Scopes lists every scope in display order.
var Scopes = []string{ScopeGeneral, ScopeProject, ScopeBoard, ScopeList, ScopeRunner, ScopeWorkOrder}

// BoardBriefMax is how much of a column description the board prompt keeps (the column prompt keeps all).
const BoardBriefMax = 600

// ListBriefMax mirrors the 2000 character column description limit.
const ListBriefMax = 2000

// Column is one finite column of a board.
type Column struct {
	ID    string
	Name  string
	Brief string // the description, raw
	Limit int    // soft WIP limit, 0 = none
}

// Board is a board and its finite columns in order.
type Board struct {
	ID      string
	Name    string
	Columns []Column
}

// Data is everything the templates may use. Callers fill what the scope needs.
type Data struct {
	Scope       string
	BaseURL     string // public URL of the server, no trailing slash needed
	InstallBase string // download base for install.sh
	NoInstall   bool   // leave out the "no CLI yet" install steps
	Compact     bool   // shorter etiquette, no concepts section
	Custom      string // editable scopes: the custom text (resolved by the caller); "" = Default(Scope)
	// Blocks holds admin overrides of the shared blocks by block name ("" or absent = BlockDefault(name)).
	Blocks map[string]string

	ProjectName string
	ProjectID   string
	Boards      []Board // project scope: the boards the caller can see
	Board       Board   // board / list / runner scope
	HasBoard    bool    // runner scope: a board was named
	List        Column  // list scope: the target column (also the work order's column)

	// work order (paths only; never card text)
	CardID        string
	CardFile      string
	BoardFile     string
	WorkOrderFile string
	OutDir        string
}

// Base returns the server URL without a trailing slash, or a placeholder.
func (d Data) Base() string {
	b := strings.TrimRight(d.BaseURL, "/")
	if b == "" {
		return "(your Weave server URL)"
	}
	return b
}

// Install returns the install base URL.
func (d Data) Install() string {
	if s := strings.TrimRight(d.InstallBase, "/"); s != "" {
		return s
	}
	return "https://github.com/crueber/weave/releases/latest/download"
}

//go:embed templates/*.tmpl
var tmplFS embed.FS

var tmpl = template.Must(template.New("agentprompt").Funcs(template.FuncMap{
	"q":       Quote,
	"oneline": OneLine,
	"brief":   Brief,
	"fenced":  FencedBrief,
	"columns": ColumnsBlock,
	"cols":    ColumnNames,
}).ParseFS(tmplFS, "templates/*.tmpl"))

// Render renders the prompt for d.Scope.
func Render(d Data) (string, error) {
	switch d.Scope {
	case ScopeGeneral, ScopeProject, ScopeBoard, ScopeRunner:
		return renderEditable(d)
	case ScopeList, ScopeWorkOrder:
	default:
		return "", fmt.Errorf("unknown prompt scope %q", d.Scope)
	}
	var sb strings.Builder
	if err := tmpl.ExecuteTemplate(&sb, d.Scope, tmplData{Data: d, Block: promptBlocks(d)}); err != nil {
		return "", err
	}
	return sb.String(), nil
}

// tmplData is what list.tmpl and workorder.tmpl see: Data plus the rendered blocks ({{.Block.connect}}).
type tmplData struct {
	Data
	Block map[string]string
}

// Title is the dialog/tab title of a rendered prompt.
func Title(d Data) string {
	switch d.Scope {
	case ScopeGeneral:
		return "About Weave"
	case ScopeProject:
		return "Project " + d.ProjectName
	case ScopeBoard:
		return "Board " + d.Board.Name
	case ScopeList:
		return "Column " + d.List.Name
	case ScopeRunner:
		return "Unattended runner"
	case ScopeWorkOrder:
		return "Work order"
	}
	return d.Scope
}

// Quote renders s as a shell double-quoted argument (prompt text only, never executed here).
func Quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`", "\n", " ", "\r", " ")
	return `"` + r.Replace(s) + `"`
}

// OneLine flattens whitespace to single spaces and removes control characters.
func OneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// Brief is a column description flattened to one line, or the "no description" note.
func Brief(s string) string {
	if s = OneLine(s); s == "" {
		return "(no description set - the column name is the brief)"
	}
	return s
}

var (
	tickRun  = regexp.MustCompile("`{3,}")
	tildeRun = regexp.MustCompile("~{3,}")
)

// FencedBrief returns a column description made safe to put inside a fenced block: control characters
// are dropped, backtick and tilde runs of three or more (which could close the fence) are
// neutralised, and the text is cut to max runes with a marker. Every line is indented by indent.
func FencedBrief(s string, max int, indent string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" {
		s = "(no description set - the column name is the brief)"
	}
	cut := false
	if max > 0 && utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max])
		cut = true
	}
	s = tickRun.ReplaceAllStringFunc(s, func(m string) string { return strings.Repeat("'", len(m)) })
	s = tildeRun.ReplaceAllStringFunc(s, func(m string) string { return strings.Repeat("-", len(m)) })
	if cut {
		s += " [...cut]"
	}
	var sb strings.Builder
	sb.WriteString(indent + "```text\n")
	for _, ln := range strings.Split(s, "\n") {
		sb.WriteString(indent + ln + "\n")
	}
	sb.WriteString(indent + "```")
	return sb.String()
}

// ColumnsBlock lists columns with id, WIP limit and the fenced brief (cut to max runes; 0 = all).
func ColumnsBlock(cols []Column, max int) string {
	var sb strings.Builder
	for i, c := range cols {
		if i > 0 {
			sb.WriteString("\n")
		}
		fmt.Fprintf(&sb, "  %d. %s (list id %s)", i+1, Quote(c.Name), c.ID)
		if c.Limit > 0 {
			fmt.Fprintf(&sb, "  WIP limit %d (advisory)", c.Limit)
		}
		sb.WriteString("\n" + FencedBrief(c.Brief, max, "     "))
	}
	return sb.String()
}

// ColumnNames is the comma-joined quoted names, for one-line summaries.
func ColumnNames(cols []Column) string {
	var n []string
	for _, c := range cols {
		n = append(n, Quote(c.Name))
	}
	return strings.Join(n, ", ")
}
