package netguard

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// floorOnly dials proxy endpoints. It has no shared transport, so it is safe
// to reference from the transports themselves.
var floorOnly = &Policy{trusted: true}

// proxyGuard wraps a transport's Proxy function so a proxy cannot carry a
// request to a destination the policy refuses.
//
// With a proxy, the guarded dial reaches the proxy, and the proxy dials the
// target, so the dial hook never sees the target. The guard therefore:
//
//   - checks the target host against the policy before handing the request
//     to the proxy (CheckHost: a resolve-and-check pre-check, since the proxy
//     resolves the name itself), and
//   - dials the proxy endpoint itself with the floor only. The proxy is
//     chosen by the operator (HTTP_PROXY / HTTPS_PROXY / NO_PROXY) or by an
//     admin (a monitor proxy), never by a restricted owner, so an in-cluster
//     egress proxy keeps working for non-admin monitors.
type proxyGuard struct {
	policy  *Policy
	next    func(*http.Request) (*url.URL, error)
	proxies sync.Map // canonical proxy host:port -> struct{}
}

func (g *proxyGuard) proxy(req *http.Request) (*url.URL, error) {
	u, err := g.next(req)
	if err != nil || u == nil {
		return u, err
	}
	if err := g.policy.CheckHost(req.Context(), req.URL.Hostname()); err != nil {
		return nil, err
	}
	g.proxies.Store(canonicalProxyAddr(u), struct{}{})
	return u, nil
}

func (g *proxyGuard) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if _, isProxy := g.proxies.Load(addr); isProxy {
		return floorOnly.DialContext(ctx, network, addr)
	}
	return g.policy.DialContext(ctx, network, addr)
}

// canonicalProxyAddr mirrors net/http: the dial address of a proxy URL is its
// host and port, with the scheme's default port when none is given.
func canonicalProxyAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			port = "443"
		case "socks5", "socks5h":
			port = "1080"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// transportWithProxy returns a stdlib-default transport whose direct dials are
// guarded by p and whose proxied requests go through proxyFn under the
// proxyGuard rules.
func (p *Policy) transportWithProxy(proxyFn func(*http.Request) (*url.URL, error)) *http.Transport {
	g := &proxyGuard{policy: p, next: proxyFn}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = g.proxy
	t.DialContext = g.dial
	return t
}

// Transport returns a fresh guarded transport. It keeps the stdlib's
// HTTP(S)_PROXY / NO_PROXY handling, under the proxyGuard rules. Callers that
// change TLS settings use this; everyone else uses SharedTransport.
func (p *Policy) Transport() *http.Transport {
	return p.transportWithProxy(http.ProxyFromEnvironment)
}

// ProxyTransport returns a fresh guarded transport that sends every request
// through proxyURL (http, https or socks5), under the proxyGuard rules.
func (p *Policy) ProxyTransport(proxyURL *url.URL) (*http.Transport, error) {
	switch strings.ToLower(proxyURL.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return nil, fmt.Errorf("unsupported proxy protocol %q", proxyURL.Scheme)
	}
	return p.transportWithProxy(http.ProxyURL(proxyURL)), nil
}

type sharedTransport struct{ t *http.Transport }

func newSharedTransport(p *Policy) *sharedTransport { return &sharedTransport{t: p.Transport()} }

// SharedTransport returns one pooled guarded transport per policy. Each tier
// has its own pool, so a connection opened for an admin-owned check is never
// reused by a restricted one.
func (p *Policy) SharedTransport() *http.Transport { return p.shared().t }

// Client returns an http.Client on the shared guarded transport.
func (p *Policy) Client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: p.SharedTransport(), Timeout: timeout}
}
