package auth

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

type APIKeyOut struct {
	Key       string  `json:"key" desc:"the full key; shown only once"`
	Prefix    string  `json:"prefix"`
	CreatedAt string  `json:"createdAt"`
	ExpiresAt *string `json:"expiresAt,omitempty" desc:"when the key stops working; absent = never"`
}

// APIKeyGenIn is the input of apikey.generate.
type APIKeyGenIn struct {
	ID            string `path:"id" desc:"user id, or \"me\""`
	ExpiresInDays int    `json:"expiresInDays" desc:"optional: the key stops working after this many days (1-3650); 0 or omitted = never expires"`
}

// maxKeyDays bounds expiresInDays (ten years).
const maxKeyDays = 3650

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

// setAPIKey stores a fresh hashed API key for the user and returns it.
// expiresInDays 0 means the key never expires.
func setAPIKey(ctx context.Context, d *db.DB, id string, expiresInDays int) (APIKeyOut, error) {
	exp, err := keyExpiry(expiresInDays)
	if err != nil {
		return APIKeyOut{}, err
	}
	key := apiKeyPrefix + db.Token(24)
	now := db.Now()
	prefix := key[:8]
	var expArg any
	if exp != nil {
		expArg = *exp
	}
	if _, err := d.ExecContext(ctx, `UPDATE users SET api_key_hash=?,api_key_prefix=?,api_key_created_at=?,api_key_expires_at=? WHERE id=?`, sha(key), prefix, now, expArg, id); err != nil {
		return APIKeyOut{}, err
	}
	return APIKeyOut{Key: key, Prefix: prefix, CreatedAt: now, ExpiresAt: exp}, nil
}

// MintKey is the break-glass path behind `weave admin mint-key`: it runs on the server
// host with direct database access (never over HTTP), creates or updates the user with
// the given email (and role, when non-empty), and returns a fresh API key. The account is
// marked SSO-linked by email; the next OIDC login re-applies IdP-owned roles.
func MintKey(ctx context.Context, d *db.DB, email, role string) (APIKeyOut, error) {
	email, err := cleanEmail(email)
	if err != nil {
		return APIKeyOut{}, err
	}
	if role != "" && !validRole(role) {
		return APIKeyOut{}, ops.Invalid("role must be one of " + strings.Join(Roles, "|"))
	}
	var id string
	err = d.QueryRowContext(ctx, `SELECT id FROM users WHERE email=? AND deleted_at IS NULL`, email).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		if role == "" {
			role = "boardUser"
		}
		name, _, _ := strings.Cut(email, "@")
		id = db.NewID()
		if _, err := d.ExecContext(ctx, `INSERT INTO users(id,email,name,role,is_sso,is_email_verified,created_at) VALUES(?,?,?,?,1,1,?)`, id, email, name, role, db.Now()); err != nil {
			return APIKeyOut{}, err
		}
	case err != nil:
		return APIKeyOut{}, err
	case role != "":
		if _, err := d.ExecContext(ctx, `UPDATE users SET role=?,is_deactivated=0,updated_at=? WHERE id=?`, role, db.Now(), id); err != nil {
			return APIKeyOut{}, err
		}
	}
	return setAPIKey(ctx, d, id, 0)
}

func init() {
	ops.Register(ops.Op{Name: "apikey.generate", Summary: "Create or rotate a personal API key (Bearer / X-API-Key); the old key stops working", Method: "POST", Path: "/users/{id}/api-key", UI: "POST /ui/settings/apikey"},
		func(e *ops.Env, in APIKeyGenIn) (APIKeyOut, error) {
			id, err := ownerOnly(e, in.ID)
			if err != nil {
				return APIKeyOut{}, err
			}
			return setAPIKey(e.Ctx, e.DB, id, in.ExpiresInDays)
		})

	ops.Register(ops.Op{Name: "apikey.revoke", Summary: "Revoke a user's API key (admins may revoke others')", Method: "DELETE", Path: "/users/{id}/api-key", UI: "POST /ui/settings/apikey/revoke"},
		func(e *ops.Env, in IDIn) (OKOut, error) {
			id, err := resolve(e, in.ID, false)
			if err != nil {
				return OKOut{}, err
			}
			_, err = e.DB.ExecContext(e.Ctx, `UPDATE users SET api_key_hash=NULL,api_key_prefix=NULL,api_key_created_at=NULL,api_key_expires_at=NULL WHERE id=?`, id)
			return OKOut{true}, err
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
			if _, err := e.DB.ExecContext(e.Ctx, `INSERT INTO user_groups(id,name,created_at) VALUES(?,?,?)`, id, name, db.Now()); err != nil {
				if db.IsUnique(err) {
					return nil, ops.Conflict("a group with this name exists")
				}
				return nil, err
			}
			for _, uid := range in.MemberIDs {
				_, _ = e.DB.ExecContext(e.Ctx, `INSERT OR IGNORE INTO user_group_members(group_id,user_id) SELECT ?,id FROM users WHERE id=?`, id, uid)
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
			if _, err := e.DB.ExecContext(e.Ctx, `INSERT OR IGNORE INTO user_group_members(group_id,user_id) VALUES(?,?)`, in.ID, in.UserID); err != nil {
				return nil, err
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

func listGroups(e *ops.Env, only string) (GroupList, error) {
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
	for rows.Next() {
		g := &Group{Members: []*User{}, MemberIDs: []string{}}
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt); err != nil {
			rows.Close()
			return out, err
		}
		out.Items = append(out.Items, g)
	}
	rows.Close()
	for _, g := range out.Items {
		mr, err := e.DB.QueryContext(e.Ctx, `SELECT `+userCols+` FROM users WHERE deleted_at IS NULL AND id IN (SELECT user_id FROM user_group_members WHERE group_id=?) ORDER BY name COLLATE NOCASE`, g.ID)
		if err != nil {
			return out, err
		}
		for mr.Next() {
			u, err := scanUser(mr)
			if err != nil {
				mr.Close()
				return out, err
			}
			g.Members = append(g.Members, redact(e, u))
			g.MemberIDs = append(g.MemberIDs, u.ID)
		}
		mr.Close()
	}
	return out, nil
}
