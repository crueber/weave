package auth

import (
	"database/sql"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
)

// User is the public representation of an account. Contact details and
// preferences are only filled for the user themself and admins.
type User struct {
	ID                            string  `json:"id"`
	Email                         string  `json:"email,omitempty"`
	Username                      *string `json:"username"`
	Name                          string  `json:"name"`
	Role                          string  `json:"role"`
	AvatarURL                     string  `json:"avatarUrl,omitempty"`
	Phone                         *string `json:"phone,omitempty"`
	Organization                  *string `json:"organization,omitempty"`
	Language                      *string `json:"language,omitempty"`
	Theme                         *string `json:"theme,omitempty"`
	CardDensity                   *string `json:"cardDensity,omitempty"`
	ColourBlindMode               *string `json:"colourBlindMode,omitempty"`
	AutoLogoutMinutes             *int    `json:"autoLogoutMinutes,omitempty"`
	DefaultEditorMode             string  `json:"defaultEditorMode,omitempty"`
	DefaultHomeView               string  `json:"defaultHomeView,omitempty"`
	DefaultProjectsOrder          string  `json:"defaultProjectsOrder,omitempty"`
	SubscribeOwnCards             bool    `json:"subscribeOwnCards"`
	SubscribeToCardWhenCommenting bool    `json:"subscribeToCardWhenCommenting"`
	TurnOffRecentCardHighlighting bool    `json:"turnOffRecentCardHighlighting"`
	EnableFavoritesByDefault      bool    `json:"enableFavoritesByDefault"`
	IsSSO                         bool    `json:"isSso"`
	IsDeactivated                 bool    `json:"isDeactivated"`
	HasAPIKey                     bool    `json:"hasApiKey"`
	APIKeyPrefix                  string  `json:"apiKeyPrefix,omitempty"`
	APIKeyExpiresAt               *string `json:"apikeyExpiresAt,omitempty" desc:"when the API key stops working; absent = never"`
	APIKeyExpired                 bool    `json:"apikeyExpired,omitempty"`
	Presence                      string  `json:"presence"`
	TermsAcceptedAt               *string `json:"termsAcceptedAt,omitempty"`
	CreatedAt                     string  `json:"createdAt"`
	IsAgent                       bool    `json:"isAgent"`
	avatarFile                    string
}

const userCols = `id,email,username,name,role,COALESCE(avatar,''),phone,organization,language,theme,card_density,colour_blind_mode,auto_logout_minutes,
default_editor_mode,default_home_view,default_projects_order,subscribe_own_cards,subscribe_to_card_when_commenting,turn_off_recent_card_highlighting,
enable_favorites_by_default,is_sso,is_deactivated,api_key_hash IS NOT NULL,COALESCE(api_key_prefix,''),presence,
last_seen_at,terms_accepted_at,created_at,is_agent,api_key_expires_at`

type rowScanner interface{ Scan(...any) error }

func scanUser(r rowScanner) (*User, error) {
	u := &User{}
	var lastSeen *string
	err := r.Scan(&u.ID, &u.Email, &u.Username, &u.Name, &u.Role, &u.avatarFile, &u.Phone, &u.Organization, &u.Language, &u.Theme, &u.CardDensity, &u.ColourBlindMode,
		&u.AutoLogoutMinutes, &u.DefaultEditorMode, &u.DefaultHomeView, &u.DefaultProjectsOrder, &u.SubscribeOwnCards, &u.SubscribeToCardWhenCommenting,
		&u.TurnOffRecentCardHighlighting, &u.EnableFavoritesByDefault, &u.IsSSO, &u.IsDeactivated, &u.HasAPIKey, &u.APIKeyPrefix,
		&u.Presence, &lastSeen, &u.TermsAcceptedAt, &u.CreatedAt, &u.IsAgent, &u.APIKeyExpiresAt)
	if err != nil {
		return nil, err
	}
	u.APIKeyExpired = keyExpired(u.APIKeyExpiresAt)
	u.Presence = effectivePresence(u.Presence, lastSeen)
	if u.avatarFile != "" {
		u.AvatarURL = "/avatars/" + u.ID + "?v=" + sha(u.avatarFile)[:8]
	}
	return u, nil
}

func loadUser(e *ops.Env, id string) (*User, error) {
	u, err := scanUser(e.DB.QueryRowContext(e.Ctx, `SELECT `+userCols+` FROM users WHERE id=? AND deleted_at IS NULL`, id))
	if err == sql.ErrNoRows {
		return nil, ops.NotFound("user")
	}
	return u, err
}

func isAdmin(e *ops.Env) bool { return e.User != nil && e.User.Role == "admin" }

// redact removes private details when e is neither the user nor an admin.
func redact(e *ops.Env, u *User) *User {
	if isAdmin(e) || (e.User != nil && e.User.UserID == u.ID) {
		return u
	}
	return &User{ID: u.ID, Username: u.Username, Name: u.Name, Role: u.Role, AvatarURL: u.AvatarURL, Presence: u.Presence, IsDeactivated: u.IsDeactivated, IsAgent: u.IsAgent}
}

// UserBrief returns a user's public fields (nil when the user does not exist).
func UserBrief(e *ops.Env, id string) *User {
	u, err := loadUser(e, id)
	if err != nil {
		return nil
	}
	return &User{ID: u.ID, Username: u.Username, Name: u.Name, AvatarURL: u.AvatarURL, IsAgent: u.IsAgent}
}

// resolve maps "me" to the caller and enforces self-or-admin access.
func resolve(e *ops.Env, id string, adminOnly bool) (string, error) {
	if e.User == nil {
		return "", ops.Unauthorized()
	}
	if id == "me" {
		id = e.User.UserID
	}
	if adminOnly && !isAdmin(e) {
		return "", ops.Forbidden()
	}
	if id != e.User.UserID && !isAdmin(e) {
		return "", ops.Forbidden()
	}
	return id, nil
}

func countOtherAdmins(e *ops.Env, id string) int {
	var n int
	_ = e.DB.QueryRowContext(e.Ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND is_deactivated=0 AND deleted_at IS NULL AND id<>?`, id).Scan(&n)
	return n
}

var usernameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{2,31}$`)

func cleanEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || !strings.Contains(s, "@") {
		return "", ops.Invalid("invalid email address")
	}
	return strings.ToLower(s), nil
}

func cleanUsername(s string) (any, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if !usernameRe.MatchString(s) || strings.Contains(s, "@") {
		return nil, ops.Invalid("username must be 3-32 characters: letters, digits, _ . -")
	}
	return s, nil
}

func ptrEmpty(s *string) any {
	if s == nil || strings.TrimSpace(*s) == "" {
		return nil
	}
	return strings.TrimSpace(*s)
}

func publish(e *ops.Env, name, id string) {
	e.Hub.Publish(realtime.Event{Name: name, Room: "global", Data: map[string]any{"id": id}})
}

// ---------------- op inputs ----------------

type NoInput struct{}
type IDIn struct {
	ID string `path:"id" desc:"user id, or \"me\""`
}
type UserList struct {
	Items []*User `json:"items"`
}
type ListIn struct {
	Q                  string `query:"q" desc:"filter by name, username or (admin) email"`
	IncludeDeactivated bool   `query:"includeDeactivated" desc:"admin only: include deactivated accounts"`
}
type UpdateIn struct {
	ID           string  `path:"id" desc:"user id, or \"me\""`
	Phone        *string `json:"phone"`
	Organization *string `json:"organization"`
}
type PrefsIn struct {
	ID                            string  `path:"id" desc:"user id, or \"me\""`
	Language                      *string `json:"language" desc:"locale tag e.g. en, de"`
	Theme                         *string `json:"theme" desc:"auto|light|dark"`
	CardDensity                   *string `json:"cardDensity" desc:"comfortable|compact"`
	ColourBlindMode               *string `json:"colourBlindMode" desc:"none|protanopia|deuteranopia|tritanopia"`
	AutoLogoutMinutes             *int    `json:"autoLogoutMinutes" desc:"idle minutes before the session ends; 0 disables"`
	DefaultEditorMode             *string `json:"defaultEditorMode" desc:"wysiwyg|markdown"`
	DefaultHomeView               *string `json:"defaultHomeView" desc:"groupedByProject|gridProjects|groupedByList"`
	DefaultProjectsOrder          *string `json:"defaultProjectsOrder" desc:"byDefault|alphabetically|byCreationTime"`
	SubscribeOwnCards             *bool   `json:"subscribeOwnCards"`
	SubscribeToCardWhenCommenting *bool   `json:"subscribeToCardWhenCommenting"`
	TurnOffRecentCardHighlighting *bool   `json:"turnOffRecentCardHighlighting"`
	EnableFavoritesByDefault      *bool   `json:"enableFavoritesByDefault"`
}
type AvatarIn struct {
	ID   string    `path:"id" desc:"user id, or \"me\""`
	File *ops.File `json:"file" desc:"png, jpeg, gif or webp image (max 5 MB)"`
}

var (
	themes     = []string{"auto", "light", "dark"}
	densities  = []string{"comfortable", "compact"}
	colourMode = []string{"none", "protanopia", "deuteranopia", "tritanopia"}
	editors    = []string{"wysiwyg", "markdown"}
	homeViews  = []string{"groupedByProject", "gridProjects", "groupedByList"}
	orders     = []string{"byDefault", "alphabetically", "byCreationTime"}
)

func oneOf(field string, v *string, allowed []string) error {
	if v == nil {
		return nil
	}
	for _, a := range allowed {
		if a == *v {
			return nil
		}
	}
	return ops.Invalid(fmt.Sprintf("%s must be one of %s", field, strings.Join(allowed, "|")))
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func init() {
	ops.Register(ops.Op{Name: "user.list", Summary: "List users (admins see everything, others a directory of active users; guests and workers only see accounts that share a board with them)", Method: "GET", Path: "/users", UI: "GET /admin/users"},
		func(e *ops.Env, in ListIn) (UserList, error) {
			q := `SELECT ` + userCols + ` FROM users WHERE deleted_at IS NULL`
			var args []any
			if directoryRestricted(e) { // guests/workers only see accounts they share a board with
				sub, subArgs := visibleUsersSQL(e)
				q += ` AND id IN (` + sub + `)`
				args = append(args, subArgs...)
			}
			if !(isAdmin(e) && in.IncludeDeactivated) {
				q += ` AND is_deactivated=0`
			}
			if in.Q != "" {
				like := "%" + strings.NewReplacer("%", `\%`, "_", `\_`).Replace(in.Q) + "%"
				if isAdmin(e) {
					q += ` AND (name LIKE ? ESCAPE '\' OR username LIKE ? ESCAPE '\' OR email LIKE ? ESCAPE '\')`
					args = append(args, like, like, like)
				} else {
					q += ` AND (name LIKE ? ESCAPE '\' OR username LIKE ? ESCAPE '\')`
					args = append(args, like, like)
				}
			}
			rows, err := e.DB.QueryContext(e.Ctx, q+` ORDER BY name COLLATE NOCASE LIMIT 1000`, args...)
			if err != nil {
				return UserList{}, err
			}
			defer rows.Close()
			out := UserList{Items: []*User{}}
			for rows.Next() {
				u, err := scanUser(rows)
				if err != nil {
					return out, err
				}
				out.Items = append(out.Items, redact(e, u))
			}
			return out, rows.Err()
		})

	ops.Register(ops.Op{Name: "user.get", Summary: "Get one user", Method: "GET", Path: "/users/{id}", UI: "GET /admin/users"},
		func(e *ops.Env, in IDIn) (*User, error) {
			if in.ID == "me" && e.User != nil {
				in.ID = e.User.UserID
			}
			u, err := loadUser(e, in.ID)
			if err != nil {
				return nil, err
			}
			if directoryRestricted(e) && !visibleUserIDs(e)[u.ID] {
				return nil, ops.NotFound("user")
			}
			return redact(e, u), nil
		})

	ops.Register(ops.Op{Name: "user.update", Summary: "Update profile fields (phone, organization); name, email and role are managed by the identity provider", Method: "PATCH", Path: "/users/{id}", UI: "POST /ui/settings/profile (self) and POST /ui/admin/users/{id} (admin)"},
		func(e *ops.Env, in UpdateIn) (*User, error) {
			id, err := resolve(e, in.ID, false)
			if err != nil {
				return nil, err
			}
			if in.Phone != nil {
				_, _ = e.DB.ExecContext(e.Ctx, `UPDATE users SET phone=?,updated_at=? WHERE id=?`, ptrEmpty(in.Phone), db.Now(), id)
			}
			if in.Organization != nil {
				_, _ = e.DB.ExecContext(e.Ctx, `UPDATE users SET organization=?,updated_at=? WHERE id=?`, ptrEmpty(in.Organization), db.Now(), id)
			}
			publish(e, "user.updated", id)
			return loadUser(e, id)
		})

	ops.Register(ops.Op{Name: "user.preferences.update", Summary: "Update language, theme, density, colour-blind mode, auto-logout, editor mode, home view, project order and subscription defaults", Method: "PATCH", Path: "/users/{id}/preferences", UI: "POST /ui/settings/preferences"},
		func(e *ops.Env, in PrefsIn) (*User, error) {
			id, err := resolve(e, in.ID, false)
			if err != nil {
				return nil, err
			}
			for _, c := range []error{oneOf("theme", in.Theme, themes), oneOf("cardDensity", in.CardDensity, densities), oneOf("colourBlindMode", in.ColourBlindMode, colourMode),
				oneOf("defaultEditorMode", in.DefaultEditorMode, editors), oneOf("defaultHomeView", in.DefaultHomeView, homeViews), oneOf("defaultProjectsOrder", in.DefaultProjectsOrder, orders)} {
				if c != nil {
					return nil, c
				}
			}
			if in.AutoLogoutMinutes != nil && (*in.AutoLogoutMinutes < 0 || *in.AutoLogoutMinutes > 525600) {
				return nil, ops.Invalid("autoLogoutMinutes out of range")
			}
			sets, args := []string{}, []any{}
			add := func(col string, v any) { sets = append(sets, col+"=?"); args = append(args, v) }
			if in.Language != nil {
				add("language", ptrEmpty(in.Language))
			}
			if in.Theme != nil {
				add("theme", *in.Theme)
			}
			if in.CardDensity != nil {
				add("card_density", *in.CardDensity)
			}
			if in.ColourBlindMode != nil {
				add("colour_blind_mode", *in.ColourBlindMode)
			}
			if in.AutoLogoutMinutes != nil {
				if *in.AutoLogoutMinutes == 0 {
					add("auto_logout_minutes", nil)
				} else {
					add("auto_logout_minutes", *in.AutoLogoutMinutes)
				}
			}
			if in.DefaultEditorMode != nil {
				add("default_editor_mode", *in.DefaultEditorMode)
			}
			if in.DefaultHomeView != nil {
				add("default_home_view", *in.DefaultHomeView)
			}
			if in.DefaultProjectsOrder != nil {
				add("default_projects_order", *in.DefaultProjectsOrder)
			}
			if in.SubscribeOwnCards != nil {
				add("subscribe_own_cards", b2i(*in.SubscribeOwnCards))
			}
			if in.SubscribeToCardWhenCommenting != nil {
				add("subscribe_to_card_when_commenting", b2i(*in.SubscribeToCardWhenCommenting))
			}
			if in.TurnOffRecentCardHighlighting != nil {
				add("turn_off_recent_card_highlighting", b2i(*in.TurnOffRecentCardHighlighting))
			}
			if in.EnableFavoritesByDefault != nil {
				add("enable_favorites_by_default", b2i(*in.EnableFavoritesByDefault))
			}
			if len(sets) > 0 {
				add("updated_at", db.Now())
				args = append(args, id)
				if _, err := e.DB.ExecContext(e.Ctx, `UPDATE users SET `+strings.Join(sets, ",")+` WHERE id=?`, args...); err != nil {
					return nil, err
				}
			}
			publish(e, "user.updated", id)
			return loadUser(e, id)
		})

	ops.Register(ops.Op{Name: "user.avatar.update", Summary: "Upload a new avatar (resized to 256px PNG)", Method: "PUT", MaxBody: 6 << 20, Path: "/users/{id}/avatar", UI: "POST /ui/settings/avatar"},
		func(e *ops.Env, in AvatarIn) (*User, error) {
			id, err := resolve(e, in.ID, false)
			if err != nil {
				return nil, err
			}
			if in.File == nil {
				return nil, ops.Invalid("file is required")
			}
			png, err := normalizeAvatar(in.File.Data)
			if err != nil {
				return nil, ops.Invalid(err.Error())
			}
			dir := filepath.Join(dataDir(e), "files", "avatars")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, err
			}
			name := id + "-" + db.Token(4) + ".png"
			if err := os.WriteFile(filepath.Join(dir, name), png, 0o644); err != nil {
				return nil, err
			}
			removeAvatarFile(e, id)
			if _, err := e.DB.ExecContext(e.Ctx, `UPDATE users SET avatar=?,updated_at=? WHERE id=?`, name, db.Now(), id); err != nil {
				return nil, err
			}
			publish(e, "user.updated", id)
			return loadUser(e, id)
		})

	ops.Register(ops.Op{Name: "user.avatar.delete", Summary: "Remove a user's avatar", Method: "DELETE", Path: "/users/{id}/avatar", UI: "POST /ui/settings/avatar/delete"},
		func(e *ops.Env, in IDIn) (*User, error) {
			id, err := resolve(e, in.ID, false)
			if err != nil {
				return nil, err
			}
			removeAvatarFile(e, id)
			_, _ = e.DB.ExecContext(e.Ctx, `UPDATE users SET avatar=NULL,updated_at=? WHERE id=?`, db.Now(), id)
			publish(e, "user.updated", id)
			return loadUser(e, id)
		})

	ops.Register(ops.Op{Name: "user.avatar.get", Summary: "Download a user's avatar image", Method: "GET", Path: "/users/{id}/avatar", Binary: true, UI: "GET /avatars/{id}"},
		func(e *ops.Env, in IDIn) (*ops.Stream, error) {
			if in.ID == "me" && e.User != nil {
				in.ID = e.User.UserID
			}
			var f string
			if e.DB.QueryRowContext(e.Ctx, `SELECT COALESCE(avatar,'') FROM users WHERE id=? AND deleted_at IS NULL`, in.ID).Scan(&f) != nil || f == "" {
				return nil, ops.NotFound("avatar")
			}
			b, err := os.ReadFile(filepath.Join(dataDir(e), "files", "avatars", filepath.Base(f)))
			if err != nil {
				return nil, ops.NotFound("avatar")
			}
			return &ops.Stream{Name: f, ContentType: "image/png", Data: b}, nil
		})

	ops.Register(ops.Op{Name: "user.deactivate", Summary: "Deactivate a user (blocks login, revokes sessions and API key access)", Method: "POST", Path: "/users/{id}/deactivate", Auth: ops.AuthAdmin, UI: "POST /ui/admin/users/{id}/deactivate"},
		func(e *ops.Env, in IDIn) (*User, error) {
			u, err := loadUser(e, in.ID)
			if err != nil {
				return nil, err
			}
			if u.ID == e.User.UserID {
				return nil, ops.Conflict("you cannot deactivate yourself")
			}
			if u.Role == "admin" && countOtherAdmins(e, u.ID) == 0 {
				return nil, ops.Conflict("cannot deactivate the last administrator")
			}
			_, _ = e.DB.ExecContext(e.Ctx, `UPDATE users SET is_deactivated=1,presence='offline',updated_at=? WHERE id=?`, db.Now(), u.ID)
			_, _ = e.DB.ExecContext(e.Ctx, `DELETE FROM sessions WHERE user_id=?`, u.ID)
			publish(e, "user.updated", u.ID)
			return loadUser(e, u.ID)
		})

	ops.Register(ops.Op{Name: "user.activate", Summary: "Re-activate a deactivated user", Method: "POST", Path: "/users/{id}/activate", Auth: ops.AuthAdmin, UI: "POST /ui/admin/users/{id}/activate"},
		func(e *ops.Env, in IDIn) (*User, error) {
			u, err := loadUser(e, in.ID)
			if err != nil {
				return nil, err
			}
			_, _ = e.DB.ExecContext(e.Ctx, `UPDATE users SET is_deactivated=0,updated_at=? WHERE id=?`, db.Now(), u.ID)
			publish(e, "user.updated", u.ID)
			return loadUser(e, u.ID)
		})

	ops.Register(ops.Op{Name: "user.delete", Summary: "Permanently delete a user", Method: "DELETE", Path: "/users/{id}", Auth: ops.AuthAdmin, UI: "POST /ui/admin/users/{id}/delete"},
		func(e *ops.Env, in IDIn) (*User, error) {
			u, err := loadUser(e, in.ID)
			if err != nil {
				return nil, err
			}
			if u.ID == e.User.UserID {
				return nil, ops.Conflict("you cannot delete yourself")
			}
			if u.Role == "admin" && countOtherAdmins(e, u.ID) == 0 {
				return nil, ops.Conflict("cannot delete the last administrator")
			}
			removeAvatarFile(e, u.ID)
			if _, err := e.DB.ExecContext(e.Ctx, `DELETE FROM users WHERE id=?`, u.ID); err != nil {
				return nil, err
			}
			publish(e, "user.deleted", u.ID)
			return u, nil
		})
}
