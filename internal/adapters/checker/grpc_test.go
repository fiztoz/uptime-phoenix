package checker

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	"google.golang.org/grpc/health/grpc_health_v1"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func grpcHealthServer(t *testing.T, status grpc_health_v1.HealthCheckResponse_ServingStatus) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	healthServer := health.NewServer()
	healthServer.SetServingStatus("phoenix", status)
	grpc_health_v1.RegisterHealthServer(server, healthServer)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		_ = listener.Close()
	})
	return listener.Addr().String()
}

func TestGRPCChecker_Check_Up(t *testing.T) {
	result, err := (GRPCChecker{}).Check(context.Background(), map[string]any{
		"url":          grpcHealthServer(t, grpc_health_v1.HealthCheckResponse_SERVING),
		"service_name": "phoenix", "timeout": 2.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusUp || !strings.Contains(result.Message, "SERVING") {
		t.Fatalf("result = %+v; want SERVING", result)
	}
}

func TestGRPCChecker_Check_Down(t *testing.T) {
	result, err := (GRPCChecker{}).Check(context.Background(), map[string]any{
		"url":          grpcHealthServer(t, grpc_health_v1.HealthCheckResponse_NOT_SERVING),
		"service_name": "phoenix", "timeout": 2.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown || !strings.Contains(result.Message, "NOT_SERVING") {
		t.Fatalf("result = %+v; want NOT_SERVING", result)
	}
}

func TestGRPCChecker_Check_Timeout(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = listener.Close() }()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		time.Sleep(2 * time.Second)
	}()

	result, err := (GRPCChecker{}).Check(context.Background(), map[string]any{
		"url": listener.Addr().String(), "timeout": 1.0,
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
