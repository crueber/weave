package auth

import (
	"bytes"
	"testing"
)

func TestGeneratedAvatar(t *testing.T) {
	a, err := GeneratedAvatarSVG("user-1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(bytes.TrimSpace(a), []byte("<svg")) || bytes.Contains(a, []byte("<script")) {
		t.Fatalf("not a clean svg: %.80s", a)
	}
	b, _ := GeneratedAvatarSVG("user-1")
	c, _ := GeneratedAvatarSVG("user-2")
	if !bytes.Equal(a, b) {
		t.Fatal("same seed must give the same avatar")
	}
	if bytes.Equal(a, c) {
		t.Fatal("different seeds should differ")
	}
}
