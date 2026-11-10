package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:6])
}

// atomicWrite writes via a temp file in the same directory and renames it into place, so a
// crash never leaves a half-written file (cursor files, config, schema cache).
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if werr == nil {
		werr = f.Sync()
	}
	cerr := f.Close()
	if werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(tmp, perm)
	}
	if werr == nil {
		werr = os.Rename(tmp, path)
	}
	if werr != nil {
		_ = os.Remove(tmp)
	}
	return werr
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// colorOK honours NO_COLOR and only colours terminals (Windows needs a VT-capable terminal).
func colorOK(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if !isTTY(f) {
		return false
	}
	if runtime.GOOS == "windows" {
		return os.Getenv("WT_SESSION") != "" || os.Getenv("ANSICON") != "" || os.Getenv("TERM") != ""
	}
	return true
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string {
	s = unsafeName.ReplaceAllString(s, "_")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// parseDur accepts Go durations ("15m", "90s") and bare numbers as seconds.
func parseDur(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return d, nil
	}
	var n float64
	if _, err := fmt.Sscan(s, &n); err == nil {
		return time.Duration(n * float64(time.Second)), nil
	}
	return 0, &APIError{Code: "usage", Message: "invalid duration " + s + " (try 30s, 15m, 2h)"}
}

var idLike = regexp.MustCompile(`^[0-9a-zA-Z]{20,}$`)

// looksLikeID reports whether v is plausibly a Weave id (long alphanumeric with a digit).
func looksLikeID(v string) bool {
	return idLike.MatchString(v) && strings.ContainsAny(v, "0123456789")
}

func (a *App) stdinTTY() bool {
	if f, ok := a.Stdin.(*os.File); ok {
		return isTTY(f)
	}
	return false
}
