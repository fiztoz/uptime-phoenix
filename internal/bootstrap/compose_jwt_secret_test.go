package bootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// Every Phoenix binary loads Config, and LoadConfig refuses to start without a
// strong JWT_SECRET. A split worker that is not handed the key crash-loops, so
// each compose file must give its worker the same JWT_SECRET source as the
// API that issues the sessions. The Helm side of this guard is
// scripts/helm-jwt-secret-check.py (make helm-validate).
func TestComposeWorkersGetTheAPIJWTSecret(t *testing.T) {
	t.Parallel()
	type composeFile struct {
		Services map[string]struct {
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	load := func(t *testing.T, name string) composeFile {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join("..", "..", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var f composeFile
		if err := yaml.Unmarshal(raw, &f); err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		return f
	}

	// The full split stack: worker and API in one file, same source string.
	split := load(t, "docker-compose.split.yml")
	api, ok := split.Services["uptime-phoenix-api"].Environment["JWT_SECRET"]
	if !ok || api == "" {
		t.Fatal("docker-compose.split.yml: uptime-phoenix-api has no JWT_SECRET")
	}
	worker := split.Services["uptime-phoenix-worker"].Environment["JWT_SECRET"]
	if worker != api {
		t.Fatalf("docker-compose.split.yml: worker JWT_SECRET = %q, want the API's %q", worker, api)
	}

	// make dev-split-worker: the API runs on the host with the Makefile's
	// JWT_SECRET, which DEPS_COMPOSE exports into compose. The worker must
	// read that variable, required, not a literal or a default.
	deps := load(t, "docker-compose.deps.yml")
	depsWorker := deps.Services["uptime-phoenix-worker"].Environment["JWT_SECRET"]
	if depsWorker != api {
		t.Fatalf("docker-compose.deps.yml: worker JWT_SECRET = %q, want %q (the variable the host API also uses)", depsWorker, api)
	}

	// Single-container compose keeps its own required key.
	single := load(t, "docker-compose.yml")
	for name, svc := range single.Services {
		if _, isPhoenix := svc.Environment["DB_DSN"]; isPhoenix && svc.Environment["JWT_SECRET"] != api {
			t.Fatalf("docker-compose.yml: %s JWT_SECRET = %q, want required %q", name, svc.Environment["JWT_SECRET"], api)
		}
	}
}
