// Package ops is the single source of truth for every Weave capability.
// Each operation is registered once; REST, the CLI and the HTMX UI are all
// driven from this registry, which guarantees feature parity by construction.
package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/realtime"
)

// Error carries an HTTP status and a stable machine code (CLI exit mapping).
type Error struct {
	Status int
	Code   string // e.g. not_found, forbidden, validation, conflict
	Msg    string
}

func (e *Error) Error() string { return e.Msg }

func NotFound(what string) *Error { return &Error{404, "not_found", what + " not found"} }
func Forbidden() *Error           { return &Error{403, "forbidden", "not allowed"} }
func Unauthorized() *Error        { return &Error{401, "unauthorized", "authentication required"} }
func Invalid(msg string) *Error   { return &Error{422, "validation", msg} }
func Conflict(msg string) *Error  { return &Error{409, "conflict", msg} }
func TooLarge(msg string) *Error  { return &Error{413, "too_large", msg} }

// Text size caps (characters for names, bytes for the description).
const (
	MaxNameLen     = 255       // list, board and label names
	MaxCardNameLen = 1000      // card names
	MaxDescBytes   = 200 << 10 // card description
)

// CheckLen returns a validation error when s has more than max characters.
func CheckLen(field, s string, max int) error {
	if len(s) > max && utf8.RuneCountInString(s) > max {
		return Invalid(fmt.Sprintf("%s is too long (max %d characters)", field, max))
	}
	return nil
}

// DefaultMaxBody caps JSON/form request bodies; ops that take uploads set Op.MaxBody.
const DefaultMaxBody int64 = 2 << 20

// multipartMemory is the in-memory threshold for multipart parsing; the rest spills to temp files.
const multipartMemory = 1 << 20

// bindErr maps a body-read error to a 413 when the size cap was hit.
func bindErr(err error, what string) *Error {
	var mb *http.MaxBytesError
	if errors.As(err, &mb) {
		return TooLarge("request body too large (limit " + fmt.Sprint(mb.Limit) + " bytes)")
	}
	return Invalid(what)
}

// Principal is the authenticated caller. Nil means anonymous.
type Principal struct {
	UserID string
	Role   string // admin | projectOwner | boardUser
	Name   string
	// IsAgent marks service accounts (AI agents) that authenticate with an API key only.
	IsAgent bool
}

// Env is passed to every handler.
type Env struct {
	Ctx  context.Context
	DB   *db.DB
	User *Principal
	Hub  *realtime.Hub
	Svc  map[string]any // shared services (mailer, storage, config…) keyed by name
	Req  *http.Request  // nil when invoked outside HTTP
	// AuthMsg lets Authenticate explain why a presented credential was refused (e.g. an expired
	// API key); it becomes the message of the resulting 401.
	AuthMsg string
}

func (e *Env) unauthorized() *Error {
	if e.AuthMsg != "" {
		return &Error{401, "unauthorized", e.AuthMsg}
	}
	return Unauthorized()
}

// File is an uploaded file for multipart ops.
type File struct {
	Name, ContentType string
	Data              []byte
}

// Op describes one operation. Input struct tags:
//
//	path:"id"   bound from the URL path   query:"q"  bound from the query string
//	json:"x"    bound from the JSON/form body   desc:"help"   shown by CLI/schema
//	required:"true"  validated before the handler runs
//
// Fields of type *ops.File are bound from multipart uploads.
type Op struct {
	Name    string // "card.create": <noun>.<verb>, unique
	Summary string
	Method  string // GET POST PATCH PUT DELETE
	Path    string // "/lists/{listId}/cards", mounted under /api/v1
	Auth    Auth
	// UI is how the browser exposes this op: a page route/partial name, or
	// "exempt:<reason>" for ops that are legitimately API/CLI only.
	UI string
	// Tags groups ops in the CLI (`weave <tag> <verb>`); defaults to Name prefix.
	Tag string

	In, Out reflect.Type
	Handler func(*Env, any) (any, error)
	Binary  bool // response is a file stream (Out must be *Stream)
	// MaxBody caps the request body in bytes (0 = DefaultMaxBody); set it only on upload/import ops.
	MaxBody int64
}

type Auth int

const (
	AuthUser   Auth = iota // any authenticated user (default)
	AuthPublic             // no auth required
	AuthAdmin              // admin role only
)

// Stream is returned by download ops.
type Stream struct {
	Name, ContentType string
	Data              []byte
}

var (
	mu       sync.RWMutex
	registry = map[string]*Op{}
)

// Register adds an op. Call from package init().
func Register[In, Out any](o Op, fn func(*Env, In) (Out, error)) {
	o.In = reflect.TypeOf((*In)(nil)).Elem()
	o.Out = reflect.TypeOf((*Out)(nil)).Elem()
	o.Handler = func(e *Env, in any) (any, error) { return fn(e, *(in.(*In))) }
	if o.Tag == "" {
		o.Tag, _, _ = strings.Cut(o.Name, ".")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[o.Name]; dup {
		panic("ops: duplicate " + o.Name)
	}
	registry[o.Name] = &o
}

func All() []*Op {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]*Op, 0, len(registry))
	for _, o := range registry {
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func Get(name string) *Op {
	mu.RLock()
	defer mu.RUnlock()
	return registry[name]
}

// Call invokes an op in-process (used by the HTMX handlers, so the UI runs
// exactly the same code path as REST and the CLI).
func Call(e *Env, name string, in any) (any, error) {
	o := Get(name)
	if o == nil {
		return nil, fmt.Errorf("unknown op %s", name)
	}
	if err := authorize(o, e); err != nil {
		return nil, err
	}
	return o.Handler(e, in)
}

// Typed is Call with a concrete output type.
func Typed[Out any](e *Env, name string, in any) (Out, error) {
	var zero Out
	// Handlers take a pointer to In; accept either In or *In.
	rv := reflect.ValueOf(in)
	if rv.Kind() != reflect.Ptr {
		p := reflect.New(rv.Type())
		p.Elem().Set(rv)
		in = p.Interface()
	}
	v, err := Call(e, name, in)
	if err != nil {
		return zero, err
	}
	out, _ := v.(Out)
	return out, nil
}

func authorize(o *Op, e *Env) error {
	switch o.Auth {
	case AuthPublic:
		return nil
	case AuthAdmin:
		if e.User == nil {
			return e.unauthorized()
		}
		if e.User.Role != "admin" {
			return Forbidden()
		}
	default:
		if e.User == nil {
			return e.unauthorized()
		}
	}
	return nil
}

// Field describes one input field for schema discovery.
type Field struct {
	Name     string `json:"name"`
	In       string `json:"in"` // path | query | body | file
	Type     string `json:"type"`
	Required bool   `json:"required"`
	Desc     string `json:"desc,omitempty"`
}

type Schema struct {
	Name    string  `json:"name"`
	Tag     string  `json:"tag"`
	Summary string  `json:"summary"`
	Method  string  `json:"method"`
	Path    string  `json:"path"`
	Auth    string  `json:"auth"`
	UI      string  `json:"ui"`
	Fields  []Field `json:"fields"`
}

func (o *Op) Schema() Schema {
	s := Schema{Name: o.Name, Tag: o.Tag, Summary: o.Summary, Method: o.Method, Path: o.Path, UI: o.UI,
		Auth: [...]string{"user", "public", "admin"}[o.Auth]}
	for i := 0; i < o.In.NumField(); i++ {
		f := o.In.Field(i)
		if !f.IsExported() {
			continue
		}
		fd := Field{Type: typeName(f.Type), Required: f.Tag.Get("required") == "true", Desc: f.Tag.Get("desc")}
		switch {
		case f.Tag.Get("path") != "":
			fd.Name, fd.In, fd.Required = f.Tag.Get("path"), "path", true
		case f.Tag.Get("query") != "":
			fd.Name, fd.In = f.Tag.Get("query"), "query"
		case f.Type == reflect.TypeOf((*File)(nil)):
			fd.Name, fd.In = jsonName(f), "file"
		default:
			fd.Name, fd.In = jsonName(f), "body"
		}
		if fd.Name == "" || fd.Name == "-" {
			continue
		}
		s.Fields = append(s.Fields, fd)
	}
	return s
}

func jsonName(f reflect.StructField) string {
	n, _, _ := strings.Cut(f.Tag.Get("json"), ",")
	if n == "" {
		n = strings.ToLower(f.Name[:1]) + f.Name[1:]
	}
	return n
}

func typeName(t reflect.Type) string {
	for t.Kind() == reflect.Ptr {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Float64:
		return "number"
	case reflect.Slice:
		return "array<" + typeName(t.Elem()) + ">"
	}
	if t == reflect.TypeOf(File{}) {
		return "file"
	}
	return "json"
}

// Authorize runs the op's auth check (no body access); serveOp calls it before Bind so
// anonymous or unprivileged callers never make the server read a body.
func (o *Op) Authorize(e *Env) error { return authorize(o, e) }

func (o *Op) bodyLimit() int64 {
	if o.MaxBody > 0 {
		return o.MaxBody
	}
	return DefaultMaxBody
}

// Bind fills the op's input from an HTTP request.
func (o *Op) Bind(r *http.Request) (any, error) {
	in := reflect.New(o.In)
	if r.Body != nil {
		r.Body = http.MaxBytesReader(nil, r.Body, o.bodyLimit())
	}
	body := map[string]json.RawMessage{}
	ct := r.Header.Get("Content-Type")
	switch {
	case strings.HasPrefix(ct, "application/json"):
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				return nil, bindErr(err, "invalid JSON body: "+err.Error())
			}
		}
	case strings.HasPrefix(ct, "multipart/form-data"):
		if err := r.ParseMultipartForm(multipartMemory); err != nil {
			return nil, bindErr(err, "invalid multipart body")
		}
	default:
		if err := r.ParseForm(); err != nil {
			var mb *http.MaxBytesError
			if errors.As(err, &mb) {
				return nil, bindErr(err, "")
			}
		}
	}
	for i := 0; i < o.In.NumField(); i++ {
		f := o.In.Field(i)
		if !f.IsExported() {
			continue
		}
		fv := in.Elem().Field(i)
		var raw string
		var have bool
		switch {
		case f.Tag.Get("path") != "":
			raw, have = r.PathValue(f.Tag.Get("path")), true
		case f.Tag.Get("query") != "":
			raw = r.URL.Query().Get(f.Tag.Get("query"))
			have = r.URL.Query().Has(f.Tag.Get("query"))
		case f.Type == reflect.TypeOf((*File)(nil)):
			if r.MultipartForm != nil {
				if fh, ok := r.MultipartForm.File[jsonName(f)]; ok && len(fh) > 0 {
					fl, err := fh[0].Open()
					if err != nil {
						return nil, Invalid("cannot read upload")
					}
					buf, err := io.ReadAll(io.LimitReader(fl, o.bodyLimit()+1))
					fl.Close()
					if err != nil {
						return nil, Invalid("cannot read upload")
					}
					fv.Set(reflect.ValueOf(&File{Name: fh[0].Filename, ContentType: fh[0].Header.Get("Content-Type"), Data: buf}))
				}
			}
			continue
		default:
			name := jsonName(f)
			if j, ok := body[name]; ok {
				if err := json.Unmarshal(j, fv.Addr().Interface()); err != nil {
					return nil, Invalid(fmt.Sprintf("field %s: %v", name, err))
				}
				continue
			}
			if r.Form != nil && r.Form.Has(name) {
				raw, have = r.Form.Get(name), true
			} else if r.MultipartForm != nil && len(r.MultipartForm.Value[name]) > 0 {
				raw, have = r.MultipartForm.Value[name][0], true
			}
		}
		if have {
			if err := setScalar(fv, raw); err != nil {
				return nil, Invalid(fmt.Sprintf("field %s: %v", f.Name, err))
			}
		}
	}
	return in.Interface(), o.validate(in.Elem())
}

func (o *Op) validate(v reflect.Value) error {
	for i := 0; i < o.In.NumField(); i++ {
		f := o.In.Field(i)
		if f.Tag.Get("required") == "true" || f.Tag.Get("path") != "" {
			if v.Field(i).IsZero() {
				return Invalid("missing required field " + o.Schema().fieldName(f))
			}
		}
	}
	return nil
}

func (s Schema) fieldName(f reflect.StructField) string {
	if p := f.Tag.Get("path"); p != "" {
		return p
	}
	return jsonName(f)
}

func setScalar(v reflect.Value, raw string) error {
	if v.Kind() == reflect.Ptr {
		nv := reflect.New(v.Type().Elem())
		if err := setScalar(nv.Elem(), raw); err != nil {
			return err
		}
		v.Set(nv)
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(raw)
	case reflect.Bool:
		v.SetBool(raw == "true" || raw == "1" || raw == "on")
	case reflect.Int, reflect.Int64, reflect.Int32:
		var n int64
		if _, err := fmt.Sscan(raw, &n); err != nil {
			return err
		}
		v.SetInt(n)
	case reflect.Float64:
		var n float64
		if _, err := fmt.Sscan(raw, &n); err != nil {
			return err
		}
		v.SetFloat(n)
	default:
		return json.Unmarshal([]byte(raw), v.Addr().Interface())
	}
	return nil
}

// Authenticate resolves the caller from a request (session cookie, Bearer token,
// or X-API-Key). Installed by the auth domain package; nil result = anonymous.
var Authenticate func(e *Env, r *http.Request) *Principal

// CallMap invokes an op in-process with input given by wire names (the path,
// query or JSON names shown by `weave schema`), so domains can compose each
// other's ops without importing each other's input types.
func CallMap(e *Env, name string, args map[string]any) (any, error) {
	o := Get(name)
	if o == nil {
		return nil, fmt.Errorf("unknown op %s", name)
	}
	in := reflect.New(o.In)
	for i := 0; i < o.In.NumField(); i++ {
		f := o.In.Field(i)
		if !f.IsExported() {
			continue
		}
		n := f.Tag.Get("path")
		if n == "" {
			n = f.Tag.Get("query")
		}
		if n == "" {
			n = jsonName(f)
		}
		v, ok := args[n]
		if !ok {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			return nil, Invalid(fmt.Sprintf("field %s: %v", n, err))
		}
		if err := json.Unmarshal(b, in.Elem().Field(i).Addr().Interface()); err != nil {
			return nil, Invalid(fmt.Sprintf("field %s: %v", n, err))
		}
	}
	return Call(e, name, in.Interface())
}
