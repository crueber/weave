// Package auth owns users, sessions, API keys, TOTP, OIDC SSO, terms, user
// groups and presence. It installs ops.Authenticate and the bootstrap-admin
// startup hook.
//
// Exported Go API for other domains:
//
//	auth.GetConfig(e, key)           config value: env WEAVE_<KEY> overrides the `config` table
//	auth.SetConfig(e, key, value)    upsert a `config` row (used for oidc.*, terms.*)
//	auth.UserBrief(e, id)            id, name, username, avatarUrl of any user (nil if missing)
//	auth.ClientIP(e)                 remote address honouring WEAVE_TRUST_PROXY
//	auth.MintKey(ctx, db, email, role)  break-glass API key minting (weave admin mint-key)
//
// Config keys read: oidc.issuer, oidc.clientId, oidc.clientSecret,
// oidc.scopes, oidc.emailClaim, oidc.nameClaim, oidc.usernameClaim, oidc.groupsClaim,
// oidc.adminGroup, oidc.ownerGroup, oidc.userGroup, oidc.workerGroup, oidc.guestGroup, oidc.requireGroup, oidc.teamPrefix, oidc.autoProvision, terms.text, auth.secret.
//
// Web routes: /login, /logout, /settings, /admin/users, /admin/groups, /admin/sso,
// /avatars/{id}, /auth/oidc/login, /auth/oidc/callback. The presence heartbeat
// script is /static/auth_presence.js (include it in the layout for global presence).
package auth
