package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crueber/weave/internal/ops"
)

// Abuse limits. Every default is generous enough that normal use (htmx polling, drag-and-drop
// bursts, pasting 100 cards, several browser tabs and a few agents) never notices them; all are
// tunable through the environment and 0 switches a limit off.
//
//	WEAVE_RATE_LIMIT          sustained requests/second per principal (default 20; burst WEAVE_RATE_BURST, default 200)
//	WEAVE_RATE_LIMIT_SEARCH   search requests per minute (default 300, burst 30)
//	WEAVE_RATE_LIMIT_HEAVY    exports, board import, notification-service tests per minute (default 60, burst 15)
//	WEAVE_RATE_LIMIT_IP       optional per-IP ceiling (requests/second) on top of the per-principal buckets (default off: offices share one IP)
//	WEAVE_RATE_LIMIT_LOGIN    sign-in starts per minute per client IP (default 20, burst 20)
//	WEAVE_MAX_STREAMS_PER_USER  concurrent /events streams per user (default 20)
type limits struct {
	rate, burst float64 // per second / bucket size; rate 0 = off
	ipRate      float64 // optional per-IP backstop for credentialed requests (0 = off)
	search      perMin
	heavy       perMin
	login       perMin
	maxStreams  int
	trustProxy  bool
	mu          sync.Mutex
	buckets     map[string]*bucket
	lastPrune   time.Time
	streamMu    sync.Mutex
	streams     map[string]int
	now         func() time.Time
}

type perMin struct{ perMinute, burst float64 }

type bucket struct {
	tokens float64
	last   time.Time
}

func envNum(k string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
		fmt.Println(k+": invalid value", v, "- using", def)
	}
	return def
}

func loadLimits(trustProxy bool) *limits {
	l := &limits{
		rate: envNum("WEAVE_RATE_LIMIT", 20), burst: envNum("WEAVE_RATE_BURST", 200), ipRate: envNum("WEAVE_RATE_LIMIT_IP", 0),
		search:     perMin{envNum("WEAVE_RATE_LIMIT_SEARCH", 300), 30},
		heavy:      perMin{envNum("WEAVE_RATE_LIMIT_HEAVY", 60), 15},
		login:      perMin{envNum("WEAVE_RATE_LIMIT_LOGIN", 20), 20},
		maxStreams: int(envNum("WEAVE_MAX_STREAMS_PER_USER", 20)),
		trustProxy: trustProxy, buckets: map[string]*bucket{}, streams: map[string]int{}, now: time.Now,
	}
	if l.burst < 1 {
		l.burst = 1
	}
	return l
}

// lim returns the (lazily built) limiter state; Server values are built by struct literal in tests.
func (s *Server) lim() *limits {
	s.limOnce.Do(func() { s.limits = loadLimits(s.Cfg.TrustProxy) })
	return s.limits
}

// take removes one token from the named bucket (rate tokens/second, capacity burst).
// It reports whether the request may proceed and, when not, how long until a token exists.
func (l *limits) take(key string, rate, burst float64) (bool, time.Duration) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastPrune) > time.Minute {
		l.lastPrune = now
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	b := l.buckets[key]
	if b == nil {
		if len(l.buckets) > 200000 { // memory guard against key spraying: fail open for new keys only
			return true, 0
		}
		b = &bucket{tokens: burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(burst, b.tokens+now.Sub(b.last).Seconds()*rate)
	b.last = now
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / rate * float64(time.Second))
}

func (l *limits) clientIP(r *http.Request) string {
	if l.trustProxy {
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			return strings.TrimSpace(strings.Split(f, ",")[0])
		}
	}
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return h
}

// principalKey identifies the caller without a database lookup: a hash of the presented
// credential (API key or session cookie) or, for anonymous callers, the client IP. Requests that
// present a credential are additionally counted against their IP (see throttle) so inventing
// credentials cannot dodge the limit.
func (l *limits) principalKey(r *http.Request) (key string, authed bool) {
	cred := ""
	if a := r.Header.Get("Authorization"); a != "" {
		cred = a
	} else if k := r.Header.Get("X-API-Key"); k != "" {
		cred = k
	} else if c, err := r.Cookie("weave_session"); err == nil && c.Value != "" {
		cred = c.Value
	}
	if cred == "" {
		return "ip:" + l.clientIP(r), false
	}
	h := sha256.Sum256([]byte(cred))
	return "cred:" + hex.EncodeToString(h[:8]), true
}

type routeClass int

const (
	classNone routeClass = iota
	classSearch
	classHeavy
	classLogin
)

func classify(r *http.Request) routeClass {
	p := strings.TrimPrefix(r.URL.Path, "/api/v1")
	switch {
	case p == "/auth/oidc/login" || p == "/auth/oidc/authorize": // sign-in starts only
		return classLogin
	case r.Method == http.MethodGet && (p == "/search" || p == "/ui/search"):
		return classSearch
	case r.Method == http.MethodGet && (strings.HasPrefix(p, "/export/") || (strings.HasPrefix(p, "/boards/") && strings.HasSuffix(p, "/export"))):
		return classHeavy
	case r.Method == http.MethodPost && (strings.HasSuffix(p, "/boards/import") || (strings.HasPrefix(p, "/notification-services/") || strings.HasPrefix(p, "/ui/notification-services/")) && strings.HasSuffix(p, "/test")):
		return classHeavy
	}
	return classNone
}

// throttle applies the token buckets. SSE streams, static files and health checks are exempt.
func (s *Server) throttle(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l := s.lim()
		if r.URL.Path == "/events" || r.URL.Path == "/healthz" || strings.HasPrefix(r.URL.Path, "/static/") {
			next.ServeHTTP(w, r)
			return
		}
		key, authed := l.principalKey(r)
		ip := l.clientIP(r)
		check := func(k string, rate, burst float64) bool {
			if rate <= 0 {
				return true
			}
			ok, wait := l.take(k, rate, burst)
			if !ok {
				tooManyRequests(w, r, wait)
			}
			return ok
		}
		if !check(key, l.rate, l.burst) {
			return
		}
		if authed && l.ipRate > 0 && !check("ip:"+ip, l.ipRate, l.ipRate*10) { // optional backstop against invented credentials from one address
			return
		}
		switch classify(r) {
		case classSearch:
			if !check("search:"+key, l.search.perMinute/60, l.search.burst) {
				return
			}
		case classHeavy:
			if !check("heavy:"+key, l.heavy.perMinute/60, l.heavy.burst) {
				return
			}
		case classLogin:
			if !check("login:"+ip, l.login.perMinute/60, l.login.burst) {
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func tooManyRequests(w http.ResponseWriter, r *http.Request, wait time.Duration) {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	msg := fmt.Sprintf("Too many requests - slow down and retry in %ds.", secs)
	switch {
	case r.Header.Get("HX-Request") != "":
		w.Header().Set("HX-Retarget", "#toasts")
		w.Header().Set("HX-Reswap", "beforeend")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintf(w, `<div class="toast toast-error" role="alert">%s</div>`, msg)
	case strings.HasPrefix(r.URL.Path, "/api/"):
		apiErr(w, &ops.Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Msg: msg})
	default:
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprintln(w, msg)
	}
}

// acquireStream reserves one live-update stream for the user; the returned func releases it.
func (l *limits) acquireStream(user string) (release func(), ok bool) {
	if l.maxStreams <= 0 {
		return func() {}, true
	}
	l.streamMu.Lock()
	defer l.streamMu.Unlock()
	if l.streams[user] >= l.maxStreams {
		return nil, false
	}
	l.streams[user]++
	var once sync.Once
	return func() {
		once.Do(func() {
			l.streamMu.Lock()
			defer l.streamMu.Unlock()
			if l.streams[user]--; l.streams[user] <= 0 {
				delete(l.streams, user)
			}
		})
	}, true
}

func tooManyStreams(w http.ResponseWriter, max int) {
	w.Header().Set("Retry-After", "5")
	apiErr(w, &ops.Error{Status: http.StatusTooManyRequests, Code: "too_many_streams",
		Msg: fmt.Sprintf("too many live-update streams open for this user (limit %d): close other tabs or clients; the limit is WEAVE_MAX_STREAMS_PER_USER", max)})
}
