// Command weave-release builds and packages the release artifacts. CI (the `release` job) and
// scripts/release-local.sh both call it, so what is verified locally is what gets published.
//
//	go run ./cmd/weave-release -out dist -version v1.2.3
//
// Output in -out:
//
//	weave_<version>_<os>_<arch>.tar.gz | .zip   slim CLI (named `weave`) + README.txt + LICENSE
//	                                            + completions/ + skills/weave/ for 6 targets
//	weave-<os>-<arch>[.exe]                     the raw slim CLI binaries
//	weave-server-linux-<arch>                   the full binaries (server + CLI) for linux
//	install.sh, install.ps1                     installers (copied from scripts/)
//	SHA256SUMS.txt                              checksums of everything above
//	SHA256SUMS.txt.sig                          ed25519 signature of SHA256SUMS.txt (base64), only when
//	                                            RELEASE_SIGNING_KEY (base64 seed) is set
//
// Key management: `go run ./cmd/weave-release -genkey -keyfile PATH` writes a new private seed to
// PATH (mode 0600, never inside the repo) and prints the public key to commit in internal/relsign
// and scripts/install.sh. Store the seed as the CI secret RELEASE_SIGNING_KEY.
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/crueber/weave/internal/cli"
	"github.com/crueber/weave/internal/relsign"
	"github.com/crueber/weave/skills"
)

const mod = "github.com/crueber/weave"

var defaultTargets = "linux/amd64,linux/arm64,darwin/amd64,darwin/arm64,windows/amd64,windows/arm64"

func main() {
	out := flag.String("out", "dist", "output directory")
	ver := flag.String("version", "dev", "version to stamp (tag)")
	targets := flag.String("targets", defaultTargets, "comma separated os/arch list for the slim CLI")
	server := flag.String("server", "linux/amd64,linux/arm64", "os/arch list for the full server binaries (empty to skip)")
	scripts := flag.String("scripts", "scripts", "directory holding install.sh / install.ps1")
	genkey := flag.Bool("genkey", false, "generate a release signing keypair: write the private seed to -keyfile, print the public key")
	keyfile := flag.String("keyfile", "", "with -genkey: where to write the private seed (must not exist; keep it out of the repository)")
	flag.Parse()
	if *genkey {
		if err := genKey(*keyfile); err != nil {
			fmt.Fprintln(os.Stderr, "weave-release:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*out, *ver, split(*targets), split(*server), *scripts); err != nil {
		fmt.Fprintln(os.Stderr, "weave-release:", err)
		os.Exit(1)
	}
}

// genKey writes a fresh private seed to path (exclusive create, 0600) and prints only the public
// key and where the seed went; the seed itself is never printed.
func genKey(path string) error {
	if path == "" {
		return fmt.Errorf("-genkey needs -keyfile PATH (outside the repository)")
	}
	seed, pub, err := relsign.GenerateKey()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(seed + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	spki, _ := relsign.SPKIBase64(pub)
	fmt.Printf("public key (commit as relsign.PublicKey): %s\n", pub)
	fmt.Printf("SPKI base64 (embed in scripts/install.sh):  %s\n", spki)
	fmt.Printf("private seed written to %s (CI secret RELEASE_SIGNING_KEY); never commit it\n", path)
	return nil
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func goBuild(version, goos, goarch, pkg, dest string) error {
	ld := fmt.Sprintf("-s -w -X %s/internal/version.Version=%s -X %s/internal/domain/admin.Version=%s", mod, version, mod, version)
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ld, "-o", dest, pkg)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func run(out, version string, targets, server []string, scriptsDir string) error {
	if err := os.RemoveAll(out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	comps := completions()
	skillFiles, err := readSkill()
	if err != nil {
		return err
	}
	readme := []byte(readmeText(version))
	license, _ := os.ReadFile("LICENSE")

	for _, t := range targets {
		goos, goarch, ok := strings.Cut(t, "/")
		if !ok {
			return fmt.Errorf("bad target %q", t)
		}
		ext := ""
		if goos == "windows" {
			ext = ".exe"
		}
		raw := filepath.Join(out, fmt.Sprintf("weave-%s-%s%s", goos, goarch, ext))
		fmt.Println("building", filepath.Base(raw))
		if err := goBuild(version, goos, goarch, "./cmd/weave-cli", raw); err != nil {
			return fmt.Errorf("build %s: %w", t, err)
		}
		bin, err := os.ReadFile(raw)
		if err != nil {
			return err
		}
		files := []entry{{"weave" + ext, bin, 0o755}, {"README.txt", readme, 0o644}}
		if len(license) > 0 {
			files = append(files, entry{"LICENSE", license, 0o644})
		}
		for _, n := range sortedKeys(comps) {
			files = append(files, entry{"completions/" + n, comps[n], 0o644})
		}
		for _, n := range sortedKeys(skillFiles) {
			files = append(files, entry{"skills/weave/" + n, skillFiles[n], 0o644})
		}
		base := fmt.Sprintf("weave_%s_%s_%s", version, goos, goarch)
		if goos == "windows" {
			err = writeZip(filepath.Join(out, base+".zip"), files)
		} else {
			err = writeTarGz(filepath.Join(out, base+".tar.gz"), files)
		}
		if err != nil {
			return err
		}
	}
	for _, t := range server {
		goos, goarch, _ := strings.Cut(t, "/")
		dest := filepath.Join(out, fmt.Sprintf("weave-server-%s-%s", goos, goarch))
		fmt.Println("building", filepath.Base(dest))
		if err := goBuild(version, goos, goarch, "./cmd/weave", dest); err != nil {
			return fmt.Errorf("build server %s: %w", t, err)
		}
	}
	for _, n := range []string{"install.sh", "install.ps1"} {
		b, err := os.ReadFile(filepath.Join(scriptsDir, n))
		if err != nil {
			return fmt.Errorf("installer: %w", err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(n, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(filepath.Join(out, n), b, mode); err != nil {
			return err
		}
	}
	if err := writeSums(out); err != nil {
		return err
	}
	return signSums(out, os.Getenv("RELEASE_SIGNING_KEY"))
}

// signSums writes SHA256SUMS.txt.sig when a signing key (base64 seed) is provided; otherwise it
// warns loudly so an unsigned release is never silent.
func signSums(dir, seed string) error {
	if strings.TrimSpace(seed) == "" {
		fmt.Fprintln(os.Stderr, "weave-release: WARNING: RELEASE_SIGNING_KEY is not set; SHA256SUMS.txt is NOT signed (installers will warn)")
		return nil
	}
	sums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS.txt"))
	if err != nil {
		return err
	}
	sig, err := relsign.Sign(seed, sums)
	if err != nil {
		return fmt.Errorf("sign: %w", err)
	}
	if pub, _ := relsign.PublicFromSeed(seed); pub != relsign.PublicKey {
		fmt.Fprintln(os.Stderr, "weave-release: WARNING: RELEASE_SIGNING_KEY does not match the public key embedded in this build (internal/relsign); installers will reject the signature")
	}
	fmt.Println("signed SHA256SUMS.txt")
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS.txt.sig"), []byte(sig+"\n"), 0o644)
}

type entry struct {
	name string
	data []byte
	mode os.FileMode
}

func sortedKeys(m map[string][]byte) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

func completions() map[string][]byte {
	m := map[string][]byte{}
	for sh, name := range map[string]string{"bash": "weave.bash", "zsh": "_weave", "fish": "weave.fish", "powershell": "weave.ps1"} {
		var b bytes.Buffer
		if cli.Run([]string{"completion", sh}, strings.NewReader(""), &b, io.Discard) == 0 {
			m[name] = b.Bytes()
		}
	}
	return m
}

func readSkill() (map[string][]byte, error) {
	m := map[string][]byte{}
	err := fs.WalkDir(skills.FS, "weave", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := skills.FS.ReadFile(p)
		m[strings.TrimPrefix(p, "weave/")] = b
		return err
	})
	if len(m) == 0 {
		return nil, fmt.Errorf("embedded skill is empty (run `make skill`)")
	}
	return m, err
}

func readmeText(version string) string {
	return fmt.Sprintf(`Weave CLI %s

Put the `+"`weave`"+` binary on your PATH, then:

  weave login --url https://weave.example.com --token wk_...     # API key from Settings -> API key
  weave whoami
  weave skill install                    # teach Claude Code / agents to use Weave
  weave listen --board "My board" --list Ready                     # stream card events
  weave agent run --board "My board" --listen Ready --claim-move-to "In progress" --done-list Review -- ./handle.sh

Shell completion scripts are in completions/ (bash: weave.bash, zsh: _weave, fish: weave.fish, PowerShell: weave.ps1).
The agent skill is in skills/weave/ (copy to ~/.claude/skills/ or run `+"`weave skill install`"+`).
Docs: docs/CLI.md and docs/AGENTS.md in the repository.
`, version)
}

var epoch = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC) // deterministic archives

func writeTarGz(path string, files []entry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, _ := gzip.NewWriterLevel(f, gzip.BestCompression)
	tw := tar.NewWriter(gz)
	for _, e := range files {
		if err := tw.WriteHeader(&tar.Header{Name: e.name, Mode: int64(e.mode), Size: int64(len(e.data)), ModTime: epoch, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(e.data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func writeZip(path string, files []entry) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for _, e := range files {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate, Modified: epoch}
		h.SetMode(e.mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		if _, err := w.Write(e.data); err != nil {
			return err
		}
	}
	return zw.Close()
}

func writeSums(dir string) error {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var sb strings.Builder
	for _, e := range ents {
		if e.IsDir() || e.Name() == "SHA256SUMS.txt" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		h := sha256.Sum256(b)
		fmt.Fprintf(&sb, "%s  %s\n", hex.EncodeToString(h[:]), e.Name())
	}
	return os.WriteFile(filepath.Join(dir, "SHA256SUMS.txt"), []byte(sb.String()), 0o644)
}
