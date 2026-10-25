//go:build linux || darwin

package provisioning

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestProbeBinarySourceRefusesNonregular(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{dir, fifo, "/dev/null", filepath.Join(dir, "missing")} {
		f, err := OpenProbeBinarySource(t.Context(), p)
		if f != nil || !errors.Is(err, domain.ErrProbeArtifactInput) {
			t.Fatal("nonregular source opened")
		}
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenProbeBinarySource(t.Context(), empty); f != nil || err == nil {
		t.Fatal("empty source opened")
	}
}
