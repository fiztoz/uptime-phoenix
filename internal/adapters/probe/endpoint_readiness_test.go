package probe

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

func readinessLoopback() EndpointPolicy {
	return EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
}

func TestEndpointReadinessWaitsForALateListener(t *testing.T) {
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close() // nothing listens yet
	r := NewEndpointReadiness(readinessLoopback())
	r.poll = 20 * time.Millisecond
	accepted := make(chan struct{})
	go func() {
		time.Sleep(150 * time.Millisecond)
		l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err != nil {
			return
		}
		defer func() { _ = l.Close() }()
		conn, err := l.Accept()
		if err == nil {
			_ = conn.Close()
			close(accepted)
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := r.WaitReady(ctx, "wss://127.0.0.1:"+strconv.Itoa(port)+"/ws/probe/v1"); err != nil {
		t.Fatalf("not ready: %v", err)
	}
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("listener never saw the readiness connection")
	}
}

func TestEndpointReadinessHonorsItsBound(t *testing.T) {
	r := NewEndpointReadiness(readinessLoopback())
	r.poll = 20 * time.Millisecond
	r.dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("refused") }
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := r.WaitReady(ctx, "wss://127.0.0.1:8443/ws/probe/v1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}

func TestEndpointReadinessRefusesForbiddenAndMalformedEndpoints(t *testing.T) {
	dialed := 0
	r := NewEndpointReadiness(EndpointPolicy{})
	r.dial = func(context.Context, string, string) (net.Conn, error) { dialed++; return nil, errors.New("no") }
	r.lookup = func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("169.254.169.254")}, nil
	}
	for _, endpoint := range []string{
		"wss://127.0.0.1:8443/ws/probe/v1",        // loopback needs an explicit CIDR
		"wss://10.0.0.5:8443/ws/probe/v1",         // private needs an explicit CIDR
		"wss://metadata.internal:8443/x",          // resolves to link-local metadata
		"https://203.0.113.9:8443/ws/probe/v1",    // wrong scheme
		"wss://user@203.0.113.9:8443/ws/probe/v1", // userinfo
		"wss://203.0.113.9/ws/probe/v1",           // no port
		"::not a url",
	} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		err := r.WaitReady(ctx, endpoint)
		cancel()
		if err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%s: err %v", endpoint, err)
		}
	}
	if dialed != 0 {
		t.Fatalf("forbidden endpoints were dialed %d times", dialed)
	}
}
