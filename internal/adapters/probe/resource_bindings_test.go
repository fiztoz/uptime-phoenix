package probe

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func resourceFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resources.json")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResourceBindingsRejectInvalidFiles(t *testing.T) {
	for name, content := range map[string]string{
		"null": `null`, "unknown field": `[{"binding_key":"docker","kind":"docker_api","endpoint":"tcp://127.0.0.1:2375","secret":"oops"}]`,
		"duplicate field":   `[{"binding_key":"one","binding_key":"two","kind":"docker_socket","endpoint":"unix:///tmp/a.sock"}]`,
		"duplicate key":     `[{"binding_key":"docker","kind":"docker_socket","endpoint":"unix:///tmp/a.sock"},{"binding_key":"docker","kind":"docker_api","endpoint":"tcp://127.0.0.1:2375"}]`,
		"invalid key":       `[{"binding_key":"/tmp/docker","kind":"docker_socket","endpoint":"unix:///tmp/a.sock"}]`,
		"credentials":       `[{"binding_key":"docker","kind":"docker_api","endpoint":"tcp://user:private-secret@host:2375"}]`,
		"relative socket":   `[{"binding_key":"docker","kind":"docker_socket","endpoint":"unix:relative.sock"}]`,
		"kind mismatch":     `[{"binding_key":"docker","kind":"docker_socket","endpoint":"tcp://host:2375"}]`,
		"tls unsupported":   `[{"binding_key":"docker","kind":"docker_api","endpoint":"https://host:2376"}]`,
		"query":             `[{"binding_key":"docker","kind":"docker_api","endpoint":"tcp://host:2375?secret=private-secret"}]`,
		"trailing document": `[] []`, "oversize": strings.Repeat(" ", maxResourceFileBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := LoadResourceBindings(t.Context(), resourceFile(t, content)); got != nil || err == nil || strings.Contains(err.Error(), "private-secret") {
				t.Fatalf("invalid map accepted or leaked: %v", err)
			}
		})
	}
	path := resourceFile(t, `[]`)
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadResourceBindings(t.Context(), path); err == nil {
		t.Fatal("public file accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(resourceFile(t, `[]`), link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadResourceBindings(t.Context(), link); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestDockerResourceBindingExecution(t *testing.T) {
	for _, kind := range []string{"docker_api", "docker_socket"} {
		t.Run(kind, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/_ping" {
					w.Header().Set("API-Version", "1.55")
					return
				}
				if !strings.HasSuffix(r.URL.Path, "/containers/phoenix/json") {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"Id":"phoenix","State":{"Status":"running"}}`))
			})
			server := httptest.NewUnstartedServer(handler)
			endpoint := "tcp://" + server.Listener.Addr().String()
			if kind == "docker_socket" {
				_ = server.Listener.Close()
				// macOS limits sockaddr_un paths to 104 bytes.
				dir, err := os.MkdirTemp("/tmp", "phx-docker-")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir) })
				path := filepath.Join(dir, "docker.sock")
				server.Listener, err = net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				endpoint = "unix://" + path
			}
			server.Start()
			defer server.Close()
			file := resourceFile(t, `[{"binding_key":"local-docker","kind":"`+kind+`","endpoint":"`+endpoint+`"}]`)
			resources, err := LoadResourceBindings(t.Context(), file)
			if err != nil {
				t.Fatal(err)
			}
			caps := PullCheckerCapabilities(checker.RegisteredPullTypes(), checker.Get, false, resources)
			if !slices.Contains(caps, "checker.docker.v1") {
				t.Fatal("configured Docker withheld")
			}
			wire, _ := json.Marshal(resources.Inventory())
			if bytes.Contains(wire, []byte(endpoint)) {
				t.Fatal("endpoint advertised")
			}
			s := m2Config(t)
			s.Assignments[0].Monitor.Type = "docker"
			s.Assignments[0].Monitor.Config = json.RawMessage(`{"container":"phoenix"}`)
			s.Assignments[0].RequiredCapabilities = []string{"checker.docker.v1"}
			s.Assignments[0].ProxyBindingKey = nil
			s.Assignments[0].ResourceBindings = resources.Inventory()
			target := domain.ProbeConfigTarget{HubID: s.HubID, ProbeID: s.ProbeID}
			document := configBytes(t, s)
			hub, err := NewHubConfigDecoder(checker.Get, notifier.Get).DecodeEdge(t.Context(), document, target)
			if err != nil || hub.Assignments[0].Monitor.Config["docker_daemon"] != nil {
				t.Fatal("hub resolved local resource", err)
			}
			resolved, err := NewEdgeConfigDecoder(checker.Get, notifier.Get, resources).DecodeEdge(t.Context(), document, target)
			if err != nil {
				t.Fatal(err)
			}
			result, err := (checker.DockerChecker{}).Check(t.Context(), resolved.Assignments[0].Monitor.Config)
			if err != nil || result.Status != domain.StatusUp {
				t.Fatalf("bound Docker not executed: %+v %v", result, err)
			}
			if _, err := NewEdgeConfigDecoder(checker.Get, notifier.Get).DecodeEdge(t.Context(), document, target); !errors.Is(err, ErrUnsupportedCapability) {
				t.Fatal("missing binding accepted", err)
			}
			s.Assignments[0].ResourceBindings[0].BindingKey = "unknown"
			if _, err := NewEdgeConfigDecoder(checker.Get, notifier.Get, resources).DecodeEdge(t.Context(), configBytes(t, s), target); !errors.Is(err, ErrUnsupportedCapability) {
				t.Fatal("unknown binding accepted", err)
			}
			s.Assignments[0].ResourceBindings = resources.Inventory()
			s.Assignments[0].Monitor.Config = json.RawMessage(`{"container":"phoenix","docker_daemon":"unix:///hub.sock"}`)
			if _, err := NewEdgeConfigDecoder(checker.Get, notifier.Get, resources).DecodeEdge(t.Context(), configBytes(t, s), target); err == nil {
				t.Fatal("hub endpoint accepted on wire")
			}
		})
	}
}

func TestDockerRemoteEncodingStripsHubEndpoint(t *testing.T) {
	d := remoteDefinitionFixture()
	d.Assignments[0].Monitor.Type = "docker"
	d.Assignments[0].Monitor.Config = map[string]any{"container": "phoenix", "docker_daemon": "unix:///private/hub.sock", "other_secret": "private-secret"}
	d.Assignments[0].ResourceBinding = &domain.ProbeResourceBinding{BindingKey: "docker", Kind: "docker_socket"}
	document, err := (RemoteConfigEncoder{}).EncodeRemote(d)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(document, []byte("hub.sock")) || bytes.Contains(document, []byte("private-secret")) {
		t.Fatal("hub endpoint leaked")
	}
	if d.Assignments[0].Monitor.Config["docker_daemon"] != "unix:///private/hub.sock" {
		t.Fatal("local config mutated")
	}
}

func TestHandshakeRequiresResourceBindings(t *testing.T) {
	hello := readFixture(t, "valid", "hello-active.json")
	welcome := readFixture(t, "valid", "welcome-active.json")
	binding := ResourceBinding{BindingKey: "docker-local", Kind: "docker_socket"}
	hello = changePayload(t, hello, map[string]any{"resource_bindings": []ResourceBinding{binding}})
	expect := handshakeExpectation()
	expect.RequiredResourceBindings = []ResourceBinding{binding}
	if _, err := ValidateHandshake(hello, welcome, expect); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ResourceBinding{{BindingKey: "unknown", Kind: "docker_socket"}, {BindingKey: binding.BindingKey, Kind: "docker_api"}} {
		expect.RequiredResourceBindings = []ResourceBinding{bad}
		if _, err := ValidateHandshake(hello, welcome, expect); !errors.Is(err, ErrUnsupportedCapability) {
			t.Fatal("missing/wrong binding accepted", err)
		}
	}
}
