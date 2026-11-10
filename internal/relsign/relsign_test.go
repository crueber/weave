package relsign

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestSignVerifyRoundTripAndTamper(t *testing.T) {
	seed, pub, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := PublicFromSeed(seed); p != pub {
		t.Fatal("PublicFromSeed mismatch")
	}
	sums := []byte("abc123  weave_v1_linux_amd64.tar.gz\n")
	sig, err := Sign(seed, sums)
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := base64.StdEncoding.DecodeString(sig); len(raw) != ed25519.SignatureSize {
		t.Fatalf("signature is %d bytes, want 64", len(raw))
	}
	if err := Verify(pub, sums, sig); err != nil {
		t.Fatalf("valid signature rejected: %v", err)
	}
	if Verify(pub, append(sums, ' '), sig) == nil {
		t.Fatal("modified checksums must fail")
	}
	other, otherPub, _ := GenerateKey()
	_ = other
	if Verify(otherPub, sums, sig) == nil {
		t.Fatal("wrong key must fail")
	}
	for _, bad := range []string{"", "not base64!", base64.StdEncoding.EncodeToString([]byte("short"))} {
		if Verify(pub, sums, bad) == nil {
			t.Fatalf("garbage signature %q accepted", bad)
		}
	}
}

// Golden vector: RFC 8032 section 7.1 TEST 1 (empty message). ed25519 is deterministic, so this
// pins the exact bytes of a published .sig.
func TestGoldenVectorRFC8032(t *testing.T) {
	seedRaw, _ := hex.DecodeString("9d61b19deffd5a60ba844af492ec2cc44449c5697b326919703bac031cae7f60")
	pubRaw, _ := hex.DecodeString("d75a980182b10ab7d54bfed3c964073a0ee172f3daa62325af021a68f707511a")
	sigRaw, _ := hex.DecodeString("e5564300c360ac729086e2cc806e828a84877f1eb8e5d974d873e065224901555fb8821590a33bacc61e39701cf9b46bd25bf5f0595bbe24655141438e7a100b")
	seed := base64.StdEncoding.EncodeToString(seedRaw)
	pub, _ := PublicFromSeed(seed)
	if pub != base64.StdEncoding.EncodeToString(pubRaw) {
		t.Fatalf("pub = %s", pub)
	}
	sig, _ := Sign(seed, nil)
	if sig != base64.StdEncoding.EncodeToString(sigRaw) {
		t.Fatalf("sig = %s", sig)
	}
	if err := Verify(pub, nil, sig); err != nil {
		t.Fatal(err)
	}
	if _, err := Sign("short", nil); err == nil {
		t.Fatal("bad seed accepted")
	}
}

// The release key embedded in install.sh must be the one embedded in the binaries.
func TestInstallScriptEmbedsTheSameKey(t *testing.T) {
	spki, err := SPKIBase64(PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile("../../scripts/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `WEAVE_RELEASE_PUBKEY_B64="`+spki+`"`) {
		t.Fatalf("scripts/install.sh does not embed the SPKI of relsign.PublicKey (%s)", spki)
	}
}
