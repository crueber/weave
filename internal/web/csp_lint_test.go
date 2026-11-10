package web

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The Content-Security-Policy forbids inline scripts, inline event handlers, javascript: URLs and
// eval, so no template (or Go string that emits HTML) may rely on them. See docs/DESIGN.md.
var cspForbidden = []struct {
	name string
	re   *regexp.Regexp
}{
	{"inline <script> without src", regexp.MustCompile(`(?i)<script\b[^>]*>`)}, // filtered below
	{"inline event handler attribute", regexp.MustCompile(`(?i)<[a-z][^>]*\son[a-z]+\s*=`)},
	{"javascript: URL", regexp.MustCompile(`(?i)javascript:`)},
	{"hx-vals js:", regexp.MustCompile(`(?i)hx-(vals|headers)\s*=\s*["']?\s*js:`)},
	{"hx-on attribute", regexp.MustCompile(`(?i)\shx-on[:\-:]`)},
	// an event filter looks like "click[cond]" (the event token carries the bracket); "from:input[name=x]" is a selector
	{"hx-trigger filter expression (needs eval)", regexp.MustCompile(`(?i)hx-trigger\s*=\s*["'](?:[^"',\s\[]+[^"']*,\s*)*[^"',\s\[:]+\[`)},
}

func lintCSP(t *testing.T, file, src string) {
	for _, f := range cspForbidden {
		for _, m := range f.re.FindAllString(src, -1) {
			if f.name == "inline <script> without src" && regexp.MustCompile(`(?i)\bsrc\s*=`).MatchString(m) {
				continue
			}
			t.Errorf("%s: %s: %.80q", file, f.name, m)
		}
	}
}

func TestTemplatesAreCSPClean(t *testing.T) {
	err := fs.WalkDir(assets, "templates", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := assets.ReadFile(p)
		lintCSP(t, p, string(b))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestStaticScriptsDoNotEval(t *testing.T) {
	bad := regexp.MustCompile(`\beval\s*\(|new Function\(|setTimeout\(\s*["'` + "`" + `]|setInterval\(\s*["'` + "`" + `]`)
	err := fs.WalkDir(assets, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") || strings.HasSuffix(p, "htmx.min.js") {
			return err
		}
		b, _ := assets.ReadFile(p)
		if m := bad.FindString(string(b)); m != "" {
			t.Errorf("%s uses %q", p, m)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Go sources that emit HTML (handlers, export pages) follow the same rules; the export print
// button's one inline script is allowed by hash in its own policy (domain/notify/export.go).
func TestGoHTMLIsCSPClean(t *testing.T) {
	err := filepath.WalkDir("../domain", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, _ := os.ReadFile(p)
		src := string(b)
		if strings.HasSuffix(p, "notify/export.go") {
			src = strings.ReplaceAll(src, "<script>{{.PrintJS}}</script>", "")
		}
		lintCSP(t, p, src)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHTMXConfigDisablesEval(t *testing.T) {
	b, _ := assets.ReadFile("templates/layout.html")
	if !strings.Contains(string(b), `"allowEval":false`) {
		t.Error("layout must set htmx allowEval=false")
	}
}
