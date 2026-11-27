package cli

// MCP client configuration: `weave mcp --print-config <tool>` and `weave skill install --mcp`.
// The entry only ever names the weave binary and `mcp`; the API key stays in the `weave login`
// profile (at most WEAVE_PROFILE is written), never in a client's config file.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// mcpSpec is the optional MCP config writer of an agent target. Paths and formats are recorded
// with their sources in docs/AGENT-TARGETS.md.
type mcpSpec struct {
	Verified   bool                     // read in the tool's official docs; unverified = explicit --target only
	User       func(e *skillEnv) string // user-scope config file; nil = none (see CLI)
	UserKey    string                   // top-level key in the user file
	Project    string                   // project-scope config file relative to the project root
	ProjectKey string
	Type       string // "stdio" when the tool wants an explicit type
	CLI        string // command that adds the server (%s = weave binary), used where we do not write a file
	TOML       bool   // Codex: TOML; printed, never written
}

func (s *mcpSpec) keyFor(scope string) string {
	if scope == "project" {
		return s.ProjectKey
	}
	return s.UserKey
}

func (s *mcpSpec) pathFor(e *skillEnv, scope string) string {
	if scope == "project" {
		if s.Project == "" {
			return ""
		}
		return filepath.Join(e.projectRoot(), filepath.FromSlash(s.Project))
	}
	if s.User == nil {
		return ""
	}
	return s.User(e)
}

func claudeDesktopConfig(e *skillEnv) string {
	switch e.GOOS {
	case "darwin":
		return e.hp("Library", "Application Support", "Claude", "claude_desktop_config.json")
	case "windows":
		return filepath.Join(e.appData(), "Claude", "claude_desktop_config.json")
	}
	return filepath.Join(e.xdgConfig(), "Claude", "claude_desktop_config.json")
}

// mcpEntry is the server entry written for every tool.
func mcpEntry(e *skillEnv, spec *mcpSpec, profile string) map[string]any {
	m := map[string]any{"command": e.exe(), "args": []any{"mcp"}}
	if spec.Type != "" {
		m["type"] = spec.Type
	}
	if profile != "" && profile != "default" {
		m["env"] = map[string]any{"WEAVE_PROFILE": profile}
	}
	return m
}

func (e *skillEnv) exe() string {
	if e.Exe != "" {
		return e.Exe
	}
	return "weave"
}

func realExe() string {
	p, err := os.Executable()
	if err != nil {
		return "weave"
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	// `go run` and test binaries live in throwaway build dirs
	if strings.Contains(p, "go-build") || strings.HasSuffix(p, ".test") || strings.HasSuffix(p, ".test.exe") {
		return "weave"
	}
	return p
}

func tomlString(s string) string { b, _ := json.Marshal(s); return string(b) }

// mcpSnippet renders the config snippet for one tool and scope.
func mcpSnippet(e *skillEnv, t *agentTarget, scope, profile string) (snippet, path, cli string) {
	spec := t.MCP
	entry := mcpEntry(e, spec, profile)
	path = spec.pathFor(e, scope)
	if spec.CLI != "" && (scope == "user" || path == "") {
		cli = fmt.Sprintf(spec.CLI, e.exe())
		if profile != "" && profile != "default" {
			cli = strings.Replace(cli, " weave -- ", " --env WEAVE_PROFILE="+profile+" weave -- ", 1)
		}
	}
	if spec.TOML {
		var b strings.Builder
		fmt.Fprintf(&b, "[mcp_servers.weave]\ncommand = %s\nargs = [\"mcp\"]\n", tomlString(e.exe()))
		if profile != "" && profile != "default" {
			fmt.Fprintf(&b, "\n[mcp_servers.weave.env]\nWEAVE_PROFILE = %s\n", tomlString(profile))
		}
		return b.String(), path, cli
	}
	key := spec.keyFor(scope)
	if key == "" {
		key = firstNonEmpty(spec.UserKey, spec.ProjectKey, "mcpServers")
	}
	b, _ := json.MarshalIndent(map[string]any{key: map[string]any{"weave": entry}}, "", "  ")
	return string(b) + "\n", path, cli
}

func mcpTargetIDs() []string {
	var ids []string
	for _, t := range agentTargets {
		if t.MCP != nil {
			ids = append(ids, t.ID)
		}
	}
	return ids
}

// mcpPrintConfig prints a pasteable snippet on stdout and where it goes on stderr.
func (a *App) mcpPrintConfig(tool, scopeArg string) int {
	t := findTarget(tool)
	if t == nil || t.MCP == nil {
		return a.failUsage("unknown MCP client " + tool + " (valid: " + strings.Join(mcpTargetIDs(), ", ") + ")")
	}
	scope := "user"
	switch strings.ToLower(scopeArg) {
	case "", "user", "global":
	case "project", "local":
		scope = "project"
	default:
		return a.failUsage("--scope must be user or project")
	}
	e := a.skillEnv()
	snip, path, cli := mcpSnippet(e, t, scope, a.Cfg.Profile)
	fmt.Fprint(a.Stdout, snip)
	if path != "" {
		key := t.MCP.keyFor(scope)
		if t.MCP.TOML {
			key = "[mcp_servers.weave]"
		}
		fmt.Fprintf(a.Stderr, "# %s (%s scope): merge into %s under %s\n", t.Name, scope, path, key)
	}
	if cli != "" {
		fmt.Fprintf(a.Stderr, "# or run: %s\n", cli)
	}
	if !t.MCP.Verified {
		fmt.Fprintf(a.Stderr, "# note: this location is not verified against %s's official docs\n", t.Name)
	}
	fmt.Fprintln(a.Stderr, "# the server uses your `weave login` profile (or WEAVE_URL/WEAVE_TOKEN); no API key is written into the config")
	return 0
}

// mcpWrite merges the weave entry into a JSON config file, leaving every other key in place.
func mcpWrite(path, key string, entry map[string]any, force, dry bool) (string, error) {
	doc := map[string]any{}
	perm := os.FileMode(0o600)
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err == nil {
		if st, err := os.Stat(path); err == nil {
			perm = st.Mode().Perm()
		}
		if len(strings.TrimSpace(string(b))) > 0 {
			if err := json.Unmarshal(b, &doc); err != nil {
				return "", &APIError{Code: "conflict", Message: path + " is not plain JSON (comments?); add the entry by hand: weave mcp --print-config"}
			}
		}
	}
	servers, _ := doc[key].(map[string]any)
	if servers == nil {
		if doc[key] != nil {
			return "", &APIError{Code: "conflict", Message: path + ": " + key + " is not an object"}
		}
		servers = map[string]any{}
	}
	want := normJSON(entry)
	act := "created"
	if have, ok := servers["weave"]; ok {
		if reflect.DeepEqual(normJSON(have), want) {
			return "unchanged", nil
		}
		if !force {
			return "", &APIError{Code: "conflict", Message: path + " already has a different \"weave\" MCP server; pass --force to replace it"}
		}
		act = "updated"
	} else if len(doc) > 0 {
		act = "updated"
	}
	if dry {
		return "would-" + strings.TrimSuffix(act, "d"), nil
	}
	servers["weave"] = entry
	doc[key] = servers
	out, _ := json.MarshalIndent(doc, "", "  ")
	return act, safeWrite(path, append(out, '\n'), perm)
}

// mcpRemove deletes our entry (when it is ours, or with force).
func mcpRemove(path, key string, entry map[string]any, force, dry bool) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "not-installed", nil
		}
		return "", err
	}
	doc := map[string]any{}
	if json.Unmarshal(b, &doc) != nil {
		return "", &APIError{Code: "conflict", Message: path + " is not plain JSON; remove the weave entry by hand"}
	}
	servers, _ := doc[key].(map[string]any)
	have, ok := servers["weave"]
	if !ok {
		return "not-installed", nil
	}
	if !reflect.DeepEqual(normJSON(have), normJSON(entry)) && !force {
		return "kept-modified", nil
	}
	if dry {
		return "would-remove", nil
	}
	delete(servers, "weave")
	out, _ := json.MarshalIndent(doc, "", "  ")
	st, _ := os.Stat(path)
	perm := os.FileMode(0o600)
	if st != nil {
		perm = st.Mode().Perm()
	}
	return "removed", safeWrite(path, append(out, '\n'), perm)
}

func normJSON(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// mcpRows applies install or uninstall of the MCP entry to the selected targets.
func (a *App) mcpRows(sel []resolved, e *skillEnv, scope string, explicit, force, remove bool) ([]skillRow, int) {
	var rows []skillRow
	exit := 0
	done := map[string]bool{}
	for _, r := range sel {
		t := r.T
		if t.MCP == nil {
			continue
		}
		row := skillRow{Target: t.ID, Name: t.Name, Kind: "mcp", Scope: scope, Detected: r.Detected, Verified: t.MCP.Verified}
		if !t.MCP.Verified && !explicit {
			row.Action, row.Reason = "skipped", "MCP config location unverified: opt in with --target "+t.ID+" --mcp"
			rows = append(rows, row)
			continue
		}
		snip, path, cli := mcpSnippet(e, t, scope, a.Cfg.Profile)
		key := t.MCP.keyFor(scope)
		if t.MCP.TOML || key == "" || path == "" {
			row.Action, row.Path = "manual", path
			switch {
			case cli != "":
				row.Reason = "run: " + cli
			case path != "":
				row.Reason = "add to " + path + ": " + strings.ReplaceAll(strings.TrimSpace(snip), "\n", " ")
			default:
				row.Reason = "no " + scope + "-scope MCP config for this tool"
			}
			rows = append(rows, row)
			continue
		}
		row.Path = path
		if done[path] {
			row.Action = "shared"
			rows = append(rows, row)
			continue
		}
		done[path] = true
		entry := mcpEntry(e, t.MCP, a.Cfg.Profile)
		var act string
		var err error
		if remove {
			act, err = mcpRemove(path, key, entry, force, a.DryRun)
		} else {
			act, err = mcpWrite(path, key, entry, force, a.DryRun)
		}
		row.Action = act
		if err != nil {
			row.Action, row.Reason = "error", err.Error()
			if ae, ok := err.(*APIError); ok && ae.Code == "conflict" {
				row.Action = "conflict"
				if exit == 0 {
					exit = ExitConflict
				}
			} else {
				exit = ExitError
			}
		}
		if act == "kept-modified" && exit == 0 {
			exit = ExitConflict
		}
		rows = append(rows, row)
	}
	return rows, exit
}
