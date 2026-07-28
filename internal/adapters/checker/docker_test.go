package checker

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func dockerTestDaemon(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return strings.Replace(server.URL, "http://", "tcp://", 1)
}

func TestDockerChecker_Check_Up(t *testing.T) {
	daemon := dockerTestDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ping" {
			w.Header().Set("API-Version", "1.55")
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.URL.Path != "/v1.55/containers/phoenix/json" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"Id":"phoenix","State":{"Status":"running"}}`)
	}))
	result, err := (DockerChecker{}).Check(context.Background(), map[string]any{
		"docker_daemon": daemon, "container": "phoenix", "timeout": 2.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusUp || result.Message != "container is running" {
		t.Fatalf("result = %+v; want running container", result)
	}
}

func TestDockerChecker_Check_Down(t *testing.T) {
	daemon := dockerTestDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"Id":"phoenix","State":{"Status":"exited"}}`)
	}))
	result, err := (DockerChecker{}).Check(context.Background(), map[string]any{
		"docker_daemon": daemon, "container": "phoenix", "timeout": 2.0,
	})
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if result.Status != domain.StatusDown || !strings.Contains(result.Message, "exited") {
		t.Fatalf("result = %+v; want exited container DOWN", result)
	}
}

func TestDockerChecker_Check_Timeout(t *testing.T) {
	daemon := dockerTestDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
			http.Error(w, "late", http.StatusGatewayTimeout)
		}
	}))
	result, err := (DockerChecker{}).Check(context.Background(), map[string]any{
		"docker_daemon": daemon, "container": "phoenix", "timeout": 1.0,
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
