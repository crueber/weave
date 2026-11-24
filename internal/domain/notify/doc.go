// Package notify owns notifications, notification services (shoutrrr), webhooks,
// the SMTP mailer, admin configuration, global search, the personal dashboard and
// board/card export. Migrations 0600-0699.
//
// # Exported Go API (for other domains)
//
//	notify.Record(e, ev)            ops.EmitHook implementation (ops.Emit calls it)
//	notify.ActionWriter             var func(*ops.Env, ops.Event) bool; writes one activity row (default: INSERT into actions),
//	                                called once per board of a fanned-out event (fanout.go)
//	notify.Wait()                   waits for background deliveries (tests, shutdown)
//	notify.SendMail(e, to, subject, text, html) error   SMTP via config smtp.* (env overrides)
//	notify.SMTPConfigured(e) bool
//	notify.Config(e, key) string / ConfigBool / ConfigAll(e, "oidc.") / ConfigDB(ctx, db, key)
//	notify.Public(e) PublicConfig   branding + login-page flags (op config.public)
//	notify.AgentPromptDefault(e, scope) (text, admin)   admin default of an editable agent prompt (config agentprompt.<scope>) or the built-in text
//	notify.AgentBlocks(e) map[string]string           admin texts of the customised shared agent prompt blocks (config agentblock.<name>)
//	notify.ReadCardNotifications(e, cardID) (int, error)   for card "read-notifications"
//	notify.MentionIDs(text) / MentionsToText(text)          "@[Name](userId)" markup helpers
//	notify.Search / IndexCard / Reindex                      global FTS5 search
//
// # Event contract (ops.Event passed to ops.Emit)
//
// Event.Type is the Planka event name (camelCase) and is also the webhook event
// name: cardCreate, cardUpdate, cardMove, commentCreate, boardUpdate, ... (see
// notify.WebhookEvents; unknown types still produce an activity row and realtime
// event but no webhook). Emit AFTER your transaction has committed: the hook
// writes on separate connections and would otherwise wait out busy_timeout.
//
// Event.Data keys notify understands (all optional, missing keys are tolerated):
//
//	"item"       the entity as sent to clients/webhooks ({id, boardId, projectId, ...})
//	"prevData"   previous state (webhook "prevData")
//	"projectId"/"boardId"   scope for webhook filtering when not derivable from BoardID
//	commentCreate:         "commentId", "text" (raw markdown with @[Name](userId) mentions)
//	cardMembershipCreate:  "userId" (the member added)
//	cardMove:              "fromListId", "toListId" (differing ids => a move; into/out of a closed column => close/reopen)
//	cardUpdate:            "changed" []string of the fields the update set ("isClosed", "dueDate" notify)
//	cardCreate:            "draft": true for inbox drafts (silent)
//
// Event.Boards (optional) lists the boards a card event concerns; without it Record asks the cards domain
// (events.Placements) and fans a card-level event out to every board the card is on, a placement event
// (events.PlacementScoped) not. One actions row and one room publish per board, each with that board's own card
// (events.CardOn) and no other board's column/board ids; every envelope carries "origin" (the first row's id, which
// the other rows store in actions.origin_id). Notifications and webhooks run once per logical event.
//
// Notification rules (planNotifications; see docs/DESIGN.md "Watching and notifications"): watchers of the card,
// its column(s) and the board hear createCard, moveCard, closeCard, reopenCard, setCardDueDate and commentCard;
// mentioned viewers get mentionInComment; the added member gets addMemberToCard. At most one notification per
// user per event (reason in data.via), never the actor, never a muted card, only users who can view the board the
// notification names (one authz.CardViewers call; placement events: the event's board). subscription.list (op) and Settings > Watching (/ui/settings/watching) are the read side.
// Every notification is pushed to realtime room user:<id> as event "notification",
// delivered to that user's notification services and (when SMTP is configured) by
// e-mail; board services are additionally told about new comments.
//
// # Config keys (config table; env WEAVE_<KEY with dots as underscores, upper-cased> overrides)
//
//	branding.appName branding.loginText branding.primaryColor branding.logoData branding.logoType
//	smtp.host smtp.port smtp.secure(starttls|tls|none) smtp.user smtp.password smtp.from smtp.skipVerify smtp.notifications
//	oidc.issuer oidc.clientId oidc.clientSecret oidc.scopes oidc.usernameClaim oidc.nameClaim
//	oidc.emailClaim oidc.groupsClaim oidc.adminGroup oidc.userGroup oidc.workerGroup oidc.guestGroup oidc.requireGroup oidc.teamPrefix oidc.autoProvision oidc.buttonText
//	terms.enabled terms.version terms.text
//
// # UI routes registered here
//
// /notifications /notification-services /search /dashboard /branding/logo
// /admin/config /admin/webhooks /admin/webhooks/{id}/deliveries
// /export/boards/{id} /export/cards/{id}, plus /ui/... fragments. Admin should link to
// /admin/config and /admin/webhooks, not re-register them.
package notify
