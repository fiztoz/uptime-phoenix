package probe

import (
	"strings"
	"unicode/utf8"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func validProbeDisplay(p ConfigProbeDisplay) bool {
	return utf8.ValidString(p.Name) && strings.TrimSpace(p.Name) != "" && utf8.RuneCountInString(p.Name) <= 200 &&
		utf8.ValidString(p.Location) && utf8.RuneCountInString(p.Location) <= 255
}

func resolvedConfigWatchdog(w ConfigWatchdog) domain.ProbeWatchdogSettings {
	return domain.ProbeWatchdogSettings{Enabled: w.Enabled, LostAfterSeconds: w.LostAfterSeconds,
		RecoverAfterSeconds: w.RecoverAfterSeconds, ResendInterval: w.ResendInterval, NotificationIDs: append([]int64{}, w.NotificationIDs...)}
}
