package checker

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"

	"github.com/coder/websocket"
)

// WebSocketChecker performs WebSocket upgrade and connectivity checks using coder/websocket.
type WebSocketChecker struct{}

func init() { Register(WebSocketChecker{}) }

// Type returns the monitor type identifier.
func (WebSocketChecker) Type() string { return "websocket" }

// Validate checks that the required config fields are present and valid.
func (WebSocketChecker) Validate(config map[string]any) error {
	url, _ := config["url"].(string)
	if url == "" {
		return fmt.Errorf("url is required")
	}
	if !strings.HasPrefix(url, "ws://") && !strings.HasPrefix(url, "wss://") {
		return fmt.Errorf("url must start with ws:// or wss://")
	}
	return nil
}

// Check performs a WebSocket upgrade handshake against the configured URL.
func (WebSocketChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	url, _ := config["url"].(string)

	// Extract timeout (default 10s).
	timeout := 10.0
	if t, ok := config["timeout"].(float64); ok && t > 0 {
		timeout = t
	}

	// Build optional custom headers.
	headers := http.Header{}
	if rawHeaders, ok := config["headers"].(map[string]any); ok {
		for k, v := range rawHeaders {
			if s, ok := v.(string); ok {
				headers.Set(k, s)
			}
		}
	}

	// Apply timeout via context.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
	defer cancel()

	start := time.Now()
	ws, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: headers,
	})
	latencyMs := time.Since(start).Milliseconds()

	if err != nil {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("websocket handshake failed: %v", err),
		}, nil
	}

	// Clean close.
	_ = ws.Close(websocket.StatusNormalClosure, "check complete")

	return ports.CheckResult{
		Status:    domain.StatusUp,
		LatencyMs: latencyMs,
		Message:   fmt.Sprintf("websocket handshake succeeded: %s", url),
	}, nil
}
