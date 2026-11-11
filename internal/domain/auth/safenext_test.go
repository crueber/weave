package auth

import "testing"

func TestSafeNext(t *testing.T) {
	ok := []string{"/", "/boards/abc", "/boards/abc?card=1&x=y", "/p/1#frag", "/a%20b"}
	for _, n := range ok {
		if got := safeNext(n); got != n {
			t.Errorf("safeNext(%q) = %q, want unchanged", n, got)
		}
	}
	bad := []string{"", "x", "//evil.com", "/\\evil.com", "/\\/evil.com", "/a\\b", "http://evil.com", "https://evil.com/",
		"/\tevil.com", "/\n/evil.com", "/\r\n/evil.com", "/ /evil.com", "/a b", "/\x00x", "/\x7f", "/\u0085x", "/ x", "/login", "/login?next=/",
		"javascript:alert(1)", "/%0a", "/a b"}
	for _, n := range bad {
		want := "/"
		if n == "/%0a" {
			want = "/%0a" // percent-encoded forms are inert in a Location header
		}
		if got := safeNext(n); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", n, got, want)
		}
	}
}
