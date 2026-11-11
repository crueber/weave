package cli

import (
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// skillFmt is how Weave's content is laid down for a tool.
type skillFmt int

const (
	fmtSkillDir skillFmt = iota // folder with SKILL.md + references/ (Agent Skills)
	fmtSharedMD                 // a file the user owns; we manage one marked block in it
	fmtOwnedMD                  // a dedicated file that is wholly ours (rules files with frontmatter)
)

// agentTarget is one registry entry. Sources and verification dates live in docs/AGENT-TARGETS.md.
type agentTarget struct {
	ID       string
	Aliases  []string
	Name     string
	Fmt      skillFmt
	Verified bool // path confirmed from an official source; unverified targets are never automatic
	OptIn    bool // verified, but only written on an explicit --target (alternative formats, legacy)
	Front    string
	User     func(e *skillEnv) string // user-scope location (skills base dir for fmtSkillDir, else the file); nil = none
	Project  string                   // project-scope location relative to the project root, slash separated; "" = none
	Detect   func(e *skillEnv) []string
	Bins     []string
	Note     string
}

func (t *agentTarget) kind() string {
	switch t.Fmt {
	case fmtSkillDir:
		return "skill"
	case fmtSharedMD:
		return "instructions"
	}
	return "rules"
}

func goos() string { return runtime.GOOS }

func (e *skillEnv) hp(parts ...string) string {
	return filepath.Join(append([]string{e.Home}, parts...)...)
}

// xdgConfig honours XDG_CONFIG_HOME on every OS (OpenCode, Zed, Crush and others use ~/.config on
// macOS and Windows too).
func (e *skillEnv) xdgConfig() string {
	if v := e.env("XDG_CONFIG_HOME"); v != "" {
		return v
	}
	return e.hp(".config")
}

func (e *skillEnv) appData() string {
	if v := e.env("APPDATA"); v != "" {
		return v
	}
	return e.hp("AppData", "Roaming")
}

func (e *skillEnv) envOr(k, def string) string {
	if v := e.env(k); v != "" {
		return v
	}
	return def
}

func (e *skillEnv) claudeDir() string { return e.envOr("CLAUDE_CONFIG_DIR", e.hp(".claude")) }
func (e *skillEnv) codexHome() string { return e.envOr("CODEX_HOME", e.hp(".codex")) }
func (e *skillEnv) piDir() string     { return e.envOr("PI_CODING_AGENT_DIR", e.hp(".pi", "agent")) }
func (e *skillEnv) geminiDir() string {
	if v := e.env("GEMINI_CLI_HOME"); v != "" {
		return filepath.Join(v, ".gemini")
	}
	return e.hp(".gemini")
}
func (e *skillEnv) copilotDir() string { return e.envOr("COPILOT_HOME", e.hp(".copilot")) }

func homeDot(names ...string) func(e *skillEnv) []string {
	return func(e *skillEnv) []string {
		var out []string
		for _, n := range names {
			out = append(out, e.hp(n))
		}
		return out
	}
}

func one(f func(e *skillEnv) string) func(e *skillEnv) []string {
	return func(e *skillEnv) []string { return []string{f(e)} }
}

func homeJoin(parts ...string) func(e *skillEnv) string {
	return func(e *skillEnv) string { return e.hp(parts...) }
}

func xdgJoin(parts ...string) func(e *skillEnv) string {
	return func(e *skillEnv) string { return filepath.Join(append([]string{e.xdgConfig()}, parts...)...) }
}

// universalUser is the cross-tool ~/.agents/skills location (agentskills.io convention, read by
// Codex, Gemini CLI, pi, OpenCode, Amp, Goose, Qwen Code, Crush, Cursor, Copilot, Roo, Augment, Windsurf).
func universalUser(e *skillEnv) string { return e.hp(".agents", "skills") }

const weaveDesc = "Weave kanban CLI: use when a task involves Weave boards, lists, cards, comments, checklists, `weave listen`, `weave next` or `weave agent run`."

// agentTargets is the registry. Order matters only for which of several tools sharing one location
// is reported as the writer.
var agentTargets = []*agentTarget{
	{ID: "claude", Aliases: []string{"claude-code"}, Name: "Claude Code", Fmt: fmtSkillDir, Verified: true,
		User: func(e *skillEnv) string { return filepath.Join(e.claudeDir(), "skills") }, Project: ".claude/skills",
		Detect: one(func(e *skillEnv) string { return e.claudeDir() }), Bins: []string{"claude"}},
	{ID: "codex", Name: "OpenAI Codex CLI", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: one(func(e *skillEnv) string { return e.codexHome() }), Bins: []string{"codex"}},
	{ID: "opencode", Name: "OpenCode", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: one(xdgJoin("opencode")), Bins: []string{"opencode"}},
	{ID: "pi", Name: "pi (pi-mono coding agent)", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: one(func(e *skillEnv) string { return e.piDir() }),
		Note:   "interpreted as badlogic/pi-mono; `pi` is too generic a binary name to detect on PATH"},
	{ID: "omp", Name: "omp (oh-my-pi)", Fmt: fmtSkillDir, Verified: true,
		User: func(e *skillEnv) string {
			return filepath.Join(e.envOr("PI_CONFIG_DIR", e.hp(".omp")), "agent", "skills")
		},
		Project: ".omp/skills",
		Detect:  one(func(e *skillEnv) string { return e.envOr("PI_CONFIG_DIR", e.hp(".omp")) }), Bins: []string{"omp"},
		Note: "native paths read from omp's discovery source (builtin.ts), not prose docs"},
	{ID: "gemini", Aliases: []string{"gemini-cli"}, Name: "Gemini CLI", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: one(func(e *skillEnv) string { return e.geminiDir() }), Bins: []string{"gemini"}},
	{ID: "antigravity", Name: "Google Antigravity", Fmt: fmtSkillDir, Verified: true,
		User:    func(e *skillEnv) string { return filepath.Join(e.geminiDir(), "config", "skills") },
		Project: ".agents/skills",
		Detect: func(e *skillEnv) []string {
			return []string{filepath.Join(e.geminiDir(), "config"), filepath.Join(e.geminiDir(), "antigravity-cli")}
		}},
	{ID: "amp", Name: "Amp", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: one(xdgJoin("amp")), Bins: []string{"amp"}},
	{ID: "goose", Name: "Goose", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: func(e *skillEnv) []string {
			return []string{filepath.Join(e.xdgConfig(), "goose"), filepath.Join(e.appData(), "Block", "goose")}
		}, Bins: []string{"goose"}},
	{ID: "cursor", Name: "Cursor", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: homeDot(".cursor"), Bins: []string{"cursor"}},
	{ID: "windsurf", Aliases: []string{"devin"}, Name: "Windsurf (Devin Desktop)", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: func(e *skillEnv) []string {
			return []string{e.hp(".codeium", "windsurf"), filepath.Join(e.xdgConfig(), "devin")}
		}, Bins: []string{"windsurf"}},
	{ID: "copilot", Aliases: []string{"github-copilot"}, Name: "GitHub Copilot (CLI, VS Code)", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: one(func(e *skillEnv) string { return e.copilotDir() })},
	{ID: "roo", Aliases: []string{"roo-code"}, Name: "Roo Code", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills", Detect: homeDot(".roo")},
	{ID: "augment", Aliases: []string{"auggie"}, Name: "Augment", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: homeDot(".augment"), Bins: []string{"auggie"}},
	{ID: "qwen", Aliases: []string{"qwen-code"}, Name: "Qwen Code", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: homeDot(".qwen"), Bins: []string{"qwen"}},
	{ID: "crush", Name: "Crush", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: one(xdgJoin("crush")), Bins: []string{"crush"}},
	{ID: "warp", Name: "Warp", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills", Detect: homeDot(".warp")},
	{ID: "zed", Name: "Zed", Fmt: fmtSkillDir, Verified: true, User: universalUser, Project: ".agents/skills",
		Detect: func(e *skillEnv) []string {
			return []string{filepath.Join(e.xdgConfig(), "zed"), filepath.Join(e.appData(), "Zed")}
		}, Bins: []string{"zed"}},
	{ID: "cline", Name: "Cline", Fmt: fmtSkillDir, Verified: true,
		User: homeJoin(".cline", "skills"), Project: ".cline/skills", Detect: homeDot(".cline", "Documents/Cline")},
	{ID: "kilo", Aliases: []string{"kilocode", "kilo-code"}, Name: "Kilo Code", Fmt: fmtSkillDir, Verified: true, Note: "docs disagree on ~/.kilo/skills; ~/.agents/skills is confirmed",
		User: universalUser, Project: ".agents/skills",
		Detect: func(e *skillEnv) []string { return []string{e.hp(".kilo"), filepath.Join(e.xdgConfig(), "kilo")} }, Bins: []string{"kilo"}},
	{ID: "kiro", Name: "Kiro", Fmt: fmtSkillDir, Verified: true,
		User: homeJoin(".kiro", "skills"), Project: ".kiro/skills", Detect: homeDot(".kiro"), Bins: []string{"kiro-cli"}},
	{ID: "junie", Name: "JetBrains Junie", Fmt: fmtSkillDir, Verified: true,
		User: universalUser, Project: ".agents/skills", Detect: homeDot(".junie")},
	{ID: "droid", Aliases: []string{"factory"}, Name: "Droid (Factory)", Fmt: fmtSkillDir, Verified: true,
		User: homeJoin(".factory", "skills"), Project: ".factory/skills", Detect: homeDot(".factory"), Bins: []string{"droid"}},
	// Cross-tool location last: it is the fallback when nothing is detected.
	{ID: "agents", Aliases: []string{"agent", "universal"}, Name: "Cross-tool Agent Skills (~/.agents/skills)", Fmt: fmtSkillDir, Verified: true,
		User: universalUser, Project: ".agents/skills", Detect: homeDot(".agents")},

	// Tools without a verified skills directory: a managed block or rules file.
	{ID: "zed-agents-md", Name: "Zed AGENTS.md", Fmt: fmtSharedMD, Verified: true, OptIn: true,
		User: func(e *skillEnv) string {
			if e.GOOS == "windows" {
				return filepath.Join(e.appData(), "Zed", "AGENTS.md")
			}
			return filepath.Join(e.xdgConfig(), "zed", "AGENTS.md")
		}, Project: "AGENTS.md"},
	{ID: "continue", Name: "Continue", Fmt: fmtOwnedMD, Verified: true, Project: ".continue/rules/weave.md", Detect: homeDot(".continue"),
		Front: "---\nname: Weave\ndescription: " + weaveDesc + "\nalwaysApply: false\n---\n\n",
		Note:  "project scope only (global rules dir not documented)"},

	// Alternative renderings, written only on an explicit --target.
	{ID: "cursor-rules", Name: "Cursor rule (.mdc)", Fmt: fmtOwnedMD, Verified: true, OptIn: true, Project: ".cursor/rules/weave.mdc",
		Front: "---\ndescription: " + weaveDesc + "\nglobs:\nalwaysApply: false\n---\n\n", Detect: homeDot(".cursor"), Note: "project scope only"},
	{ID: "windsurf-rules", Name: "Windsurf rule", Fmt: fmtOwnedMD, Verified: true, OptIn: true, Project: ".windsurf/rules/weave.md",
		Front: "---\ntrigger: model_decision\ndescription: " + weaveDesc + "\n---\n\n", Note: "project scope only"},
	{ID: "cline-rules", Name: "Cline rule", Fmt: fmtOwnedMD, Verified: true, OptIn: true, Project: ".clinerules/weave.md",
		User: homeJoin("Documents", "Cline", "Rules", "weave.md")},
	{ID: "roo-rules", Name: "Roo Code rule", Fmt: fmtOwnedMD, Verified: true, OptIn: true, Project: ".roo/rules/weave.md",
		User: homeJoin(".roo", "rules", "weave.md")},
	{ID: "kiro-steering", Name: "Kiro steering file", Fmt: fmtOwnedMD, Verified: true, OptIn: true, Project: ".kiro/steering/weave.md",
		Front: "---\ninclusion: auto\nname: weave\ndescription: " + weaveDesc + "\n---\n\n",
		User:  homeJoin(".kiro", "steering", "weave.md")},
	{ID: "copilot-instructions", Name: "GitHub Copilot instructions", Fmt: fmtSharedMD, Verified: true, OptIn: true, Project: ".github/copilot-instructions.md",
		User: func(e *skillEnv) string { return filepath.Join(e.copilotDir(), "copilot-instructions.md") }},
	{ID: "gemini-md", Name: "Gemini CLI GEMINI.md", Fmt: fmtSharedMD, Verified: true, OptIn: true, Project: "GEMINI.md",
		User: func(e *skillEnv) string { return filepath.Join(e.geminiDir(), "GEMINI.md") }},
	{ID: "codex-agents-md", Name: "Codex AGENTS.md", Fmt: fmtSharedMD, Verified: true, OptIn: true, Project: "AGENTS.md",
		User: func(e *skillEnv) string { return filepath.Join(e.codexHome(), "AGENTS.md") }},
	{ID: "opencode-agents-md", Name: "OpenCode AGENTS.md", Fmt: fmtSharedMD, Verified: true, OptIn: true, Project: "AGENTS.md",
		User: xdgJoin("opencode", "AGENTS.md")},
	{ID: "aider", Name: "Aider (CONVENTIONS.md)", Fmt: fmtSharedMD, Verified: true, OptIn: true, Project: "CONVENTIONS.md",
		Detect: homeDot(".aider.conf.yml"), Bins: []string{"aider"},
		Note: "project scope only; Aider does not load it by itself: add `read: CONVENTIONS.md` to .aider.conf.yml or run `aider --read CONVENTIONS.md`"},
	{ID: "agents-md", Aliases: []string{"agents.md"}, Name: "AGENTS.md in the current project (legacy)", Fmt: fmtSharedMD, Verified: true, OptIn: true, Project: "AGENTS.md",
		User: func(e *skillEnv) string { return filepath.Join(e.Cwd, "AGENTS.md") }},
	{ID: "dir", Name: "Custom skills folder (--dir PATH)", Fmt: fmtSkillDir, Verified: true, OptIn: true},
}

func findTarget(id string) *agentTarget {
	id = strings.ToLower(id)
	for _, t := range agentTargets {
		if t.ID == id {
			return t
		}
		for _, a := range t.Aliases {
			if a == id {
				return t
			}
		}
	}
	return nil
}

func targetNames() []string {
	var n []string
	for _, t := range agentTargets {
		n = append(n, t.ID)
	}
	sort.Strings(n)
	return n
}
