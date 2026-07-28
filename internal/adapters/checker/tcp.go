package checker

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TCPChecker performs TCP port connectivity checks.
type TCPChecker struct{}

func init() { Register(TCPChecker{}) }

// Type returns the monitor type identifier.
func (TCPChecker) Type() string { return "tcp" }

// Validate checks that the required config fields are present and valid.
func (TCPChecker) Validate(config map[string]any) error {
	hostname, _ := config["hostname"].(string)
	if hostname == "" {
		return fmt.Errorf("hostname is required")
	}

	port, ok := config["port"]
	if !ok || port == nil {
		return fmt.Errorf("port is required")
	}

	// Port arrives as float64 from JSON unmarshaling.
	portNum, ok := port.(float64)
	if !ok {
		return fmt.Errorf("port must be a number")
	}
	if portNum < 1 || portNum > 65535 {
		return fmt.Errorf("port must be between 1 and 65535")
	}

	return nil
}

// Check performs a TCP connection probe to the configured host and port.
func (TCPChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	hostname, _ := config["hostname"].(string)
	portNum, _ := config["port"].(float64)
	port := int(portNum)

	// Extract timeout (default 10s).
	timeout := 10.0
	if t, ok := config["timeout"].(float64); ok && t > 0 {
		timeout = t
	}

	addr := fmt.Sprintf("%s:%d", hostname, port)

	// Apply timeout via context.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout*float64(time.Second)))
	defer cancel()

	start := time.Now()
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	latencyMs := time.Since(start).Milliseconds()

	if err != nil {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("tcp connect failed: %v", err),
		}, nil
	}

	_ = conn.Close()

	return ports.CheckResult{
		Status:    domain.StatusUp,
		LatencyMs: latencyMs,
		Message:   fmt.Sprintf("tcp connect success: %s", addr),
	}, nil
}
