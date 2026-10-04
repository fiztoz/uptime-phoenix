package domain

import (
	"math"
	"slices"
	"time"
)

// ProbeWatchdogSettings is saved operator intent. Revision fences settings edits;
// complete snapshot revisions independently version execution and dependencies.
type ProbeWatchdogSettings struct {
	Revision            int64
	Enabled             bool
	LostAfterSeconds    int32
	RecoverAfterSeconds int32
	ResendInterval      int32 // Minutes; zero disables periodic reminders.
	NotificationIDs     []int64
}

// DefaultProbeWatchdogSettings keeps connection paging opt-in.
func DefaultProbeWatchdogSettings() ProbeWatchdogSettings {
	return ProbeWatchdogSettings{LostAfterSeconds: 90, RecoverAfterSeconds: 30, NotificationIDs: []int64{}}
}

// ValidateProbeWatchdogSettings checks the same bounds as the V1 wire contract.
// Disabled settings retain their dependencies for a later explicit enable.
func ValidateProbeWatchdogSettings(s ProbeWatchdogSettings) error {
	if s.Revision < 0 || s.LostAfterSeconds <= 0 || s.RecoverAfterSeconds <= 0 ||
		s.ResendInterval < 0 || int64(s.ResendInterval) > math.MaxInt64/int64(time.Minute) || len(s.NotificationIDs) > 1000 {
		return ErrValidation
	}
	ids := slices.Clone(s.NotificationIDs)
	slices.Sort(ids)
	for i, id := range ids {
		if id <= 0 || i > 0 && ids[i-1] == id {
			return ErrValidation
		}
	}
	return nil
}

// Timing retains the default suspect threshold and permits every positive loss
// interval in V1. Short custom intervals become suspect halfway to loss.
func (s ProbeWatchdogSettings) Timing() ProbeWatchdogTiming {
	lost := time.Duration(s.LostAfterSeconds) * time.Second
	suspect := 45 * time.Second
	if lost <= suspect {
		suspect = lost / 2
	}
	return ProbeWatchdogTiming{SuspectAfter: suspect, LostAfter: lost, RecoverAfter: time.Duration(s.RecoverAfterSeconds) * time.Second}
}

// ProbeDisplay is authoritative probe metadata for source notification context.
// It contains no endpoint, credential, administrative or monitor identity.
type ProbeDisplay struct {
	Name     string
	Location string
}
