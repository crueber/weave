package ops_test

import (
	"regexp"
	"strings"
	"testing"

	_ "github.com/crueber/weave/internal/all"
	"github.com/crueber/weave/internal/ops"
)

var (
	nameRe  = regexp.MustCompile(`^[a-z][a-z_]*(\.[a-z][a-zA-Z_]*)+$`)
	paramRe = regexp.MustCompile(`\{([A-Za-z0-9_]+)\}`)
)

// Every op must declare how the browser exposes it (UI route/fragment name) or
// an explicit exemption with a reason. The CLI covers every op automatically.
func TestUIParityDeclared(t *testing.T) {
	for _, o := range ops.All() {
		if o.UI == "" {
			t.Errorf("op %s has no UI mapping; set UI to a route (e.g. \"GET /boards/{id}\") or \"exempt:<reason>\"", o.Name)
		}
		if strings.HasPrefix(o.UI, "exempt:") && len(o.UI) < 9 {
			t.Errorf("op %s: exemption needs a reason", o.Name)
		}
	}
}

// Op names are stable CLI/API identifiers: lowercase dotted words.
func TestOpNaming(t *testing.T) {
	for _, o := range ops.All() {
		if !nameRe.MatchString(o.Name) {
			t.Errorf("op name %q must match %s", o.Name, nameRe)
		}
	}
}

// Path parameters and path-tagged input fields must correspond one to one.
func TestPathParams(t *testing.T) {
	for _, o := range ops.All() {
		inPath := map[string]bool{}
		for _, m := range paramRe.FindAllStringSubmatch(o.Path, -1) {
			inPath[m[1]] = true
		}
		tagged := map[string]bool{}
		for _, f := range o.Schema().Fields {
			if f.In == "path" {
				tagged[f.Name] = true
			}
		}
		for p := range inPath {
			if !tagged[p] {
				t.Errorf("op %s: path param {%s} has no path:%q field in input", o.Name, p, p)
			}
		}
		for p := range tagged {
			if !inPath[p] {
				t.Errorf("op %s: field path:%q is not present in Path %s", o.Name, p, o.Path)
			}
		}
	}
}

// No two ops may share a route (param names are irrelevant to the mux).
func TestUniqueRoutes(t *testing.T) {
	seen := map[string]string{}
	for _, o := range ops.All() {
		k := o.Method + " " + paramRe.ReplaceAllString(o.Path, "{}")
		if prev, dup := seen[k]; dup {
			t.Errorf("ops %s and %s share route %s", prev, o.Name, k)
		}
		seen[k] = o.Name
	}
}
