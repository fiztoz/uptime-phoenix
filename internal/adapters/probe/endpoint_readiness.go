package probe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"time"
)

const (
	endpointReadinessDialTimeout = 2 * time.Second
	endpointReadinessPoll        = 500 * time.Millisecond
)

// EndpointReadiness waits until a probe's runtime endpoint accepts TCP
// connections. It validates every resolved destination with the endpoint
// policy, connects without sending anything, and closes immediately. A
// listening socket is not authentication: TLS pinning stays with enrollment.
type EndpointReadiness struct {
	policy EndpointPolicy
	dial   func(ctx context.Context, network, address string) (net.Conn, error)
	lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	poll   time.Duration
}

// NewEndpointReadiness captures a private copy of the destination policy.
func NewEndpointReadiness(policy EndpointPolicy) *EndpointReadiness {
	policy.AllowedCIDRs = slices.Clone(policy.AllowedCIDRs)
	dialer := &net.Dialer{Timeout: endpointReadinessDialTimeout}
	return &EndpointReadiness{
		policy: policy,
		dial:   dialer.DialContext,
		lookup: func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		},
		poll: endpointReadinessPoll,
	}
}

// WaitReady returns nil once one policy-permitted address of endpoint accepts
// a TCP connection, or the context's error (or a fixed refusal for an invalid
// or forbidden endpoint) otherwise. The caller bounds the wait.
func (r *EndpointReadiness) WaitReady(ctx context.Context, endpoint string) error {
	if r == nil || ctx == nil {
		return errors.New("endpoint readiness unavailable")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "wss" || u.User != nil || u.Hostname() == "" || u.Port() == "" {
		return errors.New("invalid probe endpoint")
	}
	host, port := u.Hostname(), u.Port()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		addrs, err := r.resolve(ctx, host)
		if err != nil {
			return err
		}
		for _, addr := range addrs {
			conn, err := r.dial(ctx, "tcp", net.JoinHostPort(addr.String(), port))
			if err == nil {
				_ = conn.Close()
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.poll):
		}
	}
}

// resolve returns only policy-permitted addresses; none is a refusal.
func (r *EndpointReadiness) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else {
		lctx, cancel := context.WithTimeout(ctx, endpointReadinessDialTimeout)
		addrs, err = r.lookup(lctx, host)
		cancel()
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, ctxErr
			}
			return nil, errors.New("probe endpoint unresolvable")
		}
	}
	permitted := addrs[:0:0]
	for _, addr := range addrs {
		if ValidateDestination(addr, r.policy) == nil {
			permitted = append(permitted, addr.Unmap())
		}
	}
	if len(permitted) == 0 {
		return nil, errors.New("probe endpoint forbidden by policy")
	}
	return permitted, nil
}
