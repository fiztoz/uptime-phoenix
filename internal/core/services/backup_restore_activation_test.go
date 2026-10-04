package services

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type restoreActivationMonitorRepo struct {
	ports.MonitorRepository
	afterCreate func(context.Context, *domain.Monitor)
	deleteErr   error
	updateErr   error
}

func (r restoreActivationMonitorRepo) Create(ctx context.Context, m *domain.Monitor) error {
	if err := r.MonitorRepository.Create(ctx, m); err != nil {
		return err
	}
	r.afterCreate(ctx, m)
	return nil
}

func (r restoreActivationMonitorRepo) Delete(ctx context.Context, id int64) error {
	if r.deleteErr != nil {
		return r.deleteErr
	}
	return r.MonitorRepository.Delete(ctx, id)
}

func (r restoreActivationMonitorRepo) Update(ctx context.Context, m *domain.Monitor) error {
	if r.updateErr != nil {
		return r.updateErr
	}
	return r.MonitorRepository.Update(ctx, m)
}

func TestBackupRestoreKeepsMonitorInactiveUntilAssignmentsCommit(t *testing.T) {
	for _, failure := range []string{"none", "assignment", "cleanup", "activation"} {
		t.Run(failure, func(t *testing.T) {
			h, _, assignments := newBackupProbeHarness()
			repo := restoreActivationMonitorRepo{MonitorRepository: h.monitors, afterCreate: func(ctx context.Context, m *domain.Monitor) {
				if _, err := assignments.InitializeLocal(ctx, m.ID); err != nil {
					t.Fatal(err)
				}
				stored, err := h.monitors.GetByID(ctx, m.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Active {
					t.Error("restore exposed an active monitor with a placeholder local assignment")
				}
			}}
			if failure == "assignment" || failure == "cleanup" {
				assignments.restoreErr = errors.New("assignment unavailable")
			}
			if failure == "cleanup" {
				repo.deleteErr = errors.New("cleanup unavailable")
			}
			if failure == "activation" {
				repo.updateErr = errors.New("activation unavailable")
			}
			h.svc.monitors = repo
			h.svc.monitorSvc.repo = repo
			doc := &BackupDocument{Version: BackupDocumentVersion,
				Probes:                  []BackupProbe{{ID: backupProbeTestID, Key: "us-east", Name: "US East", Kind: domain.ProbeKindRemote}},
				Monitors:                []BackupMonitor{{ID: 7, Name: "remote", Type: "http", Active: true, Interval: 60, Timeout: 30}},
				MonitorProbeAssignments: []BackupMonitorAssignmentSet{{MonitorID: 7, HealthPolicy: domain.HealthPolicyAnyDown, Members: []BackupMonitorAssignmentMember{{ProbeKey: "us-east"}}}},
			}
			summary, err := h.svc.Import(t.Context(), 1, doc)
			if err != nil {
				t.Fatal(err)
			}
			for id, m := range h.monitors.byID {
				if failure != "none" && m.Active {
					t.Fatal("failed restore left an active monitor")
				}
				if failure == "none" {
					set, err := assignments.GetByMonitorID(t.Context(), id)
					if err != nil || domain.LocalWorkerMayRun(set) || !m.Active {
						t.Fatalf("final activation: %+v %+v %v", m, set, err)
					}
				}
			}
			if failure == "cleanup" && (len(summary.Skipped) == 0 || !strings.Contains(summary.Skipped[0].Reason, "cleanup unavailable")) {
				t.Fatalf("cleanup failure hidden: %+v", summary)
			}
			if failure == "activation" && len(summary.Skipped) == 0 {
				t.Fatal("activation failure hidden")
			}
		})
	}
}
