//go:build linux

package bootstrap

import (
	"os"
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
