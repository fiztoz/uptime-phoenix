package services

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestMonitorService_NormalizesHTTPURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		kind string
		url  string
		want string
	}{
		{name: "bare host defaults to HTTPS", kind: "http", url: "example.com/health", want: "https://example.com/health"},
		{name: "bare local host and port defaults to HTTPS", kind: "http", url: "localhost:8080", want: "https://localhost:8080"},
		{name: "scheme-relative URL defaults to HTTPS", kind: "http", url: "//example.com/status", want: "https://example.com/status"},
		{name: "explicit HTTP is preserved", kind: "http", url: "http://example.com", want: "http://example.com"},
		{name: "explicit HTTPS is preserved", kind: "http", url: "https://example.com", want: "https://example.com"},
		{name: "non-HTTP monitor is untouched", kind: "websocket", url: "example.com/socket", want: "example.com/socket"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := newCloneFakeMonitorRepo()
			svc := NewMonitorService(repo, newFakeBus())
			monitor := &domain.Monitor{
				UserID: 1,
				Name:   tc.name,
				Type:   tc.kind,
				Config: map[string]any{"url": tc.url},
			}
			if err := svc.Create(context.Background(), monitor); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if got := monitor.Config["url"]; got != tc.want {
				t.Fatalf("url = %q; want %q", got, tc.want)
			}

			monitor.Config["url"] = tc.url
			if err := svc.Update(context.Background(), monitor); err != nil {
				t.Fatalf("Update: %v", err)
			}
			if got := monitor.Config["url"]; got != tc.want {
				t.Fatalf("updated url = %q; want %q", got, tc.want)
			}
		})
	}
}
