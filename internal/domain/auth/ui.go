package auth

import (
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

const (
	oidcStateCk    = "weave_oidc_state"
	oidcNextCk     = "weave_oidc_next"
	oidcVerifierCk = "weave_oidc_pkce"
)

func init() {
	web.AddFuncs(template.FuncMap{"authInitial": func(s string) string {
		r, _ := utf8.DecodeRuneInString(strings.TrimSpace(s))
		if r == utf8.RuneError {
			return "?"
		}
		return strings.ToUpper(string(r))
	}})

	web.RoutePublic("GET /login", loginPage)
	web.RoutePublic("POST /login/terms", loginTerms)
	web.RoutePublic("GET /logout", logout) // redirects home; only POST signs out
	web.RoutePublic("POST /logout", logout)
	web.RoutePublic("GET /auth/oidc/login", oidcStart)
	web.RoutePublic("GET /auth/oidc/callback", oidcCallback)
	web.Route("GET /avatars/{id}", avatarGet)
	web.Route("POST /ui/presence", func(c *web.Ctx) error {
		if _, err := c.Do("presence.set"); err != nil {
			return err
		}
		c.W.WriteHeader(http.StatusNoContent)
		return nil
	})

	web.Route("GET /settings", func(c *web.Ctx) error { return renderSettings(c, &settingsData{}) })
	me := func(op, flash string, after func(out any, d *settingsData)) func(*web.Ctx) error {
		return settingsAction(op, flash, after)
	}
	web.Route("POST /ui/settings/profile", me("user.update", "Profile saved.", nil))
	web.Route("POST /ui/settings/avatar", me("user.avatar.update", "Avatar updated.", nil))
	web.Route("POST /ui/settings/avatar/delete", me("user.avatar.delete", "Avatar removed.", nil))
	web.Route("POST /ui/settings/preferences", me("user.preferences.update", "Preferences saved.", nil))
	web.Route("POST /ui/settings/apikey", me("apikey.generate", "", func(out any, d *settingsData) { d.NewAPIKey = out.(APIKeyOut).Key }))
	web.Route("POST /ui/settings/apikey/revoke", me("apikey.revoke", "API key revoked.", nil))
	web.Route("POST /ui/settings/sessions/{id}/revoke", me("session.revoke", "Session revoked.", nil))
	web.Route("POST /ui/settings/sessions/revoke-others", me("session.revoke_others", "Other sessions signed out.", nil))

	web.Route("GET /admin/users", adminOnly(usersPage))
	web.Route("POST /ui/admin/users/{id}/deactivate", adminOnly(usersAction("user.deactivate", "User deactivated.")))
	web.Route("POST /ui/admin/users/{id}/activate", adminOnly(usersAction("user.activate", "User activated.")))
	web.Route("POST /ui/admin/users/{id}/delete", adminOnly(usersAction("user.delete", "User deleted.")))
	web.Route("POST /ui/admin/users/{id}/relink", adminOnly(usersAction("user.relink", "SSO identity unlinked; the next SSO login with this e-mail binds a new identity.")))
	web.Route("POST /ui/admin/users/{id}/sessions/revoke-all", adminOnly(usersAction("user.sessions.revokeAll", "User signed out everywhere.")))

	web.Route("GET /admin/groups", groupsPage)
	web.Route("POST /ui/admin/groups", groupsAction("group.create"))
	web.Route("POST /ui/admin/groups/{id}", groupsAction("group.update"))
	web.Route("POST /ui/admin/groups/{id}/delete", groupsAction("group.delete"))
	web.Route("POST /ui/admin/groups/{id}/members", groupsAction("group.member.add"))
	web.Route("POST /ui/admin/groups/{id}/members/{userId}/remove", groupsAction("group.member.remove"))

	web.Route("GET /admin/sso", adminOnly(func(c *web.Ctx) error { return renderSSO(c, "", "") }))
	web.Route("POST /ui/admin/sso", adminOnly(func(c *web.Ctx) error {
		_, err := c.Do("auth.oidc.settings.update")
		return ssoResult(c, err, "SSO settings saved.")
	}))
	web.Route("POST /ui/admin/sso/terms", adminOnly(func(c *web.Ctx) error {
		_, err := c.Do("auth.terms.set")
		return ssoResult(c, err, "Terms saved.")
	}))
}

func adminOnly(h func(*web.Ctx) error) func(*web.Ctx) error {
	return func(c *web.Ctx) error {
		if !isAdmin(c.Env) {
			return ops.NotFound("page")
		}
		return h(c)
	}
}

func errMsg(err error) (string, error) {
	if oe, ok := err.(*ops.Error); ok {
		return oe.Msg, nil
	}
	return "", err
}

// ---------------- login ----------------

type loginData struct {
	Step, Error, Next, NextEnc, PendingToken string
	LoggedOut                                bool
	Terms                                    TermsOut
}

// safeNext returns n only when it is a same-site absolute path ("/x?y"); anything that a browser
// could interpret as another origin (//host, /\host, control characters, whitespace) becomes "/".
func safeNext(n string) string {
	if n == "" || n[0] != '/' || strings.HasPrefix(n, "//") || strings.HasPrefix(n, "/login") || strings.Contains(n, "\\") {
		return "/"
	}
	for _, r := range n {
		if r <= 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) || unicode.IsSpace(r) {
			return "/"
		}
	}
	u, err := url.Parse(n)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || !strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") {
		return "/"
	}
	return n
}

func renderLogin(c *web.Ctx, d loginData) error {
	if d.Next != "" {
		d.Next = safeNext(d.Next)
		d.NextEnc = url.QueryEscape(d.Next)
	}
	if c.IsHTMX() {
		return c.RenderBlock("auth_login.html", "login-box", d)
	}
	return c.Render("auth_login.html", d)
}

// loginPage shows the SSO button; with nothing to report it goes straight to the identity provider.
func loginPage(c *web.Ctx) error {
	q := c.R.URL.Query()
	if c.Env.User != nil {
		c.Redirect(safeNext(q.Get("next")))
		return nil
	}
	if q.Get("error") == "" && q.Get("loggedout") == "" {
		target := "/auth/oidc/login"
		if n := safeNext(q.Get("next")); n != "/" {
			target += "?next=" + url.QueryEscape(n)
		}
		http.Redirect(c.W, c.R, target, http.StatusSeeOther)
		return nil
	}
	return renderLogin(c, loginData{Next: q.Get("next"), Error: q.Get("error"), LoggedOut: q.Get("loggedout") != ""})
}

// secureCookies marks cookies Secure when the configured public URL is https (or TLS terminates
// here). It deliberately ignores client-supplied headers such as X-Forwarded-Proto.
func secureCookies(c *web.Ctx) bool {
	return c.R.TLS != nil || strings.HasPrefix(strings.ToLower(cfgOf(c.Env).BaseURL), "https://")
}

func setSessionCookie(c *web.Ctx, out LoginOut) {
	exp, _ := time.Parse(time.RFC3339, out.ExpiresAt)
	http.SetCookie(c.W, &http.Cookie{Name: CookieName, Value: out.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureCookies(c), Expires: exp})
}

func loginTerms(c *web.Ctx) error {
	_ = c.R.ParseForm()
	out, err := c.Do("auth.terms.accept")
	if err != nil {
		m, err2 := errMsg(err)
		if err2 != nil {
			return err2
		}
		return renderLogin(c, loginData{Error: m})
	}
	setSessionCookie(c, out.(LoginOut))
	c.Redirect(nextCookie(c))
	return nil
}

func logout(c *web.Ctx) error {
	if c.R.Method != http.MethodPost { // state-changing: never on GET (links, prefetch, <img>)
		c.Redirect("/")
		return nil
	}
	_, _ = ops.Call(c.Env, "auth.logout", &NoInput{})
	http.SetCookie(c.W, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureCookies(c)})
	c.Redirect("/login?loggedout=1")
	return nil
}

// nextCookie returns (and clears) the post-login destination stored when sign-in started.
func nextCookie(c *web.Ctx) string {
	ck, err := c.R.Cookie(oidcNextCk)
	if err != nil {
		return "/"
	}
	http.SetCookie(c.W, &http.Cookie{Name: oidcNextCk, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureCookies(c)})
	n, _ := url.QueryUnescape(ck.Value)
	return safeNext(n)
}

func oidcStart(c *web.Ctx) error {
	out, err := ops.Call(c.Env, "auth.oidc.authorize", &NoInput{})
	if err != nil {
		m, err2 := errMsg(err)
		if err2 != nil {
			return err2
		}
		http.Redirect(c.W, c.R, "/login?error="+url.QueryEscape(m), http.StatusSeeOther)
		return nil
	}
	o := out.(OIDCAuthorizeOut)
	http.SetCookie(c.W, &http.Cookie{Name: oidcStateCk, Value: o.State, Path: "/auth/oidc", MaxAge: 600, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureCookies(c)})
	http.SetCookie(c.W, &http.Cookie{Name: oidcVerifierCk, Value: o.Verifier, Path: "/auth/oidc", MaxAge: 600, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureCookies(c)})
	if n := safeNext(c.R.URL.Query().Get("next")); n != "/" {
		http.SetCookie(c.W, &http.Cookie{Name: oidcNextCk, Value: url.QueryEscape(n), Path: "/", MaxAge: 600, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureCookies(c)})
	}
	http.Redirect(c.W, c.R, o.URL, http.StatusSeeOther)
	return nil
}

func oidcCallback(c *web.Ctx) error {
	fail := func(m string) error {
		http.Redirect(c.W, c.R, "/login?error="+url.QueryEscape(m), http.StatusSeeOther)
		return nil
	}
	if e := c.R.URL.Query().Get("error"); e != "" {
		return fail("Single sign-on failed: " + e)
	}
	ck, err := c.R.Cookie(oidcStateCk)
	if err != nil || ck.Value != c.R.URL.Query().Get("state") {
		return fail("Single sign-on session expired; try again.")
	}
	http.SetCookie(c.W, &http.Cookie{Name: oidcStateCk, Value: "", Path: "/auth/oidc", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureCookies(c)})
	http.SetCookie(c.W, &http.Cookie{Name: oidcVerifierCk, Value: "", Path: "/auth/oidc", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secureCookies(c)})
	// PKCE: the verifier only ever comes from the browser's own cookie, never from the URL
	q := c.R.URL.Query()
	q.Del("code_verifier")
	if vk, err := c.R.Cookie(oidcVerifierCk); err == nil && vk.Value != "" {
		q.Set("code_verifier", vk.Value)
	}
	c.R.URL.RawQuery = q.Encode()
	out, err := c.Do("auth.oidc.callback")
	if err != nil {
		m, err2 := errMsg(err)
		if err2 != nil {
			return err2
		}
		return fail(m)
	}
	lo := out.(LoginOut)
	if lo.PendingTerms {
		t, _ := ops.Typed[TermsOut](c.Env, "auth.terms.get", NoInput{})
		return renderLogin(c, loginData{Step: "terms", PendingToken: lo.Token, Terms: t})
	}
	setSessionCookie(c, lo)
	http.Redirect(c.W, c.R, nextCookie(c), http.StatusSeeOther)
	return nil
}

func avatarGet(c *web.Ctx) error {
	h := c.W.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	out, err := c.Do("user.avatar.get")
	if err != nil {
		// No uploaded picture: serve the generated Landscape avatar for this user id.
		if e, ok := err.(*ops.Error); !ok || e.Status != 404 {
			return err
		}
		svg, gerr := GeneratedAvatarSVG(c.R.PathValue("id"))
		if gerr != nil {
			return gerr
		}
		h.Set("Content-Type", "image/svg+xml")
		h.Set("Content-Security-Policy", "sandbox")
		h.Set("Cache-Control", "private, max-age=3600")
		_, err = c.W.Write(svg)
		return err
	}
	s := out.(*ops.Stream)
	h.Set("Content-Type", s.ContentType)
	if c.R.URL.Query().Get("v") != "" {
		h.Set("Cache-Control", "private, max-age=86400") // versioned URL: safe to cache long
	} else {
		h.Set("Cache-Control", "private, max-age=60") // unversioned (navbar): picks up a new upload quickly
	}
	_, err = c.W.Write(s.Data)
	return err
}

// ---------------- settings ----------------

type settingsData struct {
	User        *User
	Sessions    []SessionInfo
	Flash       string
	Error       string
	NewAPIKey   string
	BaseURL     string
	Themes      []string
	Densities   []string
	ColourModes []string
	Editors     []string
	HomeViews   []string
	Orders      []string
}

func renderSettings(c *web.Ctx, d *settingsData) error {
	u, err := ops.Typed[*User](c.Env, "auth.me", NoInput{})
	if err != nil {
		return err
	}
	d.User = u
	if s, err := ops.Typed[SessionList](c.Env, "session.list", NoInput{}); err == nil {
		d.Sessions = s.Items
	}
	d.BaseURL = cfgOf(c.Env).BaseURL
	d.Themes, d.Densities, d.ColourModes, d.Editors, d.HomeViews, d.Orders = themes, densities, colourMode, editors, homeViews, orders
	if c.IsHTMX() && c.R.Method != "GET" {
		return c.RenderBlock("auth_settings.html", "settings-body", d)
	}
	return c.Render("auth_settings.html", d)
}

func settingsAction(op, flash string, after func(out any, d *settingsData)) func(*web.Ctx) error {
	return func(c *web.Ctx) error {
		if c.R.PathValue("id") == "" {
			c.R.SetPathValue("id", "me")
		}
		d := &settingsData{}
		out, err := c.Do(op)
		if err != nil {
			m, err2 := errMsg(err)
			if err2 != nil {
				return err2
			}
			d.Error = m
		} else {
			d.Flash = flash
			if after != nil {
				after(out, d)
			}
		}
		return renderSettings(c, d)
	}
}

// ---------------- admin users ----------------

type usersData struct {
	Users           []*User
	Roles           []string
	Q               string
	ShowDeactivated bool
	Flash, Error    string
}

func renderUsers(c *web.Ctx, flash, errm string) error {
	q := c.R.URL.Query()
	if c.R.Method != "GET" {
		q = url.Values{}
	}
	l, err := ops.Typed[UserList](c.Env, "user.list", &ListIn{Q: q.Get("q"), IncludeDeactivated: q.Get("includeDeactivated") == "true"})
	if err != nil {
		return err
	}
	d := usersData{Users: l.Items, Roles: Roles, Q: q.Get("q"), ShowDeactivated: q.Get("includeDeactivated") == "true", Flash: flash, Error: errm}
	if c.IsHTMX() {
		return c.RenderBlock("auth_admin_users.html", "users-body", d)
	}
	return c.Render("auth_admin_users.html", d)
}

func usersPage(c *web.Ctx) error { return renderUsers(c, "", "") }

func usersAction(op, flash string) func(*web.Ctx) error {
	return func(c *web.Ctx) error {
		if _, err := c.Do(op); err != nil {
			m, err2 := errMsg(err)
			if err2 != nil {
				return err2
			}
			return renderUsers(c, "", m)
		}
		return renderUsers(c, flash, "")
	}
}

// ---------------- groups ----------------

type groupsData struct {
	Groups  []*Group
	Users   []*User
	IsAdmin bool
	Error   string
}

func renderGroups(c *web.Ctx, errm string) error {
	l, err := ops.Typed[GroupList](c.Env, "group.list", NoInput{})
	if err != nil {
		return err
	}
	d := groupsData{Groups: l.Items, IsAdmin: isAdmin(c.Env), Error: errm}
	if d.IsAdmin {
		ul, _ := ops.Typed[UserList](c.Env, "user.list", &ListIn{})
		d.Users = ul.Items
	}
	if c.IsHTMX() && c.R.Method != "GET" {
		return c.RenderBlock("auth_admin_groups.html", "groups-body", d)
	}
	return c.Render("auth_admin_groups.html", d)
}

func groupsPage(c *web.Ctx) error { return renderGroups(c, "") }

func groupsAction(op string) func(*web.Ctx) error {
	return adminOnly(func(c *web.Ctx) error {
		if _, err := c.Do(op); err != nil {
			m, err2 := errMsg(err)
			if err2 != nil {
				return err2
			}
			return renderGroups(c, m)
		}
		return renderGroups(c, "")
	})
}

// ---------------- sso ----------------

type ssoData struct {
	S            OIDCSettings
	Terms        TermsOut
	Flash, Error string
}

func renderSSO(c *web.Ctx, flash, errm string) error {
	s, err := ops.Typed[OIDCSettings](c.Env, "auth.oidc.settings.get", NoInput{})
	if err != nil {
		return err
	}
	t, _ := ops.Typed[TermsOut](c.Env, "auth.terms.get", NoInput{})
	d := ssoData{S: s, Terms: t, Flash: flash, Error: errm}
	if c.IsHTMX() && c.R.Method != "GET" {
		return c.RenderBlock("auth_admin_sso.html", "sso-body", d)
	}
	return c.Render("auth_admin_sso.html", d)
}

func ssoResult(c *web.Ctx, err error, flash string) error {
	if err != nil {
		m, err2 := errMsg(err)
		if err2 != nil {
			return err2
		}
		return renderSSO(c, "", m)
	}
	return renderSSO(c, flash, "")
}
