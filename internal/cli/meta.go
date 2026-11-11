package cli

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/crueber/weave/internal/version"
)

const helpText = `weave - CLI for the Weave kanban server (built for agents and humans)

Connect:
  weave login --url https://weave.example.com --token wk_...   verify and save a profile
  weave whoami | doctor | version [--check]
  weave config list|use|set|remove|get|path                     profiles (WEAVE_PROFILE, --profile)

Work with boards (names or ids everywhere: --board "Launch plan" --list Backlog):
  weave board tree --board B               lists and cards at a glance
  weave inbox                              cards assigned to / claimed by me
  weave next --board B --list Ready [--claim] [--move-to 'In progress'] [--ttl 15m]
                                           pull the next card; exit 10 when the queue is empty
  weave card create --board B --list L --name "Title" [--desc ..] [--label X] [--assign me]
  weave card show ID [--full] | move ID --to-list L | comment ID "text" | attach ID FILE
  weave card claim|release|renew|done ID [--note ..]
  weave task add|check|uncheck|list ...    checklists
  weave list stats --board B

Listen (agents):
  weave listen --board B --list Ready --list Review [--exec CMD] [--once] [--include-existing]
  weave agent run --board B --listen Ready --claim-move-to 'In progress' --done-list Review -- ./handle.sh

Everything else is generated from the server schema:
  weave ops [tag]    weave schema card.create    weave <tag> <verb> --help

Output: JSON on stdout. --output json|ndjson|table|ids|quiet  --fields a,b.c  --jq '.item.id'
  --limit N --cursor C --all  --dry-run  --yes  --timeout 30s  --body @file.json|-  -o FILE
Errors: JSON on stderr. Exit: 0 ok 1 error 2 usage 3 unauthorized 4 not found 5 forbidden
  6 validation 7 conflict 8 network 10 nothing to do (empty queue / timeout)
Agent skill: weave skill print|install|check|targets|uninstall      Shell completion: weave completion bash|zsh|fish|powershell
Docs: docs/CLI.md, docs/AGENTS.md
`

func (a *App) help(args []string) int {
	if len(args) > 0 {
		cat := a.loadCatalog(a.ctx, true, false)
		tag := args[0]
		if f, ok := shortcutHelp[strings.Join(args, " ")]; ok {
			fmt.Fprint(a.Stdout, f)
			return 0
		}
		if len(args) > 1 {
			if o := cat.Lookup(tag, args[1]); o != nil {
				fmt.Fprint(a.Stdout, opHelp(o))
				return 0
			}
		}
		if len(cat.Tag(tag)) > 0 {
			fmt.Fprint(a.Stdout, tagHelp(cat, tag))
			return 0
		}
	}
	cat := a.loadCatalog(a.ctx, true, false)
	fmt.Fprint(a.Stdout, helpText)
	fmt.Fprintf(a.Stdout, "\nCommand groups (%s schema): %s\n", cat.Source, strings.Join(cat.Tags(), " "))
	return 0
}

// shortcutHelp holds help for hand-written commands, keyed by "tag verb" or "tag".
var shortcutHelp = map[string]string{}

func (a *App) cmdVersion(args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "check", Bool: true}})
	if err != nil {
		return a.fail(err)
	}
	item := map[string]any{"client": version.Version, "os": runtime.GOOS, "arch": runtime.GOARCH, "url": a.Cfg.URL}
	code := 0
	if p.Bool("check") || p.Bool("server") {
		cat := a.loadCatalog(a.ctx, false, p.Bool("check"))
		item["schemaSource"] = cat.Source
		r, err := a.Do(a.ctx, "system.version", nil)
		if err != nil {
			item["server"] = nil
			item["error"] = err.Error()
			_ = a.emit(map[string]any{"item": item}, nil)
			return a.fail(err)
		}
		sv := str(itemOf(r.JSON()), "version")
		item["server"] = sv
		skew := sv != "" && sv != version.Version && version.Version != "dev" && sv != "dev"
		item["skew"] = skew
		if skew && p.Bool("check") {
			code = 1
		}
	}
	if a.Out.Format == "json" && a.Out.JQ == "" && len(a.Out.Fields) == 0 && a.stdoutTTY() && !p.Bool("check") {
		fmt.Fprintf(a.Stdout, "weave %s (%s/%s)\n", version.Version, runtime.GOOS, runtime.GOARCH)
		return 0
	}
	_ = a.emit(map[string]any{"item": item}, nil)
	return code
}

func cmdWhoami(a *App, args []string) int {
	if _, err := a.parse(args, nil); err != nil {
		return a.fail(err)
	}
	me, err := a.Whoami(a.ctx)
	if err != nil {
		return a.fail(err)
	}
	item := map[string]any{}
	for k, v := range me.Raw {
		item[k] = v
	}
	item["profile"], item["url"] = a.Cfg.Profile, a.Cfg.URL
	if item["id"] == nil && item["user"] == nil {
		item["id"], item["name"] = me.ID, me.Name
	}
	if err := a.emit(map[string]any{"item": item}, nil); err != nil {
		return a.fail(err)
	}
	return 0
}

func cmdLogin(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "no-verify", Bool: true}, {Name: "use", Bool: true}})
	if err != nil {
		return a.fail(err)
	}
	token := a.Cfg.Token
	if p.Has("token") && p.Str("token") == "-" {
		tk, err := a.readValue("-")
		if err != nil {
			return a.fail(err)
		}
		token = strings.TrimSpace(tk)
	}
	if token == "" && a.stdinTTY() {
		fmt.Fprint(a.Stderr, "API key: ")
		line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
		token = strings.TrimSpace(line)
	}
	if token == "" {
		return a.failUsage("weave login --url URL --token KEY   (create a key in the browser under Settings -> API key, or `weave agent create`)")
	}
	u := strings.TrimRight(a.Cfg.URL, "/")
	if pu, err := url.Parse(u); err != nil || pu.Host == "" {
		return a.failUsage("invalid --url " + u)
	}
	a.Cfg.URL, a.Cfg.Token = u, token
	a.catalog, a.self = nil, nil
	var item map[string]any
	if !p.Bool("no-verify") {
		me, err := a.Whoami(a.ctx)
		if err != nil {
			return a.fail(err)
		}
		item = me.Raw
	}
	cf := loadConfigFile()
	name := firstNonEmpty(p.Str("profile"), a.pre.profile)
	if name == "" {
		if len(cf.Profiles) == 0 {
			name = "default"
		} else if pu, _ := url.Parse(u); pu != nil {
			name = safeName(pu.Hostname())
		}
	}
	cf.Profiles[name] = Profile{URL: u, Token: token}
	if cf.Current == "" || p.Bool("use") || len(cf.Profiles) == 1 {
		cf.Current = name
	}
	if err := saveConfigFile(cf); err != nil {
		return a.fail(&APIError{Code: "io", Message: err.Error()})
	}
	out := map[string]any{"profile": name, "url": u, "current": cf.Current == name, "config": configPath()}
	if item != nil {
		out["whoami"] = item
	}
	if err := a.emit(map[string]any{"item": out}, nil); err != nil {
		return a.fail(err)
	}
	return 0
}

func cmdLogout(a *App, args []string) int {
	if _, err := a.parse(args, nil); err != nil {
		return a.fail(err)
	}
	cf := loadConfigFile()
	if cf.Profiles[a.Cfg.Profile].URL == "" && cf.Profiles[a.Cfg.Profile].Token == "" {
		return a.fail(&APIError{Code: "not_found", Message: "no saved profile " + a.Cfg.Profile})
	}
	pr := cf.Profiles[a.Cfg.Profile]
	pr.Token = ""
	cf.Profiles[a.Cfg.Profile] = pr
	if err := saveConfigFile(cf); err != nil {
		return a.fail(&APIError{Code: "io", Message: err.Error()})
	}
	return 0
}

func cmdConfig(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "show", Bool: true}})
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) == 0 {
		return a.failUsage("weave config list|use NAME|set url|token VALUE|get KEY|remove NAME|path")
	}
	cf := loadConfigFile()
	prof := a.Cfg.Profile
	switch p.Pos[0] {
	case "path":
		fmt.Fprintln(a.Stdout, configPath())
	case "list":
		var items []any
		for _, n := range cf.names() {
			pr := cf.Profiles[n]
			items = append(items, map[string]any{"profile": n, "url": pr.URL, "tokenSet": pr.Token != "", "current": n == a.Cfg.Profile})
		}
		if items == nil {
			items = []any{}
		}
		if err := a.emit(map[string]any{"items": items}, nil); err != nil {
			return a.fail(err)
		}
	case "use":
		if len(p.Pos) < 2 {
			return a.failUsage("weave config use NAME")
		}
		if _, ok := cf.Profiles[p.Pos[1]]; !ok {
			return a.fail(&APIError{Code: "not_found", Message: "no profile " + p.Pos[1] + "; have: " + strings.Join(cf.names(), ", ")})
		}
		cf.Current = p.Pos[1]
		if err := saveConfigFile(cf); err != nil {
			return a.fail(&APIError{Code: "io", Message: err.Error()})
		}
	case "remove", "rm":
		if len(p.Pos) < 2 {
			return a.failUsage("weave config remove NAME")
		}
		if _, ok := cf.Profiles[p.Pos[1]]; !ok {
			return a.fail(&APIError{Code: "not_found", Message: "no profile " + p.Pos[1]})
		}
		delete(cf.Profiles, p.Pos[1])
		if cf.Current == p.Pos[1] {
			cf.Current = ""
			if n := cf.names(); len(n) > 0 {
				cf.Current = n[0]
			}
		}
		if err := saveConfigFile(cf); err != nil {
			return a.fail(&APIError{Code: "io", Message: err.Error()})
		}
	case "set":
		if len(p.Pos) < 3 {
			return a.failUsage("weave config set url|token VALUE [--profile NAME]")
		}
		pr := cf.Profiles[prof]
		switch p.Pos[1] {
		case "url":
			pr.URL = strings.TrimRight(p.Pos[2], "/")
		case "token":
			pr.Token = p.Pos[2]
		default:
			return a.failUsage("key must be url or token")
		}
		cf.Profiles[prof] = pr
		if cf.Current == "" {
			cf.Current = prof
		}
		if err := saveConfigFile(cf); err != nil {
			return a.fail(&APIError{Code: "io", Message: err.Error()})
		}
	case "get":
		if len(p.Pos) < 2 {
			return a.failUsage("weave config get url|token|profile")
		}
		switch p.Pos[1] {
		case "url":
			fmt.Fprintln(a.Stdout, a.Cfg.URL)
		case "profile":
			fmt.Fprintln(a.Stdout, a.Cfg.Profile)
		case "token":
			t := a.Cfg.Token
			if !p.Bool("show") && len(t) > 6 {
				t = t[:4] + strings.Repeat("*", 8)
			}
			fmt.Fprintln(a.Stdout, t)
		default:
			return a.failUsage("key must be url, token or profile")
		}
	default:
		return a.failUsage("unknown config command " + p.Pos[0])
	}
	return 0
}

type check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Warn   bool   `json:"warn,omitempty"`
}

func cmdDoctor(a *App, args []string) int {
	if _, err := a.parse(args, nil); err != nil {
		return a.fail(err)
	}
	var checks []check
	add := func(name string, ok bool, detail string, warn bool) {
		checks = append(checks, check{name, ok, detail, warn})
	}
	add("profile", true, fmt.Sprintf("%s -> %s (token %s)", a.Cfg.Profile, a.Cfg.URL, map[bool]string{true: "set", false: "MISSING"}[a.Cfg.Token != ""]), a.Cfg.Token == "")
	// connectivity + clock
	hc := &http.Client{Timeout: 8 * time.Second, CheckRedirect: redirectPolicy(a.Cfg.Token != "")}
	t0 := time.Now()
	resp, err := hc.Get(strings.TrimRight(a.Cfg.URL, "/") + "/healthz")
	if err != nil {
		add("connectivity", false, err.Error(), false)
	} else {
		resp.Body.Close()
		add("connectivity", resp.StatusCode == 200, fmt.Sprintf("GET /healthz -> %d in %dms", resp.StatusCode, time.Since(t0).Milliseconds()), false)
		if d, perr := http.ParseTime(resp.Header.Get("Date")); perr == nil {
			skew := time.Since(d)
			if skew < 0 {
				skew = -skew
			}
			add("clock", skew < 2*time.Minute, fmt.Sprintf("local clock differs from server by %s", skew.Round(time.Second)), skew >= 30*time.Second && skew < 2*time.Minute)
		}
	}
	// auth
	var me *Identity
	if err == nil {
		me, err = a.Whoami(a.ctx)
		if err != nil {
			add("auth", false, err.Error(), false)
		} else {
			add("auth", true, fmt.Sprintf("%s (%s) role=%s agent=%v, %d boards", me.Name, me.ID, me.Role, me.IsAgent, len(me.Boards)), false)
		}
	}
	// version skew + schema source
	if me != nil {
		cat := a.loadCatalog(a.ctx, false, true)
		add("schema", cat.Source != "snapshot" || true, "source: "+cat.Source+fmt.Sprintf(", %d ops", len(cat.All())), cat.Source != "server")
		if r, err := a.Do(a.ctx, "system.version", nil); err == nil {
			sv := str(itemOf(r.JSON()), "version")
			skew := sv != "" && sv != version.Version && version.Version != "dev" && sv != "dev"
			add("version", !skew, fmt.Sprintf("client %s, server %s", version.Version, sv), skew)
		}
		// SSE reachable
		ctx, cancel := context.WithTimeout(a.ctx, 4*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(a.Cfg.URL, "/")+"/events?heartbeat=1", nil)
		req.Header.Set("Authorization", "Bearer "+a.Cfg.Token)
		if r2, err := (&http.Client{CheckRedirect: redirectPolicy(true)}).Do(req); err != nil {
			add("events", false, err.Error(), false)
		} else {
			add("events", r2.StatusCode == 200 && strings.HasPrefix(r2.Header.Get("Content-Type"), "text/event-stream"),
				fmt.Sprintf("GET /events -> %d %s", r2.StatusCode, r2.Header.Get("Content-Type")), false)
			r2.Body.Close()
		}
		cancel()
	}
	allOK := true
	for _, c := range checks {
		if !c.OK {
			allOK = false
		}
	}
	if a.Out.Format == "json" && !a.stdoutTTY() || a.Out.JQ != "" || a.Out.Format != "json" {
		_ = a.emit(map[string]any{"items": checksToAny(checks), "ok": allOK}, nil)
	} else {
		for _, c := range checks {
			mark := "ok  "
			if !c.OK {
				mark = "FAIL"
			} else if c.Warn {
				mark = "warn"
			}
			fmt.Fprintf(a.Stdout, "[%s] %-12s %s\n", mark, c.Name, c.Detail)
		}
	}
	if !allOK {
		return ExitError
	}
	return 0
}

func checksToAny(cs []check) []any {
	out := make([]any, len(cs))
	for i, c := range cs {
		out[i] = map[string]any{"name": c.Name, "ok": c.OK, "detail": c.Detail, "warn": c.Warn}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
