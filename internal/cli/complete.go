package cli

import (
	"fmt"
	"sort"
	"strings"
)

var completionScripts = map[string]string{
	"bash": `# weave bash completion: source <(weave completion bash)
_weave() {
  local IFS=$'\n'
  COMPREPLY=( $(compgen -W "$(weave __complete "${COMP_WORDS[@]:1:$COMP_CWORD}" 2>/dev/null)" -- "${COMP_WORDS[COMP_CWORD]}") )
}
complete -F _weave weave
`,
	"zsh": `#compdef weave
# weave zsh completion: weave completion zsh > "${fpath[1]}/_weave"
_weave() {
  local -a r
  r=("${(@f)$(weave __complete "${(@)words[2,CURRENT]}" 2>/dev/null)}")
  compadd -a r
}
compdef _weave weave
`,
	"fish": `# weave fish completion: weave completion fish | source
complete -c weave -f -a '(weave __complete (commandline -opc)[2..-1] (commandline -ct) 2>/dev/null)'
`,
	"powershell": `# weave PowerShell completion: weave completion powershell | Out-String | Invoke-Expression
Register-ArgumentCompleter -Native -CommandName weave -ScriptBlock {
  param($wordToComplete, $commandAst, $cursorPosition)
  $words = @($commandAst.CommandElements | Select-Object -Skip 1 | ForEach-Object { $_.ToString() })
  if ($wordToComplete -eq '') { $words += '' }
  weave __complete @words 2>$null | ForEach-Object {
    [System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
  }
}
`,
}

func cmdCompletion(a *App, args []string) int {
	if len(args) != 1 {
		return a.failUsage("weave completion bash|zsh|fish|powershell")
	}
	s, ok := completionScripts[strings.ToLower(args[0])]
	if !ok {
		return a.failUsage("unknown shell " + args[0] + "; use bash, zsh, fish or powershell")
	}
	fmt.Fprint(a.Stdout, s)
	return 0
}

// builtinCommands lists the non-schema commands completed at position 1.
var builtinCommands = []string{"help", "version", "ops", "schema", "config", "login", "logout", "whoami", "doctor",
	"completion", "verify-release", "skill", "listen", "watch", "next", "inbox", "agent", "serve", "healthcheck"}

// cmdComplete prints completion candidates for the words typed so far (hidden command used by
// the generated shell scripts, so completion always reflects the server's current schema).
func cmdComplete(a *App, words []string) int {
	cat := a.loadCatalog(a.ctx, true, false)
	var out []string
	cur := ""
	if len(words) > 0 {
		cur = words[len(words)-1]
	}
	prev := words[:max(0, len(words)-1)]
	var positional []string
	for i := 0; i < len(prev); i++ {
		if strings.HasPrefix(prev[i], "-") {
			if !strings.Contains(prev[i], "=") && i+1 < len(prev) && !strings.HasPrefix(prev[i+1], "-") && !boolish(prev[i]) {
				i++
			}
			continue
		}
		positional = append(positional, prev[i])
	}
	switch {
	case strings.HasPrefix(cur, "-"):
		seen := map[string]bool{}
		add := func(n string) {
			if !seen[n] {
				seen[n] = true
				out = append(out, "--"+n)
			}
		}
		if len(positional) >= 2 {
			if o := cat.Lookup(positional[0], positional[1]); o != nil {
				for _, f := range o.Fields {
					add(f.Name)
				}
			}
		}
		for _, f := range globalSpecs() {
			add(f.Name)
		}
		for _, n := range cmdFlagNames(positional) {
			add(n)
		}
	case len(positional) == 0:
		out = append(out, builtinCommands...)
		out = append(out, cat.Tags()...)
	case len(positional) == 1:
		for _, o := range cat.Tag(positional[0]) {
			out = append(out, o.Verb())
		}
		for k := range shortcuts {
			if t, v, ok := strings.Cut(k, " "); ok && t == positional[0] {
				out = append(out, v)
			}
		}
		switch positional[0] {
		case "completion":
			out = append(out, "bash", "zsh", "fish", "powershell")
		case "skill":
			out = append(out, "print", "install", "check")
		case "config":
			out = append(out, "list", "use", "set", "get", "remove", "path")
		case "agent":
			out = append(out, "run")
		}
	}
	sort.Strings(out)
	prevW := ""
	for _, o := range out {
		if o != prevW && strings.HasPrefix(o, cur) {
			fmt.Fprintln(a.Stdout, completionQuote(o))
		}
		prevW = o
	}
	return 0
}

func boolish(flag string) bool {
	switch strings.TrimLeft(flag, "-") {
	case "json", "all", "dry-run", "yes", "quiet", "no-color", "stdin", "once", "claim", "full", "help", "include-existing", "force", "check":
		return true
	}
	return false
}

// cmdFlagNames returns flags of the hand-written commands for completion.
func cmdFlagNames(pos []string) []string {
	if len(pos) == 0 {
		return nil
	}
	key := pos[0]
	if len(pos) > 1 {
		if n, ok := commandFlags[key+" "+pos[1]]; ok {
			return n
		}
	}
	return commandFlags[key]
}

var commandFlags = map[string][]string{}
