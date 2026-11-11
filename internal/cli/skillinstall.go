package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The installer engine. A "place" is one concrete location Weave may write (a skill folder or a
// file that holds a managed block). Everything Weave writes is recorded or marked so that check
// can detect stale installs, install never clobbers user edits and uninstall removes only ours.

const (
	blockBeginPrefix = "<!-- BEGIN weave-skill"
	blockEnd         = "<!-- END weave-skill -->"
	// markers written by earlier releases (treated as ours and stale)
	legacyBegin  = "<!-- weave-skill:begin (managed by `weave skill install`) -->"
	legacyEnd    = "<!-- weave-skill:end -->"
	manifestName = ".weave-skill.json"
)

// place states
const (
	stMissing  = "missing"  // nothing of ours is there
	stCurrent  = "current"  // identical to what this binary would write
	stStale    = "stale"    // ours, unmodified, but from another version: safe to update
	stModified = "modified" // differs and was edited (or is not ours): needs --force
)

type place struct {
	Fmt   skillFmt
	Path  string // skill dir (fmtSkillDir) or file path
	Front string // frontmatter for owned files
}

// contentHash is the short identity of a rendering.
func contentHash(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// ---------- skill folders ----------

type manifest struct {
	Weave string            `json:"weave"`
	Hash  string            `json:"hash"`
	Files map[string]string `json:"files"` // relative path -> sha256 of what we wrote
}

func readManifest(dir string) (*manifest, bool) {
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, false
	}
	var m manifest
	if json.Unmarshal(b, &m) != nil || m.Files == nil {
		return nil, false
	}
	return &m, true
}

func skillBundleHash(files map[string][]byte) string {
	names := sortedKeys(files)
	var parts [][]byte
	for _, n := range names {
		parts = append(parts, []byte(n), files[n])
	}
	return contentHash(parts...)
}

func inspectDir(dir string, want map[string][]byte) (string, []string) {
	present, equal := 0, 0
	for rel, content := range want {
		b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			continue
		}
		present++
		if bytes.Equal(b, content) {
			equal++
		}
	}
	if present == 0 {
		return stMissing, nil
	}
	if equal == len(want) {
		return stCurrent, nil
	}
	if m, ok := readManifest(dir); ok {
		// ours if every file we recorded is still byte-identical to what we wrote
		for rel, h := range m.Files {
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
			if err == nil && sha(b) != h {
				return stModified, []string{rel}
			}
		}
		return stStale, nil
	}
	return stModified, nil
}

func writeDir(dir string, want map[string][]byte, perm os.FileMode, version string) error {
	for _, rel := range sortedKeys(want) {
		if err := safeWrite(filepath.Join(dir, filepath.FromSlash(rel)), want[rel], perm); err != nil {
			return err
		}
	}
	m := manifest{Weave: version, Hash: skillBundleHash(want), Files: map[string]string{}}
	for rel, c := range want {
		m.Files[rel] = sha(c)
	}
	b, _ := json.MarshalIndent(m, "", " ")
	return safeWrite(filepath.Join(dir, manifestName), append(b, '\n'), perm)
}

// removeDir deletes what we wrote. It returns the files it kept (user-modified) and whether the
// folder was ours at all.
func removeDir(dir string, want map[string][]byte, force bool) (removed, kept []string, ours bool, err error) {
	m, hasM := readManifest(dir)
	recorded := map[string]string{}
	if hasM {
		recorded = m.Files
	} else if st, _ := inspectDir(dir, want); st == stCurrent {
		for rel, c := range want {
			recorded[rel] = sha(c) // legacy install without manifest, byte-identical
		}
	}
	if len(recorded) == 0 && !force {
		return nil, nil, false, nil
	}
	if len(recorded) == 0 {
		for rel, c := range want {
			recorded[rel] = sha(c)
		}
	}
	for _, rel := range sortedKeys(recorded) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := refuseSymlink(p); err != nil {
			return removed, kept, true, err
		}
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			continue
		}
		if sha(b) != recorded[rel] && !force {
			kept = append(kept, rel)
			continue
		}
		if err := os.Remove(p); err != nil {
			return removed, kept, true, err
		}
		removed = append(removed, rel)
	}
	if len(kept) == 0 {
		_ = os.Remove(filepath.Join(dir, manifestName))
	}
	// prune now-empty directories (deepest first); never anything with other content
	var dirs []string
	for rel := range recorded {
		for d := filepath.Dir(filepath.FromSlash(rel)); d != "." && d != ""; d = filepath.Dir(d) {
			dirs = append(dirs, d)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i]) > len(dirs[j]) })
	for _, d := range dirs {
		_ = os.Remove(filepath.Join(dir, d))
	}
	_ = os.Remove(dir)
	return removed, kept, true, nil
}

// ---------- managed blocks ----------

// blockSpan locates our block in doc: [start,end) of the whole block including markers, the
// recorded hash ("" for legacy blocks) and the body.
type blockSpan struct {
	start, end int
	body       string
	recorded   string
	legacy     bool
	found      bool
}

func findBlock(doc string) blockSpan {
	if i := strings.Index(doc, blockBeginPrefix); i >= 0 {
		lineEnd := strings.Index(doc[i:], "-->")
		if lineEnd >= 0 {
			head := doc[i : i+lineEnd+3]
			bodyStart := i + lineEnd + 3
			if j := strings.Index(doc[bodyStart:], blockEnd); j >= 0 {
				endAbs := bodyStart + j + len(blockEnd)
				rec := ""
				if k := strings.Index(head, "sha:"); k >= 0 {
					rec = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(head[k+4:]), "-->"))
				}
				return blockSpan{start: i, end: endAbs, body: strings.Trim(doc[bodyStart:bodyStart+j], "\n"), recorded: rec, found: true}
			}
		}
	}
	if i := strings.Index(doc, legacyBegin); i >= 0 {
		if j := strings.Index(doc[i:], legacyEnd); j > 0 {
			return blockSpan{start: i, end: i + j + len(legacyEnd), legacy: true, found: true,
				body: strings.Trim(doc[i+len(legacyBegin):i+j], "\n")}
		}
	}
	return blockSpan{}
}

func renderBlock(body, version, hash string) string {
	return fmt.Sprintf("%s v%s sha:%s -->\n%s\n%s\n", blockBeginPrefix, version, hash, strings.Trim(body, "\n"), blockEnd)
}

// splice replaces the block in doc (or appends it) preserving every other byte.
func splice(doc string, sp blockSpan, block string) string {
	if sp.found {
		tail := doc[sp.end:]
		tail = strings.TrimPrefix(tail, "\r\n")
		tail = strings.TrimPrefix(tail, "\n")
		return doc[:sp.start] + block + tail
	}
	if doc != "" && !strings.HasSuffix(doc, "\n") {
		doc += "\n"
	}
	if doc != "" {
		doc += "\n"
	}
	return doc + block
}

// inspectFile classifies a block-bearing file. body is the intended block body; prefix is the
// intended frontmatter of an owned file (hashed together with the body).
func inspectFile(p place, body string) (state string, doc string, sp blockSpan) {
	b, err := os.ReadFile(p.Path)
	if err != nil {
		return stMissing, "", blockSpan{}
	}
	doc = string(b)
	sp = findBlock(doc)
	if !sp.found {
		if p.Fmt == fmtOwnedMD && strings.TrimSpace(doc) != "" {
			return stModified, doc, sp // a file of the same name that is not ours
		}
		return stMissing, doc, sp
	}
	want := fileHash(p, body)
	have := actualHash(p, doc, sp)
	switch {
	case sp.legacy:
		return stStale, doc, sp
	case have == want:
		return stCurrent, doc, sp
	case sp.recorded == have:
		return stStale, doc, sp
	}
	return stModified, doc, sp
}

// fileHash is the identity of the intended content; owned files include their frontmatter.
func fileHash(p place, body string) string {
	if p.Fmt == fmtOwnedMD {
		return contentHash([]byte(p.Front), []byte(strings.Trim(body, "\n")))
	}
	return contentHash([]byte(strings.Trim(body, "\n")))
}

func actualHash(p place, doc string, sp blockSpan) string {
	if p.Fmt == fmtOwnedMD {
		return contentHash([]byte(doc[:sp.start]), []byte(sp.body))
	}
	return contentHash([]byte(sp.body))
}

func renderFile(p place, doc string, sp blockSpan, body, version string) string {
	block := renderBlock(body, version, fileHash(p, body))
	if p.Fmt == fmtOwnedMD {
		return p.Front + block
	}
	return splice(doc, sp, block)
}

// removeBlock strips our block; owned files (or files that become empty) are deleted.
func removeFile(p place, body string, force bool) (action string, err error) {
	state, doc, sp := inspectFile(p, body)
	if !sp.found {
		return "not-installed", nil
	}
	if state == stModified && !force {
		return "kept-modified", nil
	}
	if p.Fmt == fmtOwnedMD {
		return "removed", os.Remove(p.Path)
	}
	rest := doc[:sp.start] + strings.TrimLeft(strings.TrimPrefix(doc[sp.end:], "\n"), "\n")
	rest = strings.TrimRight(rest, "\n")
	if strings.TrimSpace(rest) == "" {
		return "removed", os.Remove(p.Path)
	}
	return "removed", safeWrite(p.Path, []byte(rest+"\n"), 0o644)
}

// ---------- safe writes ----------

// refuseSymlink rejects a path whose final element is a symlink (we never write through one).
func refuseSymlink(p string) error {
	if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return &APIError{Code: "conflict", Message: p + " is a symlink; Weave will not write through it (replace it with a regular file or use --target dir --dir PATH)"}
	}
	return nil
}

// safeWrite writes atomically, creating parent directories 0700, and refuses symlinked leaves.
func safeWrite(path string, data []byte, perm os.FileMode) error {
	if err := refuseSymlink(path); err != nil {
		return err
	}
	if err := refuseSymlink(filepath.Dir(path)); err != nil {
		// the immediate parent (e.g. the skill folder "weave") must be a real directory;
		// grandparents such as a dotfiles-managed ~/.claude may legitimately be links
		return err
	}
	return atomicWrite(path, data, perm)
}
