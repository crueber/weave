package server_test

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

func (e *env) rawReq(method, path string, hdr map[string]string, body io.Reader) (int, string) {
	e.t.Helper()
	code, _, b := e.rawReqH(method, path, hdr, body)
	return code, b
}

func (e *env) rawReqH(method, path string, hdr map[string]string, body io.Reader) (int, http.Header, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, body)
	for k, v := range hdr {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	cl := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := cl.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(b)
}

func TestRequestBodyCaps(t *testing.T) {
	e := newEnv(t)
	big := `{"name":"` + strings.Repeat("a", 3<<20) + `"}`
	auth := map[string]string{"Authorization": "Bearer " + e.admin, "Content-Type": "application/json"}
	// oversized JSON from an authenticated caller: 413
	if code, _ := e.rawReq("POST", "/api/v1/projects", auth, strings.NewReader(big)); code != 413 {
		t.Fatalf("oversized JSON: %d, want 413", code)
	}
	// oversized form body: 413
	form := map[string]string{"Authorization": "Bearer " + e.admin, "Content-Type": "application/x-www-form-urlencoded"}
	if code, _ := e.rawReq("POST", "/api/v1/projects", form, strings.NewReader("name="+strings.Repeat("a", 3<<20))); code != 413 {
		t.Fatalf("oversized form: %d, want 413", code)
	}
	// anonymous callers are rejected (401) before the body is read, whatever its size or shape
	if code, _ := e.rawReq("POST", "/api/v1/projects", map[string]string{"Content-Type": "application/json"}, strings.NewReader(big)); code != 401 {
		t.Fatalf("anonymous oversized: %d, want 401", code)
	}
	if code, _ := e.rawReq("POST", "/api/v1/projects", map[string]string{"Content-Type": "application/json"}, bytes.NewReader([]byte("{not json"))); code != 401 {
		t.Fatalf("anonymous bad JSON: %d, want 401", code)
	}
	// a normal request still works
	if code, _ := e.rawReq("POST", "/api/v1/projects", auth, strings.NewReader(`{"name":"ok"}`)); code != 200 {
		t.Fatalf("normal request: %d", code)
	}
}
