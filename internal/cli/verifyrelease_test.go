package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/relsign"
)

func TestVerifyRelease(t *testing.T) {
	f := newFake(t)
	dir := t.TempDir()
	seed, pub, err := relsign.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	sums := []byte("deadbeef  weave_v1_linux_amd64.tar.gz\n")
	sig, _ := relsign.Sign(seed, sums)
	sumsPath, sigPath := filepath.Join(dir, "SHA256SUMS.txt"), filepath.Join(dir, "SHA256SUMS.txt.sig")
	os.WriteFile(sumsPath, sums, 0o644)
	os.WriteFile(sigPath, []byte(sig+"\n"), 0o644)

	// valid against an explicit key (a fork's), as a base64 string or a file
	if code, out, errs := f.run("verify-release", sumsPath, sigPath, "--pubkey", pub); code != 0 || !strings.Contains(out, `"ok":true`) {
		t.Fatalf("valid: %d %s %s", code, out, errs)
	}
	keyFile := filepath.Join(dir, "pub.txt")
	os.WriteFile(keyFile, []byte(pub+"\n"), 0o644)
	if code, _, errs := f.run("verify-release", sumsPath, sigPath, "--pubkey", keyFile); code != 0 {
		t.Fatalf("pubkey file: %d %s", code, errs)
	}
	// the embedded Weave key does not match a different signer
	if code, _, errs := f.run("verify-release", sumsPath, sigPath); code == 0 || !strings.Contains(errs, "INVALID") {
		t.Fatalf("wrong key must fail: %d %s", code, errs)
	}
	// tampered checksums
	os.WriteFile(sumsPath, append(sums, '#'), 0o644)
	if code, _, _ := f.run("verify-release", sumsPath, sigPath, "--pubkey", pub); code == 0 {
		t.Fatal("tampered sums must fail")
	}
	if code, _, _ := f.run("verify-release", sumsPath); code == 0 {
		t.Fatal("usage error expected")
	}
}
