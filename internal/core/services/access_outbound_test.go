package services

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestAccessService_OutboundTrustFollowsOwner(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	admin := h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)

	cases := map[string]struct {
		owner int64
		want  domain.OutboundTrust
	}{
		"admin owner":            {admin, domain.OutboundTrustAdmin},
		"non-admin owner":        {member, domain.OutboundTrustRestricted},
		"deleted owner (NULL)":   {0, domain.OutboundTrustRestricted},
		"owner that cannot load": {999, domain.OutboundTrustRestricted},
	}
	for name, tc := range cases {
		if got := h.svc.OutboundTrust(ctx, tc.owner); got != tc.want {
			t.Errorf("%s: OutboundTrust = %v; want %v", name, got, tc.want)
		}
	}
	if !h.svc.CanSaveResponseBody(ctx, admin) || h.svc.CanSaveResponseBody(ctx, member) || h.svc.CanSaveResponseBody(ctx, 0) {
		t.Error("CanSaveResponseBody must hold for admin owners only")
	}
	if ok, _ := h.svc.CanAssignMonitorProxy(ctx, member); ok {
		t.Error("a non-admin may assign a monitor proxy")
	}
	if ok, _ := h.svc.CanAssignMonitorProxy(ctx, admin); !ok {
		t.Error("an admin may not assign a monitor proxy")
	}
	for _, typ := range []string{"docker", "database"} {
		if ok, _ := h.svc.CanUseMonitorType(ctx, member, typ); ok {
			t.Errorf("a non-admin may use %s monitors", typ)
		}
	}
}

func TestAccessService_NotificationOutboundTrustAndRetarget(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	admin := h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)
	notifs := newFakeNotifRepo()
	adminNotif := &domain.Notification{UserID: admin, Name: "a", Type: "webhook"}
	if err := notifs.Create(ctx, adminNotif); err != nil {
		t.Fatal(err)
	}

	// Snapshots carry the id but not the owner: without the repository the
	// delivery is restricted, with it the owner is looked up.
	snapshot := &domain.Notification{ID: adminNotif.ID, Type: "webhook"}
	if got := h.svc.NotificationOutboundTrust(ctx, snapshot); got != domain.OutboundTrustRestricted {
		t.Fatalf("ownerless snapshot without repository = %v; want restricted", got)
	}
	h.svc.SetNotificationRepository(notifs)
	if got := h.svc.NotificationOutboundTrust(ctx, snapshot); got != domain.OutboundTrustAdmin {
		t.Fatalf("ownerless snapshot of an admin notification = %v; want admin", got)
	}
	if got := h.svc.NotificationOutboundTrust(ctx, &domain.Notification{UserID: member}); got != domain.OutboundTrustRestricted {
		t.Fatalf("member notification = %v; want restricted", got)
	}

	if ok, _ := h.svc.CanRetargetNotification(ctx, member, admin); ok {
		t.Error("a non-admin manager may retarget an admin notification")
	}
	for _, tc := range []struct{ actor, owner int64 }{{member, member}, {member, 0}, {admin, admin}, {admin, member}} {
		if ok, _ := h.svc.CanRetargetNotification(ctx, tc.actor, tc.owner); !ok {
			t.Errorf("actor %d may not retarget a notification owned by %d", tc.actor, tc.owner)
		}
	}
}

// trustRecordingSender records the outbound trust each delivery carried.
type trustRecordingSender struct{ got []domain.OutboundTrust }

func (s *trustRecordingSender) Type() string { return "webhook" }

func (s *trustRecordingSender) Validate(map[string]any) error { return nil }

func (s *trustRecordingSender) Send(ctx context.Context, _ map[string]any, _ domain.AlertContext) error {
	s.got = append(s.got, domain.OutboundTrustFrom(ctx))
	return nil
}

var _ ports.NotificationSender = (*trustRecordingSender)(nil)

// NotificationService attaches each notification owner's trust to the send
// context, which the network guard reads.
func TestNotificationService_SendCarriesOwnerTrust(t *testing.T) {
	h := newAccessHarness(t)
	ctx := context.Background()
	admin := h.addUser(t, "admin", true)
	member := h.addUser(t, "member", false)

	sender := &trustRecordingSender{}
	svc := NewNotificationService(newFakeNotifRepo(), newFakeMonitorNotifLinkRepo())
	svc.RegisterSender(sender)
	if err := svc.SendTest(ctx, &domain.Notification{ID: 1, UserID: admin, Type: "webhook"}); err != nil {
		t.Fatal(err)
	}
	svc.SetOutboundTrustResolver(h.svc)
	for _, owner := range []int64{admin, member} {
		if err := svc.SendTest(ctx, &domain.Notification{ID: 2, UserID: owner, Type: "webhook"}); err != nil {
			t.Fatal(err)
		}
	}
	want := []domain.OutboundTrust{domain.OutboundTrustUnset, domain.OutboundTrustAdmin, domain.OutboundTrustRestricted}
	if len(sender.got) != len(want) {
		t.Fatalf("sends = %v; want %v", sender.got, want)
	}
	for i := range want {
		if sender.got[i] != want[i] {
			t.Fatalf("sends = %v; want %v", sender.got, want)
		}
	}
}
