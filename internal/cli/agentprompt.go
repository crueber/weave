package cli

import (
	"fmt"
	"os"
	"strings"
)

var agentPromptFlags = []FlagSpec{
	{Name: "general", Bool: true}, {Name: "runner", Bool: true}, {Name: "work-order", Bool: true}, {Name: "compact", Bool: true},
	{Name: "project"}, {Name: "board"}, {Name: "list"}, {Name: "card"}, {Name: "card-file"}, {Name: "no-install", Bool: true},
}

func init() {
	registerSpec("agent prompt", agentPromptFlags, cmdAgentPrompt)
}

// cmdAgentPrompt prints the server-rendered prompt (op agent.prompt) as plain text, ready to paste into
// a coding agent. Names are resolved to ids like every other shortcut. The text never contains a key.
func cmdAgentPrompt(a *App, args []string) int {
	p, err := a.parse(args, agentPromptFlags)
	if err != nil {
		return a.fail(err)
	}
	ctx := a.ctx
	usage := "weave agent prompt [--general | --runner [--board B] | --project P | --board B | --board B --list L | --work-order --board B --list L --card C [--card-file F]] [--compact] [--no-install]"
	params := Params{}
	boardRaw := firstNonEmpty(p.Str("board"), os.Getenv("WEAVE_BOARD"))
	resolveBoard := func() (string, error) { return a.Resolve(ctx, "board", boardRaw, Scope{}) }
	switch {
	case p.Bool("general"):
		params["scope"] = "general"
	case p.Bool("work-order"):
		if boardRaw == "" || p.Str("list") == "" || p.Str("card") == "" {
			return a.failUsage("a work order needs --board, --list and --card\n" + usage)
		}
		bid, err := resolveBoard()
		if err != nil {
			return a.fail(err)
		}
		lid, err := a.Resolve(ctx, "list", p.Str("list"), Scope{BoardID: bid})
		if err != nil {
			return a.fail(err)
		}
		cid, err := a.Resolve(ctx, "card", p.Str("card"), Scope{BoardID: bid})
		if err != nil {
			return a.fail(err)
		}
		params["scope"], params["id"], params["cardId"] = "workorder", lid, cid
		if f := p.Str("card-file"); f != "" {
			params["cardFile"] = f
		}
	case p.Str("list") != "":
		if boardRaw == "" {
			return a.failUsage("--list needs --board\n" + usage)
		}
		bid, err := resolveBoard()
		if err != nil {
			return a.fail(err)
		}
		lid, err := a.Resolve(ctx, "list", p.Str("list"), Scope{BoardID: bid})
		if err != nil {
			return a.fail(err)
		}
		params["scope"], params["id"] = "list", lid
	case p.Bool("runner"):
		params["scope"] = "runner"
		if boardRaw != "" {
			bid, err := resolveBoard()
			if err != nil {
				return a.fail(err)
			}
			params["id"] = bid
		}
	case p.Str("project") != "":
		pid, err := a.Resolve(ctx, "project", p.Str("project"), Scope{})
		if err != nil {
			return a.fail(err)
		}
		params["scope"], params["id"] = "project", pid
	case boardRaw != "":
		bid, err := resolveBoard()
		if err != nil {
			return a.fail(err)
		}
		params["scope"], params["id"] = "board", bid
	default: // --general or nothing
		params["scope"] = "general"
	}
	if p.Bool("compact") {
		params["compact"] = true
	}
	if p.Bool("no-install") {
		params["install"] = false
	}
	r, err := a.Do(ctx, "agent.prompt", params)
	if err != nil {
		return a.fail(err)
	}
	text := str(itemOf(r.JSON()), "text")
	if text == "" {
		return a.failUsage("the server returned no prompt text")
	}
	fmt.Fprint(a.Stdout, strings.TrimRight(text, "\n")+"\n")
	return 0
}
