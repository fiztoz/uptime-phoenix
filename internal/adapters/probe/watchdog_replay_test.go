package probe

import (
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestReplayWatchdogDTOUsesAuthenticatedProbeAndNoMonitor(t *testing.T) {
	for _, name := range []string{"batch-watchdog-firing.json", "batch-watchdog-resolved.json"} {
		t.Run(name, func(t *testing.T) {
			probeID := "22222222-2222-4222-8222-222222222222"
			batch, err := decodeReplayBatch(readFixture(t, "valid", name), probeID)
			if err != nil || len(batch.Events) != 1 {
				t.Fatal("decode watchdog", err)
			}
			e := batch.Events[0]
			if e.Kind != domain.ReplayKindWatchdogTransition || e.Incident == nil || e.Incident.ProbeID != probeID || e.Incident.Scope != domain.IncidentScopeProbeConnection || e.Incident.SubjectKind != domain.IncidentSubjectWatchdog || e.Incident.MonitorID != 0 || e.Incident.AssignmentGeneration != 0 || e.Incident.SourceAlertID == "" || !domain.ValidKeyHash(e.Digest) {
				t.Fatal("lost or fabricated wire identity", e)
			}
		})
	}
}
