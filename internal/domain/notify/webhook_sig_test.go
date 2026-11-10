package notify

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Golden vector (reproduce with: printf '1700000000.{"event":"ping"}' | openssl dgst -sha256 -hmac whsec_test).
func TestSignV2GoldenVector(t *testing.T) {
	got := SignV2("whsec_test", 1700000000, `{"event":"ping"}`)
	want := "t=1700000000,v1=aa8efe37b751e71157c508c5ac4acb1e9fe5225db98355dfc00f4b680afbc447"
	if got != want {
		t.Fatalf("SignV2 = %s, want %s", got, want)
	}
	// the legacy signature is unchanged: HMAC over the body alone
	if got := Sign("whsec_test", `{"event":"ping"}`); got != "sha256=645e86a36ef1ed458be359c7d3c57737f8a202a88274a809a507f208ba54d991" {
		t.Fatalf("legacy Sign changed: %s", got)
	}
}

func TestPostSendsBothSignaturesWithSameTimestamp(t *testing.T) {
	var hdr http.Header
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	}))
	defer srv.Close()
	old := HTTPClient
	HTTPClient = srv.Client()
	defer func() { HTTPClient = old }()

	payload := `{"event":"cardCreate"}`
	if st, _, errMsg, _ := post(context.Background(), srv.URL, "", "s3cret", "cardCreate", "d1", payload); st != 200 || errMsg != "" {
		t.Fatalf("post: %d %s", st, errMsg)
	}
	if body != payload {
		t.Fatal("body")
	}
	if hdr.Get("X-Weave-Signature") != Sign("s3cret", payload) {
		t.Fatal("legacy signature header must be unchanged")
	}
	ts, err := strconv.ParseInt(hdr.Get("X-Weave-Timestamp"), 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)) > time.Minute {
		t.Fatalf("timestamp header: %q", hdr.Get("X-Weave-Timestamp"))
	}
	v2 := hdr.Get("X-Weave-Signature-V2")
	if v2 != SignV2("s3cret", ts, payload) {
		t.Fatalf("V2 header %q does not match the timestamp header", v2)
	}
	// a receiver recomputing it the documented way gets the same value
	m := hmac.New(sha256.New, []byte("s3cret"))
	m.Write([]byte(hdr.Get("X-Weave-Timestamp") + "." + payload))
	if !strings.HasSuffix(v2, "v1="+hex.EncodeToString(m.Sum(nil))) {
		t.Fatal("V2 is not HMAC(secret, ts.body)")
	}

	// no secret and no token: nothing is signed
	hdr = nil
	post(context.Background(), srv.URL, "", "", "ping", "d2", payload)
	if hdr.Get("X-Weave-Signature") != "" || hdr.Get("X-Weave-Signature-V2") != "" {
		t.Fatal("unsigned webhook must carry no signature")
	}
}
