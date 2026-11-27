package cli

// File guards for the MCP server's uploads and downloads. The MCP server runs as the user, so a
// tool call naming a local path must not become a way to read ~/.ssh/id_ed25519 or write into
// .git/hooks: paths are confined to allowed roots, a deny list applies even inside the roots,
// symlinks are resolved before the checks, files are opened through os.Root, only regular files
// are read, and every read and write is capped.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const mib = int64(1) << 20

// guardEnv is the environment the guard is built from (injectable for tests).
type guardEnv struct {
	GOOS      string
	Cwd       string
	Home      string
	Temp      string
	ConfigDir string
	CacheDir  string
	Getenv    func(string) string
}

func realGuardEnv() guardEnv {
	cwd, _ := os.Getwd()
	home, _ := os.UserHomeDir()
	return guardEnv{GOOS: goos(), Cwd: cwd, Home: home, Temp: os.TempDir(), ConfigDir: configDir(), CacheDir: cacheDir(), Getenv: os.Getenv}
}

type fileGuard struct {
	goos      string
	cwd       string
	temp      string
	roots     []string // symlink-resolved, cleaned
	rootsDesc string
	deny      []string // resolved Weave config/cache dirs

	maxUpload   int64 // 0 = the op's own cap
	maxBase64   int64
	maxDownload int64 // 0 = unlimited
	inlineImage int64

	afterStat func() // test hook: runs between the checks and the open
}

func envMB(get func(string) string, key string, def int64) int64 {
	if get == nil {
		return def * mib
	}
	v := strings.TrimSpace(get(key))
	if v == "" {
		return def * mib
	}
	n, err := strconv.ParseFloat(v, 64)
	if err != nil || n < 0 {
		return def * mib
	}
	return int64(n * float64(mib))
}

func newFileGuard(e guardEnv) *fileGuard {
	get := e.Getenv
	if get == nil {
		get = func(string) string { return "" }
	}
	g := &fileGuard{goos: e.GOOS, cwd: e.Cwd, temp: e.Temp,
		maxUpload: envMB(get, "WEAVE_MCP_MAX_UPLOAD_MB", 256), maxBase64: envMB(get, "WEAVE_MCP_MAX_BASE64_MB", 2),
		maxDownload: envMB(get, "WEAVE_MCP_MAX_DOWNLOAD_MB", 1024), inlineImage: envMB(get, "WEAVE_MCP_INLINE_IMAGE_MB", 1)}
	resolve := func(p string) string {
		if p == "" {
			return ""
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(e.Cwd, p)
		}
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Clean(r)
		}
		return filepath.Clean(p)
	}
	var descs []string
	if v := get("WEAVE_MCP_UPLOAD_ROOTS"); strings.TrimSpace(v) != "" {
		for _, p := range filepath.SplitList(v) {
			if p = strings.TrimSpace(p); p != "" {
				if r := resolve(p); r != "" {
					g.roots = append(g.roots, r)
					descs = append(descs, r)
				}
			}
		}
		g.rootsDesc = "WEAVE_MCP_UPLOAD_ROOTS=" + strings.Join(descs, string(filepath.ListSeparator))
	} else {
		cwd := resolve(e.Cwd)
		home := resolve(e.Home)
		if cwd != "" && !g.same(cwd, home) && !isFSRoot(cwd) {
			g.roots = append(g.roots, cwd)
			descs = append(descs, "cwd="+cwd)
		} else {
			descs = append(descs, "cwd not allowed: it is the home or root directory")
		}
		if t := resolve(e.Temp); t != "" {
			g.roots = append(g.roots, t)
			descs = append(descs, "temp="+t)
		}
		g.rootsDesc = strings.Join(descs, ", ")
	}
	for _, d := range []string{e.ConfigDir, e.CacheDir} {
		if r := resolve(d); r != "" {
			g.deny = append(g.deny, r)
		}
	}
	return g
}

func isFSRoot(p string) bool { return filepath.Dir(p) == p }

func (g *fileGuard) ci() bool { return g.goos == "windows" || g.goos == "darwin" }

func (g *fileGuard) same(a, b string) bool {
	if g.ci() {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// within reports whether p is root or below it, on a path-component boundary.
func (g *fileGuard) within(root, p string) bool {
	if g.same(root, p) {
		return true
	}
	prefix := root
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	if len(p) < len(prefix) {
		return false
	}
	return g.same(p[:len(prefix)], prefix)
}

func (g *fileGuard) rootFor(p string) string {
	for _, r := range g.roots {
		if g.within(r, p) {
			return r
		}
	}
	return ""
}

var (
	denyDirNames  = []string{".ssh", ".gnupg", ".aws", ".azure", ".kube", ".docker", ".git"}
	denyFileNames = []string{".netrc", ".git-credentials", ".npmrc", ".pypirc"}
)

// denied reports whether p is a credential or config path (checked on the lexical and the
// symlink-resolved path, inside the roots too).
func (g *fileGuard) denied(p string) bool {
	for _, d := range g.deny {
		if g.within(d, p) {
			return true
		}
	}
	norm := p
	if g.goos == "windows" {
		norm = strings.ReplaceAll(norm, "/", `\`)
	}
	sep := string(filepath.Separator)
	if g.goos == "windows" {
		sep = `\`
	}
	parts := strings.Split(norm, sep)
	for i, c := range parts {
		lc := c
		if g.ci() {
			lc = strings.ToLower(c)
		}
		for _, d := range denyDirNames {
			if lc == d {
				return true
			}
		}
		if lc == ".config" && i+1 < len(parts) {
			nx := parts[i+1]
			if g.ci() {
				nx = strings.ToLower(nx)
			}
			if nx == "gcloud" || nx == "gh" {
				return true
			}
		}
	}
	base := parts[len(parts)-1]
	if g.ci() {
		base = strings.ToLower(base)
	}
	if strings.HasPrefix(base, ".env") || strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || strings.HasPrefix(base, "id_") {
		return true
	}
	for _, f := range denyFileNames {
		if base == f {
			return true
		}
	}
	return false
}

// guardErr is a refusal returned to the agent as a tool error with code.
type guardErr struct{ code, msg string }

func (e *guardErr) Error() string { return e.msg }

func refuse(code, format string, a ...any) error {
	return &guardErr{code: code, msg: fmt.Sprintf(format, a...)}
}

func (g *fileGuard) outside(p string) error {
	return refuse("forbidden", "%s is outside the allowed upload roots (%s). Copy the file into one of them, or ask the user to add its directory to WEAVE_MCP_UPLOAD_ROOTS", p, g.rootsDesc)
}

// absOf validates the syntax of a user path and makes it absolute against the server's cwd.
func (g *fileGuard) absOf(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", refuse("validation", "empty path")
	}
	if p == "-" {
		return "", refuse("validation", `"-" (stdin) is not allowed: stdin is the MCP protocol pipe. Pass a file path or contentBase64`)
	}
	if strings.ContainsRune(p, 0) {
		return "", refuse("validation", "path contains a NUL byte")
	}
	if g.goos == "windows" {
		if msg := windowsPathProblem(p); msg != "" {
			return "", refuse("forbidden", "%s", msg)
		}
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(g.cwd, p)
	}
	return filepath.Clean(p), nil
}

// windowsPathProblem rejects UNC and device paths, alternate data streams and reserved device
// names. It is pure string logic so it is tested on every OS.
func windowsPathProblem(p string) string {
	s := strings.ReplaceAll(p, "/", `\`)
	if strings.HasPrefix(s, `\\`) {
		return "UNC and device paths (\\\\server\\share, \\\\?\\, \\\\.\\) are not allowed"
	}
	rest := s
	if len(rest) >= 2 && rest[1] == ':' && ((rest[0] >= 'a' && rest[0] <= 'z') || (rest[0] >= 'A' && rest[0] <= 'Z')) {
		rest = rest[2:]
	}
	if strings.Contains(rest, ":") {
		return "alternate data streams (file:stream) are not allowed"
	}
	for _, c := range strings.Split(rest, `\`) {
		name := strings.ToUpper(strings.TrimRight(c, ". "))
		if i := strings.IndexByte(name, '.'); i >= 0 {
			name = name[:i]
		}
		switch name {
		case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
			return "reserved device name " + c + " is not allowed"
		}
		if len(name) == 4 && (strings.HasPrefix(name, "COM") || strings.HasPrefix(name, "LPT")) && name[3] >= '0' && name[3] <= '9' {
			return "reserved device name " + c + " is not allowed"
		}
	}
	return ""
}

// guardedFile is an opened, checked upload source.
type guardedFile struct {
	f    *os.File
	root *os.Root
	Path string // resolved path
	Name string // base name
	Size int64
}

func (gf *guardedFile) Close() {
	gf.f.Close()
	gf.root.Close()
}

func errCause(err error) error {
	if u := errors.Unwrap(err); u != nil {
		return u
	}
	return err
}

// openUpload resolves, checks and opens p for reading at most limit bytes.
func (g *fileGuard) openUpload(p string, limit int64) (*guardedFile, error) {
	abs, err := g.absOf(p)
	if err != nil {
		return nil, err
	}
	if g.denied(abs) {
		return nil, refuse("forbidden", "refused: %s is a credential/config path", p)
	}
	res, err := filepath.EvalSymlinks(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, refuse("not_found", "no such file: %s", p)
		}
		return nil, refuse("validation", "cannot resolve %s: %v", p, errCause(err))
	}
	res = filepath.Clean(res)
	if g.denied(res) {
		return nil, refuse("forbidden", "refused: %s resolves to a credential/config path", p)
	}
	root := g.rootFor(res)
	if root == "" {
		return nil, g.outside(res)
	}
	rel, err := filepath.Rel(root, res)
	if err != nil || rel == "." {
		return nil, refuse("validation", "%s is a directory, not a file", p)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, refuse("validation", "cannot open root %s: %v", root, err)
	}
	st, err := r.Lstat(rel)
	if err != nil {
		r.Close()
		return nil, refuse("not_found", "no such file: %s", p)
	}
	switch {
	case st.IsDir():
		r.Close()
		return nil, refuse("validation", "%s is a directory, not a file", p)
	case !st.Mode().IsRegular():
		r.Close()
		return nil, refuse("validation", "%s is not a regular file (devices, FIFOs, sockets and symlink races are refused)", p)
	case limit > 0 && st.Size() > limit:
		r.Close()
		return nil, refuse("validation", "%s is %d bytes, over the upload limit of %d bytes (WEAVE_MCP_MAX_UPLOAD_MB / the op's cap)", p, st.Size(), limit)
	}
	if g.afterStat != nil {
		g.afterStat()
	}
	f, err := r.OpenFile(rel, os.O_RDONLY|openNoBlock|openNoFollow, 0)
	if err != nil {
		r.Close()
		return nil, refuse("validation", "cannot open %s: %v", p, errCause(err))
	}
	fst, err := f.Stat()
	if err != nil || !fst.Mode().IsRegular() || !os.SameFile(st, fst) {
		f.Close()
		r.Close()
		return nil, refuse("forbidden", "%s changed while it was being opened; refused", p)
	}
	return &guardedFile{f: f, root: r, Path: res, Name: filepath.Base(res), Size: fst.Size()}, nil
}

// capReader reads at most limit bytes and fails (instead of silently truncating) when the source
// has more, e.g. a file that grew after it was checked. It hashes and counts what passes.
type capReader struct {
	r     io.Reader
	limit int64
	n     int64
	h     hash.Hash
	err   error
}

func newCapReader(r io.Reader, limit int64) *capReader {
	if limit <= 0 {
		limit = 1<<62 - 1
	}
	return &capReader{r: io.LimitReader(r, limit+1), limit: limit, h: sha256.New()}
}

func (c *capReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if c.n+int64(n) > c.limit {
		n = int(c.limit - c.n)
		c.err = refuse("validation", "the file is larger than the upload limit of %d bytes (it may have grown after it was checked)", c.limit)
		c.h.Write(p[:n])
		c.n += int64(n)
		return n, c.err
	}
	c.h.Write(p[:n])
	c.n += int64(n)
	return n, err
}

func (c *capReader) Sum() string { return hex.EncodeToString(c.h.Sum(nil)) }

// openSave checks a download target (explicit saveTo) and creates it: no overwrite unless asked,
// never through a symlink, never executable.
func (g *fileGuard) openSave(p string, overwrite bool) (*os.File, *os.Root, string, error) {
	abs, err := g.absOf(p)
	if err != nil {
		return nil, nil, "", err
	}
	base := filepath.Base(abs)
	if base == "." || base == ".." || base == string(filepath.Separator) {
		return nil, nil, "", refuse("validation", "saveTo must name a file")
	}
	if g.denied(abs) {
		return nil, nil, "", refuse("forbidden", "refused: %s is a credential/config path", p)
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(abs))
	if err != nil {
		return nil, nil, "", refuse("not_found", "directory %s does not exist", filepath.Dir(abs))
	}
	target := filepath.Join(filepath.Clean(dir), base)
	if g.denied(target) {
		return nil, nil, "", refuse("forbidden", "refused: %s resolves to a credential/config path", p)
	}
	root := g.rootFor(target)
	if root == "" {
		return nil, nil, "", refuse("forbidden", "%s is outside the allowed roots (%s). Save into one of them, or ask the user to add the directory to WEAVE_MCP_UPLOAD_ROOTS", target, g.rootsDesc)
	}
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == "." {
		return nil, nil, "", refuse("validation", "saveTo must name a file")
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, nil, "", refuse("validation", "cannot open root %s: %v", root, err)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL | openNoFollow
	var existing os.FileInfo
	if st, err := r.Lstat(rel); err == nil {
		switch {
		case st.Mode()&os.ModeSymlink != 0:
			r.Close()
			return nil, nil, "", refuse("forbidden", "%s is a symlink; refusing to write through it", target)
		case !st.Mode().IsRegular():
			r.Close()
			return nil, nil, "", refuse("validation", "%s exists and is not a regular file", target)
		case !overwrite:
			r.Close()
			return nil, nil, "", refuse("conflict", "%s already exists; pass overwrite:true to replace it", target)
		}
		existing = st
		flags = os.O_WRONLY | openNoFollow | openNoBlock
	}
	if g.afterStat != nil {
		g.afterStat()
	}
	f, err := r.OpenFile(rel, flags, 0o644)
	if err != nil {
		r.Close()
		return nil, nil, "", refuse("conflict", "cannot create %s: %v", target, errCause(err))
	}
	if existing != nil {
		fst, err := f.Stat()
		if err != nil || !fst.Mode().IsRegular() || !os.SameFile(existing, fst) {
			f.Close()
			r.Close()
			return nil, nil, "", refuse("forbidden", "%s changed while it was being opened; refused", target)
		}
		if err := f.Truncate(0); err != nil {
			f.Close()
			r.Close()
			return nil, nil, "", refuse("error", "cannot truncate %s: %v", target, errCause(err))
		}
	}
	_ = f.Chmod(0o644) // an overwritten file never keeps an executable bit
	return f, r, target, nil
}
