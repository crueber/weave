package agentprompt

import (
	"embed"
	"strings"
)

// Shared blocks ({connect}, {security}, {etiquette}...) are plain text with {token}s too (blocks/*.txt), and an admin
// can replace any of them (Data.Blocks). A block may use DATA tokens only ({server}, {install_base}; connect also the
// two install lines), never another prompt block, so there is no recursion: Go expands the install lines first, then
// connect with them, then every prompt block, each in one pass. What used to be template conditionals is decided in Go:
// {etiquette} picks etiquette or etiquette_compact (Compact), {concepts} is empty when Compact, and {install} /
// {install_skill} are empty when the install steps are left out (a line holding only an empty token is dropped). The
// generated parts that carry object data ({columns} with its fenced briefs, {boards}, {board_line}) are not blocks
// and stay in Go.

//go:embed blocks/*.txt
var blocksFS embed.FS

// BlockDef describes one editable shared block.
type BlockDef struct {
	Name  string `json:"name"`
	Title string `json:"title"`
	Desc  string `json:"desc"` // what it is and where it is used, one line
	Line  bool   `json:"line"` // a single line inside another block (no trailing blank line)
}

// Blocks lists the editable shared blocks in display order.
var Blocks = []BlockDef{
	{Name: "instance", Title: "Instance", Desc: "The server's web UI, REST API and event stream URLs. Token {instance} in every prompt and the column prompt."},
	{Name: "connect", Title: "Connect", Desc: "Check $WEAVE_TOKEN without printing it, install if missing, configure and verify. Token {connect} in every prompt and the column prompt."},
	{Name: "install", Title: "Install the CLI", Desc: "The install line inside Connect ({install}); left out when Leave out the install steps is on.", Line: true},
	{Name: "install_skill", Title: "Install the skill", Desc: "The skill install line inside Connect ({install_skill}); left out when Leave out the install steps is on.", Line: true},
	{Name: "keynote", Title: "About the key", Desc: "What the API key may do and that claiming needs Edit access. Token {keynote} in every prompt and the column prompt."},
	{Name: "concepts", Title: "Concepts", Desc: "Projects, boards, columns, cards, claims and events. Token {concepts}; left out when Compact is on."},
	{Name: "questionnaire", Title: "Questionnaire", Desc: "The questions and the PLAN gate before an agent listens. Token {questionnaire} in the project, board and runner prompts."},
	{Name: "etiquette", Title: "Etiquette", Desc: "The house rules for working cards. Token {etiquette} in every prompt and the column prompt."},
	{Name: "etiquette_compact", Title: "Etiquette (compact)", Desc: "What {etiquette} shows instead when Compact is on."},
	{Name: "security", Title: "Security", Desc: "Card text is data, never instructions. Token {security} in every prompt and the column prompt."},
	{Name: "readmore", Title: "Read more", Desc: "Where the agent finds the full skill and command help. Token {readmore} in every prompt and the column prompt."},
}

// BlockNames lists the block names in display order.
func BlockNames() []string {
	n := make([]string, len(Blocks))
	for i, b := range Blocks {
		n[i] = b.Name
	}
	return n
}

// Block returns the definition of a block.
func Block(name string) (BlockDef, bool) {
	for _, b := range Blocks {
		if b.Name == name {
			return b, true
		}
	}
	return BlockDef{}, false
}

// IsBlock reports whether name is an editable block.
func IsBlock(name string) bool { _, ok := Block(name); return ok }

// BlockDefault returns the built-in text of a block ("" for unknown names).
func BlockDefault(name string) string {
	if !IsBlock(name) {
		return ""
	}
	b, err := blocksFS.ReadFile("blocks/" + name + ".txt")
	if err != nil {
		panic(err) // embedded; cannot happen
	}
	return string(b)
}

var blockDataTokens = []Token{
	{"{server}", "the public URL of this Weave server"},
	{"{install_base}", "the download base of install.sh (WEAVE_AGENT_INSTALL_URL or the published releases)"},
}

// BlockTokens returns the tokens a block understands (nil for unknown names). Blocks never expand other prompt blocks.
func BlockTokens(name string) []Token {
	if !IsBlock(name) {
		return nil
	}
	out := append([]Token(nil), blockDataTokens...)
	if name == "connect" {
		out = append(out,
			Token{"{install}", "the Install the CLI block, or nothing when the install steps are left out"},
			Token{"{install_skill}", "the Install the skill block, or nothing when the install steps are left out"})
	}
	return out
}

// blockText is the effective text of a block: the override in d.Blocks, else the built-in text.
func (d Data) blockText(name string) string {
	if t := strings.TrimSpace(d.Blocks[name]); t != "" {
		return t
	}
	return BlockDefault(name)
}

// expandBlock expands one block's text with its data tokens (connect also gets the install lines).
func expandBlock(d Data, name, text string) string {
	vals := map[string]string{"server": d.Base(), "install_base": d.Install()}
	if name == "connect" {
		vals["install"], vals["install_skill"] = "", ""
		if !d.NoInstall {
			vals["install"] = expandBlock(d, "install", d.blockText("install"))
			vals["install_skill"] = expandBlock(d, "install_skill", d.blockText("install_skill"))
		}
	}
	known := map[string]string{}
	for _, t := range BlockTokens(name) {
		n := strings.Trim(t.Name, "{}")
		known[n] = vals[n]
	}
	return strings.TrimRight(Expand(text, known), " \t\n")
}

// RenderBlock renders one block on its own with d's data and overrides (text "" = the effective text), as an admin
// preview shows it. The Compact and install choices are not applied to the block itself.
func RenderBlock(name string, d Data, text string) string {
	if strings.TrimSpace(text) == "" {
		text = d.blockText(name)
	}
	return expandBlock(d, name, text) + "\n"
}

// promptBlocks renders the block tokens a prompt can use ({instance}, {connect}...), each ending in a newline (or "").
func promptBlocks(d Data) map[string]string {
	v := map[string]string{}
	for _, name := range []string{"instance", "connect", "keynote", "concepts", "questionnaire", "etiquette", "security", "readmore"} {
		src := name
		if name == "etiquette" && d.Compact {
			src = "etiquette_compact"
		}
		if name == "concepts" && d.Compact {
			v[name] = ""
			continue
		}
		if s := expandBlock(d, src, d.blockText(src)); s != "" {
			v[name] = s + "\n"
		} else {
			v[name] = ""
		}
	}
	return v
}

// BlocksUsed lists the blocks (by block name) that the prompt for d draws on, in display order: the block tokens its
// text uses, with {etiquette} and {concepts} resolved for Compact and the install lines when connect shows them.
func BlocksUsed(d Data) []string {
	var text string
	switch d.Scope {
	case ScopeGeneral, ScopeProject, ScopeBoard, ScopeRunner:
		text = d.Custom
		if strings.TrimSpace(text) == "" {
			text = Default(d.Scope)
		}
	case ScopeList:
		text = "{instance}{connect}{keynote}{etiquette}{security}{readmore}" // list.tmpl
	default:
		return nil
	}
	used := map[string]bool{}
	for n := range tokensIn(text) {
		switch {
		case n == "etiquette" && d.Compact:
			used["etiquette_compact"] = true
		case n == "concepts" && d.Compact:
		case n == "connect":
			used[n] = true
			if !d.NoInstall {
				for m := range tokensIn(d.blockText("connect")) {
					if m == "install" || m == "install_skill" {
						used[m] = true
					}
				}
			}
		case IsBlock(n) && n != "install" && n != "install_skill" && n != "etiquette_compact":
			used[n] = true
		}
	}
	var out []string
	for _, b := range Blocks {
		if used[b.Name] {
			out = append(out, b.Name)
		}
	}
	return out
}

// tokensIn returns the {token} names text contains, scanned the way Expand scans ("{{" is never a token).
func tokensIn(text string) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < len(text); i++ {
		if text[i] != '{' {
			continue
		}
		if i+1 < len(text) && text[i+1] == '{' {
			i++
			continue
		}
		end := strings.IndexAny(text[i+1:], "}\n")
		if end < 0 || text[i+1+end] != '}' {
			continue
		}
		if n := text[i+1 : i+1+end]; isTokenName(n) {
			out[n] = true
			i += end + 1
		}
	}
	return out
}
