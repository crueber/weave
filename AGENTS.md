# AGENTS.md: working on Weave

Read this first, then `docs/DESIGN.md` (the architecture contract) and skim `docs/FEATURES.md`. This file is the
fast path: what the project is, the rules that have bitten before, how to build/verify/release, and what is still open.

> Not to be confused with `docs/AGENTS.md`, which is the *user* guide for running AI agents **on** Weave boards.

## 1. What this is

Weave is a self-hosted kanban server: Go + HTMX + SQLite, a clean-room reimplementation of Planka (including the Pro
feature list from planka.app/pro). Single binary, one data directory, **one replica only** (in-process SSE hub, SQLite).
Humans sign in with **OIDC only** (no local passwords). Machines (the `weave` CLI, AI agents) use **API keys**.
Every capability is one registered *op*, so the browser UI, REST API (`/api/v1`) and CLI always have the same features.

- Repo: `https://github.com/crueber/weave.git` (GitHub mirror - development happens upstream), branch `main`. Releases: `v0.2.0` is the latest.
- Planka licensing: this is **clean-room**. Never copy code, templates, CSS or assets from Planka. Behaviour/field names only.
- Avatars: DiceBear "Landscape" (CC0), generated in-process in `internal/domain/auth/identicon.go`. Never call the DiceBear API.

### Standing preferences from the maintainer (do not re-litigate)
- **Design**: minimal but beautiful. **No gradients anywhere** (not in CSS, SVG, logo, backgrounds). Light and dark themes
  must both look great. Judge looks by *reading screenshots from a real browser*, not by assumption.
- **UX**: composers keep you typing (Enter creates and stays focused); card details open only on an explicit click/Enter,
  never after creating a card. Live updates must never clobber what the user is typing. Popovers must never be clipped.
- **Auth**: OIDC only. IdP groups own roles (`weave-admins`, `weave-project-owners`, `weave-users`, `weave-workers`,
  `weave-guests`, team groups `weave-team-<name>`). Do not add local credentials.
- **Security work**: audit first, present findings, let the maintainer choose anything that costs usability. Simple fixes with
  no functional impact can just be done. Keep Weave readily usable; limits are generous and env-tunable. Use the advisor
  tool for security and beauty reviews.
- **Do not reuse anything from the maintainer's `roundtable` repo** (its CI and tooling "doesn't work well").
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
internal/web         HTMX plumbing (web.Route/RoutePublic, templates/, static/)   internal/cli  schema-driven CLI (never imports internal/ops)
internal/skillgen    hand-written skill sources (src/)   skills/  GENERATED skill + compact instructions (embedded in the CLI)
internal/imgguard    image decode guard   internal/relsign  release signing   internal/i18n  en.json + template func t
internal/testutil    Env(t, principal) with a temp SQLite DB
scripts/             e2e.sh  ui-smoke.sh  csp-smoke.mjs  check-contrast.py  release-local.sh  install.sh/.ps1  update-pins.sh
docs/                DESIGN.md (contract)  DESIGN-SYSTEM.md (tokens)  CLI.md  AGENTS.md (agent user guide)  WEBHOOKS.md  MAINTENANCE.md  FEATURES.md  AGENT-TARGETS.md (coding-agent paths + sources)
.forgejo/workflows/ci.yml   runner/  (Forgejo runner compose + config)
```

Domain ownership and **migration number ranges** are in `docs/DESIGN.md`. Existing migrations: `0001_core` (never edit),
`0100/0101` auth, `0200` projects, `0301` boards (colours), `0400` cards, `0500` cardextras, `0600` notify, `0800` agents/events,
`0900` api-key expiry, `0950` attachment index. Add new files in the owning domain's range (or the next free number); migrations
run in filename order, once, inside one transaction. **Never edit an applied migration**; add a new one.

## 3. Invariants (break these and tests or the maintainer will push back)

1. **One op registry.** A capability is `ops.Register[In,Out](ops.Op{Name,Method,Path,Auth,UI,...}, fn)` in an `init()`.
   REST, CLI and UI all derive from it. UI handlers mutate via `c.Do("op.name")` / `ops.Call`, never raw SQL writes.
   Every op sets `UI:` (a real route like `"POST /ui/cards/{id}/move"`) or `exempt:<reason>`. Names are `noun.verb`.
2. **Input tags**: `path:"id"` `query:"x"` `json:"x"` `required:"true"` `desc:"..."`; uploads are `*ops.File`; raise `Op.MaxBody`
   only for ops that really take big bodies. Errors: `ops.NotFound/Forbidden/Invalid/Conflict`. Missing access => **not_found**.
3. **Authorization lives in `internal/authz`** and is checked in the handler. Never invent a second permission path.
   New public/self ops go on the allowlist in `internal/server/authz_sweep_test.go` *with a reason*.
4. **After any op change run `make skill` and commit the result** (`skills/weave/**`, `internal/cli/snapshot.json`).
   `go test ./cmd/weave-skillgen` fails otherwise. Op summary/`desc:` text is user-facing documentation for agents.
5. **Emit events after commit**: `ops.Emit(e, ops.Event{...})`; it writes the `actions` row (durable, replayable) and fans out
   to SSE/webhooks/notifications. Never publish inside a transaction.
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

**Add a page or fragment**: `web.Route("GET /x", handler)` (or `RoutePublic`). Templates live in `internal/web/templates/`, named
with the domain prefix (`boards_*.html`, shared partials start with `_`). A page defines `{{define "page"}}`; fragments are named
blocks rendered with `c.RenderBlock(page, block, data)`. Mutations post to a UI route that calls the op. Lazy sections use
`hx-get ... hx-trigger="load"`. Put CSS in a domain stylesheet that uses the **design tokens** (`docs/DESIGN-SYSTEM.md`); never hardcode
colours, never use gradients. Put JS in a static file (no inline), keep it small and vanilla; prefer event delegation.

**Change the schema**: new migration file in the right range; keep migrations additive; provide defaults; update export/import and
board templates if the data should travel; add tests that run through `db.Open` (migrations apply on every test DB).

**Add a setting**: env var in `internal/config` or a `config` table key (admin UI: `notify/config.go` + `ui.go`); document it in the README
config table and `.env.example`/compose files; secrets are write-only.

**Touch realtime/agents**: see `docs/DESIGN.md` "Agents and the event stream". Event ids are zero-padded `seq`; replay and live
share one path (`events.Query`); `lists=` registers a listener; claims are leases (`card.claim|release|renew|next`).

**Add a coding-agent target** (`weave skill install`): add one entry to `agentTargets` in `internal/cli/skilltargets.go` (ID, format
`fmtSkillDir|fmtSharedMD|fmtOwnedMD`, user path func honoring the tool's env override, project path, detection dirs/binaries). Only set
`Verified: true` after reading the tool's *official* docs; record tool, paths, detection, source URL and date in `docs/AGENT-TARGETS.md`.
Unverified targets are opt-in (never auto, never in `all`). If the tool reads `~/.agents/skills`, reuse `universalUser` (it is written
once). Add a row to the path table in `internal/cli/skillinstall_test.go`. Content is never hand-written per tool: the full skill is
`skills/weave/**`, the compact form is `internal/skillgen/src/INSTRUCTIONS.md` -> `skills/instructions.md`; run `make skill`.

**Touch the CLI**: it is schema-driven from `GET /api/v1/_ops` and must not import `internal/ops` (a test checks the slim binary does
not link SQLite/server). Shortcuts (`next`, `listen`, `agent run`, `card claim`...) live in `internal/cli`; exit code 10 = nothing to do.

## 6. Verifying work (what "done" means here)

| Check | How |
|---|---|
| Unit/integration | `go vet ./... && go test ./... -count=1` (add `-race` for server/cli/auth) |
| Authorization | `internal/server/authz_sweep_test.go` walks every op and web route as anonymous/outsider/read-only |
| CSP | `internal/web/csp_lint_test.go` + `scripts/csp-smoke.mjs` (real Chromium walk of every page; must report 0 violations) |
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
- New user-controlled output: escaped; if it becomes a URL, scheme-check it; if it goes into `style`, validate it.
- New outbound request: go through the SSRF checks (`notify/ssrf.go`) unless admin-only; cap response size.
- New file upload: size cap, type handling (SVG/HTML never inline from the app origin), image decode through `imgguard`.
- New secret: write-only in ops, absent from events/logs/webhook payloads.
- Do not weaken: CSP, CSRF wrapper, security headers, session/idle logic, identity binding (`users.oidc_sub` is `issuer|sub`; an email match on an
  account bound to another subject is denied; `user.relink` / `weave admin relink` fix migrations).
- Process for a security pass: parallel read-only reviewers (authn/authz, injection/uploads/SSRF, CLI/CI/supply chain), run
  `govulncheck` and `go list -m -u all`, verify top findings locally, present a table (finding, severity, evidence, fix, usability
  impact), do the no-impact fixes, ask about the rest. The previous pass is summarised in `docs/DESIGN.md` "Security model".
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

- **Card duplicate does not copy tasks/attachments/custom fields**: `cards.DuplicateHooks` exists (`cards/ops_clone.go`) but cardextras never registers a hook. Highest-value gap.
- Drag-and-drop, the card sheet and settings interactions were verified in headless Chromium by scripts, not on touch devices or other browsers.
- OIDC and SMTP verified only against fakes; `install.ps1` never executed on Windows; macOS/Windows binaries built but not run.
- PDF export is print-ready HTML unless `WEAVE_PDF_CMD` is set; map view is a coordinate plot with OSM links; worker/guest roles mostly behave like other non-admins
  (beyond directory visibility); Planka's private/shared project type is not implemented; board colour changes do not live-update other open tabs.
- Generated avatars appear only in the navbar; other places still show initials (`/avatars/{id}` already falls back to the generated one).
- Possible follow-ups from the audit: sealing webhook secrets/service URLs at rest, per-user storage usage on the settings page, CLI token pin guard,
  CI runner hardening, signing key rotation procedure.
- Update `docs/FEATURES.md`, the README gaps list and this file when you finish something above.
