package notify_test

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/crueber/weave/internal/domain/notify"
	"github.com/crueber/weave/internal/ops"
)

func stubDNS(t *testing.T, m map[string][]string) {
	t.Helper()
	old := notify.LookupIP
	notify.LookupIP = func(_ context.Context, host string) ([]net.IP, error) {
		v, ok := m[host]
		if !ok {
			return nil, errors.New("no such host")
		}
		var out []net.IP
		for _, s := range v {
			out = append(out, net.ParseIP(s))
		}
		return out, nil
	}
	t.Cleanup(func() { notify.LookupIP = old })
}

func create(w *world, url string) error {
	_, err := ops.Call(w.bob, "notification_service.create", &notify.ServiceCreateIn{URL: url, Format: "text"})
	return err
}

func TestNotificationDestinationPolicy(t *testing.T) {
	stubDNS(t, map[string][]string{
		"public.example.com":   {"93.184.216.34"},
		"nas.lan":              {"192.168.1.20"},
		"evil.example.com":     {"127.0.0.1"},
		"mixed.example.com":    {"93.184.216.34", "169.254.169.254"},
		"v6loop.example.com":   {"::1"},
		"mapped.example.com":   {"::ffff:127.0.0.1"},
		"ec2v6.example.com":    {"fd00:ec2::254"},
		"unspec.example.com":   {"0.0.0.0"},
		"linklocal6.example.c": {"fe80::1"},
	})
	w := seed(t)
	blocked := []string{
		"generic://127.0.0.1/hook", "generic://127.1.2.3:8080/hook", "generic://[::1]/hook",
		"generic://169.254.169.254/latest/meta-data", "generic://[fe80::1]/x", "generic://[fd00:ec2::254]/x",
		"generic://0.0.0.0/x", "generic://[::]/x", "generic://localhost/x", "generic://foo.localhost/x",
		"generic://metadata.google.internal/x", "generic://instance-data/x", "generic://[::ffff:127.0.0.1]/x",
		"smtp://user:pw@127.0.0.1:25/?fromAddress=a@b.c&toAddresses=d@e.f",
		"ntfy://:tok@169.254.169.254/topic", "gotify://localhost/token",
		"generic://unresolvable.invalid/x", // dialled host that does not resolve: fail closed
		"generic://evil.example.com/x", "generic://mixed.example.com/x", "generic://v6loop.example.com/x",
		"generic://mapped.example.com/x", "generic://ec2v6.example.com/x", "generic://unspec.example.com/x",
	}
	for _, u := range blocked {
		err := create(w, u)
		if err == nil {
			t.Errorf("%s: accepted, want refused", u)
		} else if strings.Contains(err.Error(), "refused") || strings.Contains(err.Error(), "dial") {
			t.Errorf("%s: error leaks detail: %v", u, err)
		}
	}
	allowed := []string{
		"generic://public.example.com/hook", "generic://93.184.216.34/x", "generic://nas.lan:8080/x", // LAN by name
		"ntfy://:tok@10.0.0.5/topic", "gotify://192.168.1.20/token", "generic://172.16.5.5/x", // RFC1918 literals
		"discord://token@123456789", "slack://AAAAAAAAA/BBBBBBBBB/cccccccccccccccccccccccc", // token-style: host never dialled, unresolvable is fine
	}
	for _, u := range allowed {
		if err := create(w, u); err != nil {
			t.Errorf("%s: refused: %v", u, err)
		}
	}
}

func TestDestinationTargetsIncludeQueryHosts(t *testing.T) {
	got := notify.DestinationHostsForTest("teams://g@t/a/b/c?host=127.0.0.1:8443&url=http://169.254.169.254/x")
	want := map[string]bool{"t": true, "127.0.0.1": true, "169.254.169.254": true}
	if len(got) != len(want) {
		t.Fatalf("hosts = %v", got)
	}
	for _, h := range got {
		if !want[h] {
			t.Errorf("unexpected host %q in %v", h, got)
		}
	}
}

func TestNotificationDestinationBlockPrivateAndAllowlist(t *testing.T) {
	stubDNS(t, map[string][]string{"nas.lan": {"192.168.1.20"}, "evil.example.com": {"127.0.0.1"}, "cg.example.com": {"100.64.1.1"}})
	w := seed(t)
	t.Setenv("WEAVE_NOTIFY_BLOCK_PRIVATE", "true")
	for _, u := range []string{"generic://10.1.2.3/x", "generic://192.168.0.1/x", "generic://172.20.0.1/x", "generic://nas.lan/x", "generic://[fd12::1]/x", "generic://cg.example.com/x"} {
		if err := create(w, u); err == nil {
			t.Errorf("%s: accepted with WEAVE_NOTIFY_BLOCK_PRIVATE=true", u)
		}
	}
	// allowlist (host name, IP, CIDR, wildcard) overrides every block
	t.Setenv("WEAVE_NOTIFY_ALLOW_HOSTS", "nas.lan, 127.0.0.1, 10.9.0.0/16 ;*.corp.example")
	for _, u := range []string{"generic://nas.lan/x", "generic://127.0.0.1:9/x", "generic://10.9.4.4/x", "generic://ntfy.corp.example/x", "generic://evil.example.com/x"} {
		err := create(w, u)
		if u == "generic://evil.example.com/x" { // resolves to 127.0.0.1 which is allow-listed as an IP
			if err != nil {
				t.Errorf("%s: %v", u, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: refused despite allowlist: %v", u, err)
		}
	}
	if err := create(w, "generic://10.8.0.1/x"); err == nil {
		t.Error("10.8.0.1 is outside the allowed CIDR and private IPs are blocked")
	}
}

func TestNotificationSendTimeRecheckAndGenericError(t *testing.T) {
	m := map[string][]string{"hook.example.com": {"93.184.216.34"}}
	stubDNS(t, m)
	w := seed(t)
	called := 0
	oldSender := notify.Sender
	notify.Sender = func(url, title, body string) error {
		called++
		return errors.New("dial tcp 10.0.0.9:25: connect: connection refused")
	}
	t.Cleanup(func() { notify.Sender = oldSender })

	svc, err := ops.Typed[notify.ServiceOut](w.bob, "notification_service.create", notify.ServiceCreateIn{URL: "generic://hook.example.com/x", Format: "text"})
	if err != nil {
		t.Fatal(err)
	}
	// connection failure: generic message, no detail
	_, err = ops.Call(w.bob, "notification_service.test", &notify.IDIn{ID: svc.ID})
	if err == nil || !strings.Contains(err.Error(), notify.GenericDeliveryError) || strings.Contains(err.Error(), "refused") || strings.Contains(err.Error(), "10.0.0.9") {
		t.Fatalf("test error = %v, want only %q", err, notify.GenericDeliveryError)
	}
	if called != 1 {
		t.Fatalf("sender called %d times", called)
	}
	// DNS now points at the metadata address (rebinding after saving): refused before sending, same generic error
	m["hook.example.com"] = []string{"169.254.169.254"}
	called = 0
	_, err = ops.Call(w.bob, "notification_service.test", &notify.IDIn{ID: svc.ID})
	if err == nil || !strings.Contains(err.Error(), notify.GenericDeliveryError) {
		t.Fatalf("rebinding error = %v", err)
	}
	if called != 0 {
		t.Fatal("the sender must not be called for a blocked destination")
	}
	// updating an existing service to a blocked URL is refused too
	bad := "generic://127.0.0.1/x"
	if _, err := ops.Call(w.bob, "notification_service.update", &notify.ServiceUpdateIn{ID: svc.ID, URL: &bad}); err == nil {
		t.Fatal("update to loopback accepted")
	}
}
