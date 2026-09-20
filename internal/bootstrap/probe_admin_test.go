package bootstrap

import (
	"bytes"
	"testing"
)

func TestProbeAdminUnknownCommandCannotReportSuccess(t *testing.T) {
	for _, command := range []string{"", "register|enroll", "future", "status|"} {
		var out, stderr bytes.Buffer
		if code := RunProbeAdmin(t.Context(), Config{}, []string{command}, &out, &stderr); code != 2 || out.Len() != 0 {
			t.Fatalf("unknown command %q reported success or produced a receipt", command)
		}
	}
}

func TestProbeRuntimeOptInRequiresKey(t *testing.T) {
	if err := Run(Config{ProbesEnabled: true, JWTExpireH: 24}); err == nil {
		t.Fatal("remote runtime started without protected installation authority")
	}
}
