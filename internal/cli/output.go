package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
)

// ---- tiny dotted-path projector (no external deps) ----

// splitPath splits "a.b[0].c" into segments; "[]" and "[n]" are array steps.
func splitPath(p string) []string {
	p = strings.TrimPrefix(strings.TrimSpace(p), ".")
	var segs []string
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() > 0 {
			segs = append(segs, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(p); i++ {
		switch p[i] {
		case '.':
			flush()
		case '[':
			flush()
			j := strings.IndexByte(p[i:], ']')
			if j < 0 {
				cur.WriteString(p[i:])
				i = len(p)
				break
			}
			segs = append(segs, p[i:i+j+1])
			i += j
		default:
			cur.WriteByte(p[i])
		}
	}
	flush()
	return segs
}

// lookup walks segs; arrays are mapped over implicitly for key steps. Returns (value, found).
func lookup(v any, segs []string) (any, bool) {
	if len(segs) == 0 {
		return v, true
	}
	s := segs[0]
	if strings.HasPrefix(s, "[") {
		arr, ok := v.([]any)
		if !ok {
			return nil, false
		}
		idx := s[1 : len(s)-1]
		if idx == "" {
			var out []any
			for _, e := range arr {
				if r, ok := lookup(e, segs[1:]); ok {
					out = append(out, r)
				}
			}
			return out, true
		}
		n, err := strconv.Atoi(idx)
		if err != nil {
			return nil, false
		}
		if n < 0 {
			n += len(arr)
		}
		if n < 0 || n >= len(arr) {
			return nil, false
		}
		return lookup(arr[n], segs[1:])
	}
	switch t := v.(type) {
	case map[string]any:
		c, ok := t[s]
		if !ok {
			return nil, false
		}
		return lookup(c, segs[1:])
	case []any:
		var out []any
		for _, e := range t {
			if r, ok := lookup(e, segs); ok {
				out = append(out, r)
			}
		}
		return out, true
	}
	return nil, false
}

// fieldTree is a set of --fields paths as a tree: whole means "keep this value as it is".
type fieldTree struct {
	whole bool
	kids  map[string]*fieldTree
}

func buildFieldTree(paths []string) *fieldTree {
	root := &fieldTree{}
	for _, p := range paths {
		n := root
		segs := splitPath(p)
		keys := segs[:0:0]
		for _, s := range segs {
			if !strings.HasPrefix(s, "[") { // [] and [n] are optional: arrays are always mapped over
				keys = append(keys, s)
			}
		}
		if len(keys) == 0 {
			continue
		}
		for _, k := range keys {
			if n.whole {
				break
			}
			if n.kids == nil {
				n.kids = map[string]*fieldTree{}
			}
			c := n.kids[k]
			if c == nil {
				c = &fieldTree{}
				n.kids[k] = c
			}
			n = c
		}
		n.whole, n.kids = true, nil
	}
	return root
}

func projectNode(v any, t *fieldTree) any {
	if t.whole {
		return v
	}
	switch x := v.(type) {
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = projectNode(e, t)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, sub := range t.kids {
			if c, ok := x[k]; ok {
				out[k] = projectNode(c, sub)
			}
		}
		return out
	}
	return v
}

// Project keeps only the dotted paths (a,b.c,labels.name) of a map (or of every element of an array).
// Paths are relative to each record; a path that crosses an array maps over it and keeps the array, so
// lists.cards.name (or lists[].cards[].name) keeps the nesting: {"lists":[{"cards":[{"name":..}]}]}.
func Project(v any, paths []string) any {
	return projectNode(v, buildFieldTree(paths))
}

// EvalJQ evaluates a minimal jq-like expression: .a.b, .items[].id, .items[0], with | pipes.
// Results are returned as a stream. A key that no object in the stream has is an error naming the
// available keys (a typo must not look like "no results"); keys that exist with a null value yield null.
func EvalJQ(v any, expr string) ([]any, error) {
	stream := []any{v}
	for _, stage := range strings.Split(expr, "|") {
		stage = strings.TrimSpace(stage)
		if stage == "" || stage == "." {
			continue
		}
		if !strings.HasPrefix(stage, ".") {
			return nil, usageErr("unsupported --jq expression " + strconv.Quote(stage) + " (supports .a.b[0] .items[].id and |)")
		}
		for _, seg := range splitPath(stage) {
			var err error
			if stream, err = jqStep(stream, seg); err != nil {
				return nil, usageErr("--jq " + stage + ": " + err.Error())
			}
		}
	}
	return stream, nil
}

// jqStep applies one path segment (key, [] or [n]) to every value of the stream.
func jqStep(stream []any, seg string) ([]any, error) {
	var next []any
	if strings.HasPrefix(seg, "[") {
		idx := seg[1 : len(seg)-1]
		n, nerr := strconv.Atoi(idx)
		if idx != "" && nerr != nil {
			return nil, fmt.Errorf("bad index %s", seg)
		}
		for _, in := range stream {
			switch x := in.(type) {
			case nil:
				// iterating or indexing null yields nothing
			case []any:
				if idx == "" {
					next = append(next, x...)
					continue
				}
				i := n
				if i < 0 {
					i += len(x)
				}
				if i >= 0 && i < len(x) {
					next = append(next, x[i])
				}
			default:
				return nil, fmt.Errorf("cannot use %s on %s", seg, describeJSON(in))
			}
		}
		return next, nil
	}
	found := false
	var missed map[string]any
	var key func(v any) (any, bool)
	key = func(v any) (any, bool) {
		switch x := v.(type) {
		case map[string]any:
			c, ok := x[seg]
			if !ok && missed == nil {
				missed = x
			}
			return c, ok
		case []any: // implicit map over arrays (.items.id)
			out := []any{}
			for _, e := range x {
				if r, ok := key(e); ok {
					out = append(out, r)
				}
			}
			return out, len(out) > 0 || len(x) == 0
		}
		return nil, false
	}
	for _, in := range stream {
		switch in.(type) {
		case nil:
			next = append(next, nil)
			found = true
			continue
		case map[string]any, []any:
		default:
			return nil, fmt.Errorf("cannot read key %q of %s", seg, describeJSON(in))
		}
		if r, ok := key(in); ok {
			next = append(next, r)
			found = true
		}
	}
	if !found && missed != nil {
		return nil, fmt.Errorf("no key %q; available: %s", seg, strings.Join(sortedKeys(missed), ", "))
	}
	return next, nil
}

func describeJSON(v any) string {
	switch x := v.(type) {
	case map[string]any:
		return "an object (keys: " + strings.Join(sortedKeys(x), ", ") + ")"
	case []any:
		return "an array"
	case string:
		return "a string"
	case float64:
		return "a number"
	case bool:
		return "a boolean"
	}
	return "null"
}

func scalarString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// ---- output formats ----

// OutOpts are the global output settings.
type OutOpts struct {
	Format string // json | ndjson | table | ids | quiet
	Fields []string
	JQ     string
}

func (a *App) stdoutTTY() bool {
	if f, ok := a.Stdout.(*os.File); ok {
		return isTTY(f)
	}
	return false
}

// emitResponse prints an op response according to the output options.
func (a *App) emitResponse(r *Response) error {
	if !r.IsJSON() {
		_, err := a.Stdout.Write(r.Body)
		return err
	}
	var v any
	if err := json.Unmarshal(r.Body, &v); err != nil {
		_, err = a.Stdout.Write(r.Body)
		return err
	}
	return a.emit(v, r.Body)
}

// emit prints v (a decoded {"item":..}/{"items":[..]} envelope). raw, when given, is the original
// bytes used for the untouched default json output.
func (a *App) emit(v any, raw []byte) error {
	o := a.Out
	w := a.Stdout
	if o.Format == "" {
		o.Format = "json"
	}
	if o.JQ != "" {
		res, err := EvalJQ(v, o.JQ)
		if err != nil {
			return err
		}
		for _, x := range res {
			if s, ok := x.(string); ok {
				fmt.Fprintln(w, cleanText(s, true))
				continue
			}
			b, _ := json.Marshal(x)
			fmt.Fprintln(w, string(b))
		}
		return nil
	}
	rows := rowsOf(v)
	switch o.Format {
	case "quiet":
		return nil
	case "ids":
		for _, r := range rows {
			if m, ok := r.(map[string]any); ok {
				if id := str(m, "id"); id != "" {
					fmt.Fprintln(w, id)
				}
			}
		}
		return nil
	case "ndjson":
		if len(o.Fields) > 0 {
			a.noteUnmatchedFields(rows, o.Fields)
		}
		for _, r := range rows {
			if len(o.Fields) > 0 {
				r = Project(r, o.Fields)
			}
			b, _ := json.Marshal(r)
			fmt.Fprintln(w, string(b))
		}
		return nil
	case "table":
		return a.writeTable(rows)
	}
	// json
	if len(o.Fields) > 0 {
		a.noteUnmatchedFields(rows, o.Fields)
		v = projectEnvelope(v, o.Fields)
		raw = nil
	}
	if raw != nil && !a.stdoutTTY() {
		_, err := fmt.Fprintln(w, strings.TrimRight(string(raw), "\n"))
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if a.stdoutTTY() {
		enc.SetIndent("", "  ")
	}
	_ = enc.Encode(v)
	_, err := w.Write(buf.Bytes())
	return err
}

// noteUnmatchedFields warns on stderr when no --fields path matches any record, so a typo does not
// look like an empty result. The output itself is unchanged.
func (a *App) noteUnmatchedFields(rows []any, fields []string) {
	var keys []string
	for _, r := range rows {
		m, ok := r.(map[string]any)
		if !ok {
			return
		}
		if p, _ := Project(m, fields).(map[string]any); len(p) > 0 {
			return
		}
		if keys == nil {
			keys = sortedKeys(m)
		}
	}
	if keys != nil {
		fmt.Fprintf(a.Stderr, "note: --fields %s matched nothing; available: %s\n", strings.Join(fields, ","), strings.Join(keys, ", "))
	}
}

func projectEnvelope(v any, fields []string) any {
	m, ok := v.(map[string]any)
	if !ok {
		return Project(v, fields)
	}
	out := map[string]any{}
	for k, c := range m {
		if page, ok := pagedItems(c); ok && k == "item" { // {"item":{"items":[...],"nextCursor":..}}
			cp := map[string]any{}
			for pk, pv := range c.(map[string]any) {
				cp[pk] = pv
			}
			cp["items"] = Project(page, fields)
			out[k] = cp
		} else if k == "item" || k == "items" {
			out[k] = Project(c, fields)
		} else {
			out[k] = c
		}
	}
	return out
}

// rowsOf extracts the records of an envelope: items, a single item, or the value itself.
func rowsOf(v any) []any {
	switch t := v.(type) {
	case []any:
		return t
	case map[string]any:
		if it, ok := t["items"].([]any); ok {
			return it
		}
		if it, ok := t["item"]; ok {
			if page, ok := pagedItems(it); ok {
				return page
			}
			if arr, isArr := it.([]any); isArr {
				return arr
			}
			return []any{it}
		}
		return []any{t}
	}
	return []any{v}
}

// pagedItems recognises a paged list result ({"items":[...],"nextCursor":..} inside "item").
func pagedItems(v any) ([]any, bool) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, false
	}
	arr, ok := m["items"].([]any)
	return arr, ok
}

func (a *App) writeTable(rows []any) error {
	cols := a.Out.Fields
	if len(cols) == 0 {
		cols = autoColumns(rows)
	}
	tw := tabwriter.NewWriter(a.Stdout, 0, 8, 2, ' ', 0)
	head := make([]string, len(cols))
	for i, c := range cols {
		head[i] = strings.ToUpper(c)
	}
	hdr := strings.Join(head, "\t")
	if colorOK(os.Stdout) && a.Stdout == io.Writer(os.Stdout) {
		hdr = "\x1b[1m" + hdr + "\x1b[0m"
	}
	fmt.Fprintln(tw, hdr)
	for _, r := range rows {
		cells := make([]string, len(cols))
		for i, c := range cols {
			if x, ok := lookup(r, splitPath(c)); ok {
				cells[i] = cleanText(oneLine(scalarString(x), 60), false)
			}
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

func oneLine(s string, max int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\t", " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func autoColumns(rows []any) []string {
	first, _ := func() (map[string]any, bool) {
		for _, r := range rows {
			if m, ok := r.(map[string]any); ok {
				return m, true
			}
		}
		return nil, false
	}()
	var keys []string
	for k, v := range first {
		switch v.(type) {
		case map[string]any, []any:
			continue
		}
		keys = append(keys, k)
	}
	rank := func(k string) int {
		for i, p := range []string{"id", "name", "title", "type", "listId", "claimedBy", "role", "email"} {
			if k == p {
				return i
			}
		}
		return 100
	}
	sort.Slice(keys, func(i, j int) bool {
		if ri, rj := rank(keys[i]), rank(keys[j]); ri != rj {
			return ri < rj
		}
		return keys[i] < keys[j]
	})
	if len(keys) > 7 {
		keys = keys[:7]
	}
	return keys
}

// parseFields splits "a,b.c" into paths.
func parseFields(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
