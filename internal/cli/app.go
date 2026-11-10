// Package cli is the Weave command line client. It is schema-driven: the set of commands
// comes from the server's GET /api/v1/_ops (cached on disk), falling back to the registry
// compiled into the full binary or an embedded snapshot, so a slim binary stays in sync with
// whatever server it talks to. It never imports the server packages.
//
//	weave <tag> <verb> [path-args...] [--field value ...]
//	weave listen | agent run | next | inbox | whoami | doctor | skill | ...   (see `weave help`)
//
// Output is JSON on stdout by default; errors are JSON on stderr with stable exit codes
// (see the Exit* constants).
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crueber/weave/internal/version"
)

// Version is the client version shown by `weave version`; defaults to the stamped build version.
func Version() string { return version.Version }

// App is one CLI invocation.
type App struct {
	Cfg    Resolved
	Out    OutOpts
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	HTTP   *http.Client

	SkillEnv *skillEnv // test hook: injected OS, home and PATH for `weave skill`

	Timeout time.Duration
	DryRun  bool
	Yes     bool

	catalog   *Catalog
	self      *Identity
	candCache map[string][]Candidate
	pre       struct{ url, token, profile string }
	ctx       context.Context

	// listener and agent workers resolve names and read the catalog concurrently
	catMu, candMu, selfMu sync.Mutex
	refreshed             bool
	warnOnce, tokenOnce   sync.Once
}

type cmdFunc func(a *App, args []string) int

// Main runs the CLI with the process's standard streams.
func Main(args []string) int { return Run(args, os.Stdin, os.Stdout, os.Stderr) }

// Run executes one invocation and returns the process exit code.
func Run(args []string, in io.Reader, out, errw io.Writer) int {
	a := &App{Stdin: in, Stdout: out, Stderr: errw, Timeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a.ctx = ctx
	return a.dispatch(args)
}

// scanPre extracts --url/--token/--profile appearing BEFORE the command (they are only global
// there; after the command an op may own a field called --url, e.g. webhooks).
func (a *App) scanPre(args []string) []string {
	i := 0
	for i < len(args) {
		k, v, eq := strings.Cut(strings.TrimPrefix(args[i], "--"), "=")
		if !strings.HasPrefix(args[i], "--") {
			break
		}
		switch k {
		case "url", "token", "profile":
			if !eq {
				if i+1 >= len(args) {
					return args
				}
				v = args[i+1]
				i++
			}
			switch k {
			case "url":
				a.pre.url = v
			case "token":
				a.pre.token = v
			default:
				a.pre.profile = v
			}
			i++
		case "no-color":
			os.Setenv("NO_COLOR", "1")
			i++
		default:
			return args[i:]
		}
	}
	return args[i:]
}

func (a *App) dispatch(args []string) int {
	args = a.scanPre(args)
	a.warnTokenFlag(a.pre.token)
	a.Cfg = resolveConfig(a.pre.url, a.pre.token, a.pre.profile)
	if len(args) == 0 {
		return a.help(nil)
	}
	switch args[0] {
	case "help", "-h", "--help":
		return a.help(args[1:])
	case "version", "--version", "-V":
		return a.cmdVersion(args[1:])
	}
	// shortcuts: "tag verb" first, then "tag"
	wantsHelp := func(rest []string) bool {
		for _, x := range rest {
			if x == "--help" || x == "-h" {
				return true
			}
			if x == "--" {
				return false
			}
		}
		return false
	}
	if len(args) > 1 && !strings.HasPrefix(args[1], "-") {
		if f, ok := shortcuts[args[0]+" "+args[1]]; ok {
			if wantsHelp(args[2:]) {
				fmt.Fprint(a.Stdout, helpFor(args[0]+" "+args[1]))
				return 0
			}
			if o := a.delegateToOp(args[0]+" "+args[1], args[0], args[1], args[2:]); o != nil {
				return a.cmdOp(o, args[2:])
			}
			return f(a, args[2:])
		}
	}
	if f, ok := shortcuts[args[0]]; ok {
		if wantsHelp(args[1:]) {
			fmt.Fprint(a.Stdout, helpFor(args[0]))
			return 0
		}
		return f(a, args[1:])
	}
	return a.runGeneric(args)
}

// delegateToOp keeps every existing `weave <tag> <verb>` invocation working when a workflow
// shortcut shadows a real op: if the args use a flag only the op knows (e.g. card create --dueDate,
// card move --afterId), the generic schema-driven path runs instead of the shortcut.
func (a *App) delegateToOp(key, tag, verb string, args []string) *Op {
	if len(args) == 0 {
		return nil
	}
	o := a.catalogNow(false).Lookup(tag, verb)
	if o == nil {
		return nil
	}
	known := map[string]bool{}
	for _, s := range mergeSpecs(shortcutSpecs[key]) {
		known[norm(s.Name)] = true
		for _, al := range s.Aliases {
			known[norm(al)] = true
		}
	}
	opOnly := false
	for _, x := range args {
		if x == "--" {
			break
		}
		if !strings.HasPrefix(x, "--") || len(x) < 3 {
			continue
		}
		name, _, _ := strings.Cut(x[2:], "=")
		if known[norm(name)] {
			continue
		}
		for _, f := range o.Fields {
			if norm(f.Name) == norm(name) {
				opOnly = true
			}
		}
	}
	if !opOnly {
		return nil
	}
	return o
}

// fail prints a stable machine-readable error on stderr and returns its exit code.
func (a *App) fail(err error) int {
	ae, ok := err.(*APIError)
	if !ok {
		ae = &APIError{Code: "error", Message: err.Error()}
	}
	e := map[string]any{"code": ae.Code, "message": ae.Message}
	if ae.Status != 0 {
		e["status"] = ae.Status
	}
	if ae.Detail != nil {
		e["candidates"] = ae.Detail
	}
	b, _ := json.Marshal(map[string]any{"error": e})
	fmt.Fprintln(a.Stderr, string(b))
	return ae.Exit()
}

func (a *App) failUsage(msg string) int { return a.fail(usageErr(msg)) }

// parse parses args with the command's own flags plus the globals, then applies the globals.
func (a *App) parse(args []string, own []FlagSpec) (*Parsed, error) {
	p, err := ParseFlags(args, mergeSpecs(own))
	if err != nil {
		return nil, err
	}
	owns := false
	for _, s := range own {
		if norm(s.Name) == "timeout" {
			owns = true // e.g. listen --timeout is a wait duration, not the HTTP timeout
		}
	}
	return p, a.configure(p, owns)
}

func (a *App) configure(p *Parsed, ownsTimeout bool) error {
	url, token := a.pre.url, a.pre.token
	if p.Has("url") {
		url = p.Str("url")
	}
	if p.Has("token") {
		token = p.Str("token")
		a.warnTokenFlag(token)
	}
	prof := a.pre.profile
	if p.Has("profile") {
		prof = p.Str("profile")
	}
	old := a.Cfg
	a.Cfg = resolveConfig(url, token, prof)
	if old.URL != a.Cfg.URL {
		a.catalog, a.self, a.candCache = nil, nil, nil
	}
	o := OutOpts{Format: "json", Fields: parseFields(p.Str("fields")), JQ: p.Str("jq")}
	if f := p.Str("output"); f != "" {
		switch f {
		case "json", "ndjson", "table", "ids", "quiet":
			o.Format = f
		default:
			return usageErr("--output must be json, ndjson, table, ids or quiet")
		}
	}
	if p.Bool("quiet") {
		o.Format = "quiet"
	}
	a.Out = o
	if p.Has("timeout") && !ownsTimeout {
		d, err := parseDur(p.Str("timeout"))
		if err != nil {
			return err
		}
		a.Timeout = d
	}
	a.DryRun = p.Bool("dry-run") || os.Getenv("WEAVE_DRY_RUN") == "1"
	a.Yes = p.Bool("yes")
	if p.Bool("no-color") {
		os.Setenv("NO_COLOR", "1")
	}
	return nil
}

// catalogNow loads the schema (network allowed, cached).
func (a *App) catalogNow(refresh bool) *Catalog {
	return a.loadCatalog(a.ctx, false, refresh)
}

// shortcuts are registered by init() functions in the workflow files.
var shortcuts = map[string]cmdFunc{}

func register(name string, f cmdFunc) { shortcuts[name] = f }

func init() {
	register("ops", cmdOps)
	register("schema", cmdSchema)
	register("config", cmdConfig)
	register("login", cmdLogin)
	register("logout", cmdLogout)
	register("whoami", cmdWhoami)
	register("doctor", cmdDoctor)
	register("completion", cmdCompletion)
	register("__complete", cmdComplete)
	register("serve", func(a *App, _ []string) int {
		return a.failUsage("`weave serve` is only in the full weave binary (server + CLI), not the slim CLI")
	})
	register("healthcheck", func(a *App, _ []string) int {
		return a.failUsage("`weave healthcheck` is only in the full weave binary")
	})
}

func cmdOps(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "names", Bool: true}})
	if err != nil {
		return a.fail(err)
	}
	cat := a.catalogNow(p.Bool("refresh-schema"))
	var list []*Op
	if len(p.Pos) > 0 {
		list = cat.Tag(p.Pos[0])
	} else {
		list = cat.All()
	}
	if p.Bool("names") {
		for _, o := range list {
			fmt.Fprintln(a.Stdout, o.Name)
		}
		return 0
	}
	b, _ := json.Marshal(list)
	if a.Out.Format == "json" && a.Out.JQ == "" && len(a.Out.Fields) == 0 {
		// historic shape: a bare JSON array
		fmt.Fprintln(a.Stdout, string(b))
		return 0
	}
	var v any
	_ = json.Unmarshal(b, &v)
	if err := a.emit(map[string]any{"items": v}, nil); err != nil {
		return a.fail(err)
	}
	return 0
}

func cmdSchema(a *App, args []string) int {
	p, err := a.parse(args, nil)
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) < 1 {
		return a.failUsage("weave schema <op-name>")
	}
	o := a.catalogNow(p.Bool("refresh-schema")).Get(p.Pos[0])
	if o == nil {
		return a.failUsage("unknown op " + p.Pos[0] + "; see `weave ops`")
	}
	b, _ := json.MarshalIndent(o, "", "  ")
	fmt.Fprintln(a.Stdout, string(b))
	return 0
}
