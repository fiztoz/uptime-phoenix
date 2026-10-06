package services

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Backup restore must reproduce EXACTLY the backed-up monitor↔notification
// relationships, whatever new-monitor defaults the destination install has
// (issue #65). These tests run against the production-equivalent harness:
// newBackupHarness wires MonitorService with the default notification linker,
// exactly like bootstrap/run.go does.

const (
	lnkDestUser  = int64(99)
	lnkNotifID   = int64(10)
	lnkNotif2ID  = int64(11)
	lnkMonitorID = int64(20)
)

func lnkTarget(b bool) *bool { return &b }

// lnkDoc builds a version-2 document with one active DEFAULT notification
// (ID 10), optionally a non-default one (ID 11), and one inactive http
// monitor (ID 20) whose relationship list is supplied per test.
func lnkDoc(links []BackupMonitorNotification) *BackupDocument {
	return &BackupDocument{
		Version: BackupDocumentVersion,
		Notifications: []BackupNotification{
			{
				ID: lnkNotifID, Name: "Default Hook", Type: "webhook",
				Active: true, IsDefault: true,
				Config: map[string]any{"webhook_url": "https://example.invalid/hook"},
			},
			{
				ID: lnkNotif2ID, Name: "Plain Hook", Type: "webhook",
				Active: true, IsDefault: false,
				Config: map[string]any{"webhook_url": "https://example.invalid/hook2"},
			},
		},
		Monitors: []BackupMonitor{
			{
				ID: lnkMonitorID, Name: "Web", Type: "http", Active: false,
				Interval: 60, Timeout: 30,
				Config: map[string]any{"url": "https://example.invalid/"},
			},
		},
		MonitorNotifications: links,
	}
}

func lnkImported(t *testing.T, h *backupHarness) *domain.Monitor {
	t.Helper()
	monitors, err := h.monitors.List(context.Background(), ports.MonitorFilter{UserID: lnkDestUser})
	if err != nil {
		t.Fatalf("list imported: %v", err)
	}
	if len(monitors) != 1 {
		t.Fatalf("imported monitors = %d, want 1", len(monitors))
	}
	return monitors[0]
}

// TestBackupImport_DetachedDefaultStaysDetached asserts a deliberately empty
// relationship list stays empty even though the destination has an active
// default notification that new-monitor creation would auto-attach.
func TestBackupImport_DetachedDefaultStaysDetached(t *testing.T) {
	ctx := context.Background()
	dst := newBackupHarness()
	summary, err := dst.svc.Import(ctx, lnkDestUser, lnkDoc(nil))
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(summary.Skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", summary.Skipped)
	}
	if summary.MonitorNotificationsCreated != 0 {
		t.Fatalf("MonitorNotificationsCreated = %d, want 0", summary.MonitorNotificationsCreated)
	}
	links, err := dst.monitorNotifs.ListByMonitor(ctx, lnkImported(t, dst).ID)
	if err != nil {
		t.Fatalf("list links: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("import attached %d notification links not present in the backup: %+v", len(links), links)
	}
}

// TestBackupImport_IncludeTargetFlagsRoundTripExactly asserts every backed-up
// flag survives restore verbatim — including include_target:false on the
// default notification — with no duplicate-link skips.
func TestBackupImport_IncludeTargetFlagsRoundTripExactly(t *testing.T) {
	cases := map[string]*bool{
		"explicit_false": lnkTarget(false),
		"explicit_true":  lnkTarget(true),
		"legacy_omitted": nil, // backups before the flag existed restore to the create default
	}
	for name, flag := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			dst := newBackupHarness()
			doc := lnkDoc([]BackupMonitorNotification{
				{MonitorID: lnkMonitorID, NotificationID: lnkNotifID, IncludeTarget: flag},
			})
			summary, err := dst.svc.Import(ctx, lnkDestUser, doc)
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			if len(summary.Skipped) != 0 {
				t.Fatalf("unexpected skips: %+v", summary.Skipped)
			}
			if summary.MonitorNotificationsCreated != 1 {
				t.Fatalf("MonitorNotificationsCreated = %d, want 1", summary.MonitorNotificationsCreated)
			}
			links, err := dst.monitorNotifs.ListByMonitor(ctx, lnkImported(t, dst).ID)
			if err != nil {
				t.Fatalf("list links: %v", err)
			}
			if len(links) != 1 {
				t.Fatalf("links = %+v, want exactly one", links)
			}
			want := domain.DefaultIncludeTarget
			if flag != nil {
				want = *flag
			}
			if links[0].IncludeTarget != want {
				t.Fatalf("IncludeTarget = %v, want %v", links[0].IncludeTarget, want)
			}
		})
	}
}

// TestBackupImport_NonDefaultLinkSetIsExact asserts a monitor linked only to
// a non-default notification does not also pick up the default one.
func TestBackupImport_NonDefaultLinkSetIsExact(t *testing.T) {
	ctx := context.Background()
	dst := newBackupHarness()
	doc := lnkDoc([]BackupMonitorNotification{
		{MonitorID: lnkMonitorID, NotificationID: lnkNotif2ID, IncludeTarget: lnkTarget(false)},
	})
	summary, err := dst.svc.Import(ctx, lnkDestUser, doc)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if len(summary.Skipped) != 0 {
		t.Fatalf("unexpected skips: %+v", summary.Skipped)
	}
	if summary.MonitorNotificationsCreated != 1 {
		t.Fatalf("MonitorNotificationsCreated = %d, want 1", summary.MonitorNotificationsCreated)
	}
	monitor := lnkImported(t, dst)
	links, err := dst.monitorNotifs.ListByMonitor(ctx, monitor.ID)
	if err != nil {
		t.Fatalf("list links: %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("links = %+v, want exactly one", links)
	}
	var notif2ID int64
	for _, n := range dst.notifs.byID {
		if n.Name == "Plain Hook" {
			notif2ID = n.ID
		}
	}
	if notif2ID == 0 {
		t.Fatal("imported non-default notification not found")
	}
	if links[0].NotificationID != notif2ID {
		t.Fatalf("link points at notification %d, want %d (the backed-up one)", links[0].NotificationID, notif2ID)
	}
	if links[0].IncludeTarget {
		t.Fatalf("IncludeTarget = true, want false")
	}
}
