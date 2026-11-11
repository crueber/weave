package cli

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxResponseBytes bounds how much of an API response the CLI buffers.
var maxResponseBytes int64 = 64 << 20

// readLimited reads at most maxResponseBytes from r and fails when the body is larger.
func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	if err != nil {
		return b, err
	}
	if int64(len(b)) > maxResponseBytes {
		return nil, errors.New("response is larger than 64 MiB; refusing to buffer it")
	}
	return b, nil
}

// insecureURLWarning returns a one-line warning when rawURL is plain http to a host that is not
// loopback, private (RFC1918/ULA) or link-local, i.e. when the token would cross the open internet.
func insecureURLWarning(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || !strings.EqualFold(u.Scheme, "http") {
		return ""
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return ""
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()) {
		return ""
	}
	return fmt.Sprintf("warning: %s uses plain http; your API token is sent unencrypted. Use https:// unless this is a trusted network.", rawURL)
}

// warnInsecure prints insecureURLWarning once per process invocation.
func (a *App) warnInsecure() {
	a.warnOnce.Do(func() {
		if w := insecureURLWarning(a.Cfg.URL); w != "" && a.Stderr != nil {
			fmt.Fprintln(a.Stderr, w)
		}
	})
}

// warnTokenFlag nudges users away from putting the token on the command line.
func (a *App) warnTokenFlag(tok string) {
	if tok == "" || tok == "-" {
		return
	}
	a.tokenOnce.Do(func() {
		if a.Stderr != nil {
			fmt.Fprintln(a.Stderr, "warning: --token with a literal value is visible in the process list and shell history; prefer WEAVE_TOKEN or `weave login --token -`.")
		}
	})
}

// redirectPolicy refuses https->http downgrades and, when a token is attached, redirects that
// leave the original host (the token must never follow a redirect to another server).
func redirectPolicy(hasToken bool) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		first := via[0]
		if strings.EqualFold(via[len(via)-1].URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
			return errors.New("refusing to follow a redirect from https to http")
		}
		if hasToken && !strings.EqualFold(first.URL.Host, req.URL.Host) {
			return fmt.Errorf("refusing to follow a redirect to another host (%s) with an API token attached", req.URL.Host)
		}
		return nil
	}
}

// cleanText removes terminal control characters (C0, DEL, C1) from server-provided text so a
// hostile card name cannot inject escape sequences into the user's terminal. Tab and newline are
// kept when keepWhitespace is set (free text); otherwise they become spaces.
func cleanText(s string, keepWhitespace bool) string {
	clean := true
	for _, r := range s {
		if isControl(r) {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case (r == '\t' || r == '\n') && keepWhitespace:
			b.WriteRune(r)
		case r == '\t' || r == '\n' || r == '\r':
			if !keepWhitespace {
				b.WriteByte(' ')
			}
		case isControl(r):
			// dropped
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) }

var tokenPattern = regexp.MustCompile(`wk_[A-Za-z0-9]+`)

// redactSecrets masks the active token and anything shaped like a Weave API key.
func redactSecrets(s, token string) string {
	if len(token) >= 6 {
		s = strings.ReplaceAll(s, token, "[redacted]")
	}
	return tokenPattern.ReplaceAllString(s, "[redacted]")
}

// capTail keeps at most max bytes from the end of s (on a rune boundary).
func capTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	s = s[len(s)-max:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return "…" + s
}

// sanitizeTail prepares handler stderr for posting as a comment: no secrets, at most ~2 KB.
func sanitizeTail(s, token string) string {
	return capTail(redactSecrets(s, token), 2048)
}

// completionQuote makes a completion candidate safe to hand to shell scripts: control characters
// are dropped and anything with shell-special characters is single-quoted.
func completionQuote(s string) string {
	s = cleanText(s, false)
	safe := s != ""
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./:=@%+,", r) || r > 0x7f) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ensurePrivateDir creates dir (0700) and tightens an existing one to 0700.
func ensurePrivateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}
