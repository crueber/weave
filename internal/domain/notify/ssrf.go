package notify

import (
	"context"
	"errors"
	"log"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/crueber/weave/internal/ops"
)

// Destination policy for notification services (shoutrrr URLs). Anyone who can add a service can
// choose where the server connects, so loopback, link-local (cloud metadata) and unspecified
// destinations are refused, both when a service is saved and again, with a fresh DNS lookup of
// every returned address, immediately before each send. Ordinary LAN addresses (RFC 1918) stay
// allowed because self-hosted ntfy/gotify are the typical targets.
//
//	WEAVE_NOTIFY_BLOCK_PRIVATE=true   also refuse RFC 1918 / ULA / CGNAT destinations (default false)
//	WEAVE_NOTIFY_ALLOW_HOSTS=a,b,c    hostnames, IPs or CIDRs that are always allowed (overrides every block)
//
// Residual risk: shoutrrr dials on its own, so a DNS name that changes between our lookup and its
// connection (rebinding) or an HTTP redirect to a blocked address is not stopped by this check.

// ErrDestinationNotAllowed is returned for a blocked destination.
var ErrDestinationNotAllowed = errors.New("notification destination is not allowed")

// GenericDeliveryError is the only failure text callers/users get for a failed or refused send;
// connection details stay in the server log.
const GenericDeliveryError = "could not deliver: destination unreachable or not allowed"

// LookupIP resolves a host name. Replaceable in tests (stub resolver seam).
var LookupIP = func(ctx context.Context, host string) ([]net.IP, error) {
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, a.IP)
	}
	return out, nil
}

var metadataHosts = map[string]bool{
	"metadata": true, "metadata.google.internal": true, "metadata.goog": true,
	"instance-data": true, "instance-data.ec2.internal": true,
	"metadata.azure.internal": true, "metadata.tencentyun.com": true, "metadata.packet.net": true,
}

var (
	cgnat       = netip.MustParsePrefix("100.64.0.0/10")
	ec2IPv6Meta = netip.MustParseAddr("fd00:ec2::254")
	aliMeta     = netip.MustParseAddr("100.100.100.200")
)

type destPolicy struct {
	blockPrivate bool
	allow        []string // lower-case hostnames (or "*.suffix")
	allowNets    []netip.Prefix
}

func loadDestPolicy() destPolicy {
	p := destPolicy{blockPrivate: strings.EqualFold(strings.TrimSpace(os.Getenv("WEAVE_NOTIFY_BLOCK_PRIVATE")), "true")}
	for _, f := range strings.FieldsFunc(os.Getenv("WEAVE_NOTIFY_ALLOW_HOSTS"), func(r rune) bool { return r == ',' || r == ' ' || r == ';' }) {
		f = strings.ToLower(strings.TrimSpace(f))
		if f == "" {
			continue
		}
		if pf, err := netip.ParsePrefix(f); err == nil {
			p.allowNets = append(p.allowNets, pf.Masked())
		} else if a, err := netip.ParseAddr(f); err == nil {
			p.allowNets = append(p.allowNets, netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen()))
		} else {
			p.allow = append(p.allow, strings.TrimSuffix(f, "."))
		}
	}
	return p
}

func (p destPolicy) hostAllowed(host string) bool {
	for _, a := range p.allow {
		if a == host || (strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:])) {
			return true
		}
	}
	return false
}

func (p destPolicy) ipAllowed(a netip.Addr) bool {
	for _, n := range p.allowNets {
		if n.Contains(a) {
			return true
		}
	}
	return false
}

// blockedIP reports whether the policy refuses a destination address, with the reason.
func (p destPolicy) blockedIP(ip net.IP) (bool, string) {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return true, "invalid address"
	}
	a = a.Unmap()
	if p.ipAllowed(a) {
		return false, ""
	}
	switch {
	case a.IsLoopback():
		return true, "loopback"
	case a.IsUnspecified():
		return true, "unspecified"
	case a.IsLinkLocalUnicast(), a.IsLinkLocalMulticast(), a.IsInterfaceLocalMulticast(), a == ec2IPv6Meta, a == aliMeta:
		return true, "link-local or cloud metadata"
	case a.IsMulticast():
		return true, "multicast"
	}
	if p.blockPrivate && (a.IsPrivate() || cgnat.Contains(a)) {
		return true, "private network"
	}
	return false, ""
}

// destinationHost extracts the host a shoutrrr URL points at: generic://host/path,
// smtp://user:pw@host:587, ntfy://:tok@host, gotify://host/token, discord://token@id (the "host" of
// token-style schemes is not dialled).
func destinationHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	h := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if h == "" && u.Opaque != "" { // scheme:host style
		h = strings.ToLower(u.Opaque)
	}
	return h
}

// dialsURLHost lists the shoutrrr schemes that connect to the host written in the URL. For the
// others (discord, slack, telegram, ...) that part is a token or id and a lookup failure is harmless.
var dialsURLHost = map[string]bool{"generic": true, "smtp": true, "ntfy": true, "gotify": true, "matrix": true, "mattermost": true,
	"rocketchat": true, "zulip": true, "bark": true, "googlechat": true}

type target struct {
	host     string
	mustDial bool // a failed lookup refuses the destination (fail closed)
}

// destinationTargets are the hosts a URL can make the server connect to: the URL host and, for
// schemes that take one from the query string (teams ?host=), that value.
func destinationTargets(raw string) []target {
	u, err := url.Parse(raw)
	if err != nil {
		return nil
	}
	scheme, _, _ := strings.Cut(strings.ToLower(u.Scheme), "+")
	var out []target
	if h := destinationHost(raw); h != "" {
		out = append(out, target{h, dialsURLHost[scheme]})
	}
	for _, k := range []string{"host", "server", "url", "domain"} {
		for _, v := range u.Query()[k] {
			v = strings.TrimSpace(v)
			if strings.Contains(v, "://") {
				if pu, err := url.Parse(v); err == nil {
					v = pu.Host
				}
			}
			if h, _, err := net.SplitHostPort(v); err == nil {
				v = h
			}
			if v = strings.ToLower(strings.Trim(strings.TrimSuffix(v, "."), "[]")); v != "" {
				out = append(out, target{v, true})
			}
		}
	}
	return out
}

// checkDestination enforces the policy for a shoutrrr URL. Hosts the sender dials fail closed when
// they cannot be resolved (an attacker-run DNS server could otherwise time out our lookup and
// answer the sender's); token-style services have no real host and are not refused for that.
func checkDestination(ctx context.Context, raw string) error {
	p := loadDestPolicy()
	for _, t := range destinationTargets(raw) {
		if p.hostAllowed(t.host) {
			continue
		}
		if metadataHosts[t.host] || t.host == "localhost" || strings.HasSuffix(t.host, ".localhost") {
			return ErrDestinationNotAllowed
		}
		if ip := net.ParseIP(strings.Trim(t.host, "[]")); ip != nil {
			if bad, _ := p.blockedIP(ip); bad {
				return ErrDestinationNotAllowed
			}
			continue
		}
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		ips, err := LookupIP(lctx, t.host)
		cancel()
		if err != nil || len(ips) == 0 {
			if t.mustDial {
				return ErrDestinationNotAllowed
			}
			continue
		}
		for _, ip := range ips {
			if bad, _ := p.blockedIP(ip); bad {
				return ErrDestinationNotAllowed
			}
		}
	}
	return nil
}

// deliver sends one message through Sender after the destination check. Failures are logged with
// detail and returned as GenericDeliveryError only.
func deliver(ctx context.Context, svcURL, title, body string) error {
	if err := checkDestination(ctx, svcURL); err != nil {
		log.Println("notify: refused destination", destinationHost(svcURL))
		return ops.Invalid(GenericDeliveryError)
	}
	if err := Sender(svcURL, title, body); err != nil {
		log.Println("notify: service send to", destinationHost(svcURL)+":", err)
		return ops.Invalid(GenericDeliveryError)
	}
	return nil
}
