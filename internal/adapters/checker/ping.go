package checker

import (
	"context"
	"fmt"
	"time"

	probing "github.com/prometheus-community/pro-bing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// PingChecker performs ICMP ping checks using pro-bing.
type PingChecker struct{}

func init() { Register(PingChecker{}) }

func (PingChecker) Type() string { return "ping" }

func (PingChecker) Validate(config map[string]any) error {
	hostname, _ := config["hostname"].(string)
	if hostname == "" {
		return fmt.Errorf("hostname is required")
	}
	return nil
}

func (PingChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	hostname, _ := config["hostname"].(string)
	if hostname == "" {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: "hostname is required",
		}, nil
	}

	count := 3
	if c, ok := config["count"].(float64); ok && c > 0 {
		count = int(c)
	}

	timeoutSec := 10.0
	if t, ok := config["timeout"].(float64); ok && t > 0 {
		timeoutSec = t
	}

	// Create a context with the configured timeout.
	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
	defer cancel()

	pinger, err := probing.NewPinger(hostname)
	if err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("failed to create pinger: %v", err),
		}, nil
	}

	pinger.Count = count
	pinger.Timeout = time.Duration(timeoutSec * float64(time.Second))
	pinger.SetPrivileged(false)

	if err = pinger.RunWithContext(checkCtx); err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("ping failed: %v. On Linux try: sysctl -w net.ipv4.ping_group_range=\"0 2147483647\"", err),
		}, nil
	}

	stats := pinger.Statistics()
	latencyMs := stats.AvgRtt.Milliseconds()

	var msg string
	if stats.PacketsRecv > 0 {
		msg = fmt.Sprintf("sent=%d received=%d loss=%.1f%% min=%v avg=%v max=%v",
			stats.PacketsSent, stats.PacketsRecv, stats.PacketLoss,
			stats.MinRtt, stats.AvgRtt, stats.MaxRtt)
	} else {
		msg = fmt.Sprintf("sent=%d received=%d loss=%.1f%%",
			stats.PacketsSent, stats.PacketsRecv, stats.PacketLoss)
	}

	if stats.PacketLoss >= 100 {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("100%% packet loss - %s", msg),
		}, nil
	}

	return ports.CheckResult{
		Status:    domain.StatusUp,
		LatencyMs: latencyMs,
		Message:   msg,
	}, nil
}
