package notifier

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/netguard"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// A notification URL pointing at an internal address is refused before any
// request is sent.
func TestWebhookSender_RefusesInternalURL(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { hits++ }))
	defer srv.Close()

	netguard.SetDefault(netguard.NewPolicy(nil))
	t.Cleanup(netguard.AllowLoopbackForTests)

	err := WebhookSender{}.Send(context.Background(), map[string]any{"url": srv.URL}, domain.AlertContext{})
	if !errors.Is(err, netguard.ErrBlocked) {
		t.Fatalf("Send err = %v; want netguard.ErrBlocked", err)
	}
	if hits != 0 {
		t.Fatalf("internal webhook received %d requests; want 0", hits)
	}
}
