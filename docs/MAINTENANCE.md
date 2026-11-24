# Maintenance

## Before upgrading to ownership-based permissions

This release removes the admin content bypass. The `admin` role is now **system administration only** (users, groups,
SSO, settings, backup, webhooks, agent accounts, recovery): administrators no longer manage, or even see, every project
and board, only the ones they manage or are members of, like everyone else. Projects and boards belong to their owners.
Someone else's board is "not found" for an admin; their home page, search, dashboard and notifications show only their
own projects and boards. There is no impersonation.

**For admin visibility the upgrade is one-way in practice**: once it runs, an admin can no longer open a project they are
not a member of to add themselves, so whatever access admins should keep must be in place *before* you upgrade (the
schema does not change, but do not plan on downgrading to fix access). The host's break-glass `weave admin mint-key --role
admin` still creates an account with system powers, and that account sees no content either. Ownerless projects stay
recoverable (step 3). Upgrading needs no extra step (the deploy just works). The preflight is an **optional** safety check
if you want to know in advance what an admin will stop seeing: run it for every admin (`weave admin preflight`) or the one you
care about (`weave admin preflight --email you@example.com`).

**1. Optional: run the preflight on the deployed data directory** (read-only; safe while the old server runs):

```sh
# with the NEW image, against the live volume (it never migrates or writes):
docker run --rm -v weave-data:/data -e WEAVE_DATA_DIR=/data IMAGE admin preflight   # the image ENTRYPOINT is weave
# or one admin only, or machine-readable:
weave admin preflight --email you@example.com
weave admin preflight --json
weave admin preflight --db /path/to/a/backup/weave.db
```

It opens the database with `mode=ro` + `query_only` (`db.OpenReadOnly`: no migrations, no journal-mode change, no file
created) and reads only columns that exist since the first release. If the volume is mounted read-only it falls back to
`immutable=1`, which does not see writes still in the WAL: stop the server first in that case. It prints:

- **(a) projects with no active manager**: every manager is deactivated, deleted, a guest or an agent account. Shows the
  board count, how many boards have no active board owner either, and the former managers.
- **(b) projects an admin will no longer see**: for each active human admin (or `--email`), projects where they are neither
  a project manager nor a member (direct or through a group) of any of its boards. Public boards do not count.
- **(c) boards only admins manage** (informational): every active manager of the board (its project's active managers plus
  its active board owners) is an admin. Nothing changes for those boards on upgrade, but nobody else can manage them.
- a one-line summary. Exit code `0` = nothing to fix in (a) and (b), `1` = action needed (or an error), `2` = usage.
  A warning on stderr means the database was opened `immutable` (see above).

**2. Fix what it reports while the bypass still exists** (on the old server, before upgrading): add yourself (or the right person) in Project settings >
Managers (`project.manager.add`) or as a board member; give ownerless projects a manager in **Admin console > Orphaned
projects** (`project.reassign`); add a non-admin manager or board owner to admin-only boards. Re-run until it exits 0,
or until what is left is intended.

**3. After the upgrade**, ownerless projects stay recoverable without the bypass. The admin Users page marks a person
who is the only active manager of projects ("sole manager") and warns, before you deactivate or delete them, how many
projects would be left without a manager. Recovery: Admin console > Orphaned projects
(`project.orphans`, `project.reassign`) lists only ids, names, creation dates, board counts and former managers, never
content, and refuses a project that still has an active manager. If no admin can reach the UI at all, use the host
break-glass:

```sh
docker compose exec weave weave admin adopt --project PROJECT_ID --email person@example.com
```

It makes that person (active, not a guest, not an agent account) a manager of the project, ownerless or not. Like
`mint-key` it opens the database normally (migrations apply), so run it with the binary of the deployed version. Every
recovery, from the page, the API or the host command, is written to the `project_recoveries` audit trail (who, which
project, to whom, when, the previous managers; it survives deletions) and an `AUDIT` line in the server log; read it in
the Recovery log on the same admin page or with `weave project recovery.list`.

## Admin audit log

Admin-side actions that change who can reach what leave a row in the `admin_audit` table (migration 0111), written in the
same transaction as the change, plus an `AUDIT` line in the server log: rotating an agent's API key (`agent.rotateKey`, by
its creator, a manager of its boards or an admin), adding a person to a group (`group.member.add` and the initial `memberIds` of `group.create`; every real
add, a self-add is flagged `self`; re-adding a member records nothing) and unlinking an SSO identity (`user.relink` and the host
command `weave admin relink`, recorded with source `host` and no actor). Rows keep names and e-mail addresses as text and
have no foreign keys, so they survive deleting the people they name; they never hold a key, a key hash, a token or an SSO
subject (a key rotation keeps only the key's display prefix). Read them in Admin console > **Audit log** (`/admin/audit`,
filter by action, older pages) or with `weave audit list [--action group.member.add] [--limit 50] [--before CURSOR]`
(human admins only). Project recoveries stay in their own Recovery log (above). The table is append-only and grows
slowly (one row per such change); nothing prunes it. Back it up with the database as usual.

## Card placements (migration 0404) and rolling it back

Migration `0404_card_placements.sql` adds `card_placements` (one row per board a card is on; today exactly one, the
card's home) and backfills one home row per existing card, drafts and card templates included, in the migration
transaction. It is additive: `cards.board_id/list_id/position/prev_list_id/list_changed_at` keep holding the home
placement (triggers mirror it), so the previous binary still reads correct data. To check an upgraded database (with
the server stopped, or against a copy):

```sh
sqlite3 weave.db "SELECT COUNT(*) FROM cards c WHERE (SELECT COUNT(*) FROM card_placements p WHERE p.card_id=c.id AND p.is_home=1)<>1"   # 0
sqlite3 weave.db "SELECT COUNT(*) FROM cards c JOIN card_placements p ON p.card_id=c.id AND p.is_home=1 WHERE p.list_id<>c.list_id OR p.board_id<>c.board_id OR p.position IS NOT c.position"   # 0
```

There are no down migrations. To go back to a binary older than 0404: **back up first** (`admin.backup` plus
`/data/files`), stop the server, run the snippet below on the database (the container image has no `sqlite3`; run it on
the volume from the host), then start the older binary. The guard trigger must go first: while it exists the older
binary's card moves fail with "write the placement". Deleting the `schema_migrations` row lets a later upgrade run 0404
again (a fresh backfill). Extra placements (once a card can be on several boards) are lost; each card keeps its home.

```sql
DROP TRIGGER IF EXISTS cards_placement_guard;
DROP TRIGGER IF EXISTS cp_home_mirror;
DROP TRIGGER IF EXISTS cp_home_rehome;
DROP TRIGGER IF EXISTS cp_home_ai;
DROP TABLE IF EXISTS card_placements;
DELETE FROM schema_migrations WHERE name='0404_card_placements.sql';
```

Migration `0801_action_origin.sql` only adds `actions.origin_id` (and a partial index): an older binary ignores the column
(its inserts name their columns). Rows of events that were fanned out to several boards stay as ordinary per-board activity.
Nothing to undo; to make a later upgrade re-run it you would also `DROP INDEX actions_origin`, `ALTER TABLE actions DROP COLUMN
origin_id` and delete its `schema_migrations` row, which is never needed.

## Pinned base images

Every base image is pinned by digest, so a build is reproducible and a compromised or re-tagged
upstream image cannot slip in silently. The tag stays in a comment next to the digest.

| Where | Image |
|---|---|
| `Dockerfile` (build stage) | `golang:1.27` |
| `Dockerfile` (runtime) | `gcr.io/distroless/static-debian13:nonroot` |
| `.forgejo/workflows/ci.yml` (`test`, `release`) | `golang:1.27` |
| `.forgejo/workflows/ci.yml` (`package`) | `quay.io/buildah/stable:latest` |
| `runner/docker-compose.yml` | `code.forgejo.org/forgejo/runner:6`, job label `node:20-bookworm` |

The digest is the multi-architecture index digest, so amd64 and arm64 builders resolve it alike.

### Bumping the pins

```sh
scripts/update-pins.sh           # prints tag@digest for every pinned image (registry metadata only)
scripts/update-pins.sh --check   # exit 1 and list STALE entries when the repo differs from the registry
```

1. Run `scripts/update-pins.sh` and replace the old `@sha256:...` in the files above with the printed values.
2. `docker build -t weave:pin-test .` and smoke test: the image must run as `nonroot`, report
   `healthy` (HEALTHCHECK runs `weave healthcheck`) and work with the hardening in `docker-compose.yml`
   (`read_only: true`, `cap_drop: ALL`, `/tmp` tmpfs).
3. Commit "Bump pinned images". Do this monthly or when a base-image CVE is announced; `--check` is cheap
   enough to run in a scheduled job.

To change a tag (for example `golang:1.28`) edit the `IMAGES` list in `scripts/update-pins.sh` too, so
`--check` keeps watching the right thing. The script needs `docker buildx`, `crane`, or `skopeo`.

## Release signing

`SHA256SUMS.txt` of every release is signed with ed25519 (`SHA256SUMS.txt.sig`, base64 of the raw 64-byte
signature). Only the Go standard library is used (`internal/relsign`), so the CI `golang` image needs no
extra tools.

One-time setup:

1. `go run ./cmd/weave-release -genkey -keyfile /safe/place/weave-release.key` writes the private seed (mode
   0600, refuses to overwrite) and prints the public key. The seed is never printed.
2. Put the public key in `internal/relsign/relsign.go` (`PublicKey`) and its SPKI form (also printed) in
   `scripts/install.sh` and `scripts/install.ps1`. `go test ./internal/relsign` fails when `install.sh` and
   the Go constant disagree.
3. Add the seed as the Forgejo Actions secret **`RELEASE_SIGNING_KEY`** (repository settings, Actions, Secrets).
   The `release` job signs when it exists and prints a warning (the release is published unsigned) when not.
   It then verifies the signature with the freshly built CLI before publishing.

Rotation: generate a new pair, replace the three embedded copies, update the secret, and publish a release.
Installers from older releases embed the old key and keep verifying older releases. Keep the seed offline or in
a secrets manager; anyone with it can publish releases that installers accept.

Verifying by hand: `weave verify-release SHA256SUMS.txt SHA256SUMS.txt.sig`, or with OpenSSL >= 3 (the
installer does this): decode the signature (`openssl base64 -d -A`) and run
`openssl pkeyutl -verify -rawin -pubin -inkey release-pub.pem -sigfile sig.bin -in SHA256SUMS.txt`.

Forks that sign with their own key pass `WEAVE_RELEASE_PUBKEY=/path/pub.pem` to `install.sh` /
`install.ps1`, and `--pubkey` to `weave verify-release`.

## Project website (GitHub Pages)

The site lives in `site/` (static HTML/CSS and two tiny scripts, no external requests). `scripts/site-stage.sh`
copies it and `docs/demo.webm` (kept once, in `docs/`) into one directory; `make site` stages to `dist/site`,
`make site-preview` also serves it on http://localhost:8099/. The GitHub workflow `scripts/publish/pages.yml`
runs the same script, so the preview is what deploys. It is installed on the GitHub mirror by
`scripts/publish-to-github.sh` like the other `scripts/publish/*.yml` templates; it does not run on Forgejo.

Maintainer steps (once):

1. On GitHub: repository **Settings -> Pages -> Source: GitHub Actions**. Do this before the next publish,
   otherwise the first Pages run fails at `configure-pages`; if that happens, enable it and re-run the
   workflow (Actions -> pages -> Run workflow).
2. Publish from Forgejo as usual (Actions -> publish to github). The site goes live at
   https://crueber.github.io/weave/ after that publish: the squash commit touches `site/**`, which triggers
   the workflow. Later changes deploy whenever a publish changes `site/**` or `docs/demo.webm`.

The workflow also lists `release: published` and `workflow_dispatch`. A release created by another workflow
with the built-in `GITHUB_TOKEN` does not start new runs, so in practice the push trigger (the publish) and
the manual button are what deploy.

Screenshots: `scripts/site/seed.sh` seeds a throwaway instance with fictional data, `scripts/site/shots.mjs`
shoots it, `scripts/site/webp.py SHOTS site/img` converts (see each file's header). Read every image before
committing: no real names, hosts, paths or keys. `python3 scripts/site/check-contrast.py` checks the site
tokens (copied from `internal/web/static/weave.css`; keep them in sync by hand).
