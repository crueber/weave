package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/crueber/weave/internal/relsign"
)

func init() {
	registerSpec("verify-release", []FlagSpec{{Name: "pubkey"}}, cmdVerifyRelease)
}

// cmdVerifyRelease checks the signature over a release's SHA256SUMS.txt. It is also what
// install.ps1 runs (PowerShell has no ed25519). It does not check file hashes: use sha256sum -c.
func cmdVerifyRelease(a *App, args []string) int {
	p, err := a.parse(args, []FlagSpec{{Name: "pubkey"}})
	if err != nil {
		return a.fail(err)
	}
	if len(p.Pos) != 2 {
		return a.failUsage("weave verify-release SHA256SUMS.txt SHA256SUMS.txt.sig [--pubkey BASE64|FILE]")
	}
	sums, err := os.ReadFile(p.Pos[0])
	if err != nil {
		return a.fail(err)
	}
	sig, err := os.ReadFile(p.Pos[1])
	if err != nil {
		return a.fail(err)
	}
	pub := strings.TrimSpace(p.Str("pubkey"))
	if b, err := os.ReadFile(pub); err == nil && pub != "" {
		pub = strings.TrimSpace(string(b))
	}
	if err := relsign.VerifyChecksums(sums, string(sig), pub); err != nil {
		return a.fail(fmt.Errorf("release signature INVALID: %w", err))
	}
	fmt.Fprintln(a.Stdout, `{"item":{"ok":true,"signature":"valid"}}`)
	return 0
}
