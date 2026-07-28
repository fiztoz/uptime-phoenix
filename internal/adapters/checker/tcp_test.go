package checker

import (
	"context"
	"net"
	"testing"
	"time"
)

// canReachTCP tests whether a TCP address is reachable for test preflight.
func canReachTCP(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func TestTCPChecker_Type(t *testing.T) {
	c := TCPChecker{}
	if got := c.Type(); got != "tcp" {
		t.Errorf("Type() = %q, want %q", got, "tcp")
	}
}

func TestTCPChecker_Validate(t *testing.T) {
	c := TCPChecker{}

	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{
			name:    "missing hostname",
			config:  map[string]any{"port": float64(80)},
			wantErr: true,
		},
		{
			name:    "empty hostname",
			config:  map[string]any{"hostname": "", "port": float64(80)},
			wantErr: true,
		},
		{
			name:    "missing port",
			config:  map[string]any{"hostname": "example.com"},
			wantErr: true,
		},
		{
			name:    "port zero",
			config:  map[string]any{"hostname": "example.com", "port": float64(0)},
			wantErr: true,
		},
		{
			name:    "port out of range",
			config:  map[string]any{"hostname": "example.com", "port": float64(99999)},
			wantErr: true,
		},
		{
			name:    "valid config",
			config:  map[string]any{"hostname": "example.com", "port": float64(80)},
			wantErr: false,
		},
		{
			name:    "valid config with timeout",
			config:  map[string]any{"hostname": "example.com", "port": float64(443), "timeout": float64(5)},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := c.Validate(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestTCPChecker_Check_GoogleDNS(t *testing.T) {
	if !canReachTCP("8.8.8.8:53") {
		t.Skip("8.8.8.8:53 is not reachable (firewall or network restriction)")
	}

	c := TCPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"hostname": "8.8.8.8",
		"port":     float64(53),
		"timeout":  float64(10),
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Errorf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
	if result.LatencyMs < 0 {
		t.Errorf("Check() latency = %d, want >= 0", result.LatencyMs)
	}
	t.Logf("8.8.8.8:53: status=%v latency=%dms", result.Status, result.LatencyMs)
}

func TestTCPChecker_Check_ClosedPort(t *testing.T) {
	c := TCPChecker{}
	// Connect to localhost on a port that is very unlikely to be listening.
	// This should get a connection refused (DOWN), not a timeout.
	result, err := c.Check(context.Background(), map[string]any{
		"hostname": "127.0.0.1",
		"port":     float64(19999),
		"timeout":  float64(3),
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN (message: %s)", result.Status, result.Message)
	}
}

func TestTCPChecker_Check_InvalidHost(t *testing.T) {
	c := TCPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"hostname": "this-host-definitely-does-not-exist.invalid",
		"port":     float64(80),
		"timeout":  float64(5),
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN (message: %s)", result.Status, result.Message)
	}
}

func TestTCPChecker_Check_UnroutableIP(t *testing.T) {
	c := TCPChecker{}
	// Use 10.255.255.1 which is in private range but typically unreachable.
	// With a short timeout, this should fail.
	result, err := c.Check(context.Background(), map[string]any{
		"hostname": "10.255.255.1",
		"port":     float64(81),
		"timeout":  float64(2),
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN (message: %s)", result.Status, result.Message)
	}
}
