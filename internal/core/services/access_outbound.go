package services

import (
	"context"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Outbound reach (#88). A monitor check or notification delivery may reach
// what its OWNER may reach: monitors and notifications owned by an admin get
// only the always-blocked floor (link-local, cloud metadata, multicast,
// unspecified); everything else — owned by a non-admin, by a deleted user
// (user_id NULL after ON DELETE SET NULL), or by an owner that cannot be
// loaded — is restricted to public destinations plus the operator's
// OUTBOUND_ALLOWED_CIDRS. The decision is made here and enforced by the
// network guard adapter (internal/adapters/netguard), which reads it from the
// context (domain.WithOutboundTrust).
//
// Owner, not editor: CanEditMonitor lets only an admin or the owner change a
// monitor, so a non-admin can never retarget an admin-owned monitor. A
// notification may be edited by anyone with CanManageNotifications, so
// CanRetargetNotification closes that path.

// OutboundTrustResolver decides the outbound trust of the work a scheduler or
// a notification service is about to do. AccessService implements it.
type OutboundTrustResolver interface {
	// OutboundTrust returns the trust for a resource owned by ownerUserID.
	OutboundTrust(ctx context.Context, ownerUserID int64) domain.OutboundTrust
	// NotificationOutboundTrust returns the trust for a delivery through n.
	NotificationOutboundTrust(ctx context.Context, n *domain.Notification) domain.OutboundTrust
}

// notificationSendContext attaches the outbound trust for a delivery through
// n. With no resolver it attaches nothing, which the guard treats as
// restricted.
func notificationSendContext(ctx context.Context, r OutboundTrustResolver, n *domain.Notification) context.Context {
	if r == nil {
		return ctx
	}
	return domain.WithOutboundTrust(ctx, r.NotificationOutboundTrust(ctx, n))
}

// SetNotificationRepository lets NotificationOutboundTrust resolve the owner
// of a notification that arrives without one (applied snapshots and probe
// watchdog channels carry the notification id, not its user_id). Optional:
// without it such deliveries are restricted.
func (s *AccessService) SetNotificationRepository(repo ports.NotificationRepository) {
	s.mu.Lock()
	s.notifications = repo
	s.mu.Unlock()
}

// OutboundTrust returns domain.OutboundTrustAdmin when ownerUserID is an
// admin and domain.OutboundTrustRestricted otherwise. It fails closed: an
// unknown, deleted or unloadable owner is restricted. Lookups go through the
// user cache, so a scheduler can ask once per owner per tick.
func (s *AccessService) OutboundTrust(ctx context.Context, ownerUserID int64) domain.OutboundTrust {
	if ownerUserID <= 0 {
		return domain.OutboundTrustRestricted
	}
	admin, err := s.IsAdmin(ctx, ownerUserID)
	if err != nil || !admin {
		return domain.OutboundTrustRestricted
	}
	return domain.OutboundTrustAdmin
}

// NotificationOutboundTrust returns the trust for a delivery through n,
// decided by the notification's owner.
func (s *AccessService) NotificationOutboundTrust(ctx context.Context, n *domain.Notification) domain.OutboundTrust {
	if n == nil {
		return domain.OutboundTrustRestricted
	}
	owner := n.UserID
	if owner <= 0 && n.ID > 0 {
		s.mu.Lock()
		repo := s.notifications
		s.mu.Unlock()
		if repo != nil {
			if stored, err := repo.GetByID(ctx, n.ID); err == nil && stored != nil {
				owner = stored.UserID
			}
		}
	}
	return s.OutboundTrust(ctx, owner)
}

// CanAssignMonitorProxy reports whether the user may set or change a
// monitor's proxy_id. Admin only: a proxy is an egress path the outbound
// guard has to trust (it dials the proxy, not the target), so only an admin
// chooses one.
func (s *AccessService) CanAssignMonitorProxy(ctx context.Context, userID int64) (bool, error) {
	return s.IsAdmin(ctx, userID)
}

// CanSaveResponseBody reports whether a monitor owned by ownerUserID may
// enable save_body_on_error / save_body_on_success. Only admin-owned monitors
// may: a saved body is shown to everyone who can view the monitor, so for a
// restricted owner it would be a read primitive against whatever the monitor
// can reach (including OUTBOUND_ALLOWED_CIDRS ranges). The HTTP checker
// applies the same rule at check time.
func (s *AccessService) CanSaveResponseBody(ctx context.Context, ownerUserID int64) bool {
	return s.OutboundTrust(ctx, ownerUserID) == domain.OutboundTrustAdmin
}

// CanRetargetNotification reports whether actorUserID may change the config
// (the destination) of a notification owned by ownerUserID. Admins may change
// any; a non-admin manager may not change an admin-owned notification, whose
// deliveries reach private addresses on the admin's authority.
func (s *AccessService) CanRetargetNotification(ctx context.Context, actorUserID, ownerUserID int64) (bool, error) {
	admin, err := s.IsAdmin(ctx, actorUserID)
	if err != nil {
		return false, err
	}
	if admin {
		return true, nil
	}
	return s.OutboundTrust(ctx, ownerUserID) != domain.OutboundTrustAdmin, nil
}
