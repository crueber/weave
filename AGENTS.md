# AGENTS.md: working on Weave

Read this first, then `docs/DESIGN.md` (the architecture contract) and skim `docs/FEATURES.md`. This file is the
fast path: what the project is, the rules that have bitten before, how to build/verify/release, and what is still open.

> Not to be confused with `docs/AGENTS.md`, which is the *user* guide for running AI agents **on** Weave boards.

## 1. What this is

Weave is a self-hosted kanban server: Go + HTMX + SQLite, a clean-room reimplementation of Planka (including the Pro
feature list from planka.app/pro). Single binary, one data directory, **one replica only** (in-process SSE hub, SQLite).
Humans sign in with **OIDC only** (no local passwords). Machines (the `weave` CLI, AI agents) use **API keys**.
Every capability is one registered *op*, so the browser UI, REST API (`/api/v1`) and CLI always have the same features.

- Repo: `https://github.com/crueber/weave.git` (GitHub mirror - development happens upstream), branch `main`. Releases: `v0.2.1` is the latest tag; `main` is ahead of it (header consolidation, board template library, agent prompt family with admin/project/board-editable texts and blocks, admin/project/user settings redesign, new demo).
- Planka licensing: this is **clean-room**. Never copy code, templates, CSS or assets from Planka. Behaviour/field names only.
- Avatars: DiceBear "Landscape" (CC0), generated in-process in `internal/domain/auth/identicon.go`. Never call the DiceBear API.

### Standing preferences from the maintainer (do not re-litigate)
- **Design**: minimal but beautiful. **No gradients anywhere** (not in CSS, SVG, logo, backgrounds). Light and dark themes
  must both look great. Judge looks by *reading screenshots from a real browser*, not by assumption.
- **UX**: composers keep you typing (Enter creates and stays focused); card details open only on an explicit click/Enter,
  never after creating a card. Live updates must never clobber what the user is typing. Popovers must never be clipped.
- **Auth**: OIDC only. IdP groups own roles (`weave-admins`, `weave-users`, `weave-workers`, `weave-guests`; roles
  admin, boardUser, worker, guest; team groups `weave-team-<name>`). Do not add local credentials.
- **Ownership**: projects and boards belong to the people who create them and they manage them. Any human admin/boardUser creates
  projects (and manages them); each project chooses whether only its managers or all its members create boards; a non-manager
  creator becomes the board's owner (Manage on that board only). There is no project-owner role (removed on purpose).
- **Admins see only their own things**: the `admin` role is SYSTEM administration (users, groups, SSO, settings, backup, global
  webhooks, agent accounts, ownerless-project recovery) and gives **no content access**: an admin sees and manages exactly the projects
  and boards they own or are members of. New admin ops are system-only; content access comes only from membership/ownership, never
  from the role (`authz.SystemAdmin` is the one predicate; `internal/authz/nobypass_test.go` fails on any new admin-role decision
  until it is allowlisted with a system reason). No impersonation until the maintainer asks for it.
- **Security work**: audit first, present findings, let the maintainer choose anything that costs usability. Simple fixes with
  no functional impact can just be done. Keep Weave readily usable; limits are generous and env-tunable. Use the advisor
  tool for security and beauty reviews.
- **Do not reuse anything from the maintainer's `roundtable` repo** (its CI and tooling "doesn't work well").
- Code of Conduct: [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md) (same as Spindle's): no political advocacy in project spaces, from anyone. Keep demo data, seed text, sample templates and docs free of it.
- Commit/push/tag only when asked. Releases are tagged by the maintainer's request. Commit messages end with the
  `Co-Authored-By: Claude ...` trailer given in the session.

## 2. Repository map

```
cmd/weave            server + full CLI (used by Docker)          cmd/weave-cli      slim CLI (no server/SQLite), what releases ship
cmd/weave-release    release packager (CI and scripts/release-local.sh)  cmd/weave-skillgen  generates the agent skill + CLI snapshot
internal/ops         THE op registry (Register[In,Out], Bind, Env, Emit). Start here.
internal/authz       all permission rules (BoardLevel, RequireBoard/Card/Project, IsBoardMember, RoomAllowed)
internal/db          SQLite (modernc, WAL), embedded migrations, NewID/Now/PositionGap
internal/domain/     auth  projects  boards  cards  cardextras  notify  admin      (vertical slices: ops + UI + tests)
internal/events      durable event envelopes (actions table), CardSnap       internal/realtime  in-process hub, listener registry
internal/server      mux wiring, security headers, CSP, CSRF, rate limits, SSE (`sse_events.go`), HSTS, authz sweep test
internal/web         HTMX plumbing (web.Route/RoutePublic, templates/, static/)   internal/cli  schema-driven CLI (never imports internal/ops); `weave mcp` = mcp_*.go (MCP connector over stdio)
internal/agentprompt  agent prompts (leaf, golden tests): editable token texts in defaults/*.txt + editable shared blocks in blocks/*.txt (blocks.go); op `agent.prompt` lives in boards/agentprompt_op.go; `weave agent prompt`
internal/skillgen    hand-written skill sources (src/)   skills/  GENERATED skill + compact instructions (embedded in the CLI)
internal/imgguard    image decode guard   internal/relsign  release signing   internal/i18n  en.json + template func t
internal/testutil    Env(t, principal) with a temp SQLite DB   internal/all  blank-imports every domain (tests that render templates)   internal/version  build version
scripts/             e2e.sh  ui-smoke.sh  csp-smoke.mjs  notify-live-smoke.mjs  check-contrast.py  release-local.sh  install.sh/.ps1  update-pins.sh
scripts/publish/    workflow/CI files shipped to the GitHub mirror (used by publish-to-github.sh)   scripts/gen-badges.py  docs/badges/*.svg
scripts/demo/        seed.sh + seed.mjs (fictional data, throwaway instance), shots.mjs + pngopt.py (docs/screenshots), record.mjs + encode.py (README demo webm/webp, site poster; max 60 s); re-seed between shots and record; see the header comments
site/                the project website (static GitHub Pages: index.html, style.css, site.js, theme-init.js, img/); no external requests, CSP self
scripts/site-stage.sh  stages site/ + docs/demo.webm into one dir (make site / make site-preview; the Pages workflow runs it too)
scripts/site/        seed.sh + seed.mjs (fictional site data), shots.mjs (site screenshots), webp.py, check-contrast.py (site tokens)
scripts/publish/     GitHub workflow templates installed by publish-to-github.sh (*.yml glob): ci, cli-release, docker-image, pages
docs/                DESIGN.md (contract)  DESIGN-SYSTEM.md (tokens)  CLI.md  AGENTS.md (agent user guide)  WEBHOOKS.md  MAINTENANCE.md  FEATURES.md  AGENT-TARGETS.md (coding-agent paths + sources)
.forgejo/workflows/ci.yml   runner/  (Forgejo runner compose + config)
```

Domain ownership and **migration number ranges** are in `docs/DESIGN.md`. Existing migrations: `0001_core` (never edit),
`0100/0101/0102/0103` auth (`0102` turns "Watch cards I create" on for everyone, `0103` turns the removed projectOwner role into boardUser), `0200/0201/0202/0203` projects (0201 project agent prompt text, 0202 `board_creation` owners|members, 0203 `project_recoveries` audit trail of the admin recovery path), `0301/0302/0303/0304/0305` boards (colours, column descriptions, column WIP limit, template visibility, board agent prompt text; `0306` drops the board card-type settings, `0309` card counter on for existing boards, `0310` column auto-archive `lists.archive_after`, `0307` indexes `lists(board_id)` for board statistics, `0308` column watches `list_subscriptions`), `0400/0401/0402/0403/0404` cards (`0401` drops `cards.type`, `0402` adds `card_subscriptions.muted`, `0403` closes cards that sit in closed columns, `0404` `card_placements`: one row per board a card is on, home included, backfilled; mirror + guard triggers, see DESIGN.md "Placements"), `0500` cardextras, `0600` notify, `0800/0801` agents/events (`0801` `actions.origin_id`: one row per board of a fanned-out card event, see DESIGN.md "Placements" events),
`0900` api-key expiry, `0950` attachment index, `0110` auth `users.created_by` (owner-created agents), `0111` auth `admin_audit` (admin audit log), `0610` notify webhook `created_by` + `strict_dest`. Add new files in the owning domain's range (or the next free number); migrations
run in filename order, once, inside one transaction. **Never edit an applied migration**; add a new one.

## 3. Invariants (break these and tests or the maintainer will push back)

1. **One op registry.** A capability is `ops.Register[In,Out](ops.Op{Name,Method,Path,Auth,UI,...}, fn)` in an `init()`.
   REST, CLI and UI all derive from it. UI handlers mutate via `c.Do("op.name")` / `ops.Call`, never raw SQL writes.
   Every op sets `UI:` (a real route like `"POST /ui/cards/{id}/move"`) or `exempt:<reason>`. Names are `noun.verb`.
2. **Input tags**: `path:"id"` `query:"x"` `json:"x"` `required:"true"` `desc:"..."`; uploads are `*ops.File`; raise `Op.MaxBody`
   only for ops that really take big bodies. Errors: `ops.NotFound/Forbidden/Invalid/Conflict`. Missing access => **not_found**.
3. **Authorization lives in `internal/authz`** and is checked in the handler. Never invent a second permission path.
   New public/self ops go on the allowlist in `internal/server/authz_sweep_test.go` *with a reason*.
3a. **The UI asks `authz` capabilities, never re-derives them.** Pages decide buttons, links and settings sections from `authz.Home/Project/Board/Card`
   (`internal/authz/caps.go`), not from `Role == "admin"` or `Level >= X`. A new control gets a `data-cap="..."` hook and one row in the capability
   matrix (`internal/server/caps_matrix_test.go`: shown == capability == the real op's answer, for eleven personas). If an op's rule changes, change
   the shared predicate in authz so the op and the struct move together.
4. **After any op change run `make skill` and commit the result** (`skills/weave/**`, `internal/cli/snapshot.json`).
   `go test ./cmd/weave-skillgen` fails otherwise. Op summary/`desc:` text is user-facing documentation for agents.
5. **Emit events after commit**: `ops.Emit(e, ops.Event{...})`; it writes the `actions` row (durable, replayable) and fans out
   to SSE/webhooks/notifications. Never publish inside a transaction. A card event is recorded once per board the card is on
   (card-level) or on its own board only (placement events: `events.PlacementScoped`: every `cardMove` of ONE placement; a
   `cardMove` that moves every placement, `card.archive`/`card.restore`, carries `events.CardLevel`), each copy with that board's
   view and a shared `origin`; a writer that DELETES a card or a placement reads its boards before the write and passes them as
   `ops.Event.Boards` (also used to keep one board's vocabulary, labels and board-wide fields, on that board). Never put another
   board's ids in `Data`.
6. **IDs** `db.NewID()` (monotonic); **times** `db.Now()` (RFC3339 UTC); **positions** REAL with `db.PositionGap`; writes in
   `e.DB.Tx`. Do not write SQL against tables you do not own; call the owner's ops or exported funcs (see each `doc.go`).
7. **CSP is enforced** (`script-src 'self'`, no `unsafe-eval`). No inline `<script>`, no inline `on*=` handlers, no `javascript:`
   URLs, no `hx-vals='js:'`, no `hx-on`, no htmx trigger filters. Use `data-*` attributes + delegated listeners in static JS.
   `internal/web/csp_lint_test.go` enforces this. Dynamic `style=` values must be validated (hex colours go through
   `boards.NormalizeColor` helpers) because `style-src` allows inline styles.
8. **htmx runs with `allowEval=false`** (meta tag in the layout). Card fragments load `<script src>` files, so `allowScriptTags` stays on.
9. **Secrets are write-only** in ops/events/webhook payloads (`hasSecret`, `hasAccessToken`); notification-service URLs never go to events.
10. **No state-changing GETs** (logout and `card.next` are POST). The mux sits behind `http.CrossOriginProtection`.
11. **Text goes through escaping.** Markdown is escaped first (`cards/md.go`), links limited to http/https/mailto. `template.HTML/CSS/URL`
    only for values you generated and validated.
12. **Keep limits generous and env-tunable** (README config table). `0` means off. Do not add a global `WriteTimeout` (SSE).
13. **Write the placement, never the mirrored columns on `cards`.** A card's board, column, position, previous column and
    `list_changed_at` live on `card_placements` (migration 0404); `cards.board_id/list_id/position/prev_list_id/list_changed_at`
    mirror the HOME placement through triggers and a guard trigger aborts any other write to them. Move a card with
    `UPDATE card_placements ... WHERE card_id=? AND board_id=?` (or `list_id=?`) and keep `updated_at`/other card fields on `cards`
    in the same transaction; never pass `"cards"` to `boards.PlaceBetween`/`Rebalance` (use `boards.PlaceCardBetween`). Card access
    is `authz.CardAccessFor`/`RequireCard`/`RequireCardOn` (max over viewable placements), never a card's `board_id` + `RequireBoard`.
    **Read a board through its placements** (`card_placements p JOIN cards c ... WHERE p.board_id=?`, column/position from `p`,
    child rows joined on `p` too) and show only that board's labels (`db.LabelOnBoard`); a single card is read on its context board
    (`loadCardOn` + `CardAccessFor(...).BoardID`), many cards with `authz.CardAccesses`. Never name or count a hidden placement.
    `internal/db/placements_scan_test.go` and `testutil.CheckPlacements` (every `testutil.Env` cleanup) enforce it.

## 4. Everyday commands

```sh
make test                       # go vet + go test ./...   (use -count=1; add -race for server, cli, auth)
make skill / make skill-check   # regenerate / verify the agent skill + CLI snapshot
make build | make cli           # full binary | slim CLI
make release-local              # cross-build the whole release matrix into dist/ (same packager as CI)
python3 scripts/check-contrast.py   # AA contrast for tokens, tints, filled tiles, both themes (must show 0 failures)
```

Run a local instance (OIDC must be *configured* but is never contacted unless you click sign-in):

```sh
export WEAVE_DATA_DIR=/tmp/wv WEAVE_ADDR=:18080 WEAVE_BASE_URL=http://localhost:18080 \
  WEAVE_SECRET_KEY=$(openssl rand -hex 32) WEAVE_OIDC_ISSUER=http://localhost:9 WEAVE_OIDC_CLIENT_ID=w WEAVE_OIDC_CLIENT_SECRET=x
go build -o /tmp/weave ./cmd/weave && /tmp/weave serve &
KEY=$".*/\1/')   # host-only break-glass
WEAVE_URL=http://localhost:18080 WEAVE_TOKEN=$KEY go run ./cmd/weave-cli whoami
```

For browser testing without OIDC, insert a session row into SQLite: the token is stored as `sha256(token)` hex in
`sessions.token_hash` (see `internal/domain/auth/core.go`), cookie `weave_session=<token>`. Free the port afterwards with
`fuser -k PORT/tcp` (`pkill -f` can kill your own shell).

## 5. How to...

**Add an op (feature)**: put it in the owning domain; register with accurate `UI:`; authorize with `authz`; emit an event after the
write; add `desc:` tags; add tests (`internal/testutil.Env`); add the UI route/fragment (below); run `make skill`; update
`docs/FEATURES.md` if it is a user-visible feature; if it is public or self-service, update the authz sweep allowlist.

**Add a button or settings section**: compute visibility from the capability struct (add a field to `authz.BoardCaps` etc. that calls the op's own
predicate), put `data-cap="name"` on the element, add a row to `capRows` in `internal/server/caps_matrix_test.go`. Never render a section that only
says "only owners can ..."; leave it out of the navigation instead.

**Add a page or fragment**: `web.Route("GET /x", handler)` (or `RoutePublic`). Templates live in `internal/web/templates/`, named
with the domain prefix (`boards_*.html`, shared partials start with `_`). A page defines `{{define "page"}}`; fragments are named
blocks rendered with `c.RenderBlock(page, block, data)`. Mutations post to a UI route that calls the op. Lazy sections use
`hx-get ... hx-trigger="load"`. Put CSS in a domain stylesheet that uses the **design tokens** (`docs/DESIGN-SYSTEM.md`); never hardcode
colours, never use gradients. Put JS in a static file (no inline), keep it small and vanilla; prefer event delegation.

**Change the schema**: new migration file in the right range; keep migrations additive; provide defaults; update export/import and
board templates if the data should travel; add tests that run through `db.Open` (migrations apply on every test DB).

**Put a card into a column** (any new create/move/bulk/import path): write the row, then call `boards.Landed(ctx, tx, boards.Landing{CardID, ListID}...)`
in the same transaction; never compute `is_closed` yourself (rule and callers: `docs/DESIGN.md` "Closed cards"). Keep its `LandResult`: it also
carries what the column's auto-archive rule archived (`docs/DESIGN.md` "Archive after N"); after commit emit your own events, then
`boards.EmitAutoArchived(e, res)`. A bulk re-layout that must keep closed cards closed and must not trim (board.template.apply) uses
`boards.LandedKeepClosed` instead (`docs/DESIGN.md` "Board templates").

**Add a setting**: env var in `internal/config` or a `config` table key (admin UI: `notify/config.go` + `ui.go`); document it in the README
config table and `.env.example`/compose files; secrets are write-only.

**Change an agent prompt**: the wording is plain text with `{tokens}` (never `text/template` on user text). Prompts are `internal/agentprompt/defaults/*.txt`, shared blocks `blocks/*.txt`; a new token needs `Tokens(scope)`/`blocks.go` entries and a docs/AGENTS.md table row. Admin overrides are config keys `agentprompt.<scope>` and `agentblock.<name>` (env `WEAVE_AGENTPROMPT_*`/`WEAVE_AGENTBLOCK_*`); projects and boards carry `agent_prompt`. With no overrides the goldens in `testdata/` must stay byte-identical (`go test ./internal/agentprompt`).

**Touch realtime/agents**: see `docs/DESIGN.md` "Agents and the event stream". Event ids are zero-padded `seq`; replay and live
share one path (`events.Query`); `lists=` registers a listener; claims are leases (`card.claim|release|renew|next`).

**Add a coding-agent target** (`weave skill install`): add one entry to `agentTargets` in `internal/cli/skilltargets.go` (ID, format
`fmtSkillDir|fmtSharedMD|fmtOwnedMD`, user path func honoring the tool's env override, project path, detection dirs/binaries). Only set
`Verified: true` after reading the tool's *official* docs; record tool, paths, detection, source URL and date in `docs/AGENT-TARGETS.md`.
Unverified targets are opt-in (never auto, never in `all`). If the tool reads `~/.agents/skills`, reuse `universalUser` (it is written
once). Add a row to the path table in `internal/cli/skillinstall_test.go`. Content is never hand-written per tool: the full skill is
`skills/weave/**`, the compact form is `internal/skillgen/src/INSTRUCTIONS.md` -> `skills/instructions.md`; run `make skill`.

**Add a curated MCP tool**: append the op *name* to `mcpCurated` in `internal/cli/mcp_tools.go` (keep it to ~30 agent-grain ops; everything
else is reachable through `weave_ops`/`weave_call`). Name, schema, description and annotations are derived from the op schema; do not hand-write
them. Then `go test ./internal/cli -run Golden -update` only if annotations changed on purpose, and `make skill` (the tool table in
`skills/weave/references/mcp.md` is generated). File inputs/outputs are detected from `in:"file"` fields and the schema's `binary`.

**Touch the CLI**: it is schema-driven from `GET /api/v1/_ops` and must not import `internal/ops` (a test checks the slim binary does
not link SQLite/server). Shortcuts (`next`, `listen`, `agent run`, `card claim`...) live in `internal/cli`; exit code 10 = nothing to do.

## 6. Verifying work (what "done" means here)

| Check | How |
|---|---|
| Unit/integration | `go vet ./... && go test ./... -count=1` (add `-race` for server/cli/auth) |
| Authorization | `internal/server/authz_sweep_test.go` walks every op and web route as anonymous/outsider/read-only |
| CSP | `internal/web/csp_lint_test.go` + `scripts/csp-smoke.mjs` (real Chromium walk of every page; must report 0 violations) |
| Live bell / realtime | `scripts/notify-live-smoke.mjs` (two users in Chromium after `scripts/demo/seed.sh`; re-mint the demo keys first, seed revokes them) |
| Contrast | `python3 scripts/check-contrast.py` |
| End to end | `scripts/e2e.sh` against a running server (CLI-driven, needs an admin API key) |
| Skill fresh | `make skill-check` |
| Looks | screenshots from a **real Chromium**, both themes, desktop and 390px; *read the PNGs* |

**Browsers**: Playwright's Chromium is on the maintainer's machine (`~/.cache/ms-playwright/chromium_headless_shell-*`, playwright-core
under `~/.cache/pwtest`); it needs `LD_LIBRARY_PATH=/home/crueber/brlibs/usr/lib/x86_64-linux-gnu` and a fontconfig with real fonts.
Locations can change; discover them. The `obscura` headless browser (needs `--allow-private-network` before `serve`) is **not**
reliable for layout: no `<dialog>` top layer, no htmx (`XPathEvaluator` missing so lazy fragments never load), `<details>` does not
toggle, stacking/flex quirks, SSE named events never arrive. Several "bugs" it showed (menu bleed-through, avatar overflow) did not
exist in Chromium, and CSP/popover behaviour must be judged in Chromium.

## 7. Security checklist for any change

- New op: right `Auth`, authz check, body cap (default 2 MiB), rate-limit class if expensive, sweep allowlist entry only with a reason.
- **Admin-side actions that change access leave an audit row**: write it with `writeAudit` (`auth/audit.go`, table `admin_audit`) on the
  change's own transaction, snapshot names/e-mails as text, never store a key, hash, token or secret, and log an `AUDIT` line after commit.
- New user-controlled output: escaped; if it becomes a URL, scheme-check it; if it goes into `style`, validate it.
- New outbound request: go through the SSRF checks (`notify/ssrf.go`) unless admin-only; cap response size. If Weave's own HTTP client
  makes the request for a non-admin, use the strict dialer (`notify/webhook_ssrf.go`: resolve, check every address, dial the checked IP,
  no proxy, re-checked on redirects) like board owners' webhooks.
- Content (projects, boards, cards, agents' board access, board webhooks) is authorized by ownership/membership in `internal/authz`, never by
  role: not even for admins. An admin console page shows counts and accounts, never names or links of other people's content
  (`scripts/csp-smoke.mjs` admin walk, `internal/server/admin_persona_test.go`).
- New file upload: size cap, type handling (SVG/HTML never inline from the app origin), image decode through `imgguard`.
- New secret: write-only in ops, absent from events/logs/webhook payloads.
- Do not weaken: CSP, CSRF wrapper, security headers, session/idle logic, identity binding (`users.oidc_sub` is `issuer|sub`; an email match on an
  account bound to another subject is denied; `user.relink` / `weave admin relink` fix migrations).
- Process for a security pass: parallel read-only reviewers (authn/authz, injection/uploads/SSRF, CLI/CI/supply chain), run
  `govulncheck` and `go list -m -u all`, verify top findings locally, present a table (finding, severity, evidence, fix, usability
  impact), do the no-impact fixes, ask about the rest. The previous pass is summarised in `docs/DESIGN.md` "Security model".
- Admin recovery of ownerless projects goes through `project.orphans|reassign` (authz `RequireProjectRecovery`, never `ProjectLevel`) and
  the host commands `weave admin preflight|adopt`; every recovery writes `project_recoveries` in the same transaction. Do not add another
  way for an admin to take over a project, and do not let reassign accept a project that still has an active manager.
- Known accepted risks: `WEAVE_SECRET_KEY` does not encrypt the database (webhook secrets etc. are plaintext in `weave.db`); shoutrrr dials on
  its own (DNS rebinding between our check and its connect, or redirects, are not stopped); RFC1918 notification targets are allowed by default.
  Not done by choice: CLI token pin guard, CI runner socket hardening.

## 8. CI, release, Forgejo

- `.forgejo/workflows/ci.yml`: `test` (golang image, plain git checkout, **no JavaScript actions**: job images have no node), `package`
  (on push: buildah builds and pushes `ghcr.io/crueber/weave:{latest,<sha>,<tag>}`), `release` (on `v*` tags: `cmd/weave-release`
  cross-builds the slim CLI for linux/darwin/windows on amd64+arm64, server binaries for linux, archives, `install.sh/.ps1`, `SHA256SUMS.txt`
  and, when `RELEASE_SIGNING_KEY` is set, `SHA256SUMS.txt.sig`; then publishes via the Forgejo API).
- **buildah pitfall (bit us)**: the pinned buildah image (1.45) does not read `/etc/containers/storage.conf` in later steps. Every
  `buildah` call must pass `--storage-driver vfs`, or push says "image not known". Build needs `--isolation=chroot`.
- Secrets: `REGISTRY_USER`, `REGISTRY_TOKEN` (package + repository write), `RELEASE_SIGNING_KEY` (base64 ed25519 seed; if missing the release
  is published **unsigned** with a warning). Images are pinned by digest: `scripts/update-pins.sh [--check]`, `docs/MAINTENANCE.md`.
- Release: confirm `main` CI is green, `git tag -a vX.Y.Z -m ... && git push origin vX.Y.Z`, wait for `release:success`, check the asset list (17 assets)
  and the `.sig`. A signature is added only at release time; to sign an existing release cut a new patch tag.
- **GitHub is published from Forgejo only**: the maintainer runs `.forgejo/workflows/publish-github.yml` (Actions -> publish to github, secret
  `PUBLISH_GITHUB_TOKEN`; choose the patch/minor/major bump, or none plus an exact tag) when the mirror should move. Never run `scripts/publish-to-github.sh --apply` or push to
  GitHub from an agent session: an agent's job ends at the Forgejo push.
- Forgejo CLI is `tea` (aliased `tea-cli`; `repos create` wants a TTY). The Actions API has run status (`/api/v1/repos/crueber/weave/actions/tasks`) but
  **no job logs**; ask the maintainer for the failing step's log (screenshots work) or reproduce locally.
- Runner: `runner/docker-compose.yml` + `config.yml` (label `docker`). The repo config still mounts the host Docker socket into jobs; the buildah build no
  longer needs it (maintainer has not chosen to remove it).
- Deploying: `docker-compose.prod.yml` pulls the image (hardened: read-only rootfs, `/tmp` tmpfs, caps dropped). Required env: OIDC trio, `WEAVE_SECRET_KEY`,
  `WEAVE_BASE_URL`. Sessions default to 24 hours (`session.hours` / `WEAVE_SESSION_HOURS`); an expired session redirects to `/login`, which forwards to the OIDC provider. Back up with the `admin.backup` op (VACUUM INTO) **plus** `/data/files`.

## 9. Working in parallel (what worked)

Large changes were done by splitting into **vertical slices owned by non-overlapping files**, each agent in its own `git worktree`, then merged
and fully tested together. Tips: publish the interface contract (names, shapes) first; give each agent explicit file ownership; merge the slice
others depend on first; after every merge run `go mod tidy`, `go vet`, `go test ./... -count=1`, `make skill-check`; expect conflicts in
`go.mod/go.sum`, shared templates, `README.md` config table, `docs/DESIGN.md`, and `snapshot.json`/skill references (regenerate, do not hand-merge).
Isolated test packages that render templates must link every domain (`linkall_test.go` imports `internal/all`) because templates are global.

## 10. Open items and ideas (see also README "Status and known gaps")

- **One card on several boards (placements)** is complete (API, CLI, MCP, events and UI; docs/DESIGN.md "Placements" has the model,
  the maintainer decisions and the UI). The UI maps its context board (`board`, never `boardId`) onto the op's `boardId` only for the
  placement-scoped ops (`cards.doOn`/`contextOps`). Open: "Remove from this board" is offered only when another placement is VISIBLE
  (the op counts hidden ones too; maintainer to confirm); live refresh of an open sheet swaps the Boards row, crumb and menu only.
- **Card duplicate does not copy tasks/attachments/custom fields**: `cards.DuplicateHooks` exists (`cards/ops_clone.go`) but cardextras never registers a hook. Highest-value gap.
- Drag-and-drop, the card sheet and settings interactions were verified in headless Chromium by scripts, not on touch devices or other browsers.
- OIDC and SMTP verified only against fakes; `install.ps1` never executed on Windows; macOS/Windows binaries built but not run.
- PDF export is print-ready HTML unless `WEAVE_PDF_CMD` is set; map view is a coordinate plot with OSM links; worker/guest roles mostly behave like other non-admins
  (beyond directory visibility); Planka's private/shared project type is not implemented; board colour changes do not live-update other open tabs.
- Generated avatars appear only in the navbar; other places still show initials (`/avatars/{id}` already falls back to the generated one).
- Possible follow-ups from the audit: sealing webhook secrets/service URLs at rest, per-user storage usage on the settings page, CLI token pin guard,
  CI runner hardening, signing key rotation procedure.
- Update `docs/FEATURES.md`, the README gaps list and this file when you finish something above.
