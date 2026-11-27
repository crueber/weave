package notify

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

// Webhook pages: the admin console page (/admin/webhooks: global and project webhooks, plus the board webhooks of
// boards the admin manages, with a scope picker) and the Board settings
// "Webhooks" fragment (/ui/boards/{boardId}/webhooks: that board's webhooks, for its managers). Both call the
// webhook.* ops, which decide who may do what (authz.RequireWebhookScope / RequireWebhook).

type scopeOpt struct{ ID, Name string }

func registerWebhookUI() {
	web.Route("GET /admin/webhooks", adminWebhooks(pageWebhooks))
	web.Route("POST /ui/admin/webhooks", adminWebhooks(uiWebhookCreate))
	web.Route("POST /ui/admin/webhooks/{id}", adminWebhooks(uiWebhookUpdate))
	web.Route("POST /ui/admin/webhooks/{id}/delete", adminWebhooks(uiWebhookDelete))
	web.Route("POST /ui/admin/webhooks/{id}/test", adminWebhooks(uiWebhookTest))
	web.Route("GET /admin/webhooks/{id}/deliveries", adminWebhooks(pageDeliveries))
	web.Route("POST /ui/admin/webhook-deliveries/{id}/retry", adminWebhooks(uiDeliveryRetry))

	web.Route("GET /ui/boards/{boardId}/webhooks", func(c *web.Ctx) error { return renderBoardHooks(c, c.R.PathValue("boardId"), hookMsg{}) })
	web.Route("POST /ui/boards/{boardId}/webhooks", uiBoardHookCreate)
	web.Route("POST /ui/webhooks/{id}", uiBoardHookUpdate)
	web.Route("POST /ui/webhooks/{id}/delete", uiBoardHookDelete)
	web.Route("POST /ui/webhooks/{id}/test", uiBoardHookTest)
	web.Route("GET /ui/webhooks/{id}/deliveries", uiBoardHookDeliveries)
	web.Route("POST /ui/webhook-deliveries/{id}/retry", uiBoardHookRetry)
}

func adminWebhooks(h func(*web.Ctx) error) func(*web.Ctx) error {
	return func(c *web.Ctx) error {
		if !authz.SystemAdmin(c.Env) {
			return ops.NotFound("page")
		}
		return h(c)
	}
}

func formList(c *web.Ctx, name string) {
	_ = c.R.ParseForm()
	b, _ := json.Marshal(append([]string{}, c.R.Form[name]...))
	c.R.Form.Set(name, string(b))
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

// applyScope turns the admin form's scope picker into projectId/boardId (only the chosen one is kept).
func applyScope(c *web.Ctx) {
	_ = c.R.ParseForm()
	switch c.R.Form.Get("scope") {
	case "global":
		c.R.Form.Set("projectId", "")
		c.R.Form.Set("boardId", "")
	case "project":
		c.R.Form.Set("boardId", "")
	case "board":
		c.R.Form.Set("projectId", "")
	}
}

// ---- admin console ----------------------------------------------------------

// scopeChoices lists the scope picker's options. The admin role carries no content access, so the picker names only
// the projects the admin can see and the boards they manage (board webhooks need Manage on the board). A project
// webhook on a project the admin cannot see keeps its scope through a neutral option (its name stays hidden).
func scopeChoices(c *web.Ctx, hooks []WebhookOut) (projects, boards []scopeOpt) {
	e := c.Env
	uid := ""
	if e.User != nil {
		uid = e.User.UserID
	}
	var pids []string
	pname := map[string]string{}
	if uid != "" {
		if rows, err := e.DB.QueryContext(e.Ctx, `SELECT p.id,p.name FROM projects p WHERE
			EXISTS(SELECT 1 FROM project_managers pm WHERE pm.project_id=p.id AND pm.user_id=?) OR
			EXISTS(SELECT 1 FROM boards b JOIN board_memberships m ON m.board_id=b.id WHERE b.project_id=p.id AND
				(m.user_id=? OR m.group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?)))
			ORDER BY p.name COLLATE NOCASE, p.id LIMIT 500`, uid, uid, uid); err == nil {
			for rows.Next() {
				var o scopeOpt
				if rows.Scan(&o.ID, &o.Name) == nil {
					pids = append(pids, o.ID)
					pname[o.ID] = o.Name
				}
			}
			rows.Close()
		}
	}
	plv := authz.ProjectLevels(e, pids)
	seen := map[string]bool{}
	for _, id := range pids {
		if plv[id] >= authz.View && len(projects) < 500 {
			projects = append(projects, scopeOpt{id, pname[id]})
			seen[id] = true
		}
	}
	for _, w := range hooks {
		if w.ProjectID != "" && !seen[w.ProjectID] {
			projects = append(projects, scopeOpt{w.ProjectID, "A project you are not a member of"})
			seen[w.ProjectID] = true
		}
	}
	var bids []string
	bname := map[string]string{}
	if uid != "" {
		if rows, err := e.DB.QueryContext(e.Ctx, `SELECT b.id,p.name||' / '||b.name FROM boards b JOIN projects p ON p.id=b.project_id WHERE b.is_template=0 AND (
			EXISTS(SELECT 1 FROM project_managers pm WHERE pm.project_id=b.project_id AND pm.user_id=?) OR
			EXISTS(SELECT 1 FROM board_memberships m WHERE m.board_id=b.id AND m.role='owner' AND
				(m.user_id=? OR m.group_id IN (SELECT group_id FROM user_group_members WHERE user_id=?))))
			ORDER BY p.name,b.position,b.name LIMIT 500`, uid, uid, uid); err == nil {
			for rows.Next() {
				var o scopeOpt
				if rows.Scan(&o.ID, &o.Name) == nil {
					bids = append(bids, o.ID)
					bname[o.ID] = o.Name
				}
			}
			rows.Close()
		}
	}
	blv := authz.BoardLevels(e, bids)
	for _, id := range bids {
		if blv[id] >= authz.Manage && len(boards) < 500 {
			boards = append(boards, scopeOpt{id, bname[id]})
		}
	}
	return
}

func pageWebhooks(c *web.Ctx) error {
	l, err := ops.Typed[WebhookListOut](c.Env, "webhook.list", WebhookListIn{})
	if err != nil {
		return err
	}
	ev, _ := ops.Typed[EventsOut](c.Env, "webhook.events", EventsIn{})
	projects, boards := scopeChoices(c, l.Items)
	return c.Render("notify_webhooks.html", map[string]any{"Items": l.Items, "Events": ev.Items, "Blank": WebhookOut{Events: []string{}, ExcludedEvents: []string{}, Scope: "global"},
		"Saved": c.R.URL.Query().Get("saved"), "Projects": projects, "Boards": boards})
}

func uiWebhookCreate(c *web.Ctx) error {
	applyScope(c)
	formList(c, "events")
	formList(c, "excludedEvents")
	if _, err := c.Do("webhook.create"); err != nil {
		return err
	}
	c.Redirect("/admin/webhooks?saved=created")
	return nil
}

func uiWebhookUpdate(c *web.Ctx) error {
	keepWriteOnly(c)
	applyScope(c)
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

func pingToast(d DeliveryOut) string {
	if d.State == "delivered" {
		return fmt.Sprintf("Ping delivered (HTTP %d, %d ms)", d.Status, d.DurationMs)
	}
	if d.Error != "" {
		return "Ping failed: " + d.Error
	}
	return fmt.Sprintf("Ping failed (HTTP %d)", d.Status)
}

func uiWebhookTest(c *web.Ctx) error {
	d, err := typed[DeliveryOut](c, "webhook.test")
	if err != nil {
		return err
	}
	c.Toast(pingToast(d))
	c.W.WriteHeader(204)
	return nil
}

func pageDeliveries(c *web.Ctx) error {
	id := c.R.PathValue("id")
	wh, err := manageable(c.Env, id)
	if err != nil {
		return err
	}
	d, err := ops.Typed[DeliveriesOut](c.Env, "webhook.deliveries", DeliveriesIn{ID: id, Limit: 100})
	if err != nil {
		return err
	}
	return c.Render("notify_deliveries.html", map[string]any{"W": wh.redacted(), "Items": d.Items})
}

func uiDeliveryRetry(c *web.Ctx) error {
	d, err := typed[DeliveryOut](c, "webhook.retry")
	if err != nil {
		return err
	}
	c.Redirect("/admin/webhooks/" + d.WebhookID + "/deliveries")
	return nil
}

// ---- Board settings > Webhooks ---------------------------------------------

type hookMsg struct {
	Msg, Err string
	Open     string       // webhook kept expanded after the action
	Ping     *DeliveryOut // result of the last test ping (Open's)
}

func renderBoardHooks(c *web.Ctx, boardID string, m hookMsg) error {
	l, err := ops.Typed[WebhookListOut](c.Env, "webhook.list", WebhookListIn{BoardID: boardID})
	if err != nil {
		return err
	}
	ev, _ := ops.Typed[EventsOut](c.Env, "webhook.events", EventsIn{Scope: "board"})
	return c.RenderBlock("notify_board_webhooks.html", "notify_board_webhooks", map[string]any{
		"BoardID": boardID, "Items": l.Items, "Events": ev.Items, "Blank": WebhookOut{Events: []string{}, ExcludedEvents: []string{}},
		"Msg": m.Msg, "Err": m.Err, "Open": m.Open, "Ping": m.Ping,
		"AllowPrivate": userAllowPrivate(c.Env.Ctx, c.Env.DB),
	})
}

// boardHookAction runs a webhook op from the board fragment and re-renders it; validation errors show inline.
func boardHookAction(c *web.Ctx, op, boardID, done string, prep func()) error {
	if prep != nil {
		prep()
	}
	out, err := c.Do(op)
	if err != nil {
		if oe, ok := err.(*ops.Error); ok && oe.Status != 404 {
			return renderBoardHooks(c, boardID, hookMsg{Err: oe.Msg, Open: c.R.PathValue("id")})
		}
		return err
	}
	m := hookMsg{Msg: done}
	if w, ok := out.(WebhookOut); ok && op != "webhook.delete" {
		m.Open = w.ID
	}
	return renderBoardHooks(c, boardID, m)
}

func formBoard(c *web.Ctx) string {
	_ = c.R.ParseForm()
	return c.R.Form.Get("_board")
}

func uiBoardHookCreate(c *web.Ctx) error {
	b := c.R.PathValue("boardId")
	return boardHookAction(c, "webhook.create", b, "Webhook created.", func() {
		formList(c, "events")
		formList(c, "excludedEvents")
		c.R.Form.Set("boardId", b)
		c.R.Form.Del("projectId")
		c.R.Form.Del("userId")
	})
}

func uiBoardHookUpdate(c *web.Ctx) error {
	b := formBoard(c)
	return boardHookAction(c, "webhook.update", b, "Webhook saved.", func() {
		keepWriteOnly(c)
		formList(c, "events")
		formList(c, "excludedEvents")
		c.R.Form.Del("projectId")
		c.R.Form.Del("userId")
		c.R.Form.Set("boardId", b)
	})
}

func uiBoardHookDelete(c *web.Ctx) error {
	return boardHookAction(c, "webhook.delete", formBoard(c), "Webhook deleted.", nil)
}

func uiBoardHookTest(c *web.Ctx) error {
	b := formBoard(c)
	d, err := typed[DeliveryOut](c, "webhook.test")
	if err != nil {
		return err
	}
	return renderBoardHooks(c, b, hookMsg{Open: d.WebhookID, Ping: &d})
}

func renderHookDeliveries(c *web.Ctx, id string) error {
	w, err := manageable(c.Env, id)
	if err != nil {
		return err
	}
	d, err := ops.Typed[DeliveriesOut](c.Env, "webhook.deliveries", DeliveriesIn{ID: id, Limit: 50})
	if err != nil {
		return err
	}
	return c.RenderBlock("notify_board_webhooks.html", "notify_wh_deliveries", map[string]any{"W": w.redacted(), "Items": d.Items})
}

func uiBoardHookDeliveries(c *web.Ctx) error { return renderHookDeliveries(c, c.R.PathValue("id")) }

func uiBoardHookRetry(c *web.Ctx) error {
	d, err := typed[DeliveryOut](c, "webhook.retry")
	if err != nil {
		return err
	}
	return renderHookDeliveries(c, d.WebhookID)
}
