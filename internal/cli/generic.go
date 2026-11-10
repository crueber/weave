package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
)

func (a *App) runGeneric(args []string) int {
	cat := a.catalogNow(false)
	tag := args[0]
	if len(cat.Tag(tag)) == 0 {
		cat = a.catalogNow(true) // the server may know newer tags than our cache
		if len(cat.Tag(tag)) == 0 {
			return a.fail(usageErr(fmt.Sprintf("unknown command %q; run `weave help` (tags: %s)", tag, strings.Join(cat.Tags(), " "))))
		}
	}
	if len(args) == 1 || strings.HasPrefix(args[1], "-") || args[1] == "help" {
		fmt.Fprint(a.Stdout, tagHelp(cat, tag))
		if len(args) == 1 {
			return ExitUsage
		}
		return 0
	}
	verb := args[1]
	o := cat.Lookup(tag, verb)
	if o == nil {
		if c2 := a.catalogNow(true); c2.Lookup(tag, verb) != nil {
			o = c2.Lookup(tag, verb)
		}
	}
	if o == nil {
		var verbs []string
		for _, x := range cat.Tag(tag) {
			verbs = append(verbs, x.Verb())
		}
		return a.fail(usageErr(fmt.Sprintf("unknown command %s %s; verbs: %s", tag, verb, strings.Join(verbs, " "))))
	}
	return a.cmdOp(o, args[2:])
}

func fieldSpecs(o *Op) []FlagSpec {
	var specs []FlagSpec
	for _, f := range o.Fields {
		s := FlagSpec{Name: f.Name, Bool: f.Type == "bool"}
		if kindOfField(f.Name, o.Tag) != "" && f.Name != "id" && strings.HasSuffix(f.Name, "Id") {
			s.Aliases = append(s.Aliases, strings.TrimSuffix(f.Name, "Id"))
		}
		if f.Name == "assigneeUserId" {
			s.Aliases = append(s.Aliases, "assignee")
		}
		specs = append(specs, s)
	}
	return append(specs, FlagSpec{Name: "output-file", Aliases: []string{"out"}, Short: "o"})
}

func (a *App) cmdOp(o *Op, args []string) int {
	p, err := a.parse(args, fieldSpecs(o))
	if err != nil {
		if ae, ok := err.(*APIError); ok && strings.HasPrefix(ae.Message, "unknown flag") && !a.refreshed {
			// our cached schema may predate a server upgrade: refresh once and retry
			a.refreshed = true
			if o2 := a.catalogNow(true).Get(o.Name); o2 != nil && len(o2.Fields) != len(o.Fields) {
				return a.cmdOp(o2, args)
			}
		}
		return a.fail(err)
	}
	if p.Bool("help") {
		fmt.Fprint(a.Stdout, opHelp(o))
		return 0
	}
	ctx := a.ctx
	vals := map[string]any{}
	// payload: --body JSON / @file / - and --stdin
	if (p.Has("body") && o.Field("body") == nil) || (p.Bool("stdin") && o.Field("stdin") == nil) {
		src := p.Str("body")
		if p.Bool("stdin") && src == "" {
			src = "-"
		}
		raw, err := a.readValue(src)
		if err != nil {
			return a.fail(err)
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			return a.failUsage("--body must be a JSON object: " + err.Error())
		}
		for k, v := range m {
			vals[k] = v
		}
	}
	// positional path args fill unset path fields in order
	var pathFields []Field
	for _, f := range o.Fields {
		if f.In == "path" {
			pathFields = append(pathFields, f)
		}
	}
	pi := 0
	raws := map[string]string{}
	for _, f := range o.Fields {
		if p.Has(f.Name) && f.In != "file" || (f.In == "file" && p.Has(f.Name)) {
			raws[f.Name] = joinMulti(p.All(f.Name))
		}
	}
	for _, pos := range p.Pos {
		for pi < len(pathFields) && (raws[pathFields[pi].Name] != "" || vals[pathFields[pi].Name] != nil) {
			pi++
		}
		if pi >= len(pathFields) {
			return a.failUsage("unexpected argument " + strconv.Quote(pos) + " (see `weave " + o.Tag + " " + o.Verb() + " --help`)")
		}
		raws[pathFields[pi].Name] = pos
		pi++
	}
	// context for name resolution
	sc := Scope{}
	var rerr error
	if v := firstNonEmpty(raws["projectId"], p.Str("project"), os.Getenv("WEAVE_PROJECT")); v != "" && o.Tag != "project" {
		if sc.ProjectID, rerr = a.Resolve(ctx, "project", v, Scope{}); rerr != nil {
			return a.fail(rerr)
		}
		if _, ok := raws["projectId"]; ok {
			raws["projectId"] = sc.ProjectID
		}
	}
	boardRaw := firstNonEmpty(raws["boardId"], p.Str("board"), os.Getenv("WEAVE_BOARD"))
	if o.Tag == "board" && o.Field("id") != nil {
		if r, ok := raws["id"]; ok {
			boardRaw = r
		}
	}
	if boardRaw != "" {
		if sc.BoardID, rerr = a.Resolve(ctx, "board", boardRaw, Scope{ProjectID: sc.ProjectID}); rerr != nil {
			return a.fail(rerr)
		}
	}
	if o.Tag == "card" && looksLikeID(raws["id"]) {
		sc.CardID = raws["id"]
	}
	for _, f := range o.Fields {
		raw, ok := raws[f.Name]
		if !ok {
			continue
		}
		if f.In == "file" {
			vals[f.Name] = raw
			continue
		}
		if kind := kindOfField(f.Name, o.Tag); kind != "" && f.Type == "string" {
			if f.Name == "boardId" || (f.Name == "id" && o.Tag == "board") {
				raw = sc.BoardID
			} else if kind == "card" && f.Name == "id" && !looksLikeID(raw) {
				id, err := a.Resolve(ctx, "card", raw, sc)
				if err != nil {
					return a.fail(err)
				}
				raw = id
			} else if !(kind == "project" && sc.ProjectID != "" && f.Name == "projectId") {
				id, err := a.Resolve(ctx, kind, raw, sc)
				if err != nil {
					return a.fail(err)
				}
				raw = id
			}
		} else if f.Type == "string" || f.Type == "json" || strings.HasPrefix(f.Type, "array") || f.Type == "number" {
			if raw, err = a.readValueMaybe(raw); err != nil {
				return a.fail(err)
			}
		}
		vals[f.Name] = parseValue(f.Type, raw)
	}
	var files map[string]string
	for _, f := range o.Fields {
		if f.Required && vals[f.Name] == nil {
			return a.failUsage(fmt.Sprintf("missing %s (%s); see `weave %s %s --help`", flagHint(&f), orDash(f.Desc, f.Type), o.Tag, o.Verb()))
		}
		if f.In == "file" {
			if v, ok := vals[f.Name].(string); ok {
				if files == nil {
					files = map[string]string{}
				}
				files[f.Name] = v
				delete(vals, f.Name)
			}
		}
	}
	params := Params(vals)
	if (p.Has("limit") && o.Field("limit") == nil) || (p.Has("cursor") && o.Field("cursor") == nil) || p.Bool("all") {
		// handled by pagination below
	} else {
		if p.Has("limit") {
			params["limit"] = parseValue("number", p.Str("limit"))
		}
		if p.Has("cursor") {
			params["cursor"] = p.Str("cursor")
		}
	}
	if isDestructive(o) && !a.Yes && !a.DryRun && a.stdinTTY() {
		if !a.confirm(fmt.Sprintf("Run %s %v?", o.Name, redactParams(params))) {
			return a.failUsage("aborted (pass --yes to skip the prompt)")
		}
	}
	limit := 0
	if p.Has("limit") {
		limit, _ = strconv.Atoi(p.Str("limit"))
	}
	resp, err := a.fetchPaged(ctx, o, params, files, limit, p.Bool("all"))
	if err != nil {
		return a.fail(err)
	}
	return a.finish(resp, p.Str("output-file"))
}

func joinMulti(v []string) string {
	if len(v) == 1 {
		return v[0]
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func flagHint(f *Field) string {
	if f.In == "path" {
		return "<" + f.Name + "> (or --" + f.Name + ")"
	}
	return "--" + f.Name
}

func orDash(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// finish writes the response: to --output-file, or to stdout per the output options.
func (a *App) finish(resp *Response, outFile string) int {
	if outFile != "" {
		if err := atomicWrite(outFile, resp.Body, 0o644); err != nil {
			return a.fail(&APIError{Code: "io", Message: err.Error()})
		}
		_ = a.emit(map[string]any{"item": map[string]any{"saved": outFile, "bytes": len(resp.Body)}}, nil)
		return 0
	}
	if resp.Status == 204 || len(resp.Body) == 0 {
		return 0
	}
	if !resp.IsJSON() && a.stdoutTTY() {
		return a.failUsage("binary response; use -o FILE to save it")
	}
	if err := a.emitResponse(resp); err != nil {
		return a.fail(err)
	}
	return 0
}

// ---- pagination ----

func nextCursorOf(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	for _, k := range []string{"nextCursor", "next_cursor", "cursor", "next"} {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	if pg, ok := m["page"].(map[string]any); ok {
		for _, k := range []string{"nextCursor", "next", "cursor"} {
			if s, ok := pg[k].(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// fetchPaged calls an op once, or (all=true) follows the server's cursor until exhausted and
// merges every page into one {"items":[...]} response. limit is the page size when the op has a
// `limit` field, otherwise a client-side cap.
func (a *App) fetchPaged(ctx context.Context, o *Op, params Params, files map[string]string, limit int, all bool) (*Response, error) {
	paged := o.Field("limit") != nil && o.Field("cursor") != nil
	if limit > 0 && o.Field("limit") != nil {
		params["limit"] = float64(limit)
	}
	if !all {
		r, err := a.Call(ctx, o, params, files)
		if err != nil {
			return r, err
		}
		if limit > 0 && o.Field("limit") == nil {
			r = truncateItems(r, limit)
		}
		return r, nil
	}
	if !paged {
		r, err := a.Call(ctx, o, params, files)
		if err == nil && limit > 0 {
			r = truncateItems(r, limit)
		}
		return r, err
	}
	if limit == 0 {
		params["limit"] = float64(100)
	}
	var items []any
	seen := map[string]bool{}
	var last *Response
	for page := 0; page < 100000; page++ {
		r, err := a.Call(ctx, o, params, files)
		if err != nil {
			return r, err
		}
		last = r
		v := r.JSON()
		items = append(items, itemsOf(v)...)
		next := nextCursorOf(v)
		if next == "" || seen[next] || (limit > 0 && len(items) >= limit) {
			break
		}
		seen[next] = true
		params["cursor"] = next
	}
	if limit > 0 && len(items) > limit {
		items = items[:limit]
	}
	b, _ := json.Marshal(map[string]any{"items": items})
	return &Response{Status: 200, Header: last.Header, Body: b}, nil
}

func truncateItems(r *Response, n int) *Response {
	var m map[string]any
	if json.Unmarshal(r.Body, &m) != nil {
		return r
	}
	if it, ok := m["items"].([]any); ok && len(it) > n {
		m["items"] = it[:n]
		b, _ := json.Marshal(m)
		return &Response{Status: r.Status, Header: r.Header, Body: b}
	}
	return r
}

// ---- value helpers ----

func parseValue(typ, v string) any {
	switch {
	case typ == "bool":
		return v == "true" || v == "1"
	case typ == "number":
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
		return v
	case typ == "string":
		return v
	case strings.HasPrefix(typ, "array"):
		var j []any
		if json.Unmarshal([]byte(v), &j) == nil {
			return j
		}
		var out []any
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	var j any
	if json.Unmarshal([]byte(v), &j) == nil {
		return j
	}
	return v
}

// readValue reads "-" (stdin) or "@path" (file); anything else is returned as is.
func (a *App) readValue(src string) (string, error) {
	switch {
	case src == "-":
		b, err := io.ReadAll(a.Stdin)
		if err != nil {
			return "", usageErr("reading stdin: " + err.Error())
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	case strings.HasPrefix(src, "@") && len(src) > 1:
		b, err := os.ReadFile(src[1:])
		if err != nil {
			return "", usageErr(err.Error())
		}
		return string(b), nil
	}
	return src, nil
}

// readValueMaybe expands "-" and "@file" but leaves "@name" alone when no such file exists.
func (a *App) readValueMaybe(src string) (string, error) {
	if strings.HasPrefix(src, "@") && len(src) > 1 {
		if _, err := os.Stat(src[1:]); err != nil {
			return src, nil
		}
	}
	return a.readValue(src)
}

func isDestructive(o *Op) bool {
	if o.Method == "DELETE" {
		return true
	}
	switch o.Verb() {
	case "clear", "reset", "revoke", "rotateKey", "purge", "delete", "revoke_others":
		return true
	}
	return false
}

func (a *App) confirm(q string) bool {
	fmt.Fprintf(a.Stderr, "%s [y/N] ", q)
	line, _ := bufio.NewReader(a.Stdin).ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "y" || line == "yes"
}

func redactParams(p Params) string {
	keys := make([]string, 0, len(p))
	for k := range p {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		s := scalarString(p[k])
		if len(s) > 40 {
			s = s[:40] + "..."
		}
		parts = append(parts, k+"="+s)
	}
	return strings.Join(parts, " ")
}

// ---- help generated from the schema ----

func placeholder(f Field) string {
	switch f.Type {
	case "bool":
		return "true"
	case "number":
		return "1"
	}
	switch strings.ToLower(f.Name) {
	case "name":
		return `"Example"`
	case "text":
		return `"text"`
	}
	if k := kindOfField(f.Name, ""); k != "" || strings.HasSuffix(f.Name, "Id") || f.Name == "id" {
		return "<" + f.Name + ">"
	}
	return "<" + f.Name + ">"
}

func opHelp(o *Op) string {
	var sb strings.Builder
	var pos []string
	for _, f := range o.Fields {
		if f.In == "path" {
			pos = append(pos, "<"+f.Name+">")
		}
	}
	fmt.Fprintf(&sb, "weave %s %s %s [flags]\n\n%s\n", o.Tag, o.Verb(), strings.Join(pos, " "), o.Summary)
	fmt.Fprintf(&sb, "\nOp: %s   %s %s   auth: %s\n", o.Name, o.Method, o.Path, o.Auth)
	if len(o.Fields) > 0 {
		sb.WriteString("\nFields (flags; path fields may also be positional):\n")
		for _, f := range o.Fields {
			req := ""
			if f.Required {
				req = " (required)"
			}
			fmt.Fprintf(&sb, "  --%-18s %-8s%s %s\n", f.Name, f.Type, req, f.Desc)
		}
	}
	sb.WriteString("\nExample:\n  " + exampleFor(o) + "\n")
	sb.WriteString(globalFlagHelp)
	return sb.String()
}

func exampleFor(o *Op) string {
	parts := []string{"weave", o.Tag, o.Verb()}
	for _, f := range o.Fields {
		if f.In == "path" {
			parts = append(parts, placeholder(f))
		}
	}
	for _, f := range o.Fields {
		if f.Required && f.In != "path" {
			parts = append(parts, "--"+f.Name, placeholder(f))
		}
	}
	return strings.Join(parts, " ")
}

const globalFlagHelp = `
Global flags: --profile NAME  --url U  --token K  --output json|ndjson|table|ids|quiet  --fields a,b.c
  --jq '.item.id'  --limit N --cursor C --all  --dry-run  --yes  --timeout 30s  --body @file.json|-  --stdin
  -o/--output-file PATH   Names work wherever an id is expected: --board "Launch plan" --list Backlog.
`

func tagHelp(cat *Catalog, tag string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "weave %s <verb> ...\n\n", tag)
	for _, o := range cat.Tag(tag) {
		fmt.Fprintf(&sb, "  %-22s %s\n", o.Verb(), o.Summary)
	}
	sb.WriteString("\nweave " + tag + " <verb> --help for fields and an example.\n")
	return sb.String()
}
