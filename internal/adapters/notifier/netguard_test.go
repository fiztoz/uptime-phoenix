package notifier

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/netguard"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// withEmptyAllowlist removes the package TestMain's loopback allowance for one
// test, so loopback is an ordinary private destination again.
func withEmptyAllowlist(t *testing.T) {
	t.Helper()
	netguard.SetRestricted(netguard.NewPolicy(nil))
	t.Cleanup(netguard.AllowLoopbackForTests)
}

// A notification owned by a non-admin (or with no owner trust attached) that
// points at an internal address is refused before any request is sent.
func TestWebhookSender_RestrictedOwnerCannotReachInternalURL(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	withEmptyAllowlist(t)

	for name, ctx := range map[string]context.Context{
		"unset":      context.Background(),
		"restricted": domain.WithOutboundTrust(context.Background(), domain.OutboundTrustRestricted),
	} {
		err := WebhookSender{}.Send(ctx, map[string]any{"url": srv.URL}, domain.AlertContext{})
		if !errors.Is(err, netguard.ErrBlocked) {
			t.Fatalf("%s: Send err = %v; want netguard.ErrBlocked", name, err)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("internal webhook received %d requests; want 0", n)
	}
}

// An admin-owned notification keeps reaching an internal webhook without any
// allowlist, as it did before the guard existed.
func TestWebhookSender_AdminOwnerReachesInternalURL(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	withEmptyAllowlist(t)

	ctx := domain.WithOutboundTrust(context.Background(), domain.OutboundTrustAdmin)
	if err := (WebhookSender{}).Send(ctx, map[string]any{"url": srv.URL}, domain.AlertContext{}); err != nil {
		t.Fatalf("admin-owned Send: %v", err)
	}
	if n := hits.Load(); n != 1 {
		t.Fatalf("internal webhook received %d requests; want 1", n)
	}
}

// The metadata floor holds for admin-owned notifications too.
func TestWebhookSender_AdminOwnerCannotReachMetadata(t *testing.T) {
	ctx := domain.WithOutboundTrust(context.Background(), domain.OutboundTrustAdmin)
	err := WebhookSender{}.Send(ctx, map[string]any{"url": "http://169.254.169.254/latest/meta-data/"}, domain.AlertContext{})
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("Send err = %v; want netguard.ErrBlocked", err)
	}
}
