package checker

import (
	"context"
	"fmt"
	"time"

	"github.com/moby/moby/client"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// DockerChecker checks Docker container health via the Docker API.
type DockerChecker struct{}

func init() { Register(DockerChecker{}) }

func (DockerChecker) Type() string { return "docker" }

// Validate checks that container is present in the config.
func (DockerChecker) Validate(config map[string]any) error {
	containerID, ok := config["container"].(string)
	if !ok || containerID == "" {
		return fmt.Errorf("container is required and must be a string")
	}
	return nil
}

// Check inspects a Docker container and reports whether it is running.
// Config fields:
//   - docker_daemon (optional, string, default "unix:///var/run/docker.sock") — Docker daemon address
//   - container (required, string) — container name or ID
//   - timeout (optional, float64, default 10) — timeout in seconds
//
// Never returns an error — all failures are returned as StatusDown with the error in Message.
func (DockerChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	// Extract config.
	dockerDaemon, _ := config["docker_daemon"].(string)
	if dockerDaemon == "" {
		dockerDaemon = "unix:///var/run/docker.sock"
	}

	containerID, _ := config["container"].(string)
	if containerID == "" {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: "container is required",
		}, nil
	}

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
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
	defer cancel()

	// Create Docker client.
	start := time.Now()
	cli, err := client.New(client.WithHost(dockerDaemon))
	if err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("failed to create Docker client: %s", err.Error()),
		}, nil
	}
	defer func() { _ = cli.Close() }()

	// Inspect the container.
	insp, err := cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	latencyMs := time.Since(start).Milliseconds()

	if err != nil {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("failed to inspect container: %s", err.Error()),
		}, nil
	}

	status := string(insp.Container.State.Status)

	// Check if the container is running by comparing the string status.
	// We compare the string value rather than the typed constant because the
	// state field may come through as untyped JSON.
	if status != "running" {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("container status is %q (expected running)", status),
		}, nil
	}

	return ports.CheckResult{
		Status:    domain.StatusUp,
		LatencyMs: latencyMs,
		Message:   "container is running",
	}, nil
}
