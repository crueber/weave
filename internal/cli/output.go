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

func setPath(dst map[string]any, segs []string, val any) {
	for i, s := range segs {
		if i == len(segs)-1 {
			dst[s] = val
			return
		}
		next, _ := dst[s].(map[string]any)
		if next == nil {
			next = map[string]any{}
			dst[s] = next
		}
		dst = next
	}
}

// Project keeps only the dotted paths (a,b.c,labels.name) of a map (or of every element of an array).
func Project(v any, paths []string) any {
	switch t := v.(type) {
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = Project(e, paths)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for _, p := range paths {
			segs := splitPath(p)
			if len(segs) == 0 {
				continue
			}
			if r, ok := lookup(t, segs); ok {
				setPath(out, segs, r)
			}
		}
		return out
	}
	return v
}

// EvalJQ evaluates a minimal jq-like expression: .a.b, .items[].id, .items[0], with | pipes.
// Results are returned as a stream.
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
		var next []any
		for _, in := range stream {
			r, ok := lookup(in, splitPath(stage))
			if !ok {
				continue
			}
			if arr, isArr := r.([]any); isArr && strings.Contains(stage, "[]") {
				next = append(next, arr...)
			} else {
				next = append(next, r)
			}
		}
		stream = next
	}
	return stream, nil
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

func projectEnvelope(v any, fields []string) any {
	m, ok := v.(map[string]any)
	if !ok {
		return Project(v, fields)
	}
	out := map[string]any{}
	for k, c := range m {
		if k == "item" || k == "items" {
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
			if arr, isArr := it.([]any); isArr {
				return arr
			}
			return []any{it}
		}
		return []any{t}
	}
	return []any{v}
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
