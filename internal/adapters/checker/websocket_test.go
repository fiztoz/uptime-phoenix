package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestWebSocketChecker_Check_Up(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Phoenix-Test") != "present" {
			http.Error(w, "missing header", http.StatusBadRequest)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close(websocket.StatusNormalClosure, "test complete")
	}))
	defer server.Close()

	result, err := (WebSocketChecker{}).Check(context.Background(), map[string]any{
		"url":     strings.Replace(server.URL, "http://", "ws://", 1),
		"headers": map[string]any{"X-Phoenix-Test": "present"}, "timeout": 2.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusUp || !strings.Contains(result.Message, "succeeded") {
		t.Fatalf("result = %+v; want successful upgrade", result)
	}
}

func TestWebSocketChecker_Check_Down(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not a websocket", http.StatusBadRequest)
	}))
	defer server.Close()
	result, err := (WebSocketChecker{}).Check(context.Background(), map[string]any{
		"url": strings.Replace(server.URL, "http://", "ws://", 1), "timeout": 1.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown || !strings.Contains(result.Message, "handshake failed") {
		t.Fatalf("result = %+v; want failed handshake", result)
	}
}

func TestWebSocketChecker_Check_Timeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
			http.Error(w, "late", http.StatusGatewayTimeout)
		}
	}))
	defer server.Close()
	result, err := (WebSocketChecker{}).Check(context.Background(), map[string]any{
		"url": strings.Replace(server.URL, "http://", "ws://", 1), "timeout": 0.05,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown {
		t.Fatalf("status = %s; want DOWN", result.Status)
	}
	message := strings.ToLower(result.Message)
	if !strings.Contains(message, "deadline") && !strings.Contains(message, "timeout") {
		t.Fatalf("message = %q; want timeout diagnostic", result.Message)
	}
}
