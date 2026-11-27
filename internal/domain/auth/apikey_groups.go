package auth

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

type APIKeyOut struct {
	ID        string  `json:"id" desc:"key id (use it with apikey.revoke); the key itself is never shown again"`
	Name      string  `json:"name" desc:"your label for the key, e.g. the computer it lives on"`
	Key       string  `json:"key" desc:"the full key; shown only once"`
	Prefix    string  `json:"prefix"`
	CreatedAt string  `json:"createdAt"`
	ExpiresAt *string `json:"expiresAt,omitempty" desc:"when the key stops working; absent = never"`
}

// APIKeyInfo is one listed key: everything except the key itself.
type APIKeyInfo struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Prefix     string  `json:"prefix" desc:"the first characters of the key, to recognise it"`
	CreatedAt  string  `json:"createdAt"`
	LastUsedAt *string `json:"lastUsedAt,omitempty" desc:"when the key last authenticated a request (rounded to 10 minutes); absent = never used"`
	ExpiresAt  *string `json:"expiresAt,omitempty" desc:"when the key stops working; absent = never"`
	Expired    bool    `json:"expired,omitempty" desc:"true when the key has expired and is rejected"`
}

// APIKeyListIn is the input of apikey.list.
type APIKeyListIn struct {
	ID string `path:"id" desc:"user id, or \"me\"; only your own keys"`
}

// APIKeyGenIn is the input of apikey.generate.
type APIKeyGenIn struct {
	ID            string `path:"id" desc:"user id, or \"me\""`
	Name          string `json:"name" desc:"optional label for this key, e.g. the computer it is for (max 80 characters; default \"Key YYYY-MM-DD\")"`
	ExpiresInDays int    `json:"expiresInDays" desc:"optional: the key stops working after this many days (1-3650); 0 or omitted = never expires"`
}

// APIKeyRevokeIn is the input of apikey.revoke.
type APIKeyRevokeIn struct {
	ID    string `path:"id" desc:"user id, or \"me\""`
	KeyID string `query:"keyId" desc:"the key to revoke (from apikey.list)"`
	All   bool   `query:"all" desc:"true revokes every key of this user instead; give keyId or all=true, never neither"`
}

// maxKeyDays bounds expiresInDays (ten years).
const maxKeyDays = 3650

// maxKeyNameLen bounds an API key's label.
const maxKeyNameLen = 80

// defaultAPIKeysPerUser caps the keys one person may hold (WEAVE_MAX_API_KEYS; 0 = no cap).
const defaultAPIKeysPerUser = 50

var (
	maxKeysOnce sync.Once
	maxKeysVal  = defaultAPIKeysPerUser
	// maxKeysEnv reads the variable; tests replace it and reset maxKeysOnce.
	maxKeysEnv = func() string { return os.Getenv("WEAVE_MAX_API_KEYS") }
)

// maxAPIKeys parses WEAVE_MAX_API_KEYS once (a negative or garbage value logs one warning and falls back to 50).
func maxAPIKeys() int {
	maxKeysOnce.Do(func() {
		if v := strings.TrimSpace(maxKeysEnv()); v != "" {
			if x, err := strconv.Atoi(v); err == nil && x >= 0 {
				maxKeysVal = x
			} else {
				fmt.Println("WEAVE_MAX_API_KEYS: invalid value", v, "- using", defaultAPIKeysPerUser)
			}
		}
	})
	return maxKeysVal
}

// lastUsedEvery is the most often a key's last_used_at is written (authentication touches it at most this often).
const lastUsedEvery = 10 * time.Minute

func keyExpiry(days int) (*string, error) {
	if days < 0 || days > maxKeyDays {
		return nil, ops.Invalid("expiresInDays must be between 1 and 3650 (or 0 for no expiry)")
	}
	if days == 0 {
		return nil, nil
	}
	s := time.Now().UTC().Add(time.Duration(days) * 24 * time.Hour).Format(time.RFC3339)
	return &s, nil
}

// keyName trims a label; empty becomes "Key YYYY-MM-DD".
func keyName(s string) (string, error) {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F {
			return -1
		}
		return r
	}, s))
	if s == "" {
		return "Key " + time.Now().UTC().Format("2006-01-02"), nil
	}
	if len([]rune(s)) > maxKeyNameLen {
		return "", ops.Invalid("name is at most 80 characters")
	}
	return s, nil
}

type Group struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	CreatedAt string   `json:"createdAt"`
	Members   []*User  `json:"members"`
	MemberIDs []string `json:"memberIds"`
}
type GroupList struct {
	Items []*Group `json:"items"`
}
type GroupIDIn struct {
	ID string `path:"id"`
}
type GroupCreateIn struct {
	Name      string   `json:"name" required:"true"`
	MemberIDs []string `json:"memberIds" desc:"initial member user ids"`
}
type GroupUpdateIn struct {
	ID   string `path:"id"`
	Name string `json:"name" required:"true"`
}
type GroupMemberIn struct {
	ID     string `path:"id" desc:"group id"`
	UserID string `path:"userId"`
}
type GroupMemberAddIn struct {
	ID     string `path:"id" desc:"group id"`
	UserID string `json:"userId" required:"true"`
}

// ownerOnly resolves id and requires it to be the caller (API keys are personal).
func ownerOnly(e *ops.Env, id string) (string, error) {
	if id == "me" || id == e.User.UserID {
		return e.User.UserID, nil
	}
	return "", ops.Forbidden()
}

// addAPIKey inserts one more key for the user (existing keys stay valid) and returns it. expiresInDays 0 means the
// key never expires. Callers own the rules around it (cap, single-key agents).
func addAPIKey(ctx context.Context, d auditExec, userID, name string, expiresInDays int) (APIKeyOut, error) {
	exp, err := keyExpiry(expiresInDays)
	if err != nil {
		return APIKeyOut{}, err
	}
	key := apiKeyPrefix + db.Token(24)
	out := APIKeyOut{ID: db.NewID(), Name: name, Key: key, Prefix: key[:8], CreatedAt: db.Now(), ExpiresAt: exp}
	var expArg any
	if exp != nil {
		expArg = *exp
	}
	if _, err := d.ExecContext(ctx, `INSERT INTO api_keys(id,user_id,name,key_hash,prefix,created_at,expires_at) VALUES(?,?,?,?,?,?,?)`,
		out.ID, userID, name, sha(key), out.Prefix, out.CreatedAt, expArg); err != nil {
		return APIKeyOut{}, err
	}
	return out, nil
}

// replaceAPIKey is the agent rule: an agent account holds exactly one key, so the old ones are deleted and one new key is
// inserted (used by agent.create and agent.rotateKey, inside the caller's transaction).
func replaceAPIKey(ctx context.Context, d auditExec, userID string, expiresInDays int) (APIKeyOut, error) {
	if _, err := d.ExecContext(ctx, `DELETE FROM api_keys WHERE user_id=?`, userID); err != nil {
		return APIKeyOut{}, err
	}
	return addAPIKey(ctx, d, userID, "Agent key", expiresInDays)
}

// listAPIKeys returns the user's keys, newest first (never the key or its hash).
func listAPIKeys(e *ops.Env, userID string) ([]APIKeyInfo, error) {
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT id,name,prefix,created_at,last_used_at,expires_at FROM api_keys WHERE user_id=? ORDER BY created_at DESC, id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIKeyInfo{}
	for rows.Next() {
		var k APIKeyInfo
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &k.CreatedAt, &k.LastUsedAt, &k.ExpiresAt); err != nil {
			return nil, err
		}
		k.Expired = keyExpired(k.ExpiresAt)
		out = append(out, k)
	}
	return out, rows.Err()
}

// MintKey is the break-glass path behind `weave admin mint-key`: it runs on the server
// host with direct database access (never over HTTP), creates or updates the user with
// the given email (and role, when non-empty), and returns an ADDITIONAL API key named "mint-key" (existing keys keep working). The account is
// marked SSO-linked by email; the next OIDC login re-applies IdP-owned roles.
func MintKey(ctx context.Context, d *db.DB, email, role string) (APIKeyOut, error) {
	email, err := cleanEmail(email)
	if err != nil {
		return APIKeyOut{}, err
	}
	if role != "" && !validRole(role) {
		return APIKeyOut{}, ops.Invalid("role must be one of " + strings.Join(Roles, "|"))
	}
	var out APIKeyOut
	err = d.Tx(ctx, func(tx *sql.Tx) error {
		var id string
		err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE email=? AND deleted_at IS NULL`, email).Scan(&id)
		switch {
		case err == sql.ErrNoRows:
			r := role
			if r == "" {
				r = "boardUser"
			}
			name, _, _ := strings.Cut(email, "@")
			id = db.NewID()
			if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,email,name,role,is_sso,is_email_verified,subscribe_own_cards,created_at) VALUES(?,?,?,?,1,1,1,?)`, id, email, name, r, db.Now()); err != nil {
				return err
			}
		case err != nil:
			return err
		case role != "":
			if _, err := tx.ExecContext(ctx, `UPDATE users SET role=?,is_deactivated=0,updated_at=? WHERE id=?`, role, db.Now(), id); err != nil {
				return err
			}
		default:
			if _, err := tx.ExecContext(ctx, `UPDATE users SET is_deactivated=0,updated_at=? WHERE id=? AND is_deactivated=1`, db.Now(), id); err != nil {
				return err
			}
		}
		out, err = addAPIKey(ctx, tx, id, "mint-key", 0)
		return err
	})
	return out, err
}

func init() {
	ops.Register(ops.Op{Name: "apikey.list", Summary: "List your personal API keys (name, prefix, created, last used, expiry; never the key itself)", Method: "GET", Path: "/users/{id}/api-keys", UI: "GET /settings (API key)"},
		func(e *ops.Env, in APIKeyListIn) ([]APIKeyInfo, error) {
			id, err := ownerOnly(e, in.ID)
			if err != nil {
				return nil, err
			}
			return listAPIKeys(e, id)
		})

	ops.Register(ops.Op{Name: "apikey.generate", Summary: "Add a personal API key (Bearer / X-API-Key); your other keys keep working. One key per computer or tool", Method: "POST", Path: "/users/{id}/api-key", UI: "POST /ui/settings/apikey"},
		func(e *ops.Env, in APIKeyGenIn) (APIKeyOut, error) {
			id, err := ownerOnly(e, in.ID)
			if err != nil {
				return APIKeyOut{}, err
			}
			if e.User.IsAgent {
				return APIKeyOut{}, &ops.Error{Status: 403, Code: "forbidden", Msg: "an agent holds exactly one key: agent keys are rotated with agent.rotateKey by whoever owns the agent"}
			}
			name, err := keyName(in.Name)
			if err != nil {
				return APIKeyOut{}, err
			}
			if _, err := keyExpiry(in.ExpiresInDays); err != nil {
				return APIKeyOut{}, err
			}
			var out APIKeyOut
			// the cap check and the insert share one (IMMEDIATE) transaction so concurrent calls cannot overshoot it
			err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
				if limit := maxAPIKeys(); limit > 0 {
					var n int
					if err := tx.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM api_keys WHERE user_id=? AND (expires_at IS NULL OR expires_at>?)`, id, time.Now().UTC().Format(time.RFC3339)).Scan(&n); err != nil {
						return err
					}
					if n >= limit {
						return ops.Conflict(fmt.Sprintf("you already have %d API keys, the most one person may hold (WEAVE_MAX_API_KEYS); revoke one first", n))
					}
				}
				var err error
				out, err = addAPIKey(e.Ctx, tx, id, name, in.ExpiresInDays)
				return err
			})
			return out, err
		})

	ops.Register(ops.Op{Name: "apikey.revoke", Summary: "Revoke one API key (keyId) or all of a user's keys (all=true); admins may revoke others' keys (audited)", Method: "DELETE", Path: "/users/{id}/api-key", UI: "POST /ui/settings/apikey/revoke"},
		func(e *ops.Env, in APIKeyRevokeIn) (OKOut, error) {
			id, err := resolve(e, in.ID, false)
			if err != nil {
				return OKOut{}, err
			}
			keyID := strings.TrimSpace(in.KeyID)
			if (keyID == "") == !in.All {
				return OKOut{}, ops.Invalid("pass keyId to revoke one key, or all=true to revoke every key")
			}
			// the user is read first so an admin's audit row can name them (an unknown id answers not_found)
			var tName, tEmail string
			var tAgent bool
			if err := e.DB.QueryRowContext(e.Ctx, `SELECT name,email,is_agent FROM users WHERE id=? AND deleted_at IS NULL`, id).Scan(&tName, &tEmail, &tAgent); err != nil {
				if err == sql.ErrNoRows {
					return OKOut{}, ops.NotFound("user")
				}
				return OKOut{}, err
			}
			others := id != e.User.UserID // only an admin gets here (resolve); the owner revoking their own keys needs no audit row
			var au *AuditEntry
			err = e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
				au = nil
				q, args := `SELECT prefix FROM api_keys WHERE user_id=?`, []any{id}
				if keyID != "" {
					q, args = q+` AND id=?`, append(args, keyID)
				}
				var prefixes []string
				rows, err := tx.QueryContext(e.Ctx, q, args...)
				if err != nil {
					return err
				}
				for rows.Next() {
					var p string
					if rows.Scan(&p) == nil {
						prefixes = append(prefixes, p)
					}
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					return err
				}
				if keyID != "" && len(prefixes) == 0 {
					return ops.NotFound("key") // an unknown id, or another user's key: nothing is deleted
				}
				var delErr error
				if keyID != "" {
					_, delErr = tx.ExecContext(e.Ctx, `DELETE FROM api_keys WHERE user_id=? AND id=?`, id, keyID)
				} else {
					_, delErr = tx.ExecContext(e.Ctx, `DELETE FROM api_keys WHERE user_id=?`, id)
				}
				if delErr != nil {
					return delErr
				}
				if others && len(prefixes) > 0 {
					kind := "user"
					if tAgent {
						kind = "agent"
					}
					au = &AuditEntry{Action: AuditAPIKeyRevoke, TargetKind: kind, TargetID: id, TargetName: tName, TargetEmail: tEmail, Source: auditSource(e),
						Details: map[string]any{"keysRevoked": len(prefixes), "keyPrefixes": prefixes, "all": keyID == ""}}
					auditActor(e.Ctx, tx, e.User, au)
					return writeAudit(e.Ctx, tx, au)
				}
				return nil
			})
			if err != nil {
				return OKOut{}, err
			}
			if au != nil {
				logAudit(*au)
			}
			return OKOut{true}, nil
		})

	// ---- user groups ----
	ops.Register(ops.Op{Name: "group.list", Summary: "List user groups with members", Method: "GET", Path: "/user-groups", UI: "GET /admin/groups"},
		func(e *ops.Env, _ NoInput) (GroupList, error) { return listGroups(e, "") })

	ops.Register(ops.Op{Name: "group.get", Summary: "Get a user group", Method: "GET", Path: "/user-groups/{id}", UI: "GET /admin/groups"},
		func(e *ops.Env, in GroupIDIn) (*Group, error) {
			l, err := listGroups(e, in.ID)
			if err != nil {
				return nil, err
			}
			if len(l.Items) == 0 {
				return nil, ops.NotFound("group")
			}
			return l.Items[0], nil
		})

	ops.Register(ops.Op{Name: "group.create", Summary: "Create a user group (admin)", Method: "POST", Path: "/user-groups", Auth: ops.AuthAdmin, UI: "POST /ui/admin/groups"},
		func(e *ops.Env, in GroupCreateIn) (*Group, error) {
			name := strings.TrimSpace(in.Name)
			if name == "" {
				return nil, ops.Invalid("name is required")
			}
			id := db.NewID()
			// the group and its initial members in one transaction; every member added is audited like group.member.add
			var audits []AuditEntry
			err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
				audits = nil
				if _, err := tx.ExecContext(e.Ctx, `INSERT INTO user_groups(id,name,created_at) VALUES(?,?,?)`, id, name, db.Now()); err != nil {
					if db.IsUnique(err) {
						return ops.Conflict("a group with this name exists")
					}
					return err
				}
				for _, uid := range in.MemberIDs {
					au, err := addGroupMember(e, tx, id, uid)
					if err != nil {
						return err
					}
					if au != nil {
						audits = append(audits, *au)
					}
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			for _, a := range audits {
				logAudit(a)
			}
			e.Hub.Publish(realtime.Event{Name: "group.created", Room: "global", Data: map[string]any{"id": id}})
			return ops.Typed[*Group](e, "group.get", GroupIDIn{ID: id})
		})

	ops.Register(ops.Op{Name: "group.update", Summary: "Rename a user group (admin)", Method: "PATCH", Path: "/user-groups/{id}", Auth: ops.AuthAdmin, UI: "POST /ui/admin/groups/{id}"},
		func(e *ops.Env, in GroupUpdateIn) (*Group, error) {
			name := strings.TrimSpace(in.Name)
			if name == "" {
				return nil, ops.Invalid("name is required")
			}
			r, err := e.DB.ExecContext(e.Ctx, `UPDATE user_groups SET name=? WHERE id=?`, name, in.ID)
			if db.IsUnique(err) {
				return nil, ops.Conflict("a group with this name exists")
			}
			if err != nil {
				return nil, err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				return nil, ops.NotFound("group")
			}
			e.Hub.Publish(realtime.Event{Name: "group.updated", Room: "global", Data: map[string]any{"id": in.ID}})
			return ops.Typed[*Group](e, "group.get", GroupIDIn{ID: in.ID})
		})

	ops.Register(ops.Op{Name: "group.delete", Summary: "Delete a user group (admin); board memberships of the group go with it", Method: "DELETE", Path: "/user-groups/{id}", Auth: ops.AuthAdmin, UI: "POST /ui/admin/groups/{id}/delete"},
		func(e *ops.Env, in GroupIDIn) (OKOut, error) {
			r, err := e.DB.ExecContext(e.Ctx, `DELETE FROM user_groups WHERE id=?`, in.ID)
			if err != nil {
				return OKOut{}, err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				return OKOut{}, ops.NotFound("group")
			}
			e.Hub.Publish(realtime.Event{Name: "group.deleted", Room: "global", Data: map[string]any{"id": in.ID}})
			return OKOut{true}, nil
		})

	ops.Register(ops.Op{Name: "group.member.add", Summary: "Add a user to a group (admin)", Method: "POST", Path: "/user-groups/{id}/members", Auth: ops.AuthAdmin, UI: "POST /ui/admin/groups/{id}/members"},
		func(e *ops.Env, in GroupMemberAddIn) (*Group, error) {
			var n int
			_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM user_groups WHERE id=?`, in.ID).Scan(&n)
			if n == 0 {
				return nil, ops.NotFound("group")
			}
			if _, err := loadUser(e, in.UserID); err != nil {
				return nil, err
			}
			var au *AuditEntry
			err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
				var err error
				au, err = addGroupMember(e, tx, in.ID, in.UserID)
				return err
			})
			if err != nil {
				return nil, err
			}
			if au != nil {
				logAudit(*au)
			}
			e.Hub.Publish(realtime.Event{Name: "group.updated", Room: "global", Data: map[string]any{"id": in.ID}})
			return ops.Typed[*Group](e, "group.get", GroupIDIn{ID: in.ID})
		})

	ops.Register(ops.Op{Name: "group.member.remove", Summary: "Remove a user from a group (admin)", Method: "DELETE", Path: "/user-groups/{id}/members/{userId}", Auth: ops.AuthAdmin, UI: "POST /ui/admin/groups/{id}/members/{userId}/remove"},
		func(e *ops.Env, in GroupMemberIn) (*Group, error) {
			r, err := e.DB.ExecContext(e.Ctx, `DELETE FROM user_group_members WHERE group_id=? AND user_id=?`, in.ID, in.UserID)
			if err != nil {
				return nil, err
			}
			if n, _ := r.RowsAffected(); n == 0 {
				return nil, ops.NotFound("group member")
			}
			e.Hub.Publish(realtime.Event{Name: "group.updated", Room: "global", Data: map[string]any{"id": in.ID}})
			return ops.Typed[*Group](e, "group.get", GroupIDIn{ID: in.ID})
		})
}

// addGroupMember adds an existing, not deleted user to a group inside tx and writes the audit row in the same
// transaction (an admin adding themself is flagged self). Adding someone who is already a member, or an unknown
// user, changes nothing and records nothing (nil entry). Used by group.member.add and group.create's memberIds.
func addGroupMember(e *ops.Env, tx *sql.Tx, groupID, userID string) (*AuditEntry, error) {
	var u struct {
		name, email string
		agent       bool
	}
	if err := tx.QueryRowContext(e.Ctx, `SELECT name,email,is_agent FROM users WHERE id=? AND deleted_at IS NULL`, userID).Scan(&u.name, &u.email, &u.agent); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	r, err := tx.ExecContext(e.Ctx, `INSERT OR IGNORE INTO user_group_members(group_id,user_id) VALUES(?,?)`, groupID, userID)
	if err != nil {
		return nil, err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return nil, nil
	}
	var gname string
	_ = tx.QueryRowContext(e.Ctx, `SELECT name FROM user_groups WHERE id=?`, groupID).Scan(&gname)
	a := AuditEntry{Action: AuditGroupMemberAdd, TargetKind: map[bool]string{true: "agent", false: "user"}[u.agent], TargetID: userID, TargetName: u.name, TargetEmail: u.email,
		Details: map[string]any{"groupId": groupID, "groupName": gname, "self": e.User != nil && e.User.UserID == userID}, Source: auditSource(e)}
	auditActor(e.Ctx, tx, e.User, &a)
	return &a, writeAudit(e.Ctx, tx, &a)
}

type prefixScanner struct {
	prefix any
	inner  rowScanner
}

func (p prefixScanner) Scan(dest ...any) error {
	args := make([]any, 0, len(dest)+1)
	args = append(args, p.prefix)
	args = append(args, dest...)
	return p.inner.Scan(args...)
}

func listGroups(e *ops.Env, only string) (GroupList, error) {
	restricted := directoryRestricted(e)
	var visGroups, visUsers map[string]bool
	if restricted {
		visGroups = visibleGroupIDs(e)
		if only != "" && !visGroups[only] {
			return GroupList{Items: []*Group{}}, nil
		}
		visUsers = visibleUserIDs(e)
	}
	q := `SELECT id,name,created_at FROM user_groups`
	var args []any
	if only != "" {
		q += ` WHERE id=?`
		args = append(args, only)
	}
	rows, err := e.DB.QueryContext(e.Ctx, q+` ORDER BY name COLLATE NOCASE`, args...)
	if err != nil {
		return GroupList{}, err
	}
	out := GroupList{Items: []*Group{}}
	byID := map[string]*Group{}
	var groupIDs []string
	for rows.Next() {
		g := &Group{Members: []*User{}, MemberIDs: []string{}}
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt); err != nil {
			rows.Close()
			return out, err
		}
		if restricted && !visGroups[g.ID] {
			continue
		}
		out.Items = append(out.Items, g)
		byID[g.ID] = g
		groupIDs = append(groupIDs, g.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, err
	}
	rows.Close()
	if len(groupIDs) == 0 {
		return out, nil
	}
	const chunk = 500
	for i := 0; i < len(groupIDs); i += chunk {
		part := groupIDs[i:min(i+chunk, len(groupIDs))]
		ph := "?" + strings.Repeat(",?", len(part)-1)
		margs := make([]any, len(part))
		for j, id := range part {
			margs[j] = id
		}
		mr, err := e.DB.QueryContext(e.Ctx, `SELECT gm.group_id,`+userCols+` FROM user_group_members gm JOIN users ON users.id=gm.user_id
			WHERE users.deleted_at IS NULL AND gm.group_id IN (`+ph+`) ORDER BY users.name COLLATE NOCASE`, margs...)
		if err != nil {
			return out, err
		}
		for mr.Next() {
			var gid string
			u, err := scanUser(prefixScanner{&gid, mr})
			if err != nil {
				mr.Close()
				return out, err
			}
			if restricted && !visUsers[u.ID] {
				continue
			}
			if g := byID[gid]; g != nil {
				g.Members = append(g.Members, redact(e, u))
				g.MemberIDs = append(g.MemberIDs, u.ID)
			}
		}
		if err := mr.Err(); err != nil {
			mr.Close()
			return out, err
		}
		mr.Close()
	}
	return out, nil
}
