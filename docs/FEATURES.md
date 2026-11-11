# Weave feature ledger (reconstruction obligations)
Sources: OSS Planka 2.3.x routes/models/client (observed in repo), planka.app/pro feature list (public), planka.app docs.
Status per item: `todo` → `done` | `partial(reason)`. Owners update their own rows. Each row must map to ≥1 registered op (`weave ops`) and a UI entry.

## auth
- [x] OIDC-only sign-in (auth.oidc.*; UI /login SSO button, /auth/oidc/*), logout, session list/revoke; terms step (auth.logout, auth.me, session.*). No local passwords: removed by design
- [x] users provisioned on first SSO login; IdP owns name/email/role; avatar upload, deactivate/activate/delete, roles admin|projectOwner|boardUser|worker|guest (user.*; UI /admin/users read-only roles, /settings). partial(authz.go does not yet special-case worker/guest beyond the stored role)
- [x] API keys (create/rotate/revoke, hashed), Bearer + X-API-Key auth (apikey.generate/revoke; UI /settings)
- [x] two-factor is delegated to the IdP (local TOTP/recovery/trusted devices removed by design)
- [x] OIDC SSO (go-oidc; env WEAVE_OIDC_* required at startup; IdP groups map to roles weave-admins/-project-owners/-users/-workers/-guests, highest wins, applied every login, last admin protected; weave-team-<name> groups sync into user groups; requireGroup; autoProvision). CLI uses API keys; break-glass `weave admin mint-key` / `weave admin relink`; accounts bind to the first SSO identity (issuer|sub), mismatches are denied until an admin relinks; optional API-key expiry; max-idle bound; sign out everywhere. Tested against a fake IdP only
- [x] terms acceptance, user preferences (language, theme, density, colour-blind, auto-logout, editor mode, home view, project order) (auth.terms.*, user.preferences.update; UI /login terms step, /admin/sso, /settings)
- [x] user groups (CRUD, members), presence (online/idle/away) (group.*, presence.*; UI /admin/groups; presence heartbeat static/auth_presence.js, pushed over SSE as presence.updated on room global). partial(heartbeat script is only linked from /settings until the layout includes it)
## projects
- [x] project CRUD, hidden, description, background (color/gradient/uploaded image + thumbnail), managers add/remove, favorites, project-wide labels toggle, base custom-field groups & fields — done (ops `project.*`; UI `/`, `/projects/{id}`; `projects` domain). Not implemented: Planka's private/shared project `type` (no column in core schema).
## boards
- [x] board CRUD/reorder, settings (default view, card types, creator display, task-list expansion, counter, ages), memberships (user/group, editor/viewer/canComment)
- [x] labels CRUD/position, label groups (single-select), project-wide labels
- [x] lists CRUD/move, sort, clear, move-cards, archive/trash lists; kanban/list/grid views; filters (member/label/text/due) + saved filters  (calendar/map views are linked, rendered by cards)
- [~] public boards (anon view), board templates (publish/instantiate), board import (Planka JSON/Trello/CSV) & export  -- partial(OSS Planka has no board JSON export: "planka" import format is Weave's own export document `weave-board` v1, Trello + CSV follow public formats; Trello attachments/members not imported)
## cards
- [x] card CRUD/move/duplicate/close, cover, due date + completed, types project|story, memberships, labels (single-select groups), subscriptions, read-notifications — ops card.list/get/create/update/delete/move/archive/restore/duplicate/close/cover.set/due.set/due.complete/location.set/member.add|remove/label.add|remove/subscribe/unsubscribe/notifications.read/options; UI card modal `GET /ui/cards/{id}`. partial: duplicate copies tasks/attachments/custom fields only once cardextras registers `cards.DuplicateHooks`
- [x] drafts + board inbox (card.draft.create/publish, card.inbox.set/list), recurring cards (card.recurrence.set/run + minute scheduler; daily/weekly(weekdays)/monthly/yearly, due|completed trigger, until/count), card templates (card.template.save/list, card.fromTemplate), sharing links (card.share.create/revoke/view, `/share/{token}`), calendar view (`/boards/{id}/calendar`, drag to reschedule), map view (`/boards/{id}/map`; partial: tile-less SVG coordinate plot + list with OSM links, no vendored map library), collaborative-edit locks (card.edit.touch/release/list, 60s idle takeover)
## cardextras
- [x] task lists/tasks (assignee, completed, link to card, hide completed, show on front, drag reorder/move), comments (+counter, author edit, author/manager delete, canComment), attachments (file/link, thumbs via x/image, cover, authorized download, storage_usage, board media view w/ image/video/doc filter) — ops tasklist.*, task.*, comment.*, attachment.*; UI /ui/cards/{id}/{tasks,comments,attachments}, /boards/{id}/media
- [x] custom fields: group/field/value, board/card/base scopes, repeatable groups (sortable table rows), field types text|number|date|checkbox|select|url — ops customfieldgroup.*, customfield.*, customfieldvalue.*, customfieldrow.*; UI /ui/cards/{id}/fields. (Base group CRUD itself stays with projects; cardextras creates fields inside base groups and groups derived from them.)
- [x] time tracking (stopwatch start/stop/reset, manual entries, totals; cards.stopwatch_* kept consistent), activity feeds (board & card) + `cardextras.RecordAction` writer — ops time.*, activity.*; UI /ui/cards/{id}/{time,activity}, /ui/boards/{id}/activity
## notify
- [x] notifications list/read/read-all/badge (`notification.*`, /notifications, /ui/notifications/badge); rules: mention/comment/move/add-member, realtime `notification` to user room, e-mail when SMTP configured
- [x] notification services per user/board (shoutrrr URL, text|markdown|html, test send) (`notification_service.*`, /notification-services, /ui/boards/{id}/notification-services)
- [x] webhooks CRUD (+scope project/board/user, 60+ event names include/exclude, Bearer access token + HMAC-SHA256 `X-Weave-Signature`, async delivery with retry/backoff, delivery log, ping, redeliver) (`webhook.*`, /admin/webhooks)
- [x] SMTP mailer (go-mail; `smtp.*` config + env override; `smtp.test`; `notify.SendMail`)
- [x] admin config: branding (name/logo/login text/accent), SMTP, OIDC settings storage (consumed by auth via `notify.Config*`), registration/terms (`config.*`, /admin/config)
- [x] global FTS5 search across cards/comments/boards/projects, access-filtered, trigger-maintained, `search.reindex` (`search.query`, /search)
- [x] personal dashboard: my cards, due soon, overdue, recent activity; panels/limit saved in users.dashboard (`dashboard.*`, /dashboard)
- [~] export board/card (`export.board`/`export.card`): standalone HTML = done. PDF = partial(no bundled PDF engine: returns print-optimised HTML with @page CSS + "Print / Save as PDF" button; set WEAVE_PDF_CMD, e.g. `wkhtmltopdf -q - -`, to get real application/pdf)
## admin / platform
- [ ] storage usage, version/health, CLI everything, docker compose, e2e smoke, i18n scaffolding (en + key tables)
## agents and the event stream
- [x] agent accounts (`agent.create|list|update|rotateKey|delete|access`, `auth.whoami`; UI /admin/agents with one-time key + copy, rotate, delete, per-board access; robot badge on /admin/users, board tiles, board settings members). partial(card sheet member chips do not show the badge: cards_modal.html is being redesigned elsewhere)
- [x] event stream `GET /events?boards=&lists=&types=&since=&heartbeat=&agent=` + `Last-Event-ID`: durable replay from `actions` (seq, 90 d default retention, >= 30), ordered, no gaps/dups, access-filtered, heartbeats, `ready` frame
- [x] card claims with leases (`card.claim|release|renew|next`, sweeper, `card.claimed|released` events); tile and card-sheet "Claimed by X until HH:MM" (card sheet via `_cards_claim.html`, one include line in cards_modal.html)
- [x] `weave skill install|check|uninstall|targets`: one command teaches every detected coding agent (Claude Code, Codex, OpenCode, pi, Gemini, Cursor, Copilot, Cline, Roo, Kilo, Kiro, Amp, Goose, Droid, Qwen, Crush, Zed, ...) via a data-driven target registry (`docs/AGENT-TARGETS.md`); skill folders or marked instruction blocks, user or project scope, `--dry-run`, stale/modified detection.
- [x] listener presence (`list.listeners`, `board.listeners`, `listener.joined|left`, robot indicator with popover in list headers)
- [x] context-efficient reads (`board.tree`, `card.get?full=true`, `card.list` filters + cursor)
