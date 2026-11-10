package notify

import (
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

func init() {
	web.AddFuncs(template.FuncMap{
		"notifyTime": func(s string) string {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return t.Local().Format("2006-01-02 15:04")
			}
			return s
		},
		"notifyHTML": func(s string) template.HTML { return template.HTML(s) }, // only fed by markSnippet (escaped)
		"notifyHas": func(l []string, s string) bool {
			for _, x := range l {
				if x == s {
					return true
				}
			}
			return false
		},
		"notifyDeref": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
	})
	web.Route("GET /notifications", pageNotifications)
	web.Route("GET /ui/notifications/badge", uiBadge)
	web.Route("POST /ui/notifications/{id}/read", uiNotifRead)
	web.Route("POST /ui/notifications/read-all", uiNotifReadAll)

	web.Route("GET /notification-services", pageServices)
	web.Route("GET /ui/boards/{boardId}/notification-services", fragBoardServices)
	web.Route("POST /ui/notification-services", uiServiceCreate)
	web.Route("POST /ui/notification-services/{id}", uiServiceUpdate)
	web.Route("POST /ui/notification-services/{id}/delete", uiServiceDelete)
	web.Route("POST /ui/notification-services/{id}/test", uiServiceTest)

	web.Route("GET /admin/webhooks", pageWebhooks)
	web.Route("POST /ui/admin/webhooks", uiWebhookCreate)
	web.Route("POST /ui/admin/webhooks/{id}", uiWebhookUpdate)
	web.Route("POST /ui/admin/webhooks/{id}/delete", uiWebhookDelete)
	web.Route("POST /ui/admin/webhooks/{id}/test", uiWebhookTest)
	web.Route("GET /admin/webhooks/{id}/deliveries", pageDeliveries)
	web.Route("POST /ui/admin/webhook-deliveries/{id}/retry", uiDeliveryRetry)

	web.Route("GET /admin/config", pageConfig)
	web.Route("POST /ui/admin/config", uiConfigSave)
	web.Route("POST /ui/admin/config/logo", uiLogoUpload)
	web.Route("POST /ui/admin/config/logo/delete", uiLogoDelete)
	web.Route("POST /ui/admin/config/smtp-test", uiSMTPTest)
	web.Route("POST /ui/admin/search/reindex", uiReindex)
	web.RoutePublic("GET /branding/logo", serveLogo)

	web.Route("GET /search", pageSearch)
	web.Route("GET /ui/search", fragSearch)
	web.Route("GET /dashboard", pageDashboard)
	web.Route("POST /ui/dashboard", uiDashboardSave)

	web.Route("GET /export/boards/{id}", serveExport("export.board"))
	web.Route("GET /export/cards/{id}", serveExport("export.card"))
}

func typed[T any](c *web.Ctx, op string) (T, error) {
	v, err := c.Do(op)
	var zero T
	if err != nil {
		return zero, err
	}
	out, _ := v.(T)
	return out, nil
}

// ---- notifications ---------------------------------------------------------

func listData(c *web.Ctx) (ListOut, error) {
	return ops.Typed[ListOut](c.Env, "notification.list", ListIn{Unread: c.R.URL.Query().Get("unread") == "1", Limit: 100})
}

func pageNotifications(c *web.Ctx) error {
	d, err := listData(c)
	if err != nil {
		return err
	}
	return c.Render("notify_notifications.html", map[string]any{"L": d, "Unread": c.R.URL.Query().Get("unread") == "1"})
}

func badgeData(c *web.Ctx) int {
	o, _ := ops.Typed[CountOut](c.Env, "notification.unread_count", NoInput{})
	return o.Count
}

func uiBadge(c *web.Ctx) error {
	return c.RenderBlock("notify_notifications.html", "notify_badge", map[string]any{"Count": badgeData(c), "OOB": false})
}

func uiNotifRead(c *web.Ctx) error {
	if _, err := c.Do("notification.read"); err != nil {
		return err
	}
	d, err := listData(c)
	if err != nil {
		return err
	}
	return c.RenderBlock("notify_notifications.html", "notify_list_oob", map[string]any{"L": d, "Count": d.UnreadCount})
}

func uiNotifReadAll(c *web.Ctx) error {
	if _, err := c.Do("notification.read_all"); err != nil {
		return err
	}
	d, err := listData(c)
	if err != nil {
		return err
	}
	return c.RenderBlock("notify_notifications.html", "notify_list_oob", map[string]any{"L": d, "Count": d.UnreadCount})
}

// ---- notification services -------------------------------------------------

func servicesFrag(c *web.Ctx, boardID string) error {
	l, err := ops.Typed[ServiceListOut](c.Env, "notification_service.list", ServiceListIn{BoardID: boardID})
	if err != nil {
		return err
	}
	return c.RenderBlock("notify_services.html", "notify_services_list", map[string]any{"Items": l.Items, "BoardID": boardID})
}

func pageServices(c *web.Ctx) error {
	l, err := ops.Typed[ServiceListOut](c.Env, "notification_service.list", ServiceListIn{})
	if err != nil {
		return err
	}
	return c.Render("notify_services.html", map[string]any{"Items": l.Items, "BoardID": ""})
}

func fragBoardServices(c *web.Ctx) error {
	return servicesFrag(c, c.R.PathValue("boardId"))
}

func uiServiceCreate(c *web.Ctx) error {
	if _, err := c.Do("notification_service.create"); err != nil {
		return err
	}
	return servicesFrag(c, c.R.FormValue("boardId"))
}
func uiServiceUpdate(c *web.Ctx) error {
	if _, err := c.Do("notification_service.update"); err != nil {
		return err
	}
	return servicesFrag(c, c.R.FormValue("boardId"))
}
func uiServiceDelete(c *web.Ctx) error {
	if _, err := c.Do("notification_service.delete"); err != nil {
		return err
	}
	return servicesFrag(c, c.R.FormValue("boardId"))
}
func uiServiceTest(c *web.Ctx) error {
	if _, err := c.Do("notification_service.test"); err != nil {
		return err
	}
	c.Toast("Test notification sent")
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- webhooks --------------------------------------------------------------

func formList(c *web.Ctx, name string) {
	_ = c.R.ParseForm()
	b, _ := json.Marshal(append([]string{}, c.R.Form[name]...))
	c.R.Form.Set(name, string(b))
}

func pageWebhooks(c *web.Ctx) error {
	l, err := ops.Typed[WebhookListOut](c.Env, "webhook.list", NoInput{})
	if err != nil {
		return err
	}
	ev, _ := ops.Typed[EventsOut](c.Env, "webhook.events", NoInput{})
	return c.Render("notify_webhooks.html", map[string]any{"Items": l.Items, "Events": ev.Items, "Blank": WebhookOut{Events: []string{}, ExcludedEvents: []string{}}, "Saved": c.R.URL.Query().Get("saved")})
}

func uiWebhookCreate(c *web.Ctx) error {
	formList(c, "events")
	formList(c, "excludedEvents")
	if _, err := c.Do("webhook.create"); err != nil {
		return err
	}
	c.Redirect("/admin/webhooks?saved=created")
	return nil
}

// keepWriteOnly makes blank write-only fields (secret, accessToken) mean "unchanged" in the
// edit form; the "clear" checkbox empties them explicitly.
func keepWriteOnly(c *web.Ctx) {
	_ = c.R.ParseForm()
	for _, f := range []string{"secret", "accessToken"} {
		clear := c.R.Form.Get("clear"+strings.ToUpper(f[:1])+f[1:]) != ""
		if c.R.Form.Get(f) == "" && !clear {
			delete(c.R.Form, f)
		}
	}
}

func uiWebhookUpdate(c *web.Ctx) error {
	keepWriteOnly(c)
	formList(c, "events")
	formList(c, "excludedEvents")
	if _, err := c.Do("webhook.update"); err != nil {
		return err
	}
	c.Redirect("/admin/webhooks?saved=updated")
	return nil
}
func uiWebhookDelete(c *web.Ctx) error {
	if _, err := c.Do("webhook.delete"); err != nil {
		return err
	}
	c.Redirect("/admin/webhooks?saved=deleted")
	return nil
}
func uiWebhookTest(c *web.Ctx) error {
	d, err := typed[DeliveryOut](c, "webhook.test")
	if err != nil {
		return err
	}
	if d.State == "delivered" {
		c.Toast(fmt.Sprintf("Ping delivered (HTTP %d, %d ms)", d.Status, d.DurationMs))
	} else {
		c.Toast("Ping failed: " + d.Error)
	}
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}

func pageDeliveries(c *web.Ctx) error {
	id := c.R.PathValue("id")
	l, err := ops.Typed[WebhookListOut](c.Env, "webhook.list", NoInput{})
	if err != nil {
		return err
	}
	var wh WebhookOut
	for _, w := range l.Items {
		if w.ID == id {
			wh = w
		}
	}
	if wh.ID == "" {
		return ops.NotFound("webhook")
	}
	d, err := ops.Typed[DeliveriesOut](c.Env, "webhook.deliveries", DeliveriesIn{ID: id, Limit: 100})
	if err != nil {
		return err
	}
	return c.Render("notify_deliveries.html", map[string]any{"W": wh, "Items": d.Items})
}

func uiDeliveryRetry(c *web.Ctx) error {
	d, err := typed[DeliveryOut](c, "webhook.retry")
	if err != nil {
		return err
	}
	c.Redirect("/admin/webhooks/" + d.WebhookID + "/deliveries")
	return nil
}

// ---- config ----------------------------------------------------------------

type formField struct {
	Key, Label, Type, Help string
	Options                []string
}
type formGroup struct {
	Title, ID string
	Fields    []formField
}

var configForm = []formGroup{
	{"Branding", "branding", []formField{
		{"branding.appName", "Application name", "text", "Shown in the header, emails and the login page.", nil},
		{"branding.loginText", "Login page text", "textarea", "Shown above the login form.", nil},
		{"branding.primaryColor", "Accent colour", "text", "Any CSS colour, e.g. #2f6fed.", nil},
	}},
	{"SMTP (e-mail)", "smtp", []formField{
		{"smtp.host", "Host", "text", "", nil},
		{"smtp.port", "Port", "text", "Default 587 (STARTTLS) or 465 (TLS).", nil},
		{"smtp.secure", "Security", "select", "", []string{"starttls", "tls", "none"}},
		{"smtp.user", "Username", "text", "", nil},
		{"smtp.password", "Password", "password", "Write-only; leave blank to keep the stored one.", nil},
		{"smtp.from", "From address", "text", "e.g. Weave <noreply@example.com>", nil},
		{"smtp.skipVerify", "Skip TLS certificate verification", "bool", "", nil},
		{"smtp.notifications", "E-mail users about notifications", "bool", "Sent when SMTP is configured.", nil},
	}},
	{"OpenID Connect (SSO)", "oidc", []formField{
		{"oidc.issuer", "Issuer URL", "text", "", nil},
		{"oidc.clientId", "Client id", "text", "", nil},
		{"oidc.clientSecret", "Client secret", "password", "Write-only; leave blank to keep the stored one.", nil},
		{"oidc.scopes", "Scopes", "text", "Default: openid profile email groups", nil},
		{"oidc.usernameClaim", "Username claim", "text", "Default: preferred_username", nil},
		{"oidc.nameClaim", "Name claim", "text", "Default: name", nil},
		{"oidc.emailClaim", "E-mail claim", "text", "Default: email", nil},
		{"oidc.groupsClaim", "Groups claim", "text", "Default: groups", nil},
		{"oidc.adminGroup", "Admin group", "text", "Default: weave-admins", nil},
		{"oidc.ownerGroup", "Project owner group", "text", "Default: weave-project-owners", nil},
		{"oidc.userGroup", "User group", "text", "Default: weave-users", nil},
		{"oidc.workerGroup", "Worker group", "text", "Default: weave-workers", nil},
		{"oidc.guestGroup", "Guest group", "text", "Default: weave-guests", nil},
		{"oidc.requireGroup", "Require a role group to sign in", "bool", "", nil},
		{"oidc.teamPrefix", "Team group prefix", "text", "Default: weave-team-; matching groups sync into Weave user groups.", nil},
		{"oidc.autoProvision", "Create users on first SSO login", "bool", "", nil},
		{"oidc.buttonText", "Button text", "text", "", nil},
	}},
	{"Sessions", "session", []formField{
		{"session.maxIdleMinutes", "Maximum idle minutes", "text", "Sessions idle longer than this end, whatever the user's own auto-logout says. 0 or empty = no limit. Session lifetime is WEAVE_SESSION_HOURS (default 168 = 7 days).", nil},
	}},
	{"Terms", "terms", []formField{
		{"terms.enabled", "Require terms acceptance", "bool", "", nil},
		{"terms.version", "Terms version", "text", "Change to force everyone to accept again.", nil},
		{"terms.text", "Terms text", "textarea", "Markdown.", nil},
	}},
}

func pageConfig(c *web.Ctx) error {
	v, err := ops.Typed[SettingsView](c.Env, "config.get", NoInput{})
	if err != nil {
		return err
	}
	return c.Render("notify_config.html", map[string]any{"V": v, "Groups": configForm, "Saved": c.R.URL.Query().Get("saved"), "SMTPOn": SMTPConfigured(c.Env)})
}

func uiConfigSave(c *web.Ctx) error {
	if _, err := c.Do("config.update"); err != nil {
		return err
	}
	c.Redirect("/admin/config?saved=1")
	return nil
}
func uiLogoUpload(c *web.Ctx) error {
	if _, err := c.Do("config.logo.upload"); err != nil {
		return err
	}
	c.Redirect("/admin/config?saved=1")
	return nil
}
func uiLogoDelete(c *web.Ctx) error {
	if _, err := c.Do("config.logo.delete"); err != nil {
		return err
	}
	c.Redirect("/admin/config?saved=1")
	return nil
}
func uiSMTPTest(c *web.Ctx) error {
	if _, err := c.Do("smtp.test"); err != nil {
		return err
	}
	c.Toast("Test e-mail sent")
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}
func uiReindex(c *web.Ctx) error {
	o, err := typed[ReindexOut](c, "search.reindex")
	if err != nil {
		return err
	}
	c.Toast(fmt.Sprintf("Search index rebuilt (%d entries)", o.Indexed))
	c.W.WriteHeader(http.StatusNoContent)
	return nil
}

func serveLogo(c *web.Ctx) error {
	st, err := typed[*ops.Stream](c, "config.logo.get")
	if err != nil {
		http.NotFound(c.W, c.R)
		return nil
	}
	c.W.Header().Set("Content-Type", st.ContentType)
	c.W.Header().Set("Cache-Control", "public, max-age=300")
	c.W.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	c.W.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = c.W.Write(st.Data)
	return nil
}

// ---- search / dashboard / export --------------------------------------------

func searchData(c *web.Ctx) (SearchOut, string, error) {
	q := strings.TrimSpace(c.R.URL.Query().Get("q"))
	if q == "" {
		return SearchOut{Items: []SearchHit{}}, "", nil
	}
	in := SearchIn{Q: q, Kind: c.R.URL.Query().Get("kind"), BoardID: c.R.URL.Query().Get("boardId")}
	o, err := ops.Typed[SearchOut](c.Env, "search.query", in)
	if e, ok := err.(*ops.Error); ok && e.Code == "validation" {
		return SearchOut{Items: []SearchHit{}}, q, nil
	}
	return o, q, err
}

func pageSearch(c *web.Ctx) error {
	o, q, err := searchData(c)
	if err != nil {
		return err
	}
	return c.Render("notify_search.html", map[string]any{"Q": q, "Kind": c.R.URL.Query().Get("kind"), "R": o})
}
func fragSearch(c *web.Ctx) error {
	o, q, err := searchData(c)
	if err != nil {
		return err
	}
	return c.RenderBlock("notify_search.html", "notify_results", map[string]any{"Q": q, "R": o})
}

func pageDashboard(c *web.Ctx) error {
	d, err := ops.Typed[DashboardOut](c.Env, "dashboard.get", NoInput{})
	if err != nil {
		return err
	}
	return c.Render("notify_dashboard.html", map[string]any{"D": d, "Saved": c.R.URL.Query().Get("saved")})
}
func uiDashboardSave(c *web.Ctx) error {
	_ = c.R.ParseForm()
	c.R.Form.Set("panels", strings.Join(c.R.Form["panels"], ","))
	if _, err := c.Do("dashboard.update"); err != nil {
		return err
	}
	c.Redirect("/dashboard?saved=1")
	return nil
}

func serveExport(op string) func(*web.Ctx) error {
	return func(c *web.Ctx) error {
		st, err := typed[*ops.Stream](c, op)
		if err != nil {
			return err
		}
		c.W.Header().Set("Content-Type", st.ContentType)
		c.W.Header().Set("X-Content-Type-Options", "nosniff")
		c.W.Header().Set("Content-Security-Policy", exportCSP())
		disp := "attachment"
		if strings.HasSuffix(st.Name, ".print.html") || c.R.URL.Query().Get("inline") == "1" {
			disp = "inline" // print view: user prints / saves as PDF from the browser
		}
		c.W.Header().Set("Content-Disposition", fmt.Sprintf("%s; filename=%q", disp, st.Name))
		_, _ = c.W.Write(st.Data)
		return nil
	}
}
