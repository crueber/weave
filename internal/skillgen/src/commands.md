# Weave CLI command reference

Generic form: `weave <tag> <verb> [path-args...] [--field value ...]`. The op `card.create` is
`weave card create`; the verb is the op name after the first dot (`card.label.add` is `weave card label.add`).
Path parameters can be positional (in order) or flags. Flags match case-insensitively without dashes:
`--listId`, `--list-id` and `--list` are the same. `weave <tag> <verb> --help` prints the fields and an example.
The complete op list with every field is in `ops.md`.

## Global flags (every command)

| Flag | Meaning |
|---|---|
| `--profile NAME`, `--url U`, `--token K` | choose server/credentials (`WEAVE_PROFILE`, `WEAVE_URL`, `WEAVE_TOKEN`); `--url/--token` go before the command |
| `--output json\|ndjson\|table\|ids\|quiet` | output format (default json envelope, indented on a terminal) |
| `--fields a,b.c` | keep only these dotted paths of each item |
| `--jq '.items[].id'` | tiny jq-like projection (`.a.b`, `[n]`, `[]`, `\|`); strings print raw |
| `--limit N --cursor C --all` | pagination; `--all` follows the cursor and merges pages |
| `--dry-run` | show the request, do not send writes |
| `--yes` | skip the delete/clear confirmation (only prompts on a terminal) |
| `--timeout 30s` | HTTP timeout |
| `--body @file.json`, `--body -`, `--stdin` | JSON payload for body fields (flags override) |
| `-o`, `--output-file PATH` | write the response (or a download) to a file |
| `--no-color` | disable colour (also `NO_COLOR`) |

Any string value may be `-` (read stdin) or `@path` (read a file): `weave card comment CARD --text @notes.md`.

## Names instead of ids

`--board`, `--list`, `--project`, `--label`, `--user` (and the matching `xxxId` flags and positional ids)
accept a name, an id, or an unambiguous prefix. `me` is the current user. Lists and labels need a board:
`--board "Launch plan" --list Backlog` (or `WEAVE_BOARD`). `Project/Board` disambiguates board names.
Ambiguity is an error (exit 2, `code: ambiguous`, candidates listed).

## Workflow commands

| Command | Does |
|---|---|
| `weave login --url U --token K [--profile P]` | verify with whoami and save a profile |
| `weave whoami` / `weave doctor` / `weave version [--check]` | identity, health checks, version skew |
| `weave config list\|use NAME\|set url\|token V\|get K\|remove NAME\|path` | profiles |
| `weave board tree --board B` | lists with their cards |
| `weave inbox [--assigned\|--claimed]` | my cards across boards |
| `weave list stats --board B [--list L]` | per-list counts: cards, claimed, unclaimed, overdue |
| `weave next --board B --list L [--list L2] [--claim] [--move-to L] [--ttl 15m] [--wait 60s]` | pull the next card; exit 10 if none |
| `weave card create --board B --list L --name N [--desc T\|@f] [--label X]... [--assign U]... [--due T] [--from-stdin]` | create; `--from-stdin` reads NDJSON/lines for bulk |
| `weave card show CARD [--full]` | card, optionally with comments, checklists, attachments |
| `weave card move CARD --to-list L [--position N]` | move |
| `weave card comment CARD "text"` / `--stdin` | comment |
| `weave card attach CARD FILE...` | upload attachments |
| `weave card claim CARD [--ttl 15m] [--note T]` / `release` / `renew` | lease (exit 7 if already claimed) |
| `weave card done CARD [--note T] [--to-list L]` | comment + move + release |
| `weave task add CARD "text"...` / `task check\|uncheck ID` / `task check --card C --n 2` / `task list CARD` | checklists |
| `weave listen ...` / `weave watch ...` | event stream (see below) |
| `weave agent run ... -- CMD` | listen + claim + run + report |
| `weave skill print\|install\|check\|targets\|uninstall` | this skill; `install` covers every detected coding agent (Claude Code, Codex, OpenCode, pi, Gemini, Cursor, ...) |
| `weave completion bash\|zsh\|fish\|powershell` | shell completion |
| `weave ops [tag]` / `weave schema OP` | discovery |

## `weave listen`

`weave listen --board B --list L1 --list L2 [--type card.moved,card.created] [--exec CMD] [--once]
[--timeout DUR] [--cursor-file PATH] [--since ID] [--include-existing] [--agent-label NAME]
[--claim] [--claim-move-to LIST] [--ttl 15m] [--concurrency N] [--exec-timeout DUR]
[--on-error release|comment|release,comment] [--ignore-self=false] [--print] [--no-cursor]`

- Output: NDJSON envelopes on stdout (with `--exec`, only the command's output unless `--print`).
- `--exec CMD` runs through `sh -c` (`cmd /C` on Windows) per event: envelope on stdin, env `WEAVE_EVENT_TYPE`,
  `WEAVE_EVENT_ID`, `WEAVE_CARD_ID`, `WEAVE_CARD_NAME`, `WEAVE_LIST_ID`, `WEAVE_FROM_LIST_ID`, `WEAVE_BOARD_ID`,
  `WEAVE_ACTOR_ID`, plus `WEAVE_URL/TOKEN/PROFILE`. A non-zero exit is logged and does not stop the listener.
- The cursor (last fully handled event id) is saved atomically to `<config dir>/cursors/` per profile, board and lists,
  so restarts neither miss nor repeat events. With concurrency the cursor only advances past finished events.
- `--timeout` exits 10 when nothing arrived in time; `--once` exits after the first event; SIGINT/SIGTERM exit 0 after in-flight commands finish (`--grace`, default 30s).
- `weave watch --rooms board:ID` is the legacy raw realtime stream; `weave watch` otherwise equals `listen`.

## `weave agent run`

`weave agent run --board B --listen L [--listen L2] [--claim-move-to L] [--done-list L] [--fail-list L]
[--ttl 15m] [--concurrency N] [--comment-output] [--no-claim] [--keep-claim] [--once] [--include-existing] -- CMD [ARGS...]`

Exit codes of CMD: 0 success, 11 decline (release, no comment), anything else failure.

## Exit codes

0 ok, 1 error, 2 usage (also ambiguous names), 3 unauthorized, 4 not found, 5 forbidden, 6 validation,
7 conflict, 8 network, 10 nothing to do.
