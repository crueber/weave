package cli

import (
	"fmt"
	"sort"
	"strings"
)

// shortcutUsage is the --help text of the hand-written commands (the generic ops get theirs
// generated from the schema).
var shortcutUsage = map[string]string{
	"next": `weave next --board B --list Ready [--list L2] [--claim] [--move-to 'In progress'] [--ttl 15m] [--wait 60s]

Pull the next unclaimed card from the first list that has one and print {"item": card}.
Exit 10 (and no output) when every list is empty, so shell loops work:
  while card=$(weave next --board Product --list Ready --claim --ttl 15m); do ...; done`,
	"inbox": `weave inbox [--board B] [--assigned|--claimed]

Open cards assigned to me or claimed by me across every board I can see.`,
	"board tree":   `weave board tree --board B   (also: weave board tree B)   lists with their cards; indented text on a terminal, JSON otherwise.`,
	"list stats":   `weave list stats --board B [--list L]   per list: cards, claimed, unclaimed, overdue, closed.`,
	"card show":    `weave card show CARD [--full] [--board B]   --full adds comments, checklists and attachments.`,
	"card move":    `weave card move CARD --to-list NAME [--position N] [--board B]`,
	"card comment": `weave card comment CARD "text"      |  ... | weave card comment CARD --stdin   (text may be - or @file)`,
	"card attach":  `weave card attach CARD FILE [FILE...] [--name NAME]`,
	"card claim":   `weave card claim CARD [--ttl 15m] [--note TEXT] [--list NAME]   exit 7 when someone else holds the claim.`,
	"card release": `weave card release CARD [--note TEXT]`,
	"card renew":   `weave card renew CARD [--ttl 15m]`,
	"card done":    `weave card done CARD [--note TEXT] [--to-list NAME] [--keep-claim]   comment, move, release.`,
	"card create": `weave card create --board B --list L --name "Title" [--desc TEXT|@file] [--label X]... [--assign USER]... [--due RFC3339] [--position N] [--claim]
weave card create --board B --list L --from-stdin     one card per stdin line: JSON {"name","description","labels":[],"assign":[],"list","dueDate"} or a bare title`,
	"task add":     `weave task add CARD "text" ["text"...] [--list CHECKLIST] [--assignee USER]`,
	"task check":   `weave task check TASK_ID...  |  weave task check --card CARD (--n INDEX | --text PREFIX)`,
	"task uncheck": `weave task uncheck TASK_ID...  |  weave task uncheck --card CARD (--n INDEX | --text PREFIX)`,
	"task list":    `weave task list CARD`,
	"listen": `weave listen --board B --list L1 --list L2 [flags]

Stream change events of one or more columns as NDJSON (one envelope per line).
  --type card.moved,card.created   only these event types (server-side filter)
  --exec CMD                       run CMD per event (envelope on stdin, WEAVE_* env); --concurrency N, --exec-timeout D, --on-error release|comment
  --once                           exit after the first event
  --timeout D                      exit 10 if no event within D (0 otherwise)
  --include-existing               first emit card.existing for cards already in the lists
  --claim [--claim-move-to L] [--ttl D]  claim each card (409 = skipped) before delivering it
  --cursor-file P | --no-cursor    resume position (default: per profile/board/lists under the config dir)
  --since ID                       start after this event id
  --agent-label NAME               how this listener shows up in the UI
  --ignore-self=false              also receive events caused by this identity
  --print                          with --exec, also print envelopes
Reconnects automatically (backoff + jitter), resumes from the last event id, de-duplicates, and exits 0 on SIGINT/SIGTERM.`,
	"watch": `weave watch --rooms board:ID[,project:ID]   legacy raw realtime stream
weave watch --board B --list L ...          alias of weave listen`,
	"agent run": `weave agent run --board B --listen L [--listen L2] [--claim-move-to L] [--done-list L] [--fail-list L]
                [--ttl 15m] [--concurrency N] [--comment-output] [--no-claim] [--keep-claim] [--once] [--include-existing]
                -- COMMAND [ARGS...]

Listen on the columns, claim each card, run COMMAND (event JSON on stdin; WEAVE_EVENT_TYPE, WEAVE_CARD_ID,
WEAVE_CARD_NAME, WEAVE_LIST_ID, WEAVE_BOARD_ID, WEAVE_URL, WEAVE_TOKEN in the environment), keep the lease
renewed, then report back. COMMAND exit 0 = success: optional comment, move to --done-list, release.
Exit 11 = decline: release quietly. Any other exit = failure: comment with the error tail, release, optional --fail-list.`,
	"skill": `weave skill print [--file references/commands.md|list] [--compact]
weave skill install [--target a,b|all] [--scope user|project] [--dir PATH] [--force] [--dry-run]
weave skill targets [--scope user|project]
weave skill check [--target ...]        (exit 1 when missing or stale)
weave skill uninstall [--target ...] [--force]

With no --target, installs into every coding agent it detects (config dir or binary on PATH): Claude Code, Codex, OpenCode,
pi, Gemini CLI, Cursor, Copilot, Cline, Roo, Kiro, Droid, Zed and more (the full skill folder where the tool reads Agent Skills,
a marked block otherwise). --target all installs every verified target; "weave skill targets" lists them.`,
	"login":  `weave login --url https://weave.example.com --token KEY|- [--profile NAME] [--no-verify]   verifies with whoami and saves the profile.`,
	"config": `weave config list | use NAME | set url|token VALUE [--profile NAME] | get url|token|profile [--show] | remove NAME | path`,
	"whoami": `weave whoami   the authenticated identity (user, role, isAgent, accessible boards).`,
	"doctor": `weave doctor   checks profile, connectivity, auth, schema source, version skew, clock skew and the event stream.`,
	"verify-release": `weave verify-release SHA256SUMS.txt SHA256SUMS.txt.sig [--pubkey BASE64|FILE]   verify the ed25519 release signature (exit 0 = valid).
Uses the release key built into this binary; --pubkey is for forks that sign with their own key.`,
	"completion": `weave completion bash|zsh|fish|powershell   print a completion script (source <(weave completion bash)).`,
	"ops":        `weave ops [tag] [--names]   the op schema this server exposes.`,
	"schema":     `weave schema OP   one op with its fields.`,
}

func init() {
	for k, v := range shortcutUsage {
		shortcutHelp[k] = v + "\n" + globalFlagHelp
	}
}

// helpFor returns help for a shortcut command, listing registered flags when no text exists.
func helpFor(key string) string {
	if h, ok := shortcutHelp[key]; ok {
		return h
	}
	names := commandFlags[key]
	sort.Strings(names)
	return fmt.Sprintf("weave %s [flags]\nFlags: --%s\n%s", key, strings.Join(names, " --"), globalFlagHelp)
}
