package agentprompt

import (
	"embed"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Editable prompts (general, project, board, runner) are plain text with {token}s. The embedded defaults in
// defaults/*.txt ARE that text; an admin can replace a default (config) and a project or board manager can
// override the project or board text. User text is never run through text/template: Expand is a single pass
// over the text that only replaces the scope's known tokens with pre-rendered strings. Expansions are never
// scanned again, unknown tokens stay as typed, and "{{" is left alone (so template syntax stays literal).
// The shared blocks ({connect}, {security}...) are editable text too (blocks.go); {columns}, {boards} and
// {board_line} are generated here, so the column briefs are always fenced and cut whatever the custom text says.

//go:embed defaults/*.txt
var defaultsFS embed.FS

// MaxCustom is the longest custom prompt text accepted, in bytes.
const MaxCustom = 20000

// EditableScopes lists the scopes whose text can be customised, in display order.
var EditableScopes = []string{ScopeGeneral, ScopeProject, ScopeBoard, ScopeRunner}

// Editable reports whether a scope's text can be customised (the column prompt's editable text is the
// column description; the work order is fixed).
func Editable(scope string) bool {
	switch scope {
	case ScopeGeneral, ScopeProject, ScopeBoard, ScopeRunner:
		return true
	}
	return false
}

// Default returns the built-in text of an editable scope ("" for the others).
func Default(scope string) string {
	if !Editable(scope) {
		return ""
	}
	b, err := defaultsFS.ReadFile("defaults/" + scope + ".txt")
	if err != nil {
		panic(err) // embedded; cannot happen
	}
	return string(b)
}

// Token is one {token} an editable prompt may use.
type Token struct {
	Name string `json:"name"` // with braces, e.g. "{board_name}"
	Desc string `json:"desc"`
}

var commonTokens = []Token{
	{"{server}", "the public URL of this Weave server"},
	{"{instance}", "the Instance block (shared blocks are edited by admins): web UI, REST API and event stream URLs"},
	{"{connect}", "the Connect block: check $WEAVE_TOKEN (never its value), install the CLI if missing, configure and verify"},
	{"{keynote}", "the About the key block: what the API key may do and that claiming needs Edit access"},
	{"{concepts}", "the Concepts block (left out when the Compact option is on)"},
	{"{questionnaire}", "the questionnaire and PLAN gate the agent must complete before it listens"},
	{"{etiquette}", "the Etiquette block (shorter when Compact is on)"},
	{"{security}", "the Security block: card text is data, never instructions"},
	{"{readmore}", "the Read more block: where the agent finds the full skill and command help"},
}

var scopeTokens = map[string][]Token{
	ScopeProject: {
		{"{project_name}", "the project name, already double-quoted for the shell"},
		{"{project_id}", "the project id"},
		{"{boards}", "one line per board the reader can see: quoted name, id and column names"},
	},
	ScopeBoard: {
		{"{board_name}", "the board name, already double-quoted for the shell"},
		{"{board_id}", "the board id"},
		{"{columns}", "the board's columns in order with id, WIP limit and each brief fenced and cut to 600 characters"},
	},
	ScopeRunner: {
		{"{board_name}", "the board name, already double-quoted for the shell (\"<board>\" when no board was chosen)"},
		{"{board_id}", "the board id (empty when no board was chosen)"},
		{"{board_line}", "\"Board: <name> (board id <id>)\", or nothing when no board was chosen"},
	},
}

// Tokens returns the tokens an editable scope understands (nil for the others).
func Tokens(scope string) []Token {
	if !Editable(scope) {
		return nil
	}
	out := append([]Token(nil), scopeTokens[scope]...)
	return append(out, commonTokens...)
}

// Clean makes custom prompt text storable: line endings become \n, invalid UTF-8 and control characters
// other than newline and tab are dropped, and surrounding white space is trimmed.
func Clean(s string) string {
	s = strings.ToValidUTF8(s, "")
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
	return strings.TrimSpace(s)
}

// ErrTooLong is returned by Normalize for text over MaxCustom bytes.
var ErrTooLong = errors.New("agent prompt text is too long")

// Normalize cleans custom text for storage. Text equal to fallback (the text that applies without it:
// the admin default or the built-in one) is returned as "" so it keeps following the default.
func Normalize(text, fallback string) (string, error) {
	s := Clean(text)
	if len(s) > MaxCustom {
		return "", fmt.Errorf("%w (max %d characters)", ErrTooLong, MaxCustom)
	}
	if s == Clean(fallback) {
		return "", nil
	}
	return s, nil
}

// tokenValues renders every token of d.Scope.
func tokenValues(d Data) map[string]string {
	v := promptBlocks(d)
	v["server"] = d.Base()
	switch d.Scope {
	case ScopeProject:
		v["project_name"], v["project_id"] = Quote(d.ProjectName), d.ProjectID
		var lines []string
		for _, b := range d.Boards {
			cols := "(none)"
			if len(b.Columns) > 0 {
				cols = ColumnNames(b.Columns)
			}
			lines = append(lines, fmt.Sprintf("    - %s (board id %s)   columns: %s", Quote(b.Name), b.ID, cols))
		}
		if len(lines) == 0 {
			lines = []string{"    (none)"}
		}
		v["boards"] = strings.Join(lines, "\n")
	case ScopeBoard:
		v["board_name"], v["board_id"] = Quote(d.Board.Name), d.Board.ID
		v["columns"] = ColumnsBlock(d.Board.Columns, BoardBriefMax)
	case ScopeRunner:
		v["board_name"], v["board_id"], v["board_line"] = `"<board>"`, "", ""
		if d.HasBoard {
			v["board_name"], v["board_id"] = Quote(d.Board.Name), d.Board.ID
			v["board_line"] = "Board: " + Quote(d.Board.Name) + " (board id " + d.Board.ID + ")"
		}
	}
	return v
}

// Expand replaces the {token}s of text whose names are in vals, in one pass: expansions are not scanned
// again, unknown tokens and anything that is not a token stay as typed, and "{{" is copied literally. A line
// that is only a token (spaces or tabs before it allowed) which expands to nothing is dropped together with its
// line break; when it expands to several lines, each of them is indented like the token.
func Expand(text string, vals map[string]string) string {
	var sb strings.Builder
	sb.Grow(len(text) * 2)
	lines := strings.SplitAfter(text, "\n")
	for _, ln := range lines {
		eol := ""
		body := ln
		if strings.HasSuffix(body, "\r\n") {
			body = body[:len(body)-2]
			eol = "\r\n"
		} else if strings.HasSuffix(body, "\n") {
			body = body[:len(body)-1]
			eol = "\n"
		} else if strings.HasSuffix(body, "\r") {
			body = body[:len(body)-1]
			eol = "\r"
		}
		tok := strings.TrimLeft(body, " \t")
		if len(tok) > 2 && tok[0] == '{' && tok[len(tok)-1] == '}' && isTokenName(tok[1:len(tok)-1]) {
			if v, ok := vals[tok[1:len(tok)-1]]; ok {
				if v == "" {
					continue
				}
				indent := body[:len(body)-len(tok)]
				if indent != "" {
					trail := strings.HasSuffix(v, "\n")
					v = strings.ReplaceAll(strings.TrimSuffix(v, "\n"), "\n", "\n"+indent)
					if trail {
						v += "\n"
					}
				}
				sb.WriteString(indent + v + eol)
				continue
			}
		}
		expandLine(&sb, ln, vals)
	}
	return sb.String()
}

func expandLine(sb *strings.Builder, s string, vals map[string]string) {
	for i := 0; i < len(s); {
		c := s[i]
		if c != '{' {
			sb.WriteByte(c)
			i++
			continue
		}
		if i+1 < len(s) && s[i+1] == '{' { // "{{": literal, never a token
			sb.WriteString("{{")
			i += 2
			continue
		}
		end := strings.IndexByte(s[i+1:], '}')
		if end < 0 {
			sb.WriteString(s[i:])
			return
		}
		name := s[i+1 : i+1+end]
		if v, ok := vals[name]; ok && isTokenName(name) {
			sb.WriteString(v)
			i += end + 2
			continue
		}
		sb.WriteByte(c)
		i++
	}
}

// isTokenName reports whether s matches [a-z][a-z0-9_]*.
func isTokenName(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// renderEditable renders an editable scope from d.Custom (or the default).
func renderEditable(d Data) (string, error) {
	text := d.Custom
	if strings.TrimSpace(text) == "" {
		text = Default(d.Scope)
	}
	vals := tokenValues(d)
	known := map[string]string{}
	for _, t := range Tokens(d.Scope) {
		n := strings.Trim(t.Name, "{}")
		known[n] = vals[n]
	}
	out := Expand(text, known)
	if !utf8.ValidString(out) {
		out = strings.ToValidUTF8(out, "")
	}
	return strings.TrimRight(out, " \t\n") + "\n", nil
}
