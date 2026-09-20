package bootstrap

import (
	"bytes"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
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

func TestProbeAdminWatchdogSettingsRejectTruncationAndAmbiguity(t *testing.T) {
	for _, test := range []struct {
		lost, recovery, resend int64
		channels               string
	}{
		{1<<32 + 90, 30, 0, ""}, {90, -1, 0, ""}, {90, 30, 1 << 32, ""}, {90, 30, 153722868, ""},
		{90, 30, 0, "1,1"}, {90, 30, 0, "01"}, {90, 30, 0, "1,"}, {90, 30, 0, "-1"},
	} {
		if _, err := probeAdminWatchdogSettings(true, test.lost, test.recovery, test.resend, test.channels); !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("bad settings accepted: %+v %v", test, err)
		}
	}
	s, err := probeAdminWatchdogSettings(true, 90, 30, 5, "2,1")
	if err != nil || !s.Enabled || s.ResendInterval != 5 || len(s.NotificationIDs) != 2 {
		t.Fatal("valid complete settings rejected", err)
	}
}
