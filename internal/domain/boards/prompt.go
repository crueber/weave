package boards

import (
	"strings"

	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/ops"
)

// The "Agent prompt" a column can hand to an AI agent: enough to connect to this server, install the
// CLI/skill if needed, find the column and understand what a card there asks for. It is copied as
// plain text and is meant to be edited: adding another `--list` watches another column.

// agentInstallBase returns the download base for install.sh/install.ps1 (config or the published
// release page). It is only used for the "no CLI or skill yet" part of the prompt.
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

// AgentPrompt renders the copy-paste brief for target, listing the board's other columns so the
// text can be extended with one `--list` line per extra column.
func AgentPrompt(baseURL, installBase string, b Board, target List, lists []List) string {
	base := strings.TrimRight(baseURL, "/")
	if base == "" {
		base = "(your Weave server URL)"
	}
	name := listName(target)
	board := b.Name
	var s strings.Builder
	w := func(parts ...string) {
		for _, p := range parts {
			s.WriteString(p)
		}
		s.WriteString("\n")
	}

	w("Use your Weave agent skill (the `weave` CLI) to work this board column.")
	w("")
	w("Server: ", base)
	w("Board:  ", quote(board), " (board id ", b.ID, ")")
	w("Column: ", quote(name), " (list id ", target.ID, ")")
	w("Brief:  ", brief(target.Description))
	w("")
	w("Connect with an API key (agents never use the browser sign-in):")
	w("  WEAVE_URL=", base)
	w("  WEAVE_TOKEN=<paste an API key here: Settings -> API key in the browser, or an agent key from your admin>")
	w("  weave login --url \"$WEAVE_URL\" --token \"$WEAVE_TOKEN\"")
	w("  weave whoami && weave doctor")
	w("")
	w("Listen for cards in this column:")
	w("  weave listen --board ", quote(board), " --list ", quote(name))
	w("")
	w("The column name and its description are the brief: read them, infer what the operator wants done")
	w("with a card that lands here, and do that. Claim a card before working on it, comment the result on")
	w("the card, then move it on. If the brief is unclear, ask on the card instead of guessing.")
	others := otherColumns(target, lists)
	if len(others) > 0 {
		w("")
		w("To watch more columns, add one --list flag per column:")
		for _, l := range others {
			line := "  --list " + quote(listName(l))
			if b := strings.Join(strings.Fields(deref(l.Description)), " "); b != "" {
				line += "   " + b
			}
			w(line)
		}
		w("  e.g. weave listen --board ", quote(board), " --list ", quote(name), " --list ", quote(listName(others[0])))
	}
	w("")
	w("No Weave CLI or skill yet? Get both from ", installBase, " :")
	w("  curl -fsSL ", installBase, "/install.sh | sh")
	w("  weave skill install      # writes the skill for this coding agent; `weave skill print` shows it")
	return s.String()
}

// otherColumns lists the board's other finite columns in board order.
func otherColumns(target List, lists []List) []List {
	var out []List
	for _, l := range lists {
		if l.ID != target.ID && isFinite(l.Type) {
			out = append(out, l)
		}
	}
	return out
}

// brief is a column description flattened to one line for the prompt.
func brief(d *string) string {
	if d == nil {
		return "(no description set - the column name is the brief)"
	}
	s := strings.Join(strings.Fields(*d), " ")
	if s == "" {
		return "(no description set - the column name is the brief)"
	}
	return s
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

// quote renders s as a shell double-quoted argument (prompt text only, never executed).
func quote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", `\$`, "`", "\\`")
	return `"` + r.Replace(s) + `"`
}
