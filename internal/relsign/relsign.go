// Package relsign signs and verifies Weave release checksums with ed25519 (Go standard library
// only, so the maintainer's CI image needs no extra tooling).
//
// The release packager signs SHA256SUMS.txt and publishes the raw 64-byte signature, base64
// encoded, as SHA256SUMS.txt.sig. `weave verify-release` and install.sh (via openssl >= 3) check
// it against the public key embedded here / in scripts/install.sh.
package relsign

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// PublicKey is the release signing public key: base64 of the raw 32-byte ed25519 key.
// The matching private seed is the CI secret RELEASE_SIGNING_KEY. install.sh embeds the same key
// in SubjectPublicKeyInfo form (see SPKIBase64); a test keeps the two in sync.
const PublicKey = "X2HjqHRYLLIO9QVyhCHsgJ8UqLT115QJqCMxxGhKkXk="

// spkiPrefix is the DER prefix of an ed25519 SubjectPublicKeyInfo (RFC 8410).
var spkiPrefix = []byte{0x30, 0x2a, 0x30, 0x05, 0x06, 0x03, 0x2b, 0x65, 0x70, 0x03, 0x21, 0x00}

// SPKIBase64 is the base64 DER SubjectPublicKeyInfo of a raw base64 public key, the form
// `openssl pkeyutl -verify -pubin` reads once wrapped in PEM armor.
func SPKIBase64(pubB64 string) (string, error) {
	raw, err := decodeKey(pubB64, ed25519.PublicKeySize, "public key")
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(append(append([]byte{}, spkiPrefix...), raw...)), nil
}

func decodeKey(s string, want int, what string) ([]byte, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil || len(b) != want {
		return nil, fmt.Errorf("%s must be %d bytes of base64", what, want)
	}
	return b, nil
}

// GenerateKey returns a fresh keypair: the private seed and the public key, both base64.
func GenerateKey() (seedB64, pubB64 string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(priv.Seed()), base64.StdEncoding.EncodeToString(pub), nil
}

// Sign signs msg with the private seed (base64) and returns the base64 signature.
func Sign(seedB64 string, msg []byte) (string, error) {
	seed, err := decodeKey(seedB64, ed25519.SeedSize, "signing key")
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(seed), msg)), nil
}

// PublicFromSeed derives the public key (base64) from a private seed (base64).
func PublicFromSeed(seedB64 string) (string, error) {
	seed, err := decodeKey(seedB64, ed25519.SeedSize, "signing key")
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)), nil
}

// Verify checks the base64 signature over msg against the base64 public key.
func Verify(pubB64 string, msg []byte, sigB64 string) error {
	pub, err := decodeKey(pubB64, ed25519.PublicKeySize, "public key")
	if err != nil {
		return err
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(sigB64))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("signature must be 64 bytes of base64")
	}
	if !ed25519.Verify(pub, msg, sig) {
		return errors.New("signature does not match (the checksums were not signed by the Weave release key, or were modified)")
	}
	return nil
}

// VerifyChecksums verifies SHA256SUMS.txt content against its signature with the embedded key
// (or pubB64 when non-empty, for forks that sign with their own key).
func VerifyChecksums(sums []byte, sigB64, pubB64 string) error {
	if pubB64 == "" {
		pubB64 = PublicKey
	}
	return Verify(pubB64, sums, sigB64)
}
