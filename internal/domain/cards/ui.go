package cards

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/web"
)

var weekdayNames = []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}

func init() {
	web.AddFuncs(template.FuncMap{
		"cards_md": func(s *string) template.HTML {
			if s == nil {
				return ""
			}
			return RenderMarkdown(*s)
		},
		"cards_md_s": func(s string) template.HTML { return RenderMarkdown(s) },
		"cards_f": func(f *float64) string {
			if f == nil {
				return ""
			}
			return strconv.FormatFloat(*f, 'f', -1, 64)
		},
		"cards_done": func(b *bool) bool { return b != nil && *b },
		"cards_overdue": func(s *string) bool {
			if s == nil {
				return false
			}
			t, err := parseTime(*s)
			return err == nil && t.Before(time.Now())
		},
		"cards_when": func(s any) string {
			var v string
			switch x := s.(type) {
			case *string:
				if x != nil {
					v = *x
				}
			case string:
				v = x
			}
			if t, err := parseTime(v); err == nil {
				return t.Format("Jan 2, 2006 15:04")
			}
			return v
		},
		"cards_dt": func(s *string) string {
			if s == nil {
				return ""
			}
			if t, err := parseTime(*s); err == nil {
				return t.Format("2006-01-02T15:04")
			}
			return ""
		},
		"cards_date": func(s string) string {
			if t, err := parseTime(s); err == nil {
				return t.Format("2006-01-02")
			}
			return ""
		},
		"cards_has": func(list []string, id string) bool {
			for _, x := range list {
				if x == id {
					return true
				}
			}
			return false
		},
		"cards_hasint": func(list []int, n int) bool {
			for _, x := range list {
				if x == n {
					return true
				}
			}
			return false
		},
		"cards_rec":      func(s *string) *Recurrence { return parseRecurrence(s) },
		"cards_freqs":    func() []string { return []string{"daily", "weekly", "monthly", "yearly"} },
		"cards_weekdays": func() []string { return weekdayNames },
		"cards_initials": func(n string) string {
			f := strings.Fields(n)
			switch len(f) {
			case 0:
				return "?"
			case 1:
				return strings.ToUpper(string([]rune(f[0])[:1]))
			}
			return strings.ToUpper(string([]rune(f[0])[:1]) + string([]rune(f[len(f)-1])[:1]))
		},
	})

	web.Route("GET /ui/cards/{id}", func(c *web.Ctx) error { return renderModal(c, c.R.PathValue("id"), "view", nil) })
	web.Route("GET /cards/{id}", func(c *web.Ctx) error {
		d, err := buildModal(c, c.R.PathValue("id"), "view", nil)
		if err != nil {
			return err
		}
		return c.Render("cards_modal.html", d)
	})
	web.Route("GET /ui/cards/{id}/editing", func(c *web.Ctx) error { return renderDesc(c, c.R.PathValue("id"), "view", nil) })

	// plain mutations: run the op, then re-render the modal
	for _, m := range []struct{ pat, op string }{
		{"POST /ui/cards/{id}/archive", "card.archive"}, {"POST /ui/cards/{id}/restore", "card.restore"},
		{"POST /ui/cards/{id}/close", "card.close"}, {"POST /ui/cards/{id}/cover", "card.cover.set"},
		{"POST /ui/cards/{id}/due/complete", "card.due.complete"}, {"POST /ui/cards/{id}/location", "card.location.set"},
		{"POST /ui/cards/{id}/recurrence/run", "card.recurrence.run"},
		{"POST /ui/cards/{id}/members", "card.member.add"}, {"POST /ui/cards/{id}/members/{userId}/remove", "card.member.remove"},
		{"POST /ui/cards/{id}/labels", "card.label.add"}, {"POST /ui/cards/{id}/labels/{labelId}/remove", "card.label.remove"},
		{"POST /ui/cards/{id}/subscribe", "card.subscribe"}, {"POST /ui/cards/{id}/unsubscribe", "card.unsubscribe"},
		{"POST /ui/cards/{id}/share", "card.share.create"}, {"POST /ui/cards/{id}/share/revoke", "card.share.revoke"},
		{"POST /ui/cards/{id}/template", "card.template.save"},
	} {
		op := m.op
		web.Route(m.pat, func(c *web.Ctx) error {
			if _, err := c.Do(op); err != nil {
				return err
			}
			return renderModal(c, c.R.PathValue("id"), "view", nil)
		})
	}
	web.Route("POST /ui/cards/{id}/notifications/read", func(c *web.Ctx) error {
		if _, err := c.Do("card.notifications.read"); err != nil {
			return err
		}
		c.Toast("Notifications marked as read")
		c.W.WriteHeader(http.StatusNoContent)
		return nil
	})

	web.Route("POST /ui/cards/{id}/update", uiUpdate)
	web.Route("POST /ui/cards/{id}/due", func(c *web.Ctx) error {
		_ = c.R.ParseForm()
		id := c.R.PathValue("id")
		// a bare date (calendar drop) keeps the existing time of day
		if d := c.R.Form.Get("dueDate"); len(d) == 10 {
			if card, err := cardGetCard(c.Env, id); err == nil && card.DueDate != nil && len(*card.DueDate) > 10 {
				c.R.Form.Set("dueDate", d+(*card.DueDate)[10:])
			}
		}
		if _, err := c.Do("card.due.set"); err != nil {
			return err
		}
		if c.R.Form.Get("_part") == "none" {
			c.W.WriteHeader(http.StatusNoContent)
			return nil
		}
		return renderModal(c, id, "view", nil)
	})
	web.Route("POST /ui/cards/{id}/recurrence", func(c *web.Ctx) error {
		_ = c.R.ParseForm()
		f := c.R.Form
		in := RecurrenceIn{ID: c.R.PathValue("id")}
		if f.Get("freq") != "" {
			r := Recurrence{Freq: f.Get("freq"), Trigger: f.Get("trigger"), Until: f.Get("until"), ListID: f.Get("listId")}
			r.Interval, _ = strconv.Atoi(f.Get("interval"))
			r.Count, _ = strconv.Atoi(f.Get("count"))
			if r.Freq == "weekly" {
				for _, w := range f["weekdays"] {
					if n, err := strconv.Atoi(w); err == nil {
						r.Weekdays = append(r.Weekdays, n)
					}
				}
			}
			b, _ := json.Marshal(r)
			in.Recurrence = string(b)
		}
		if _, err := ops.Call(c.Env, "card.recurrence.set", &in); err != nil {
			return err
		}
		return renderModal(c, in.ID, "view", nil)
	})
	web.Route("POST /ui/cards/{id}/move", func(c *web.Ctx) error {
		_ = c.R.ParseForm()
		if c.R.Form.Get("listId") == "" && c.R.Form.Get("container") != "" { // weave.js sortable posts "container"
			c.R.Form.Set("listId", c.R.Form.Get("container"))
		}
		if _, err := c.Do("card.move"); err != nil {
			return err
		}
		if c.R.Form.Get("_part") == "modal" {
			return renderModal(c, c.R.PathValue("id"), "view", nil)
		}
		c.W.WriteHeader(http.StatusNoContent)
		return nil
	})
	web.Route("POST /ui/cards/{id}/delete", func(c *web.Ctx) error {
		id := c.R.PathValue("id")
		card, _ := cardGetCard(c.Env, id)
		if _, err := c.Do("card.delete"); err != nil {
			return err
		}
		c.W.Header().Set("HX-Trigger", fmt.Sprintf(`{"cardsClosed":{"boardId":%q}}`, card.BoardID))
		c.W.WriteHeader(http.StatusNoContent)
		return nil
	})
	// operations that produce a different card: show it in the modal
	for _, m := range []struct{ pat, op string }{
		{"POST /ui/cards/{id}/duplicate", "card.duplicate"}, {"POST /ui/cards/{id}/from-template", "card.fromTemplate"},
		{"POST /ui/cards/{id}/publish", "card.draft.publish"}, {"POST /ui/lists/{listId}/cards", "card.create"},
	} {
		op := m.op
		web.Route(m.pat, func(c *web.Ctx) error {
			out, err := c.Do(op)
			if err != nil {
				return err
			}
			card := out.(Card)
			if c.R.Form.Get("_part") == "none" {
				c.W.Header().Set("HX-Trigger", fmt.Sprintf(`{"cardCreated":{"id":%q,"listId":%q}}`, card.ID, card.ListID))
				c.W.WriteHeader(http.StatusNoContent)
				return nil
			}
			c.W.Header().Set("HX-Retarget", "#modal")
			c.W.Header().Set("HX-Reswap", "innerHTML")
			return renderModal(c, card.ID, "view", nil)
		})
	}

	// description editing + locks
	web.Route("POST /ui/cards/{id}/editing/touch", func(c *web.Ctx) error {
		out, err := ops.Typed[EditTouchOut](c.Env, "card.edit.touch", mustBind(c, "card.edit.touch"))
		if err != nil {
			return err
		}
		if out.Granted {
			return renderDesc(c, c.R.PathValue("id"), "edit", nil)
		}
		return renderDesc(c, c.R.PathValue("id"), "locked", out.Holder)
	})
	web.Route("POST /ui/cards/{id}/editing/release", func(c *web.Ctx) error {
		if _, err := c.Do("card.edit.release"); err != nil {
			return err
		}
		return renderDesc(c, c.R.PathValue("id"), "view", nil)
	})
	web.Route("POST /ui/cards/{id}/description/preview", func(c *web.Ctx) error {
		if _, err := authz.RequireCard(c.Env, c.R.PathValue("id"), authz.View); err != nil {
			return err
		}
		_ = c.R.ParseForm()
		c.W.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(c.W, RenderMarkdown(c.R.Form.Get("description")))
		return nil
	})

	// inbox + templates
	web.Route("GET /ui/boards/{boardId}/inbox", func(c *web.Ctx) error { return renderInbox(c, c.R.PathValue("boardId")) })
	web.Route("POST /ui/boards/{boardId}/inbox", func(c *web.Ctx) error {
		if _, err := c.Do("card.draft.create"); err != nil {
			return err
		}
		return renderInbox(c, c.R.PathValue("boardId"))
	})
	web.Route("POST /ui/boards/{boardId}/inbox/settings", func(c *web.Ctx) error {
		if _, err := c.Do("card.inbox.set"); err != nil {
			return err
		}
		return renderInbox(c, c.R.PathValue("boardId"))
	})
	web.Route("GET /ui/projects/{projectId}/card-templates", func(c *web.Ctx) error {
		out, err := ops.Typed[TemplatesOut](c.Env, "card.template.list", TemplatesIn{c.R.PathValue("projectId")})
		if err != nil {
			return err
		}
		return c.RenderBlock("cards_templates.html", "cards_templates", map[string]any{"Items": out.Items, "ListID": c.R.URL.Query().Get("listId")})
	})

	// views
	web.Route("GET /boards/{id}/calendar", uiCalendar)
	web.Route("GET /boards/{id}/map", uiMap)
	web.RoutePublic("GET /share/{token}", func(c *web.Ctx) error {
		out, err := ops.Typed[SharedCard](c.Env, "card.share.view", ShareViewIn{c.R.PathValue("token")})
		if err != nil {
			if e, ok := err.(*ops.Error); ok && e.Status == 404 {
				http.NotFound(c.W, c.R)
				return nil
			}
			return err
		}
		return c.Render("cards_share.html", out)
	})
}

func mustBind(c *web.Ctx, op string) any {
	in, err := c.Bind(op)
	if err != nil {
		return EditTouchIn{ID: c.R.PathValue("id")}
	}
	return in
}

type modalData struct {
	Card     Card
	Opts     Options
	Editors  []EditHolder
	DescMode string // view | edit | locked
	Locker   *EditHolder
	Origin   string
}

func buildModal(c *web.Ctx, id, mode string, locker *EditHolder) (modalData, error) {
	d := modalData{DescMode: mode, Locker: locker}
	var err error
	if d.Card, err = cardGetCard(c.Env, id); err != nil {
		return d, err
	}
	if d.Opts, err = ops.Typed[Options](c.Env, "card.options", IDIn{id}); err != nil {
		return d, err
	}
	if el, err := ops.Typed[EditListOut](c.Env, "card.edit.list", IDIn{id}); err == nil {
		d.Editors = el.Editors
	}
	// links are built from the configured public URL, never from the client-controlled Host header
	if cfg, _ := c.Env.Svc["config"].(config.Config); strings.TrimSpace(cfg.BaseURL) != "" {
		d.Origin = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	} else {
		scheme := "http"
		if c.R.TLS != nil {
			scheme = "https"
		}
		d.Origin = scheme + "://" + c.R.Host
	}
	return d, nil
}

func renderModal(c *web.Ctx, id, mode string, locker *EditHolder) error {
	d, err := buildModal(c, id, mode, locker)
	if err != nil {
		return err
	}
	return c.RenderBlock("cards_modal.html", "cards_modal_body", d)
}

func renderDesc(c *web.Ctx, id, mode string, locker *EditHolder) error {
	d, err := buildModal(c, id, mode, locker)
	if err != nil {
		return err
	}
	return c.RenderBlock("cards_modal.html", "cards_desc", d)
}

// uiUpdate handles the title, description and type forms of the modal.
func uiUpdate(c *web.Ctx) error {
	_ = c.R.ParseForm()
	id, part := c.R.PathValue("id"), c.R.Form.Get("_part")
	if part == "desc" || part == "autosave" {
		// refuse to overwrite if somebody else took the lock over meanwhile
		if el, err := ops.Typed[EditListOut](c.Env, "card.edit.list", IDIn{id}); err == nil {
			for _, h := range el.Editors {
				if !h.Me && h.Field == "description" && !h.Idle {
					return ops.Conflict(h.Name + " is editing the description now")
				}
			}
		}
	}
	if _, err := c.Do("card.update"); err != nil {
		return err
	}
	switch part {
	case "autosave":
		// keep editing: just confirm the draft was stored (the editor keeps its lock and focus)
		_, _ = ops.Call(c.Env, "card.edit.touch", &EditTouchIn{ID: id, Field: "description"})
		c.W.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(c.W, "Saved")
		return nil
	case "desc":
		_, _ = ops.Call(c.Env, "card.edit.release", &EditReleaseIn{ID: id, Field: "description"})
		c.W.Header().Set("HX-Trigger", `{"cards:saved":true}`)
		return renderDesc(c, id, "view", nil)
	case "title":
		d, err := buildModal(c, id, "view", nil)
		if err != nil {
			return err
		}
		return c.RenderBlock("cards_modal.html", "cards_title", d)
	}
	return renderModal(c, id, "view", nil)
}

func renderInbox(c *web.Ctx, boardID string) error {
	out, err := ops.Typed[InboxOut](c.Env, "card.inbox.list", InboxIn{boardID})
	if err != nil {
		return err
	}
	data := map[string]any{"Enabled": out.Enabled, "Items": out.Items, "BoardID": boardID,
		"CanManage": authz.BoardLevel(c.Env, boardID) >= authz.Manage, "Lists": []ListOpt{}}
	if len(out.Items) > 0 {
		if o, err := ops.Typed[Options](c.Env, "card.options", IDIn{out.Items[0].ID}); err == nil {
			var ls []ListOpt
			for _, l := range o.Lists {
				if l.BoardID == boardID {
					ls = append(ls, l)
				}
			}
			data["Lists"] = ls
		}
	}
	return c.RenderBlock("cards_inbox.html", "cards_inbox", data)
}

type calDay struct {
	Date    string
	Num     int
	InMonth bool
	Today   bool
	Cards   []CalendarCard
}

func uiCalendar(c *web.Ctx) error {
	board := c.R.PathValue("id")
	out, err := ops.Typed[CalendarOut](c.Env, "card.calendar", CalendarIn{BoardID: board, Month: c.R.URL.Query().Get("month")})
	if err != nil {
		return err
	}
	first, _ := time.Parse("2006-01", out.Month)
	from, _ := time.Parse("2006-01-02", out.From)
	to, _ := time.Parse("2006-01-02", out.To)
	byDay := map[string][]CalendarCard{}
	for _, cd := range out.Cards {
		byDay[strings.SplitN(cd.DueDate, "T", 2)[0]] = append(byDay[strings.SplitN(cd.DueDate, "T", 2)[0]], cd)
	}
	today := time.Now().UTC().Format("2006-01-02")
	var days []calDay
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		k := d.Format("2006-01-02")
		days = append(days, calDay{Date: k, Num: d.Day(), InMonth: d.Month() == first.Month(), Today: k == today, Cards: byDay[k]})
	}
	return c.Render("cards_calendar.html", map[string]any{
		"BoardID": board, "Title": first.Format("January 2006"), "Prev": first.AddDate(0, -1, 0).Format("2006-01"), "Next": first.AddDate(0, 1, 0).Format("2006-01"),
		"Weekdays": []string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}, "Days": days, "Unscheduled": out.Unscheduled,
	})
}

type mapPt struct {
	MapPoint
	X, Y, LY float64
}
type mapLine struct{ X1, Y1, X2, Y2 float64 }

func uiMap(c *web.Ctx) error {
	board := c.R.PathValue("id")
	out, err := ops.Typed[MapOut](c.Env, "card.map", InboxIn{board})
	if err != nil {
		return err
	}
	const W, H = 800.0, 440.0
	data := map[string]any{"BoardID": board, "W": W, "H": H, "Points": []mapPt{}, "Grid": []mapLine{}}
	if len(out.Points) > 0 {
		minLat, maxLat, minLng, maxLng := 90.0, -90.0, 180.0, -180.0
		for _, p := range out.Points {
			minLat, maxLat = math.Min(minLat, p.Lat), math.Max(maxLat, p.Lat)
			minLng, maxLng = math.Min(minLng, p.Lng), math.Max(maxLng, p.Lng)
		}
		padLat, padLng := math.Max(1, (maxLat-minLat)*0.15), math.Max(1, (maxLng-minLng)*0.15)
		minLat, maxLat, minLng, maxLng = minLat-padLat, maxLat+padLat, minLng-padLng, maxLng+padLng
		spanLat, spanLng := maxLat-minLat, maxLng-minLng
		s := math.Min((W-80)/spanLng, (H-80)/spanLat) // uniform degrees-per-pixel
		ox, oy := (W-spanLng*s)/2, (H-spanLat*s)/2
		var pts []mapPt
		for _, p := range out.Points {
			x, y := ox+(p.Lng-minLng)*s, oy+(maxLat-p.Lat)*s
			pts = append(pts, mapPt{p, math.Round(x*10) / 10, math.Round(y*10) / 10, math.Round(y*10)/10 - 12})
		}
		var grid []mapLine
		step := math.Pow(10, math.Floor(math.Log10(math.Max(spanLat, spanLng)/3)))
		for lng := math.Ceil(minLng/step) * step; lng <= maxLng; lng += step {
			x := ox + (lng-minLng)*s
			grid = append(grid, mapLine{x, oy, x, oy + spanLat*s})
		}
		for lat := math.Ceil(minLat/step) * step; lat <= maxLat; lat += step {
			y := oy + (maxLat-lat)*s
			grid = append(grid, mapLine{ox, y, ox + spanLng*s, y})
		}
		data["Points"], data["Grid"] = pts, grid
		data["Bounds"] = fmt.Sprintf("lat %.2f..%.2f, lng %.2f..%.2f; grid every %g degrees", minLat, maxLat, minLng, maxLng, step)
	}
	return c.Render("cards_map.html", data)
}
