package notify

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/crueber/weave/internal/db"
	"github.com/crueber/weave/internal/ops"
)

// Strict destination policy for webhooks created by non-admins (webhooks.strict_dest=1, board owners).
//
// Unlike shoutrrr, the webhook sender is our own http.Client, so the policy is enforced where the connection is
// made: the transport's DialContext resolves the host itself (through the LookupIP seam), refuses the whole answer
// when any address is blocked, and dials the checked IP literal. A DNS name that changes between the check and the
// connection (rebinding) therefore cannot reach a blocked address, and every redirect hop goes through the same
// dialer (redirects are also limited to 3 hops and http/https). No proxy is used (a proxy would dial for us).
//
// Always refused: loopback, unspecified, link-local (incl. 169.254.169.254 and fd00:ec2::254), multicast, broadcast,
// 0.0.0.0/8, cloud metadata host names, localhost names, and IPv4 addresses embedded in NAT64 (64:ff9b::/96) or
// 6to4 (2002::/16) addresses that would be refused themselves. Refused by default: private networks (RFC 1918, ULA
// fc00::/7, CGNAT 100.64.0.0/10). The admin switch webhook.userAllowPrivate (WEAVE_WEBHOOK_USER_ALLOW_PRIVATE) allows
// private networks for homelab setups; WEAVE_NOTIFY_ALLOW_HOSTS (admin-controlled) overrides every block for the hosts,
// IPs or CIDRs it names. Admin-created webhooks keep the admin policy (HTTPClient, no destination restriction).

// WebhookDialer opens the TCP connection to an address the strict policy already approved (an IP literal and port).
// Replaceable in tests.
var WebhookDialer = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext

// UserAllowPrivateKey is the admin config key (env WEAVE_WEBHOOK_USER_ALLOW_PRIVATE) that lets board webhooks reach
// private-network addresses.
const UserAllowPrivateKey = "webhook.userAllowPrivate"

var (
	thisNet   = netip.MustParsePrefix("0.0.0.0/8")
	bcast     = netip.MustParseAddr("255.255.255.255")
	nat64     = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour = netip.MustParsePrefix("2002::/16")
)

func userAllowPrivate(ctx context.Context, d *db.DB) bool {
	switch strings.ToLower(strings.TrimSpace(ConfigDB(ctx, d, UserAllowPrivateKey))) {
	case "true", "1", "yes", "on":
		return true
	}
	return false
}

func strictPolicy(allowPrivate bool) destPolicy {
	p := loadDestPolicy()
	p.blockPrivate = !allowPrivate
	return p
}

// strictBlocked is blockedIP plus the ranges that are never a legitimate webhook receiver.
func (p destPolicy) strictBlocked(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true
	}
	a = a.Unmap()
	if p.ipAllowed(a) {
		return false
	}
	if bad, _ := p.blockedIP(ip); bad {
		return true
	}
	if a.Is4() && (thisNet.Contains(a) || a == bcast) {
		return true
	}
	if a.Is6() {
		b := a.As16()
		var v4 netip.Addr
		switch {
		case nat64.Contains(a):
			v4 = netip.AddrFrom4([4]byte{b[12], b[13], b[14], b[15]})
		case sixToFour.Contains(a):
			v4 = netip.AddrFrom4([4]byte{b[2], b[3], b[4], b[5]})
		}
		if v4.IsValid() {
			return p.strictBlocked(net.IP(v4.AsSlice()))
		}
	}
	return false
}

// resolveStrict returns the addresses a strict webhook may connect to for host, or ErrDestinationNotAllowed.
func resolveStrict(ctx context.Context, p destPolicy, host string) ([]net.IP, error) {
	host = strings.ToLower(strings.TrimSuffix(strings.Trim(host, "[]"), "."))
	if host == "" {
		return nil, ErrDestinationNotAllowed
	}
	allowed := p.hostAllowed(host)
	if !allowed && (metadataHosts[host] || host == "localhost" || strings.HasSuffix(host, ".localhost")) {
		return nil, ErrDestinationNotAllowed
	}
	if ip := net.ParseIP(host); ip != nil {
		if !allowed && p.strictBlocked(ip) {
			return nil, ErrDestinationNotAllowed
		}
		return []net.IP{ip}, nil
	}
	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	ips, err := LookupIP(lctx, host)
	cancel()
	if err != nil || len(ips) == 0 {
		return nil, ErrDestinationNotAllowed // fail closed
	}
	if !allowed {
		for _, ip := range ips {
			if p.strictBlocked(ip) {
				return nil, ErrDestinationNotAllowed // one bad answer refuses the name
			}
		}
	}
	return ips, nil
}

func strictDial(p destPolicy) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, ErrDestinationNotAllowed
		}
		ips, err := resolveStrict(ctx, p, host)
		if err != nil {
			return nil, err
		}
		last := error(ErrDestinationNotAllowed)
		for _, ip := range ips {
			c, err := WebhookDialer(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
}

var errRedirect = errors.New("redirect not followed")

// strictClient is the HTTP client for one delivery of a strict webhook.
func strictClient(p destPolicy) *http.Client {
	tr := &http.Transport{
		Proxy:                  nil,
		DialContext:            strictDial(p),
		TLSHandshakeTimeout:    10 * time.Second,
		ResponseHeaderTimeout:  15 * time.Second,
		MaxResponseHeaderBytes: 64 << 10,
		DisableKeepAlives:      true,
	}
	return &http.Client{Timeout: 15 * time.Second, Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
			return errRedirect
		}
		return nil
	}}
}

// checkStrictURL is the save-time check of a strict webhook URL (the authoritative check runs on every connection).
func checkStrictURL(ctx context.Context, d *db.DB, raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return ops.Invalid("url must be an http(s) URL")
	}
	if _, err := resolveStrict(ctx, strictPolicy(userAllowPrivate(ctx, d)), u.Hostname()); err != nil {
		return ops.Invalid("url: this destination is not allowed for board webhooks (loopback, link-local, cloud metadata and private-network addresses are refused; an admin can allow private networks) or its host name does not resolve")
	}
	return nil
}
