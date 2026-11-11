# Agent targets for `weave skill install`

One row per coding-agent tool. The code is the registry in `internal/cli/skilltargets.go`; this file records where each path
came from. Verified date for every row: **2026-11-10**. "Verified" means read on the tool's official docs or source that day;
the ecosystem moves fast, so re-check before changing a path. Unverified targets are opt-in only (never automatic, never in
`--target all`); there are currently none.

`Detection` = config dir exists **or** binary on PATH. In project scope a tool is also detected by its project dir (`.cursor/`, ...).
`~/.agents/skills` (project: `.agents/skills`) is the cross-tool location from the Agent Skills ecosystem; every tool marked
"universal" reads it and Weave writes it once, then reports the other tools as "same location as X".

## Skill folders (SKILL.md + references/, kind = skill)

| Tool (target id) | Global path | Project path | Env override | Detection | Source |
|---|---|---|---|---|---|
| Claude Code (`claude`) | `~/.claude/skills/weave` (Windows `%USERPROFILE%\.claude\...`) | `.claude/skills/weave` | `CLAUDE_CONFIG_DIR` | `~/.claude` or `$CLAUDE_CONFIG_DIR`, `claude` | https://code.claude.com/docs/en/skills , https://code.claude.com/docs/en/claude-directory |
| OpenAI Codex CLI (`codex`) | universal `~/.agents/skills/weave` | `.agents/skills/weave` | `CODEX_HOME` (AGENTS.md/config only) | `~/.codex` or `$CODEX_HOME`, `codex` | https://developers.openai.com/codex/skills (redirects to https://learn.chatgpt.com/docs/build-skills): "User: $HOME/.agents/skills" |
| OpenCode (`opencode`) | universal | `.agents/skills/weave` | `XDG_CONFIG_HOME` (config dir only) | `~/.config/opencode`, `opencode` | https://opencode.ai/docs/skills/ (also reads `~/.config/opencode/skills`, `~/.claude/skills`, `~/.agents/skills`) |
| **pi** (`pi`) | universal | `.agents/skills/weave` | `PI_CODING_AGENT_DIR` (native dir `~/.pi/agent`) | `~/.pi/agent` or `$PI_CODING_AGENT_DIR` (no PATH check: `pi` is too generic) | https://github.com/badlogic/pi-mono/blob/main/packages/coding-agent/docs/skills.md , .../docs/configuration.md |
| **omp** (`omp`) | `~/.omp/agent/skills/weave` | `.omp/skills/weave` | `PI_CONFIG_DIR` (root, default `~/.omp`) | `~/.omp`, `omp` | https://github.com/can1357/oh-my-pi (docs/skills.md says native `.omp` user/project skills come from `src/discovery/builtin.ts`); the exact paths are read from https://raw.githubusercontent.com/can1357/oh-my-pi/main/packages/coding-agent/src/discovery/builtin.ts (user: `<agentDir>/skills`, comment "~/.omp/agent/skills/"; project: `.omp/skills`). Source-code, not prose, evidence. omp also reads `.agent[s]/skills` at project level. |
| Gemini CLI (`gemini`) | universal (alias of `~/.gemini/skills`) | `.agents/skills/weave` | `GEMINI_CLI_HOME` | `~/.gemini`, `gemini` | https://geminicli.com/docs/cli/skills/ |
| Google Antigravity (`antigravity`) | `~/.gemini/config/skills/weave` | `.agents/skills/weave` | `GEMINI_CLI_HOME` | `~/.gemini/config`, `~/.gemini/antigravity-cli` | https://antigravity.google/docs/skills |
| Amp (`amp`) | universal | `.agents/skills/weave` | - | `~/.config/amp`, `amp` | https://ampcode.com/docs/customize/skills |
| Goose (`goose`) | universal | `.agents/skills/weave` | - | `~/.config/goose`, `%APPDATA%\Block\goose`, `goose` | https://goose-docs.ai/docs/guides/context-engineering/using-skills |
| Cursor (`cursor`) | universal (also `~/.cursor/skills`) | `.agents/skills/weave` | - | `~/.cursor`, `cursor` | https://cursor.com/docs/context/skills |
| Windsurf / Devin Desktop (`windsurf`) | universal (primary is `~/.config/devin/skills`, `~/.agents/skills` "is also read") | `.agents/skills/weave` | - | `~/.codeium/windsurf`, `~/.config/devin`, `windsurf` | https://docs.devin.ai/desktop/cascade/skills (docs.windsurf.com redirects here) |
| GitHub Copilot, CLI and VS Code (`copilot`) | universal (also `~/.copilot/skills`) | `.agents/skills/weave` | `COPILOT_HOME` (detection only) | `~/.copilot` or `$COPILOT_HOME` | https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-skills |
| Roo Code (`roo`) | universal (also `~/.roo/skills`) | `.agents/skills/weave` | - | `~/.roo` | https://roocodeinc.github.io/Roo-Code/features/skills |
| Augment / Auggie (`augment`) | universal (also `~/.augment/skills`) | `.agents/skills/weave` | - | `~/.augment`, `auggie` | https://docs.augmentcode.com/cli/skills |
| Qwen Code (`qwen`) | universal (also `~/.qwen/skills`) | `.agents/skills/weave` | - | `~/.qwen`, `qwen` | https://qwenlm.github.io/qwen-code-docs/en/users/features/skills/ |
| Crush (`crush`) | universal (also `~/.config/crush/skills`) | `.agents/skills/weave` | `XDG_CONFIG_HOME`, `CRUSH_SKILLS_DIR` | `~/.config/crush`, `crush` | https://github.com/charmbracelet/crush (README, "Agent Skills") |
| Warp (`warp`) | universal | `.agents/skills/weave` | - | `~/.warp` | https://docs.warp.dev/agent-platform/capabilities/skills/ |
| Zed (`zed`) | universal ("loaded from `~/.agents/skills/` and `<worktree>/.agents/skills/` only") | `.agents/skills/weave` | `XDG_CONFIG_HOME`, `%APPDATA%` (detection) | `~/.config/zed`, `%APPDATA%\Zed`, `zed` | https://zed.dev/docs/ai/skills |
| Kilo Code (`kilo`) | universal | `.agents/skills/weave` | - | `~/.kilo`, `~/.config/kilo`, `kilo` | https://kilo.ai/docs/customize/skills/ (the page is inconsistent about `~/.kilo/skills`; `~/.agents/skills` is stated both ways) |
| JetBrains Junie (`junie`) | universal (also `~/.junie/skills`) | `.agents/skills/weave` | - | `~/.junie` | https://junie.jetbrains.com/docs/agent-skills.html |
| Cline (`cline`) | `~/.cline/skills/weave` | `.cline/skills/weave` | - | `~/.cline`, `~/Documents/Cline` | https://docs.cline.bot/customization/skills (does not read `.agents/skills`) |
| Kiro (`kiro`) | `~/.kiro/skills/weave` | `.kiro/skills/weave` | - | `~/.kiro`, `kiro-cli` | https://kiro.dev/docs/skills/ |
| Droid / Factory (`droid`) | `~/.factory/skills/weave` | `.factory/skills/weave` | - | `~/.factory`, `droid` | https://docs.factory.com/cli/configuration/skills |
| Cross-tool (`agents`) | `~/.agents/skills/weave` | `.agents/skills/weave` | - | `~/.agents`; the fallback when nothing is detected | https://agentskills.io/specification (format only; no install dirs defined), plus the per-tool pages above |

Windows: every `~/` path is `%USERPROFILE%\...` (Go `os.UserHomeDir`). Only Claude Code documents this explicitly; for the other tools it is the
same dot-directory under the user profile (an assumption, not documented by those tools). `~/.config` honors `XDG_CONFIG_HOME` on all OSes.

## Instruction files and rules (kind = instructions | rules)

Used when a tool has no verified skills dir, or on explicit request (OptIn = never automatic, not in `--target all`).

| Target id | Kind | Global path | Project path | Auto? | Source |
|---|---|---|---|---|---|
| `continue` | rules (frontmatter `name`, `description`, `alwaysApply`) | none documented | `.continue/rules/weave.md` | project scope, when `.continue/` exists | https://docs.continue.dev/customize/deep-dives/rules (no official skills page found) |
| `cursor-rules` | rules `.mdc` (`description`, `globs`, `alwaysApply`) | none (Cursor user rules are settings-only) | `.cursor/rules/weave.mdc` | OptIn | https://cursor.com/docs/context/rules |
| `windsurf-rules` | rules (`trigger: model_decision`) | none (global rules file location ambiguous) | `.windsurf/rules/weave.md` | OptIn | https://docs.devin.ai/desktop/cascade/memories |
| `cline-rules` | rules | `~/Documents/Cline/Rules/weave.md` (Windows `Documents\Cline\Rules`) | `.clinerules/weave.md` | OptIn | https://docs.cline.bot/customization/cline-rules |
| `roo-rules` | rules | `~/.roo/rules/weave.md` | `.roo/rules/weave.md` | OptIn | https://roocodeinc.github.io/Roo-Code/features/custom-instructions |
| `kiro-steering` | rules (`inclusion: auto`, `name`, `description`) | `~/.kiro/steering/weave.md` | `.kiro/steering/weave.md` | OptIn | https://kiro.dev/docs/steering/ |
| `copilot-instructions` | instructions block | `~/.copilot/copilot-instructions.md` (`COPILOT_HOME`) | `.github/copilot-instructions.md` | OptIn | https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-custom-instructions |
| `gemini-md` | instructions block | `~/.gemini/GEMINI.md` (`GEMINI_CLI_HOME`) | `GEMINI.md` | OptIn | https://geminicli.com/docs/cli/gemini-md/ |
| `codex-agents-md` | instructions block | `$CODEX_HOME/AGENTS.md` (default `~/.codex`) | `AGENTS.md` | OptIn | https://learn.chatgpt.com/docs/agent-configuration/agents-md |
| `opencode-agents-md` | instructions block | `~/.config/opencode/AGENTS.md` | `AGENTS.md` | OptIn | https://opencode.ai/docs/rules/ |
| `zed-agents-md` | instructions block | `~/.config/zed/AGENTS.md` (Windows `%APPDATA%\Zed\AGENTS.md`) | `AGENTS.md` | OptIn | https://zed.dev/docs/ai/instructions |
| `aider` | instructions block (`CONVENTIONS.md`) | none | `CONVENTIONS.md` | OptIn; Aider does not load it by itself, add `read: CONVENTIONS.md` to `.aider.conf.yml` or use `aider --read CONVENTIONS.md` | https://aider.chat/docs/usage/conventions.html |
| `agents-md` | instructions block (legacy name) | `./AGENTS.md` in the current directory | `AGENTS.md` at the project root | OptIn | https://agents.md |
| `dir` | skill folder | `--dir PATH` (the skill goes straight into PATH) | - | OptIn | - |

## How pi and omp were interpreted

- **pi** = `badlogic/pi-mono`, package `@mariozechner/pi-coding-agent` (agent dir `~/.pi/agent`, override `PI_CODING_AGENT_DIR`). Its docs list
  `~/.agents/skills` and `.agents/skills`, so it shares the universal location. Detected by its config dir only.
- **omp** = `can1357/oh-my-pi` (fork lineage of pi, config root `~/.omp`, binary `omp`). Native skills dirs are read from its source
  (see above). It also reads `.agent[s]/skills` at project level, but user-level foreign dirs are opt-in in omp, so Weave uses the native paths.

## Known gaps and judgment calls

- Windows paths other than Claude Code's are inferred (`%USERPROFILE%\<dot-dir>`); `install.ps1` and the Windows binary have not been run on Windows.
- Windsurf global rules moved to `.devin/global_rules.md` (exact home location not documented); not targeted. Skills cover Windsurf.
- VS Code Copilot user-level instructions moved to `~/.copilot/instructions/` according to search snippets only; not targeted. Copilot reads skills from `~/.agents/skills`.
- Continue: no official skills docs; only project rules are written. Kilo: docs inconsistent on `~/.kilo/skills`.
- Env overrides for OpenCode (`OPENCODE_CONFIG_DIR`) and Qwen could not be confirmed and are not honored.
- Amp, Goose, Droid personal instruction files (`~/.config/amp/AGENTS.md`, `~/.config/goose/.goosehints`, `~/.factory`) are not targeted; their skills are.
