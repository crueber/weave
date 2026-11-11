// Package web provides the HTMX UI plumbing: routes, templates, helpers.
// Domain packages register page/fragment handlers with web.Route in init().
// UI handlers MUST perform mutations via ops.Call so UI == REST == CLI.
package web

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/crueber/weave/internal/ops"
)

//go:embed templates/*.html static/*
var assets embed.FS

// Static returns the embedded static files (css, js).
func Static() fs.FS { sub, _ := fs.Sub(assets, "static"); return sub }

// Ctx is handed to UI handlers.
type Ctx struct {
	W   http.ResponseWriter
	R   *http.Request
	Env *ops.Env
}

type route struct {
	pattern string // "GET /boards/{id}"
	h       func(*Ctx) error
	public  bool
}

var routes []route

// Route registers a UI handler; requires login unless RoutePublic.
func Route(pattern string, h func(*Ctx) error) { routes = append(routes, route{pattern, h, false}) }
func RoutePublic(pattern string, h func(*Ctx) error) {
	routes = append(routes, route{pattern, h, true})
}

// RoutePatterns lists every registered UI route as {pattern, "1" when public else "0"}
// (used by the authorization sweep test).
func RoutePatterns() [][2]string {
	out := make([][2]string, 0, len(routes))
	for _, r := range routes {
		p := "0"
		if r.public {
			p = "1"
		}
		out = append(out, [2]string{r.pattern, p})
	}
	return out
}

// Mount installs all UI routes on mux; mkEnv builds the per-request Env (auth resolved).
func Mount(mux *http.ServeMux, mkEnv func(*http.Request) *ops.Env) {
	for _, rt := range routes {
		rt := rt
		mux.HandleFunc(rt.pattern, func(w http.ResponseWriter, r *http.Request) {
			c := &Ctx{W: w, R: r, Env: mkEnv(r)}
			if !rt.public && c.Env.User == nil {
				if r.Header.Get("HX-Request") != "" {
					w.Header().Set("HX-Redirect", loginURL(r))
				} else {
					http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				}
				return
			}
			if err := rt.h(c); err != nil {
				c.Fail(err)
			}
		})
	}
}

// loginURL is /login carrying the page the user was on (htmx sends it as HX-Current-URL), so a
// re-authentication lands back where they were.
func loginURL(r *http.Request) string {
	if cur := r.Header.Get("HX-Current-URL"); cur != "" {
		if u, err := url.Parse(cur); err == nil && u.Path != "" && u.Path != "/login" {
			return "/login?next=" + url.QueryEscape(u.RequestURI())
		}
	}
	return "/login"
}

// Fail renders an error: htmx requests get a toast fragment with the right status.
func (c *Ctx) Fail(err error) {
	status, msg := 500, "internal error"
	if e, ok := err.(*ops.Error); ok {
		status, msg = e.Status, e.Msg
	} else {
		fmt.Println("ui error:", err)
	}
	if status == 401 {
		// expired session: send the browser back through the identity provider, which owns the real lifetime
		if c.R.Header.Get("HX-Request") == "" {
			http.Redirect(c.W, c.R, "/login?next="+url.QueryEscape(c.R.URL.RequestURI()), http.StatusSeeOther)
		} else {
			c.W.Header().Set("HX-Redirect", loginURL(c.R))
			c.W.WriteHeader(status)
		}
		return
	}
	c.W.Header().Set("HX-Retarget", "#toasts")
	c.W.Header().Set("HX-Reswap", "beforeend")
	c.W.WriteHeader(status)
	fmt.Fprintf(c.W, `<div class="toast toast-error" role="alert">%s</div>`, template.HTMLEscapeString(msg))
}

// Bind builds an op input from the request using the op's own struct tags.
func (c *Ctx) Bind(op string) (any, error) { return ops.Get(op).Bind(c.R) }

// Do binds the request to op's input and calls it (the standard UI mutation path).
func (c *Ctx) Do(op string) (any, error) {
	in, err := c.Bind(op)
	if err != nil {
		return nil, err
	}
	return ops.Call(c.Env, op, in)
}

// Redirect works for both full-page and htmx requests.
func (c *Ctx) Redirect(url string) {
	if c.R.Header.Get("HX-Request") != "" {
		c.W.Header().Set("HX-Redirect", url)
		return
	}
	http.Redirect(c.W, c.R, url, http.StatusSeeOther)
}

// Toast triggers a client-side toast through HX-Trigger.
func (c *Ctx) Toast(msg string) {
	c.W.Header().Set("HX-Trigger", fmt.Sprintf(`{"toast":%q}`, msg))
}

func (c *Ctx) IsHTMX() bool { return c.R.Header.Get("HX-Request") != "" }

var (
	tplOnce sync.Once
	pages   map[string]*template.Template
	funcs   = template.FuncMap{
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
		// noHistory marks pages whose content (emails, secrets-adjacent admin data) must not be
		// snapshotted into htmx's browser-side history cache.
		"noHistory": func(page string) bool {
			return strings.HasPrefix(page, "auth_admin_") || strings.HasPrefix(page, "admin_") || page == "auth_settings.html" ||
				page == "notify_config.html" || page == "notify_webhooks.html" || page == "notify_deliveries.html"
		},
		"deref": func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		},
	}
)

// AddFuncs lets domain packages add template helpers in init() (before first Render).
func AddFuncs(m template.FuncMap) {
	for k, v := range m {
		funcs[k] = v
	}
}

func load() {
	base := template.New("").Funcs(funcs)
	ents, _ := fs.ReadDir(assets, "templates")
	for _, e := range ents { // shared: layout.html and _*.html partials
		if e.Name() == "layout.html" || strings.HasPrefix(e.Name(), "_") {
			b, _ := assets.ReadFile("templates/" + e.Name())
			template.Must(base.Parse(string(b)))
		}
	}
	pages = map[string]*template.Template{}
	for _, e := range ents {
		if e.Name() == "layout.html" || strings.HasPrefix(e.Name(), "_") {
			continue
		}
		t := template.Must(template.Must(base.Clone()).Parse(mustRead(e.Name())))
		pages[e.Name()] = t
	}
}

func mustRead(n string) string { b, _ := assets.ReadFile("templates/" + n); return string(b) }

// Render executes a full page (page "board.html" runs its "page" template inside the layout).
func (c *Ctx) Render(page string, data any) error { return c.RenderBlock(page, "page", data) }

// RenderBlock executes a named template defined in page file (htmx fragments).
func (c *Ctx) RenderBlock(page, block string, data any) error {
	tplOnce.Do(load)
	t, ok := pages[page]
	if !ok {
		return fmt.Errorf("template %s not found", page)
	}
	var buf bytes.Buffer
	name := block
	if block == "page" && !c.IsHTMX() {
		name = "layout"
	}
	if err := t.ExecuteTemplate(&buf, name, pageData{Data: data, User: c.Env.User, Page: page}); err != nil {
		return err
	}
	c.W.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, err := c.W.Write(buf.Bytes())
	return err
}

type pageData struct {
	Data any
	User *ops.Principal
	Page string
}
