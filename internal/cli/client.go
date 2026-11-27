package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/crueber/weave/internal/version"
)

// Exit codes (stable; documented in docs/CLI.md).
const (
	ExitOK           = 0
	ExitError        = 1
	ExitUsage        = 2
	ExitUnauthorized = 3
	ExitNotFound     = 4
	ExitForbidden    = 5
	ExitValidation   = 6
	ExitConflict     = 7
	ExitNetwork      = 8
	ExitEmpty        = 10 // `weave next` / `weave listen --timeout`: nothing to do
)

var exitFor = map[string]int{
	"usage": ExitUsage, "unauthorized": ExitUnauthorized, "not_found": ExitNotFound, "forbidden": ExitForbidden,
	"validation": ExitValidation, "conflict": ExitConflict, "network": ExitNetwork, "empty": ExitEmpty,
	"ambiguous": ExitUsage,
}

// APIError is every failure the CLI reports: a stable machine code plus a message.
type APIError struct {
	Status  int
	Code    string
	Message string
	Detail  any // optional structured detail (e.g. candidates)

	retryAfter time.Duration
}

func (e *APIError) Error() string { return e.Message }
func (e *APIError) Exit() int {
	if c, ok := exitFor[e.Code]; ok {
		return c
	}
	return ExitError
}

func usageErr(msg string) *APIError { return &APIError{Code: "usage", Message: msg} }

func codeForStatus(st int) string {
	switch st {
	case 401:
		return "unauthorized"
	case 403:
		return "forbidden"
	case 404:
		return "not_found"
	case 409:
		return "conflict"
	case 400, 422:
		return "validation"
	}
	return "error"
}

func (a *App) httpClient(timeout time.Duration) *http.Client {
	if a.HTTP != nil {
		if timeout == 0 {
			return a.HTTP
		}
		cp := *a.HTTP
		cp.Timeout = timeout
		return &cp
	}
	return &http.Client{Timeout: timeout, CheckRedirect: redirectPolicy(a.Cfg.Token != "")}
}

// Response is a raw API response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

func (r *Response) JSON() any {
	var v any
	if json.Unmarshal(r.Body, &v) != nil {
		return nil
	}
	return v
}

func (r *Response) IsJSON() bool {
	return strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
}

// Params are op inputs by field name. Values may be string, bool, number, []any, map, nil.
type Params map[string]any

// Call executes an op by schema: path fields are substituted, query/body split by field `in`,
// file fields upload multipart (value = local path). Fields unknown to the schema go to the
// query string for GET and the JSON body otherwise, so newer server fields work with an older
// schema. Non-GET calls honour --dry-run (nothing is sent).
func (a *App) Call(ctx context.Context, o *Op, params Params, files map[string]string) (*Response, error) {
	return a.call(ctx, o, params, callOpts{files: files})
}

// Upload is one multipart file part streamed from an already opened reader (the MCP server opens
// and checks the file itself; nothing here touches the filesystem).
type Upload struct {
	Field string    // the op's file field (in:"file")
	Name  string    // filename sent in the part header
	R     io.Reader // content; read once, to EOF
}

// callOpts selects how a request body is built and how a successful response is consumed.
type callOpts struct {
	files   map[string]string // CLI: field -> local path ("-" = stdin), buffered
	uploads []Upload          // streamed multipart parts (no buffering, no path handling)
	// stream, when set, receives a successful (< 400) response instead of it being buffered, so
	// downloads are not bound by the 64 MiB response cap. The returned Response has no Body.
	stream func(*http.Response) error
	noDry  bool // ignore --dry-run (the MCP server never runs in dry-run mode)
}

// CallStream runs an op with streamed uploads and/or a streamed response body.
func (a *App) CallStream(ctx context.Context, o *Op, params Params, uploads []Upload, stream func(*http.Response) error) (*Response, error) {
	return a.call(ctx, o, params, callOpts{uploads: uploads, stream: stream, noDry: true})
}

func (a *App) call(ctx context.Context, o *Op, params Params, opts callOpts) (*Response, error) {
	files := opts.files
	path := o.Path
	q := url.Values{}
	body := map[string]any{}
	known := map[string]*Field{}
	for i := range o.Fields {
		known[o.Fields[i].Name] = &o.Fields[i]
	}
	for _, k := range sortedKeys(params) {
		v := params[k]
		if v == nil {
			continue
		}
		in := "body"
		if f := known[k]; f != nil {
			in = f.In
		} else if o.Method == "GET" {
			in = "query"
		}
		switch in {
		case "path":
			path = strings.ReplaceAll(path, "{"+k+"}", url.PathEscape(fmt.Sprint(v)))
		case "query":
			q.Set(k, queryString(v))
		case "file":
			// handled via files
		default:
			if o.Method == "GET" {
				q.Set(k, queryString(v))
			} else {
				body[k] = v
			}
		}
	}
	u := strings.TrimRight(a.Cfg.URL, "/") + "/api/v1" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	if a.DryRun && o.Method != "GET" && !opts.noDry {
		dr := map[string]any{"dryRun": true, "op": o.Name, "method": o.Method, "url": u}
		if len(files) > 0 {
			dr["files"] = files
		}
		if len(body) > 0 {
			dr["body"] = body
		}
		b, _ := json.Marshal(map[string]any{"item": dr})
		return &Response{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: b}, nil
	}
	uploads := opts.uploads
	var opened []*os.File
	defer func() {
		for _, f := range opened {
			_ = f.Close()
		}
	}()
	if len(uploads) == 0 && len(files) > 0 {
		uploads = make([]Upload, 0, len(files))
		for _, k := range sortedKeys(files) {
			p := files[k]
			var r io.Reader
			name := filepath.Base(p)
			if p == "-" {
				r = a.Stdin
				name = "stdin"
			} else {
				f, err := os.Open(p)
				if err != nil {
					return nil, usageErr(err.Error())
				}
				opened = append(opened, f)
				r = f
			}
			if n, ok := params["name"].(string); ok && n != "" && len(files) == 1 {
				name = n
			}
			uploads = append(uploads, Upload{Field: k, Name: name, R: r})
		}
	}
	var rdr io.Reader
	ct := ""
	if len(uploads) > 0 {
		// stream the multipart body through a pipe: an upload may be hundreds of MiB
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		ct = mw.FormDataContentType()
		pipeDone := make(chan struct{})
		go func() {
			defer close(pipeDone)
			err := writeMultipart(mw, body, uploads)
			if err == nil {
				err = mw.Close()
			}
			pw.CloseWithError(err)
		}()
		defer func() {
			pr.Close() // unblocks the writer if the request fails before draining the body
			<-pipeDone // ensure the writer finishes reading opened files before they are closed
		}()
		rdr = pr
	} else if o.Method != "GET" {
		bb, err := json.Marshal(body)
		if err != nil {
			return nil, usageErr(err.Error())
		}
		rdr, ct = bytes.NewReader(bb), "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, o.Method, u, rdr)
	if err != nil {
		return nil, usageErr(err.Error())
	}
	if ct != "" {
		req.Header.Set("Content-Type", ct)
	}
	if a.Cfg.Token != "" {
		req.Header.Set("Authorization", "Bearer "+a.Cfg.Token)
	}
	req.Header.Set("User-Agent", "weave-cli/"+version.Version)
	req.Header.Set("Accept", "application/json")
	a.warnInsecure()
	timeout := a.Timeout
	if opts.stream != nil || len(uploads) > 0 {
		timeout = 0 // large transfers are bounded by the caller's context and byte caps instead
	}
	resp, err := a.httpClient(timeout).Do(req)
	if err != nil {
		return nil, &APIError{Code: "network", Message: err.Error()}
	}
	defer resp.Body.Close()
	if opts.stream != nil && resp.StatusCode < 400 {
		if err := opts.stream(resp); err != nil {
			return nil, err
		}
		return &Response{Status: resp.StatusCode, Header: resp.Header}, nil
	}
	b, rerr := readLimited(resp.Body)
	if rerr != nil {
		return nil, &APIError{Code: "network", Message: rerr.Error()}
	}
	r := &Response{Status: resp.StatusCode, Header: resp.Header, Body: b}
	if resp.StatusCode >= 400 {
		return r, errorFromResponse(r)
	}
	return r, nil
}

// writeMultipart writes the non-file body fields (sorted, JSON for non-strings) and then each
// upload part, copying from its reader.
func writeMultipart(mw *multipart.Writer, body map[string]any, ups []Upload) error {
	for _, k := range sortedKeys(body) {
		s, isStr := body[k].(string)
		if !isStr {
			bb, err := json.Marshal(body[k])
			if err != nil {
				return err
			}
			s = string(bb)
		}
		if err := mw.WriteField(k, s); err != nil {
			return err
		}
	}
	for _, u := range ups {
		fw, err := mw.CreateFormFile(u.Field, u.Name)
		if err != nil {
			return err
		}
		if _, err := io.Copy(fw, u.R); err != nil {
			return err
		}
	}
	return nil
}

func queryString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case []string:
		return strings.Join(t, ",")
	case []any:
		parts := make([]string, len(t))
		for i, x := range t {
			parts[i] = fmt.Sprint(x)
		}
		return strings.Join(parts, ",")
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return fmt.Sprint(v)
}

func errorFromResponse(r *Response) *APIError {
	var e struct {
		Error struct{ Code, Message string }
	}
	_ = json.Unmarshal(r.Body, &e)
	code := e.Error.Code
	if code == "" {
		code = codeForStatus(r.Status)
	}
	msg := e.Error.Message
	if msg == "" {
		msg = strings.TrimSpace(string(r.Body))
		if len(msg) > 300 || msg == "" {
			msg = fmt.Sprintf("HTTP %d", r.Status)
		}
		// A non-JSON error body did not come from Weave; name the proxy instead of dumping markup.
		if pm, ok := proxyErrorPage(r); ok {
			msg = pm
		}
	}
	return &APIError{Status: r.Status, Code: code, Message: msg}
}

// Do calls an op by name.
func (a *App) Do(ctx context.Context, name string, params Params) (*Response, error) {
	o, err := a.mustOp(ctx, name)
	if err != nil {
		return nil, err
	}
	return a.Call(ctx, o, params, nil)
}

// Item runs an op and returns the object under "item" (or the whole object).
func (a *App) Item(ctx context.Context, name string, params Params) (map[string]any, error) {
	r, err := a.Do(ctx, name, params)
	if err != nil {
		return nil, err
	}
	return itemOf(r.JSON()), nil
}

// Items runs an op and returns the array under "items".
func (a *App) Items(ctx context.Context, name string, params Params) ([]any, error) {
	r, err := a.Do(ctx, name, params)
	if err != nil {
		return nil, err
	}
	return itemsOf(r.JSON()), nil
}

func itemOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	if it, ok := m["item"].(map[string]any); ok {
		return it
	}
	return m
}

func itemsOf(v any) []any {
	switch t := v.(type) {
	case map[string]any:
		if it, ok := t["items"].([]any); ok {
			return it
		}
		// the server wraps list outputs that are structs ({"item":{"items":[...],"nextCursor":...}})
		if in, ok := t["item"].(map[string]any); ok {
			if _, has := in["items"]; has {
				return itemsOf(in)
			}
		}
	case []any:
		return t
	}
	return nil
}

func str(m map[string]any, k string) string {
	switch v := m[k].(type) {
	case string:
		return v
	case float64:
		return fmt.Sprint(v)
	}
	return ""
}
