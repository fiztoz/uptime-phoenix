//go:build linux

package bootstrap

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// The real gatherer only reads; on any Linux host it must produce a valid
// report naming this kernel, this architecture and this effective uid.
func TestGatherHostProbeInstallReportReadsThisHost(t *testing.T) {
	report, err := gatherHostProbeInstallReport(t.Context())
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if !report.Valid() || report.Kernel != "Linux" {
		t.Fatalf("report %+v", report)
	}
	if got := domain.ProbeInstallHostArchitecture(report.Machine); got != runtime.GOARCH {
		t.Fatalf("machine %q maps to %q, want %q", report.Machine, got, runtime.GOARCH)
	}
	if report.Root != (os.Geteuid() == 0) {
		t.Fatalf("root=%v euid=%d", report.Root, os.Geteuid())
	}
	if !report.Root && report.Authority {
		t.Fatal("authority reported without root")
	}
}

// DockerCLI needs the daemon socket to be a socket itself: a regular file, a
// directory, a link to a socket or nothing at all never qualifies.
func TestProbeInstallSocketRequiresARealSocket(t *testing.T) {
	// Unix socket paths are short-limited, so keep the directory name short.
	dir, err := os.MkdirTemp("", "phx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		path string
		want bool
	}{
		"socket":              {sock, true},
		"regular file":        {file, false},
		"directory":           {dir, false},
		"symlink to a socket": {link, false},
		"missing":             {filepath.Join(dir, "absent"), false},
	} {
		if got := probeInstallSocket(tc.path); got != tc.want {
			t.Errorf("%s: probeInstallSocket=%v, want %v", name, got, tc.want)
		}
	}
	if probeInstallDockerSocket != "/var/run/docker.sock" {
		t.Fatalf("docker socket path drifted: %q", probeInstallDockerSocket)
	}
}
