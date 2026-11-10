// Package server mounts REST (/api/v1), SSE (/events), static files and the HTMX UI.
package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/config"
	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
	"github.com/crueber/weave/internal/realtime"
	"github.com/crueber/weave/internal/web"
)

type Server struct {
	Cfg config.Config
	DB  *db.DB
	Hub *realtime.Hub
	Svc map[string]any // shared services; domain init hooks may add (see Hooks)

	limOnce sync.Once
	limits  *limits
}

// Hooks run at startup after the Server is built (seed admin, start workers…).
var Hooks []func(*Server) error

func (s *Server) env(r *http.Request) *ops.Env {
	e := &ops.Env{Ctx: r.Context(), DB: s.DB, Hub: s.Hub, Svc: s.Svc, Req: r}
	if ops.Authenticate != nil {
		e.User = ops.Authenticate(e, r)
	}
	return e
}

func (s *Server) Handler() (http.Handler, error) {
	mux := http.NewServeMux()
	for _, o := range ops.All() {
		o := o
		mux.HandleFunc(o.Method+" /api/v1"+o.Path, func(w http.ResponseWriter, r *http.Request) { s.serveOp(o, w, r) })
	}
	mux.HandleFunc("GET /api/v1/_ops", func(w http.ResponseWriter, r *http.Request) {
		var out []ops.Schema
		for _, o := range ops.All() {
			out = append(out, o.Schema())
		}
		writeJSON(w, 200, map[string]any{"items": out})
	})
	mux.HandleFunc("GET /events", s.sse)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(web.Static())))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	web.Mount(mux, s.env)
	return withHSTS(s.Cfg.BaseURL, securityHeaders(cspModeFromEnv(), s.throttle(s.csrf(mux)))), nil
}

// csrf rejects cross-origin state-changing browser requests (Sec-Fetch-Site / Origin checks).
// Requests without those headers (CLI, curl, agents using Bearer/X-API-Key) pass untouched.
func (s *Server) csrf(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	if u, err := url.Parse(s.Cfg.BaseURL); err == nil && u.Scheme != "" && u.Host != "" {
		if err := cop.AddTrustedOrigin(u.Scheme + "://" + u.Host); err != nil {
			fmt.Println("csrf: ignoring WEAVE_BASE_URL as trusted origin:", err)
		}
	}
	return cop.Handler(next)
}

// pagePolicy is the Content-Security-Policy of the UI. Scripts only run from /static (no inline
// scripts, handlers, javascript: URLs or eval); inline style attributes stay allowed (dynamic
// colours are hex-validated and JS sets styles through the CSSOM). Downloads, exports and share
// pages send their own stricter policy, which replaces this one.
const pagePolicy = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"media-src 'self'; font-src 'self'; connect-src 'self'; form-action 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'"

// cspModeFromEnv reads WEAVE_CSP: enforce (default) | report-only | off (escape hatch).
func cspModeFromEnv() string {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv("WEAVE_CSP"))); v {
	case "report-only", "off":
		return v
	case "", "enforce":
	default:
		fmt.Println("WEAVE_CSP: unknown value", v, "- using enforce")
	}
	return "enforce"
}

// securityHeaders adds conservative response headers. Handlers may override any of them.
func securityHeaders(csp string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		switch csp {
		case "report-only":
			h.Set("Content-Security-Policy", "frame-ancestors 'none'")
			h.Set("Content-Security-Policy-Report-Only", pagePolicy)
		case "off":
			h.Set("Content-Security-Policy", "frame-ancestors 'none'")
		default:
			h.Set("Content-Security-Policy", pagePolicy)
		}
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
		public := strings.HasPrefix(r.URL.Path, "/share/") || strings.HasPrefix(r.URL.Path, "/public/")
		if public {
			h.Set("Referrer-Policy", "no-referrer")
		} else {
			h.Set("Referrer-Policy", "same-origin")
		}
		next.ServeHTTP(&headerWriter{ResponseWriter: w, private: !public && !strings.HasPrefix(r.URL.Path, "/static/")}, r)
	})
}

// headerWriter adds "Cache-Control: private" to HTML pages that did not choose their own caching.
type headerWriter struct {
	http.ResponseWriter
	private, wrote bool
}

func (w *headerWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		h := w.Header()
		if w.private && h.Get("Cache-Control") == "" && strings.HasPrefix(h.Get("Content-Type"), "text/html") {
			h.Set("Cache-Control", "private")
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *headerWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		if w.Header().Get("Content-Type") == "" {
			w.Header().Set("Content-Type", http.DetectContentType(b))
		}
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

func (w *headerWriter) Flush() {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *headerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) serveOp(o *ops.Op, w http.ResponseWriter, r *http.Request) {
	e := s.env(r)
	// authenticate/authorize before touching the body so anonymous callers cannot make us buffer it
	err := o.Authorize(e)
	var in any
	if err == nil {
		in, err = o.Bind(r)
	}
	if err == nil {
		var out any
		if out, err = ops.Call(e, o.Name, in); err == nil {
			if st, ok := out.(*ops.Stream); ok {
				w.Header().Set("Content-Type", st.ContentType)
				w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", st.Name))
				_, _ = w.Write(st.Data)
				return
			}
			resp := map[string]any{"item": out}
			if rv := reflect.ValueOf(out); rv.Kind() == reflect.Slice {
				resp["items"] = out // documented shape for lists; "item" kept for existing clients
			}
			writeJSON(w, 200, resp)
			return
		}
	}
	apiErr(w, err)
}

func apiErr(w http.ResponseWriter, err error) {
	st, code, msg := 500, "internal", "internal error"
	if e, ok := err.(*ops.Error); ok {
		st, code, msg = e.Status, e.Code, e.Msg
	} else {
		fmt.Println("api error:", err)
	}
	writeJSON(w, st, map[string]any{"error": map[string]any{"code": code, "message": msg}})
}

func writeJSON(w http.ResponseWriter, st int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st)
	_ = json.NewEncoder(w).Encode(v)
}

// sse streams realtime events. ?rooms=board:ID,project:ID ("user:<me>" is implicit).
func (s *Server) sse(w http.ResponseWriter, r *http.Request) {
	e := s.env(r)
	if e.User == nil {
		apiErr(w, ops.Unauthorized())
		return
	}
	release, ok := s.lim().acquireStream(e.User.UserID)
	if !ok {
		tooManyStreams(w, s.lim().maxStreams)
		return
	}
	defer release()
	if envelopeMode(r) {
		s.sseEvents(w, r, e)
		return
	}
	want := map[string]bool{"user:" + e.User.UserID: true}
	for _, rm := range strings.Split(r.URL.Query().Get("rooms"), ",") {
		if rm != "" && authz.RoomAllowed(e, rm) {
			want[rm] = true
		}
	}
	ch, cancel := s.Hub.Subscribe(func(room string) bool { return want[room] })
	defer cancel()
	fl, ok := w.(http.Flusher)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprint(w, ": ok\n\n")
	fl.Flush()
	// a comment every 25s: a half-open connection (sleeping laptop, changed Wi-Fi) fails the write,
	// so the stream ends and its per-user slot is released
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			// event name enables htmx sse extension triggers (sse:<name>)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, b)
			fl.Flush()
		}
	}
}
