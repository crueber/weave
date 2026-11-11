package cli

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/crueber/weave/skills"
)

var skillFlags = []FlagSpec{
	{Name: "target", Multi: true}, {Name: "scope"}, {Name: "dir"},
	{Name: "force", Bool: true}, {Name: "file"}, {Name: "compact", Bool: true},
}

const skillUsage = "weave skill print [--file references/commands.md|list] [--compact] | install [--target a,b|all] [--scope user|project] [--dir PATH] [--force] | targets | check [--target ..] | uninstall [--target ..]"

func init() {
	registerSpec("skill", skillFlags, cmdSkill)
}

// skillFiles returns the embedded skill: relative path (e.g. "SKILL.md", "references/commands.md") -> content.
func skillFiles() map[string][]byte {
	out := map[string][]byte{}
	_ = fs.WalkDir(skills.FS, "weave", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, _ := skills.FS.ReadFile(p)
		out[strings.TrimPrefix(p, "weave/")] = b
		return nil
	})
	return out
}

// instructionsBody is the compact rendering used for tools that only read instruction files.
func instructionsBody() string {
	b, _ := skills.FS.ReadFile("instructions.md")
	return strings.TrimSpace(string(b)) + "\n"
}

// ---------- environment (injectable for tests) ----------

type skillEnv struct {
	GOOS     string
	Home     string
	Cwd      string
	Getenv   func(string) string
	LookPath func(string) (string, error)
}

func realSkillEnv() *skillEnv {
	h, _ := os.UserHomeDir()
	cwd, _ := os.Getwd()
	return &skillEnv{GOOS: goos(), Home: h, Cwd: cwd, Getenv: os.Getenv, LookPath: exec.LookPath}
}

func (a *App) skillEnv() *skillEnv {
	if a.SkillEnv != nil {
		return a.SkillEnv
	}
	return realSkillEnv()
}

func (e *skillEnv) env(k string) string {
	if e.Getenv == nil {
		return ""
	}
	return e.Getenv(k)
}

func (e *skillEnv) has(bin string) bool {
	if e.LookPath == nil {
		return false
	}
	_, err := e.LookPath(bin)
	return err == nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// projectRoot is the nearest ancestor of Cwd holding .git, else Cwd.
func (e *skillEnv) projectRoot() string {
	for d := e.Cwd; ; {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		p := filepath.Dir(d)
		if p == d {
			return e.Cwd
		}
		d = p
	}
}

// ---------- resolution ----------

type resolved struct {
	T        *agentTarget
	Scope    string
	Place    place
	Skip     string // why this target has no place in this scope
	Detected bool
	Shared   string // another target with the same place that does the writing
}

func (e *skillEnv) resolve(t *agentTarget, scope, dirOverride string) resolved {
	r := resolved{T: t, Scope: scope, Detected: e.detected(t, scope)}
	var p string
	switch {
	case t.ID == "dir":
		p = dirOverride
	case scope == "project":
		if t.Project == "" {
			r.Skip = "no project scope"
			return r
		}
		p = filepath.Join(e.projectRoot(), filepath.FromSlash(t.Project))
	default:
		if t.User == nil {
			r.Skip = "no user scope"
			return r
		}
		p = t.User(e)
		if p == "" {
			r.Skip = "no user scope"
			return r
		}
	}
	if t.Fmt == fmtSkillDir && t.ID != "dir" {
		p = filepath.Join(p, "weave")
	}
	if dirOverride != "" && t.ID != "dir" {
		base := dirOverride
		if t.Fmt == fmtSkillDir {
			p = filepath.Join(base, "weave")
		} else {
			p = filepath.Join(base, filepath.Base(p))
		}
	}
	r.Place = place{Fmt: t.Fmt, Path: p, Front: t.Front}
	return r
}

func (e *skillEnv) detected(t *agentTarget, scope string) bool {
	if t.Detect != nil {
		for _, d := range t.Detect(e) {
			if d != "" && isDir(d) {
				return true
			}
		}
	}
	for _, b := range t.Bins {
		if e.has(b) {
			return true
		}
	}
	if scope == "project" && t.Project != "" {
		first := strings.SplitN(t.Project, "/", 2)[0]
		if strings.HasPrefix(first, ".") && isDir(filepath.Join(e.projectRoot(), first)) {
			return true
		}
	}
	return false
}

func (r *resolved) state(want map[string][]byte, body string) string {
	if r.Skip != "" {
		return "n/a"
	}
	if r.Place.Fmt == fmtSkillDir {
		s, _ := inspectDir(r.Place.Path, want)
		return s
	}
	s, _, _ := inspectFile(r.Place, body)
	return s
}

// selectTargets turns --target values (or auto mode) into resolved places, deduplicated by path.
func (a *App) selectTargets(p *Parsed, e *skillEnv, scope string, mode string) ([]resolved, bool, error) {
	var names []string
	for _, v := range p.All("target") {
		for _, n := range strings.Split(v, ",") {
			if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
				names = append(names, n)
			}
		}
	}
	dir := p.Str("dir")
	if dir != "" {
		dir = absPath(e, dir)
	}
	if dir != "" && len(names) == 0 {
		names = []string{"claude"} // legacy: --dir alone is the Claude-style skills dir
	}
	explicit := len(names) > 0
	var out []resolved
	switch {
	case explicit:
		var ts []*agentTarget
		for _, n := range names {
			if n == "all" {
				for _, t := range agentTargets {
					if t.Verified && !t.OptIn {
						ts = append(ts, t)
					}
				}
				continue
			}
			if n == "auto" {
				continue
			}
			t := findTarget(n)
			if t == nil {
				return nil, false, usageErr("unknown --target " + n + " (valid: " + strings.Join(targetNames(), ", ") + ", all)")
			}
			ts = append(ts, t)
		}
		if dir != "" && len(ts) != 1 {
			return nil, false, usageErr("--dir works with exactly one --target")
		}
		if len(ts) == 1 && ts[0].ID == "dir" && dir == "" {
			return nil, false, usageErr("--target dir needs --dir PATH")
		}
		for _, t := range ts {
			out = append(out, e.resolve(t, scope, dir))
		}
	default:
		for _, t := range agentTargets {
			if t.OptIn {
				continue
			}
			r := e.resolve(t, scope, "")
			if !t.Verified {
				if mode == "install" && r.Detected && r.Skip == "" {
					r.Skip = "detected, but its skills path is unverified: opt in with --target " + t.ID
					out = append(out, r)
				}
				continue
			}
			if r.Detected && r.Skip == "" {
				out = append(out, r)
			} else if mode != "install" && r.Skip == "" {
				// check/uninstall also cover tools that carry a Weave install although undetected
				if s := r.state(skillFiles(), instructionsBody()); s != stMissing {
					out = append(out, r)
				}
			}
		}
		active := 0
		for _, r := range out {
			if r.Skip == "" {
				active++
			}
		}
		if active == 0 {
			out = append(out, e.resolve(findTarget("agents"), scope, ""))
		}
	}
	// several tools read the same place; write it once
	seen := map[string]string{}
	for i := range out {
		if out[i].Skip != "" {
			continue
		}
		k := out[i].Place.Path
		if first, ok := seen[k]; ok {
			out[i].Shared = first
		} else {
			seen[k] = out[i].T.ID
		}
	}
	if !explicit && mode != "install" {
		// undetected tools that merely read a location already covered by a detected one add noise
		kept := out[:0]
		for _, r := range out {
			if r.Shared == "" || r.Detected {
				kept = append(kept, r)
			}
		}
		out = kept
	}
	return out, explicit, nil
}

func absPath(e *skillEnv, p string) string {
	if strings.HasPrefix(p, "~") {
		p = filepath.Join(e.Home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.Cwd, p)
	}
	return filepath.Clean(p)
}

// ---------- result rows ----------

type skillRow struct {
	Target     string   `json:"target"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Scope      string   `json:"scope"`
	Path       string   `json:"path,omitempty"`
	Action     string   `json:"action,omitempty"`
	State      string   `json:"state,omitempty"`
	Detected   bool     `json:"detected"`
	Verified   bool     `json:"verified"`
	SharedWith string   `json:"sharedWith,omitempty"`
	Note       string   `json:"note,omitempty"`
	Reason     string   `json:"reason,omitempty"`
	Detail     string   `json:"detail,omitempty"` // one-line human summary for the table
	Files      []string `json:"files,omitempty"`
}

func rowOf(r resolved) skillRow {
	return skillRow{Target: r.T.ID, Name: r.T.Name, Kind: r.T.kind(), Scope: r.Scope, Path: r.Place.Path,
		Detected: r.Detected, Verified: r.T.Verified, SharedWith: r.Shared}
}

func (a *App) emitRows(rows []skillRow, cols ...string) int {
	items := make([]any, 0, len(rows))
	for _, r := range rows {
		switch {
		case r.Reason != "":
			r.Detail = r.Reason
		case r.SharedWith != "":
			r.Detail = "same location as " + r.SharedWith
		default:
			r.Detail = r.Note
		}
		b, _ := json.Marshal(r)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		items = append(items, m)
	}
	if a.Out.Format == "" && a.stdoutTTY() {
		a.Out.Format = "table"
	}
	if a.Out.Format == "table" && len(a.Out.Fields) == 0 {
		a.Out.Fields = cols
	}
	return a.emitItems(items)
}

// ---------- command ----------

func cmdSkill(a *App, args []string) int {
	p, err := a.parse(args, skillFlags)
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) == 0 {
		return a.failUsage(skillUsage)
	}
	switch p.Pos[0] {
	case "print":
		return a.skillPrint(p)
	case "install":
		return a.skillInstall(p)
	case "check":
		return a.skillCheck(p)
	case "uninstall", "remove":
		return a.skillUninstall(p)
	case "targets", "list":
		return a.skillTargets(p)
	}
	return a.failUsage(skillUsage)
}

func scopeOf(p *Parsed) (string, error) {
	switch s := strings.ToLower(p.Str("scope")); s {
	case "", "user", "global":
		return "user", nil
	case "project", "local":
		return "project", nil
	default:
		return "", usageErr("--scope must be user or project")
	}
}

func (a *App) skillPrint(p *Parsed) int {
	if p.Bool("compact") {
		fmt.Fprint(a.Stdout, instructionsBody())
		return 0
	}
	files := skillFiles()
	name := firstNonEmpty(p.Str("file"), "SKILL.md")
	if name == "list" {
		names := append(sortedKeys(files), "(compact: --compact)")
		fmt.Fprintln(a.Stdout, strings.Join(names, "\n"))
		return 0
	}
	b, ok := files[path.Clean(name)]
	if !ok {
		return a.fail(&APIError{Code: "not_found", Message: "no such skill file " + name + "; try --file list"})
	}
	a.Stdout.Write(b)
	return 0
}

func (a *App) skillTargets(p *Parsed) int {
	scope, err := scopeOf(p)
	if err != nil {
		return a.fail(err)
	}
	e := a.skillEnv()
	want, body := skillFiles(), instructionsBody()
	var rows []skillRow
	for _, t := range agentTargets {
		if t.ID == "dir" {
			continue
		}
		r := e.resolve(t, scope, "")
		row := rowOf(r)
		row.State = r.state(want, body)
		row.Reason = r.Skip
		switch {
		case !t.Verified:
			row.Note = "unverified, opt-in (--target " + t.ID + ")"
		case t.OptIn:
			row.Note = "opt-in (--target " + t.ID + ")"
		}
		if t.Note != "" {
			row.Note = strings.TrimSpace(row.Note + " " + t.Note)
		}
		rows = append(rows, row)
	}
	return a.emitRows(rows, "target", "name", "kind", "detected", "state", "path", "detail")
}

func (a *App) skillInstall(p *Parsed) int {
	scope, err := scopeOf(p)
	if err != nil {
		return a.fail(err)
	}
	e := a.skillEnv()
	sel, explicit, err := a.selectTargets(p, e, scope, "install")
	if err != nil {
		return a.fail(err)
	}
	force := p.Bool("force")
	want, body := skillFiles(), instructionsBody()
	bundle := skillBundleHash(want)
	ver := Version()
	var rows []skillRow
	exit := 0
	done := map[string]string{}
	for _, r := range sel {
		row := rowOf(r)
		if r.Skip != "" {
			row.Action, row.Reason = "skipped", r.Skip
			if !explicit && !strings.HasPrefix(r.Skip, "detected") {
				continue
			}
			rows = append(rows, row)
			continue
		}
		if r.Shared != "" {
			row.Action = done[r.Place.Path]
			rows = append(rows, row)
			continue
		}
		act, files, werr := a.installOne(r, want, body, bundle, ver, force)
		row.Action, row.Files = act, files
		if r.T.Note != "" {
			row.Note = r.T.Note
		}
		if !r.T.Verified {
			row.Note = strings.TrimSpace("unverified path. " + row.Note)
		}
		done[r.Place.Path] = act
		if werr != nil {
			row.Action, row.Reason = "error", werr.Error()
			if ae, ok := werr.(*APIError); ok && ae.Code == "conflict" {
				row.Action = "conflict"
				if exit == 0 {
					exit = ExitConflict
				}
			} else {
				exit = ExitError
			}
		}
		rows = append(rows, row)
	}
	if code := a.emitRows(rows, "target", "action", "path", "detail"); code != 0 {
		return code
	}
	return exit
}

func (a *App) installOne(r resolved, want map[string][]byte, body, bundle, ver string, force bool) (string, []string, error) {
	pl := r.Place
	dry := a.DryRun
	if pl.Fmt == fmtSkillDir {
		if err := refuseSymlink(pl.Path); err != nil {
			return "", nil, err
		}
		st, _ := inspectDir(pl.Path, want)
		files := sortedKeys(want)
		switch st {
		case stCurrent:
			return "unchanged", files, nil
		case stModified:
			if !force {
				return "", nil, &APIError{Code: "conflict", Message: pl.Path + " has local changes or another version of the skill; pass --force to overwrite"}
			}
		}
		act := map[string]string{stMissing: "created", stStale: "updated", stModified: "updated"}[st]
		if dry {
			return "would-" + strings.TrimSuffix(act, "d"), files, nil
		}
		return act, files, writeDir(pl.Path, want, 0o644, ver)
	}
	if err := refuseSymlink(pl.Path); err != nil {
		if !force {
			return "", nil, err
		}
		if res, rerr := filepath.EvalSymlinks(pl.Path); rerr == nil {
			pl.Path = res
		}
	}
	st, doc, sp := inspectFile(pl, body)
	switch st {
	case stCurrent:
		return "unchanged", nil, nil
	case stModified:
		if !force {
			return "", nil, &APIError{Code: "conflict", Message: pl.Path + " has a Weave block (or file) with local changes; pass --force to overwrite it"}
		}
	}
	act := map[string]string{stMissing: "created", stStale: "updated", stModified: "updated"}[st]
	if st == stMissing && doc != "" {
		act = "updated" // block appended to the user's existing file
	}
	if dry {
		return "would-" + strings.TrimSuffix(act, "d"), nil, nil
	}
	return act, nil, safeWriteMode(pl.Path, []byte(renderFile(pl, doc, sp, body, ver)), 0o644)
}

func safeWriteMode(p string, b []byte, perm os.FileMode) error { return safeWrite(p, b, perm) }

func (a *App) skillCheck(p *Parsed) int {
	scope, err := scopeOf(p)
	if err != nil {
		return a.fail(err)
	}
	e := a.skillEnv()
	sel, _, err := a.selectTargets(p, e, scope, "check")
	if err != nil {
		return a.fail(err)
	}
	want, body := skillFiles(), instructionsBody()
	var rows []skillRow
	bad := false
	for _, r := range sel {
		row := rowOf(r)
		row.State = r.state(want, body)
		row.Reason = r.Skip
		if r.Skip == "" && row.State != stCurrent {
			bad = true
		}
		rows = append(rows, row)
	}
	code := a.emitRows(rows, "target", "state", "path", "detail")
	if code == 0 && bad {
		code = ExitError
	}
	return code
}

func (a *App) skillUninstall(p *Parsed) int {
	scope, err := scopeOf(p)
	if err != nil {
		return a.fail(err)
	}
	e := a.skillEnv()
	sel, explicit, err := a.selectTargets(p, e, scope, "uninstall")
	if err != nil {
		return a.fail(err)
	}
	if !explicit && len(sel) == 1 && sel[0].State() == stMissing {
		sel = nil
	}
	force := p.Bool("force")
	want, body := skillFiles(), instructionsBody()
	var rows []skillRow
	exit := 0
	for _, r := range sel {
		row := rowOf(r)
		if r.Skip != "" {
			continue
		}
		if r.Shared != "" {
			row.Action = "shared"
			rows = append(rows, row)
			continue
		}
		switch {
		case a.DryRun:
			st := r.state(want, body)
			row.Action = map[string]string{stMissing: "not-installed"}[st]
			if row.Action == "" {
				row.Action = "would-remove"
			}
		case r.Place.Fmt == fmtSkillDir:
			removed, kept, ours, rerr := removeDir(r.Place.Path, want, force)
			switch {
			case rerr != nil:
				row.Action, row.Reason, exit = "error", rerr.Error(), ExitError
			case !ours:
				row.Action = "not-installed"
			case len(kept) > 0:
				row.Action, row.Files = "kept-modified", kept
			default:
				row.Action, row.Files = "removed", removed
			}
		default:
			act, rerr := removeFile(r.Place, body, force)
			if rerr != nil {
				row.Action, row.Reason, exit = "error", rerr.Error(), ExitError
			} else {
				row.Action = act
			}
		}
		if row.Action == "kept-modified" && exit == 0 {
			exit = ExitConflict
		}
		rows = append(rows, row)
	}
	if code := a.emitRows(rows, "target", "action", "path", "detail"); code != 0 {
		return code
	}
	return exit
}

func (r resolved) State() string {
	if r.Skip != "" {
		return "n/a"
	}
	return r.state(skillFiles(), instructionsBody())
}

var _ = sort.Strings
