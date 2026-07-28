package checker

import (
	"context"
	"fmt"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// GRPCChecker performs gRPC health check protocol queries.
type GRPCChecker struct{}

func init() { Register(GRPCChecker{}) }

func (GRPCChecker) Type() string { return "grpc" }

// Validate checks that url (host:port) is present.
func (GRPCChecker) Validate(config map[string]any) error {
	url, _ := config["url"].(string)
	if url == "" {
		return fmt.Errorf("url is required (e.g. \"host:50051\")")
	}
	return nil
}

// Check performs a gRPC health check against the target server.
// Config fields:
//   - url (required, string) — target gRPC server address, e.g. "host:50051"
//   - service_name (optional, string, default "") — gRPC service name for health check;
//     empty string means the server's overall health status
//   - tls (optional, bool, default false) — whether to use TLS; when false, uses
//     insecure.NewCredentials()
//   - timeout (optional, float64, default 10) — timeout in seconds
//
// Never returns an error — all failures are returned as StatusDown with the error in Message.
func (GRPCChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	// Extract url.
	url, _ := config["url"].(string)
	if url == "" {
		return ports.CheckResult{Status: domain.StatusDown, Message: "url is required"}, nil
	}

	// Service name: optional, defaults to empty (server-level health).
	serviceName, _ := config["service_name"].(string)

	// TLS: optional, defaults to false.
	tls, _ := config["tls"].(bool)

	// Timeout: default 10 seconds, minimum 1 second.
	timeoutSec := 10.0
	if timeoutVal, ok := config["timeout"]; ok {
		if tf, ok := timeoutVal.(float64); ok && tf > 0 {
			timeoutSec = tf
		}
	}
	if timeoutSec < 1 {
		timeoutSec = 1
	}

	checkCtx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
	defer cancel()

	// Build dial options. grpc.NewClient is non-blocking (lazy); an
	// unreachable server surfaces as a failure on the health RPC below,
	// which is bounded by the checkCtx deadline.
	dialOpts := []grpc.DialOption{}

	if tls {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(nil))
	} else {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Dial the gRPC server.
	start := time.Now()
	conn, err := grpc.NewClient(url, dialOpts...)
	latencyMs := time.Since(start).Milliseconds()

	if err != nil {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("failed to dial gRPC server: %s", err.Error()),
		}, nil
	}
	defer func() { _ = conn.Close() }()

	// Create the gRPC health client and perform the check.
	healthClient := grpc_health_v1.NewHealthClient(conn)
	healthCheckCtx, healthCancel := context.WithTimeout(checkCtx, time.Duration(timeoutSec*float64(time.Second))/2)
	defer healthCancel()

	healthStart := time.Now()
	resp, err := healthClient.Check(healthCheckCtx, &grpc_health_v1.HealthCheckRequest{Service: serviceName})
	latencyMs = time.Since(healthStart).Milliseconds()

	if err != nil {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("gRPC health check failed: %s", err.Error()),
		}, nil
	}

	// Map the serving status.
	switch resp.Status {
	case grpc_health_v1.HealthCheckResponse_SERVING:
		msg := "gRPC server is SERVING"
		if serviceName != "" {
			msg = fmt.Sprintf("gRPC service %q is SERVING", serviceName)
		}
		return ports.CheckResult{
			Status:    domain.StatusUp,
			LatencyMs: latencyMs,
			Message:   msg,
		}, nil

	case grpc_health_v1.HealthCheckResponse_NOT_SERVING:
		msg := "gRPC server is NOT_SERVING"
		if serviceName != "" {
			msg = fmt.Sprintf("gRPC service %q is NOT_SERVING", serviceName)
		}
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   msg,
		}, nil

	case grpc_health_v1.HealthCheckResponse_SERVICE_UNKNOWN:
		msg := fmt.Sprintf("gRPC service %q is unknown", serviceName)
		if serviceName == "" {
			msg = "gRPC server health is unknown (no server-level health endpoint)"
		}
		return ports.CheckResult{
			Status:    domain.StatusPending,
			LatencyMs: latencyMs,
			Message:   msg,
		}, nil

	default: // UNKNOWN (0) or any other value
		return ports.CheckResult{
			Status:    domain.StatusPending,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("gRPC health status: %s", resp.Status.String()),
		}, nil
	}
}
