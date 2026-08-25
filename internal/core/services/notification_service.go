package services

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// NotificationTagReader supplies the named tag values attached to a monitor.
// TagService satisfies it; keeping the read surface small lets notification
// dispatch include template metadata without depending on repositories.
type NotificationTagReader interface {
	TagsForMonitor(ctx context.Context, monitorID int64) ([]MonitorTagDetail, error)
}

// NotificationService handles notification CRUD and dispatching.
type NotificationService struct {
	repo             ports.NotificationRepository
	templateRepo     ports.NotificationTemplateRepository
	monitorNotifRepo ports.MonitorNotificationRepository
	groupNotifRepo   ports.GroupNotificationRepository // optional: group (folder) alerting
	tagReader        NotificationTagReader             // optional: monitor template metadata
	senders          map[string]ports.NotificationSender
}

// SetTemplateRepository wires reusable notification templates into validation
// and dispatch. A notification without a template keeps working when it is unset.
func (s *NotificationService) SetTemplateRepository(repo ports.NotificationTemplateRepository) {
	s.templateRepo = repo
}

// SetTagReader wires monitor tag values into reusable notification templates.
// Tag lookup remains best-effort so metadata failure never suppresses an alert.
func (s *NotificationService) SetTagReader(reader NotificationTagReader) {
	s.tagReader = reader
}

// NewNotificationService creates a new NotificationService.
func NewNotificationService(
	repo ports.NotificationRepository,
	monitorNotifRepo ports.MonitorNotificationRepository,
) *NotificationService {
	return &NotificationService{
		repo:             repo,
		monitorNotifRepo: monitorNotifRepo,
		senders:          make(map[string]ports.NotificationSender),
	}
}

// SetGroupNotificationRepo wires the monitor-group ↔ notification link store, so
// notifications can be attached to a FOLDER and fire on that folder's own derived
// status.
//
// Optional, and it fails CLOSED when unset: the group attach/detach/list methods
// return an error rather than pretending to work, and NotifyGroup sends nothing.
func (s *NotificationService) SetGroupNotificationRepo(repo ports.GroupNotificationRepository) {
	s.groupNotifRepo = repo
}

// RegisterSender adds a notification sender to the dispatch pool.
func (s *NotificationService) RegisterSender(sender ports.NotificationSender) {
	s.senders[sender.Type()] = sender
}

// Create creates a new notification configuration.
func (s *NotificationService) Create(ctx context.Context, n *domain.Notification) error {
	if err := s.validateTemplateAssignment(ctx, n); err != nil {
		return err
	}
	return s.repo.Create(ctx, n)
}

// GetByID retrieves a notification by its ID.
func (s *NotificationService) GetByID(ctx context.Context, id int64) (*domain.Notification, error) {
	return s.repo.GetByID(ctx, id)
}

// List retrieves all notifications for a user.
func (s *NotificationService) List(ctx context.Context, userID int64) ([]*domain.Notification, error) {
	return s.repo.List(ctx, userID)
}

// ListAll retrieves every notification in the install. Callers must have checked
// that the principal is an admin or holds the can_manage_notifications
// capability — this method performs no authorization of its own.
func (s *NotificationService) ListAll(ctx context.Context) ([]*domain.Notification, error) {
	return s.repo.ListAll(ctx)
}

// ListForMonitors returns the notifications attached to any of the given
// monitors, deduplicated and ordered by id.
//
// This is the read-only view a non-admin WITHOUT the can_manage_notifications
// capability gets: they see the notifications wired to the monitors they can see,
// and nothing else. Pass the caller's visible-monitor set — an empty set yields an
// empty list, which is the correct answer for a user with no grants.
func (s *NotificationService) ListForMonitors(ctx context.Context, monitorIDs []int64) ([]*domain.Notification, error) {
	byID := make(map[int64]*domain.Notification)
	for _, monitorID := range monitorIDs {
		linked, err := s.repo.GetByMonitorID(ctx, monitorID)
		if err != nil {
			return nil, err
		}
		for _, n := range linked {
			byID[n.ID] = n
		}
	}
	out := make([]*domain.Notification, 0, len(byID))
	for _, n := range byID {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Update updates a notification configuration.
func (s *NotificationService) Update(ctx context.Context, n *domain.Notification) error {
	if err := s.validateTemplateAssignment(ctx, n); err != nil {
		return err
	}
	return s.repo.Update(ctx, n)
}

// Delete deletes a notification by its ID.
func (s *NotificationService) Delete(ctx context.Context, id int64) error {
	return s.repo.Delete(ctx, id)
}

// Notify dispatches a status-change alert to all assigned notification providers
// for a monitor. Errors from individual senders are logged but do not halt the
// dispatch loop.
func (s *NotificationService) Notify(ctx context.Context, monitor *domain.Monitor, status domain.Status, prevStatus domain.Status) error {
	return s.NotifyWithAck(ctx, monitor, status, prevStatus, "", "")
}

// NotifyWithAck is Notify plus an optional deep-link acknowledgement URL (F2.2)
// and optional checkOutput (heartbeat message / saved HTTP response body).
// When ackURL is non-empty it is set on AlertContext and appended to Message so
// every provider surfaces the link without per-sender changes. checkOutput is
// placed on AlertContext.CheckOutput so providers that render it (Discord,
// Telegram, webhook, …) include the check detail.
func (s *NotificationService) NotifyWithAck(ctx context.Context, monitor *domain.Monitor, status domain.Status, prevStatus domain.Status, ackURL, checkOutput string) error {
	return s.NotifyWithAlertDetails(
		ctx, monitor, status, prevStatus, ackURL, checkOutput, time.Time{}, 0,
	)
}

// NotifyWithAlertDetails dispatches a monitor status alert with optional
// lifecycle timing. NotificationDispatcher supplies the persisted alert's
// FiredAt and elapsed duration for initial DOWN, resends, and recovery; direct
// callers may leave both zero and the matching template variables render empty.
func (s *NotificationService) NotifyWithAlertDetails(
	ctx context.Context,
	monitor *domain.Monitor,
	status, prevStatus domain.Status,
	ackURL, checkOutput string,
	startedAt time.Time,
	duration time.Duration,
) error {
	msg := fmt.Sprintf("%s is %s", monitor.Name, status.String())
	if ackURL != "" && status == domain.StatusDown {
		msg = msg + "\nAcknowledge: " + ackURL
	}
	if !startedAt.IsZero() {
		startedAt = startedAt.UTC()
	}
	alert := domain.AlertContext{
		AlertScope:         domain.AlertScopeMonitor,
		MonitorID:          monitor.ID,
		MonitorName:        monitor.Name,
		MonitorType:        monitor.Type,
		MonitorTarget:      monitor.Target(),
		MonitorDescription: monitor.Description,
		MonitorOwner:       monitor.Owner,
		Status:             status,
		PreviousStatus:     prevStatus,
		Message:            msg,
		CheckOutput:        checkOutput,
		Duration:           duration,
		StartedAt:          startedAt,
		Tags:               s.tagsForMonitor(ctx, monitor.ID),
		EventKind:          domain.AlertEventStatusChange,
		AckURL:             ackURL,
	}
	return s.Dispatch(ctx, monitor, alert)
}

func (s *NotificationService) tagsForMonitor(ctx context.Context, monitorID int64) map[string]string {
	tags := make(map[string]string)
	if s.tagReader == nil {
		return tags
	}
	details, err := s.tagReader.TagsForMonitor(ctx, monitorID)
	if err != nil {
		slog.Warn("notification service: tag lookup failed; sending without tags",
			"monitor_id", monitorID, "error", err)
		return tags
	}
	for _, tag := range details {
		if tag.Name != "" {
			tags[tag.Name] = tag.Value
		}
	}
	return tags
}

// Dispatch sends a pre-built AlertContext to every active notification attached
// to the monitor. Used by status-change Notify and by CertificateAlertService
// (certificate_expiry). Provider failures are logged; they do not fail the
// overall call or the heartbeat path that invoked it.
func (s *NotificationService) Dispatch(ctx context.Context, monitor *domain.Monitor, alert domain.AlertContext) error {
	notifications, err := s.repo.GetByMonitorID(ctx, monitor.ID)
	if err != nil {
		return fmt.Errorf("notification service: get by monitor: %w", err)
	}

	for _, n := range notifications {
		if !n.Active {
			continue
		}
		sender, ok := s.senders[n.Type]
		if !ok {
			slog.Warn("notification service: unknown sender type, skipping",
				"type", n.Type, "notification_id", n.ID, "monitor_id", monitor.ID)
			continue
		}
		notificationAlert, err := s.alertForNotification(ctx, n, alert)
		if err != nil {
			slog.Error("notification service: resolve template failed",
				"type", n.Type, "notification_id", n.ID, "monitor_id", monitor.ID, "error", err)
			continue
		}
		if err := sender.Send(ctx, n.Config, notificationAlert); err != nil {
			slog.Error("notification service: send failed",
				"type", n.Type, "notification_id", n.ID, "monitor_id", monitor.ID, "error", err)
		}
	}

	return nil
}

// DispatchTracked sends an auxiliary alert and reports whether at least one
// active channel accepted it. Unlike Dispatch, it returns per-channel failures
// so lifecycle callers do not advance a notification cursor when nothing was
// delivered. A partial delivery returns delivered=true together with the joined
// errors; callers can advance a coarse cursor while still making the failure
// observable.
func (s *NotificationService) DispatchTracked(ctx context.Context, monitor *domain.Monitor, alert domain.AlertContext) (bool, error) {
	notifications, err := s.repo.GetByMonitorID(ctx, monitor.ID)
	if err != nil {
		return false, fmt.Errorf("notification service: get by monitor: %w", err)
	}

	delivered := 0
	var errs []error
	for _, n := range notifications {
		if !n.Active {
			continue
		}
		sender, ok := s.senders[n.Type]
		if !ok {
			errs = append(errs, fmt.Errorf("notification %d: unknown sender type %q", n.ID, n.Type))
			continue
		}
		notificationAlert, templateErr := s.alertForNotification(ctx, n, alert)
		if templateErr != nil {
			errs = append(errs, fmt.Errorf("notification %d template: %w", n.ID, templateErr))
			continue
		}
		if sendErr := sender.Send(ctx, n.Config, notificationAlert); sendErr != nil {
			errs = append(errs, fmt.Errorf("notification %d: %w", n.ID, sendErr))
			continue
		}
		delivered++
	}
	return delivered > 0, errors.Join(errs...)
}

// DispatchToNotificationIDs sends a pre-built AlertContext to an EXPLICIT list
// of channels, bypassing the monitor's own attachments. It is how an escalation
// step (F2.3) pages its own ladder rung without inheriting whatever happens to
// be wired to the monitor.
//
// It deliberately does not return nil on every path: per-channel failures are
// collected and returned joined, and a non-empty list that reached NOBODY —
// every id missing, inactive, or backed by an unknown sender type — is itself
// an error. Callers log and continue; the point is that the failure is visible
// at all.
func (s *NotificationService) DispatchToNotificationIDs(ctx context.Context, notificationIDs []int64, alert domain.AlertContext) error {
	if len(notificationIDs) == 0 {
		return nil
	}
	var errs []error
	delivered := 0
	for _, id := range notificationIDs {
		n, err := s.repo.GetByID(ctx, id)
		if err != nil {
			if errors.Is(err, ports.ErrNotFound) {
				// Deleted between policy save and dispatch — skip, but say so.
				errs = append(errs, fmt.Errorf("notification %d: %w", id, ports.ErrNotFound))
				continue
			}
			errs = append(errs, fmt.Errorf("notification %d: %w", id, err))
			continue
		}
		if !n.Active {
			continue
		}
		sender, ok := s.senders[n.Type]
		if !ok {
			errs = append(errs, fmt.Errorf("notification %d: unknown sender type %q", id, n.Type))
			continue
		}
		notificationAlert, err := s.alertForNotification(ctx, n, alert)
		if err != nil {
			errs = append(errs, fmt.Errorf("notification %d template: %w", id, err))
			continue
		}
		if err := sender.Send(ctx, n.Config, notificationAlert); err != nil {
			slog.Error("notification service: escalation send failed",
				"type", n.Type, "notification_id", n.ID, "monitor_id", alert.MonitorID, "error", err)
			errs = append(errs, fmt.Errorf("notification %d: %w", id, err))
			continue
		}
		delivered++
	}
	if delivered == 0 && len(errs) == 0 {
		return fmt.Errorf("notification service: escalation reached no active channel out of %d", len(notificationIDs))
	}
	return errors.Join(errs...)
}

// NotifyGroup dispatches an alert to the notifications attached to a monitor
// GROUP, for a transition of that group's own derived status.
//
// It reads ONLY group_notifications: an alert about the folder goes to the
// folder's own providers, never to the ones attached to the monitors inside it
// (those already alert on their own transitions) and never to a sibling folder's.
//
// The caller (GroupAlertService) has already decided that this transition is
// alert-worthy AND has won the compare-and-set that claims it, so this method
// asks no further questions — it just sends.
func (s *NotificationService) NotifyGroup(ctx context.Context, group *domain.MonitorGroup, status, prevStatus domain.Status) error {
	if s.groupNotifRepo == nil {
		return fmt.Errorf("notification service: notify group: no group notification repository wired")
	}
	notifications, err := s.groupNotifRepo.ListNotificationsByGroup(ctx, group.ID)
	if err != nil {
		return fmt.Errorf("notification service: get by group: %w", err)
	}

	alert := domain.AlertContext{
		// The alert is ABOUT the group, so the monitor fields carry the group's
		// identity: the senders template MonitorName into their titles, and
		// "Payments API" reads correctly there while a monitor id would not.
		// MonitorID stays 0 — there is no monitor this alert is about, and putting
		// the group id in a field every sender treats as a monitor id would produce
		// links to the wrong page.
		AlertScope:              domain.AlertScopeGroup,
		MonitorName:             group.Name,
		MonitorType:             "group",
		GroupID:                 group.ID,
		GroupName:               group.Name,
		GroupDescription:        group.Description,
		GroupOwner:              group.Owner,
		GroupCondition:          group.Condition,
		GroupThreshold:          group.Threshold,
		GroupThresholdIsPercent: group.ThresholdIsPercent,
		Status:                  status,
		PreviousStatus:          prevStatus,
		Message:                 fmt.Sprintf("Group %q is %s", group.Name, status.String()),
		Tags:                    map[string]string{},
	}

	for _, n := range notifications {
		if !n.Active {
			continue
		}
		sender, ok := s.senders[n.Type]
		if !ok {
			slog.Warn("notification service: unknown sender type, skipping",
				"type", n.Type, "notification_id", n.ID, "group_id", group.ID)
			continue
		}
		notificationAlert, err := s.alertForNotification(ctx, n, alert)
		if err != nil {
			slog.Error("notification service: resolve group template failed",
				"type", n.Type, "notification_id", n.ID, "group_id", group.ID, "error", err)
			continue
		}
		if err := sender.Send(ctx, n.Config, notificationAlert); err != nil {
			slog.Error("notification service: group send failed",
				"type", n.Type, "notification_id", n.ID, "group_id", group.ID, "error", err)
		}
	}

	return nil
}

// AttachToGroup links a notification to a monitor group, so the notification
// fires when that GROUP's derived status trips — not once per monitor inside it.
//
// Idempotent: re-attaching an existing pair is a no-op (the UI toggles a
// checkbox; a double-click must not 500).
func (s *NotificationService) AttachToGroup(ctx context.Context, groupID, notificationID int64) error {
	if s.groupNotifRepo == nil {
		return fmt.Errorf("notification service: attach to group: no group notification repository wired")
	}
	return s.groupNotifRepo.Attach(ctx, groupID, notificationID)
}

// DetachFromGroup removes the link between a notification and a monitor group.
func (s *NotificationService) DetachFromGroup(ctx context.Context, groupID, notificationID int64) error {
	if s.groupNotifRepo == nil {
		return fmt.Errorf("notification service: detach from group: no group notification repository wired")
	}
	return s.groupNotifRepo.Detach(ctx, groupID, notificationID)
}

// GetByGroupID retrieves the notifications attached to a monitor group.
func (s *NotificationService) GetByGroupID(ctx context.Context, groupID int64) ([]*domain.Notification, error) {
	if s.groupNotifRepo == nil {
		return nil, fmt.Errorf("notification service: get by group: no group notification repository wired")
	}
	return s.groupNotifRepo.ListNotificationsByGroup(ctx, groupID)
}

// SendTest dispatches a test alert through the given notification.
func (s *NotificationService) SendTest(ctx context.Context, n *domain.Notification) error {
	sender, ok := s.senders[n.Type]
	if !ok {
		return fmt.Errorf("notification service: unknown sender type: %s", n.Type)
	}
	alert := domain.AlertContext{
		AlertScope:    domain.AlertScopeMonitor,
		MonitorName:   "Test Monitor",
		MonitorType:   "http",
		MonitorTarget: "https://example.com",
		Status:        domain.StatusUp,
		Message:       "This is a test notification from Phoenix.",
	}
	notificationAlert, err := s.alertForNotification(ctx, n, alert)
	if err != nil {
		return fmt.Errorf("notification service: resolve test template: %w", err)
	}
	return sender.Send(ctx, n.Config, notificationAlert)
}

func (s *NotificationService) validateTemplateAssignment(ctx context.Context, n *domain.Notification) error {
	if n == nil || n.TemplateID == nil {
		return nil
	}
	if s.templateRepo == nil {
		return fmt.Errorf("notification service: %w: notification templates are unavailable", domain.ErrValidation)
	}
	template, err := s.templateRepo.GetByID(ctx, *n.TemplateID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) || errors.Is(err, domain.ErrNotFound) {
			return fmt.Errorf("notification service: %w: notification template not found", domain.ErrValidation)
		}
		return fmt.Errorf("notification service: get template: %w", err)
	}
	if template.Provider != n.Type {
		return fmt.Errorf("notification service: %w: template provider %q does not match notification type %q", domain.ErrValidation, template.Provider, n.Type)
	}
	return nil
}

func (s *NotificationService) alertForNotification(ctx context.Context, n *domain.Notification, alert domain.AlertContext) (domain.AlertContext, error) {
	if n == nil || n.TemplateID == nil {
		return alert, nil
	}
	if s.templateRepo == nil {
		return alert, fmt.Errorf("notification template repository is unavailable")
	}
	template, err := s.templateRepo.GetByID(ctx, *n.TemplateID)
	if err != nil {
		return alert, err
	}
	if template.Provider != n.Type {
		return alert, fmt.Errorf("template provider %q does not match notification type %q", template.Provider, n.Type)
	}
	alert.TemplateTitle = template.TitleTemplate
	alert.TemplateBody = template.BodyTemplate
	alert.TemplateConfig = template.Config
	return alert, nil
}

// AttachToMonitor links a notification to a monitor so the notification is
// triggered whenever that monitor changes status.
func (s *NotificationService) AttachToMonitor(ctx context.Context, monitorID, notificationID int64) error {
	return s.monitorNotifRepo.Attach(ctx, monitorID, notificationID)
}

// DetachFromMonitor removes the link between a notification and a monitor.
func (s *NotificationService) DetachFromMonitor(ctx context.Context, monitorID, notificationID int64) error {
	return s.monitorNotifRepo.Detach(ctx, monitorID, notificationID)
}

// GetByMonitorID retrieves all notifications assigned to a monitor.
func (s *NotificationService) GetByMonitorID(ctx context.Context, monitorID int64) ([]*domain.Notification, error) {
	return s.repo.GetByMonitorID(ctx, monitorID)
}

// ListByNotification retrieves all monitor-notification associations for a
// given notification ID.
func (s *NotificationService) ListByNotification(ctx context.Context, notificationID int64) ([]*domain.MonitorNotification, error) {
	return s.monitorNotifRepo.ListByNotification(ctx, notificationID)
}
