package notify

import (
	"context"
	"database/sql"
	"encoding/base64"
	"os"
	"reflect"
	"strconv"
	"strings"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// envName maps a config key to its overriding environment variable:
// "smtp.host" -> WEAVE_SMTP_HOST, "oidc.clientId" -> WEAVE_OIDC_CLIENTID.
func envName(key string) string {
	if key == "session.maxIdleMinutes" {
		return "WEAVE_SESSION_MAX_IDLE_MINUTES" // matches auth.GetConfig, which enforces it
	}
	return "WEAVE_" + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// EnvOverridden reports whether key is pinned by the environment.
func EnvOverridden(key string) bool { return os.Getenv(envName(key)) != "" }

// ConfigDB reads one config value: environment first, then the config table.
func ConfigDB(ctx context.Context, d *db.DB, key string) string {
	if v := os.Getenv(envName(key)); v != "" {
		return v
	}
	var v string
	if err := d.QueryRowContext(ctx, `SELECT value FROM config WHERE key=?`, key).Scan(&v); err != nil && err != sql.ErrNoRows {
		return ""
	}
	return v
}

// Config reads one config value (env overrides the config table).
func Config(e *ops.Env, key string) string { return ConfigDB(e.Ctx, e.DB, key) }

// ConfigBool is Config interpreted as a boolean.
func ConfigBool(e *ops.Env, key string) bool {
	v := strings.ToLower(Config(e, key))
	return v == "true" || v == "1" || v == "yes" || v == "on"
}

// ConfigAll returns every effective key with the given prefix (e.g. "oidc.").
func ConfigAll(e *ops.Env, prefix string) map[string]string {
	out := map[string]string{}
	rows, err := e.DB.QueryContext(e.Ctx, `SELECT key,value FROM config WHERE key LIKE ?`, prefix+"%")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var k, v string
			_ = rows.Scan(&k, &v)
			out[k] = v
		}
	}
	for _, k := range settingKeys {
		if strings.HasPrefix(k, prefix) {
			if v := os.Getenv(envName(k)); v != "" {
				out[k] = v
			}
		}
	}
	return out
}

func setConfig(tx *sql.Tx, key, val string) error {
	if val == "" {
		_, err := tx.Exec(`DELETE FROM config WHERE key=?`, key)
		return err
	}
	_, err := tx.Exec(`INSERT INTO config(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, val)
	return err
}

// Settings is the editable admin configuration. Each field's json tag is its config key.
type Settings struct {
	AppName      *string `json:"branding.appName" desc:"Application name shown in the header and emails"`
	LoginText    *string `json:"branding.loginText" desc:"Text shown on the login page"`
	PrimaryColor *string `json:"branding.primaryColor" desc:"Accent colour (CSS colour)"`

	SMTPHost          *string `json:"smtp.host" desc:"SMTP server host"`
	SMTPPort          *string `json:"smtp.port" desc:"SMTP port (default 587, or 465 for tls)"`
	SMTPUser          *string `json:"smtp.user" desc:"SMTP username"`
	SMTPPassword      *string `json:"smtp.password" desc:"SMTP password (write-only; empty keeps the current one)"`
	SMTPFrom          *string `json:"smtp.from" desc:"From address, e.g. Weave <noreply@example.com>"`
	SMTPSecure        *string `json:"smtp.secure" desc:"Transport security: none | starttls | tls"`
	SMTPSkipVerify    *string `json:"smtp.skipVerify" desc:"true to skip TLS certificate verification"`
	SMTPNotifications *string `json:"smtp.notifications" desc:"false to disable e-mail for in-app notifications"`

	OIDCIssuer        *string `json:"oidc.issuer" desc:"OIDC issuer URL"`
	OIDCClientID      *string `json:"oidc.clientId" desc:"OIDC client id"`
	OIDCClientSecret  *string `json:"oidc.clientSecret" desc:"OIDC client secret (write-only; empty keeps the current one)"`
	OIDCScopes        *string `json:"oidc.scopes" desc:"Space separated scopes (default: openid profile email groups)"`
	OIDCUsernameClaim *string `json:"oidc.usernameClaim" desc:"Claim used as username (default preferred_username)"`
	OIDCNameClaim     *string `json:"oidc.nameClaim" desc:"Claim used as display name (default name)"`
	OIDCEmailClaim    *string `json:"oidc.emailClaim" desc:"Claim used as e-mail (default email)"`
	OIDCGroupsClaim   *string `json:"oidc.groupsClaim" desc:"Claim containing the user's groups (default groups)"`
	OIDCAdminGroup    *string `json:"oidc.adminGroup" desc:"Group granting admin (default weave-admins)"`
	OIDCOwnerGroup    *string `json:"oidc.ownerGroup" desc:"Group granting projectOwner (default weave-project-owners)"`
	OIDCUserGroup     *string `json:"oidc.userGroup" desc:"Group granting boardUser (default weave-users)"`
	OIDCWorkerGroup   *string `json:"oidc.workerGroup" desc:"Group granting worker (default weave-workers)"`
	OIDCGuestGroup    *string `json:"oidc.guestGroup" desc:"Group granting guest (default weave-guests)"`
	OIDCRequireGroup  *string `json:"oidc.requireGroup" desc:"true to deny sign-in unless the user is in a role group"`
	OIDCTeamPrefix    *string `json:"oidc.teamPrefix" desc:"Groups with this prefix sync into Weave user groups (default weave-team-)"`
	OIDCAutoProvision *string `json:"oidc.autoProvision" desc:"true to create users on first SSO login"`
	OIDCButtonText    *string `json:"oidc.buttonText" desc:"Label of the SSO button"`

	TermsEnabled *string `json:"terms.enabled" desc:"true to require terms acceptance"`
	TermsText    *string `json:"terms.text" desc:"Terms of service text (markdown)"`
	TermsVersion *string `json:"terms.version" desc:"Bump to force re-acceptance"`

	SessionMaxIdleMinutes *string `json:"session.maxIdleMinutes" desc:"Maximum idle minutes before any session ends (upper bound on per-user auto-logout); 0 or empty = no limit"`
}

var (
	settingKeys   []string
	secretKeys    = map[string]bool{"smtp.password": true, "oidc.clientSecret": true}
	boolSettings  = map[string]bool{"smtp.skipVerify": true, "smtp.notifications": true, "oidc.autoProvision": true, "oidc.requireGroup": true, "terms.enabled": true}
	settingFields []reflect.StructField
)

func init() {
	t := reflect.TypeOf(Settings{})
	for i := 0; i < t.NumField(); i++ {
		settingFields = append(settingFields, t.Field(i))
		settingKeys = append(settingKeys, t.Field(i).Tag.Get("json"))
	}
}

// SettingKeys lists every editable config key (for the UI form).
func SettingKeys() []string { return append([]string(nil), settingKeys...) }

type NoInput struct{}

// SettingsView is the admin read model: secrets are masked, env-pinned keys flagged.
type SettingsView struct {
	Values     map[string]string `json:"values"`
	EnvLocked  []string          `json:"envLocked"`
	SecretsSet []string          `json:"secretsSet"`
	HasLogo    bool              `json:"hasLogo"`
}

func settingsView(e *ops.Env) SettingsView {
	v := SettingsView{Values: map[string]string{}, EnvLocked: []string{}, SecretsSet: []string{}}
	for _, k := range settingKeys {
		val := Config(e, k)
		if secretKeys[k] {
			if val != "" {
				v.SecretsSet = append(v.SecretsSet, k)
			}
			val = ""
		}
		v.Values[k] = val
		if EnvOverridden(k) {
			v.EnvLocked = append(v.EnvLocked, k)
		}
	}
	v.HasLogo = Config(e, "branding.logoData") != ""
	return v
}

// PublicConfig is what the login page needs; no secrets.
type PublicConfig struct {
	AppName        string `json:"appName"`
	LoginText      string `json:"loginText"`
	PrimaryColor   string `json:"primaryColor,omitempty"`
	LogoURL        string `json:"logoUrl,omitempty"`
	OIDCButtonText string `json:"oidcButtonText,omitempty"`
	TermsEnabled   bool   `json:"termsEnabled"`
}

// Public builds the public configuration (exported for auth's login page).
func Public(e *ops.Env) PublicConfig {
	p := PublicConfig{
		AppName: Config(e, "branding.appName"), LoginText: Config(e, "branding.loginText"), PrimaryColor: Config(e, "branding.primaryColor"),
		OIDCButtonText: Config(e, "oidc.buttonText"),
		TermsEnabled:   ConfigBool(e, "terms.enabled"),
	}
	if p.AppName == "" {
		p.AppName = "Weave"
	}
	if Config(e, "branding.logoData") != "" {
		p.LogoURL = "/branding/logo"
	}
	return p
}

type LogoIn struct {
	Logo *ops.File `json:"logo" desc:"PNG, JPEG, GIF, WebP or SVG image, max 1 MiB"`
}

type SMTPTestIn struct {
	To string `json:"to" required:"true" desc:"Recipient of the test message"`
}
type OKOut struct {
	OK bool `json:"ok"`
}

var logoTypes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "image/svg+xml": true}

func normBool(v string) string {
	switch strings.ToLower(v) {
	case "on", "true", "1", "yes":
		return "true"
	}
	return "false"
}

func init() {
	ops.Register(ops.Op{Name: "config.get", Summary: "Admin configuration (secrets masked)", Method: "GET", Path: "/config/settings", Auth: ops.AuthAdmin, UI: "GET /admin/config"},
		func(e *ops.Env, _ NoInput) (SettingsView, error) { return settingsView(e), nil })
	ops.Register(ops.Op{Name: "config.public", Summary: "Public branding/login configuration", Method: "GET", Path: "/config/public", Auth: ops.AuthPublic, UI: "exempt:consumed by the login page via notify.Public"},
		func(e *ops.Env, _ NoInput) (PublicConfig, error) { return Public(e), nil })
	ops.Register(ops.Op{Name: "config.update", Summary: "Update admin configuration (branding, SMTP, OIDC, registration, terms)", Method: "PATCH", Path: "/config/settings", Auth: ops.AuthAdmin, UI: "POST /ui/admin/config"},
		func(e *ops.Env, in Settings) (SettingsView, error) {
			rv := reflect.ValueOf(in)
			err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
				for i, f := range settingFields {
					p := rv.Field(i)
					if p.IsNil() {
						continue
					}
					key := f.Tag.Get("json")
					val := strings.TrimSpace(p.Elem().String())
					if EnvOverridden(key) {
						continue
					}
					if secretKeys[key] && val == "" {
						continue // write-only: blank keeps existing
					}
					if boolSettings[key] {
						val = normBool(val)
					}
					if key == "smtp.secure" && val != "" && val != "none" && val != "starttls" && val != "tls" {
						return ops.Invalid("smtp.secure must be none, starttls or tls")
					}
					if key == "session.maxIdleMinutes" && val != "" {
						if n, err := strconv.Atoi(val); err != nil || n < 0 || n > 525600 {
							return ops.Invalid("session.maxIdleMinutes must be a whole number of minutes (0 = no limit)")
						}
					}
					if key == "smtp.port" && val != "" {
						n := 0
						for _, c := range val {
							if c < '0' || c > '9' || n > 65535 {
								return ops.Invalid("smtp.port must be a number")
							}
							n = n*10 + int(c-'0')
						}
					}
					if err := setConfig(tx, key, val); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return SettingsView{}, err
			}
			Record(e, ops.Event{Type: "configUpdate", Name: "config.updated", Room: "global", Data: map[string]any{"item": map[string]any{"keys": changedKeys(in)}}})
			return settingsView(e), nil
		})
	ops.Register(ops.Op{Name: "config.logo.upload", Summary: "Upload the branding logo", Method: "POST", MaxBody: 2 << 20, Path: "/config/logo", Auth: ops.AuthAdmin, UI: "POST /ui/admin/config/logo"},
		func(e *ops.Env, in LogoIn) (OKOut, error) {
			if in.Logo == nil || len(in.Logo.Data) == 0 {
				return OKOut{}, ops.Invalid("logo file required")
			}
			if len(in.Logo.Data) > 1<<20 {
				return OKOut{}, ops.Invalid("logo must be at most 1 MiB")
			}
			ct := strings.ToLower(strings.SplitN(in.Logo.ContentType, ";", 2)[0])
			if !logoTypes[ct] {
				if strings.HasSuffix(strings.ToLower(in.Logo.Name), ".svg") {
					ct = "image/svg+xml"
				} else {
					ct = sniffType(in.Logo.Data)
				}
			}
			if !logoTypes[ct] {
				return OKOut{}, ops.Invalid("unsupported image type")
			}
			err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
				if err := setConfig(tx, "branding.logoData", base64.StdEncoding.EncodeToString(in.Logo.Data)); err != nil {
					return err
				}
				return setConfig(tx, "branding.logoType", ct)
			})
			return OKOut{err == nil}, err
		})
	ops.Register(ops.Op{Name: "config.logo.delete", Summary: "Remove the branding logo", Method: "DELETE", Path: "/config/logo", Auth: ops.AuthAdmin, UI: "POST /ui/admin/config/logo/delete"},
		func(e *ops.Env, _ NoInput) (OKOut, error) {
			err := e.DB.Tx(e.Ctx, func(tx *sql.Tx) error {
				if err := setConfig(tx, "branding.logoData", ""); err != nil {
					return err
				}
				return setConfig(tx, "branding.logoType", "")
			})
			return OKOut{err == nil}, err
		})
	ops.Register(ops.Op{Name: "config.logo.get", Summary: "Download the branding logo", Method: "GET", Path: "/config/logo", Auth: ops.AuthPublic, Binary: true, UI: "GET /branding/logo"},
		func(e *ops.Env, _ NoInput) (*ops.Stream, error) {
			raw := Config(e, "branding.logoData")
			if raw == "" {
				return nil, ops.NotFound("logo")
			}
			b, err := base64.StdEncoding.DecodeString(raw)
			if err != nil {
				return nil, ops.NotFound("logo")
			}
			return &ops.Stream{Name: "logo", ContentType: Config(e, "branding.logoType"), Data: b}, nil
		})
	ops.Register(ops.Op{Name: "smtp.test", Summary: "Send a test e-mail using the current SMTP settings", Method: "POST", Path: "/config/smtp/test", Auth: ops.AuthAdmin, UI: "POST /ui/admin/config/smtp-test"},
		func(e *ops.Env, in SMTPTestIn) (OKOut, error) {
			if err := SendMail(e, []string{in.To}, "Weave SMTP test", "This is a test message from Weave. SMTP is working.", "<p>This is a test message from Weave. SMTP is working.</p>"); err != nil {
				return OKOut{}, ops.Invalid("SMTP send failed: " + err.Error())
			}
			return OKOut{true}, nil
		})
}

func changedKeys(in Settings) []string {
	var ks []string
	rv := reflect.ValueOf(in)
	for i, f := range settingFields {
		if !rv.Field(i).IsNil() && !secretKeys[f.Tag.Get("json")] {
			ks = append(ks, f.Tag.Get("json"))
		}
	}
	return ks
}

func sniffType(b []byte) string {
	switch {
	case len(b) > 8 && string(b[1:4]) == "PNG":
		return "image/png"
	case len(b) > 3 && b[0] == 0xFF && b[1] == 0xD8:
		return "image/jpeg"
	case len(b) > 6 && string(b[:3]) == "GIF":
		return "image/gif"
	case len(b) > 12 && string(b[:4]) == "RIFF" && string(b[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}
