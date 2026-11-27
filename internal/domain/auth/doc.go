// Package auth owns users, sessions, API keys, AI agent accounts, OIDC SSO,
// terms, user groups, admin audit log and presence. It installs
// ops.Authenticate and the OIDC startup validation hook.
//
// Exported Go API for other domains:
//
//	auth.GetConfig(e, key)           config value: env WEAVE_<KEY> overrides the `config` table
//	auth.SetConfig(e, key, value)    upsert a `config` row (used for oidc.*, terms.*)
//	auth.UserBrief(e, id)            id, name, username, avatarUrl, isAgent of any user (nil if missing)
//	auth.ClientIP(e)                 remote address honouring WEAVE_TRUST_PROXY
//	auth.MintKey(ctx, db, email, role)  break-glass: ADDS an API key named "mint-key" (weave admin mint-key)
//
// Config keys read: oidc.issuer, oidc.clientId, oidc.clientSecret,
// oidc.scopes, oidc.emailClaim, oidc.nameClaim, oidc.usernameClaim, oidc.groupsClaim,
// oidc.adminGroup, oidc.userGroup, oidc.workerGroup, oidc.guestGroup, oidc.requireGroup, oidc.teamPrefix, oidc.autoProvision, terms.text, auth.secret.
//
// Web routes: /login, /logout, /settings, /admin/users, /admin/groups, /admin/sso,
// /admin/agents, /admin/audit, /avatars/{id}, /auth/oidc/login, /auth/oidc/callback. The presence heartbeat
// script is /static/auth_presence.js (include it in the layout for global presence).
package auth
