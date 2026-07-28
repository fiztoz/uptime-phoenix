package services

import (
	"context"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const confirmationTokenTTL = 24 * time.Hour

// ErrSubscriptionsUnavailable is returned when PUBLIC_URL is empty, the page
// has no active SMTP channel, or SMTP handoff fails for a confirmation request.
var ErrSubscriptionsUnavailable = fmt.Errorf("%w: subscriptions unavailable", domain.ErrInternal)

// errNoSubscriptionChannel reports the ONE benign reason loadSMTPChannel can
// fail: the status page has no subscription channel configured, so a fan-out
// genuinely has nothing to do. Every other failure — a database error, a
// dangling notification reference, a deactivated channel, a missing config —
// is a fault an operator must be able to see. Keeping them distinguishable is
// the whole point: they were previously flattened into a silent `return nil`
// that stopped subscriber email with no error and no log line.
var errNoSubscriptionChannel = errors.New("no subscription channel configured")

// SubscriptionService manages double-opt-in status-page email subscriptions
// and fan-out for incidents / maintenance announcements.
type SubscriptionService struct {
	pages         ports.StatusPageRepository
	subscribers   ports.StatusPageSubscriberRepository
	notifications ports.NotificationRepository
	tokens        ports.SubscriberTokenCodec
	mailer        ports.TransactionalEmailSender
	passwords     ports.PasswordHasher
	publicURL     string // absolute http(s) origin; empty disables subscriptions
}

// NewSubscriptionService wires subscription use cases. publicURL must be an
// absolute http/https origin or empty (empty disables all subscription mail).
func NewSubscriptionService(
	pages ports.StatusPageRepository,
	subscribers ports.StatusPageSubscriberRepository,
	notifications ports.NotificationRepository,
	tokens ports.SubscriberTokenCodec,
	mailer ports.TransactionalEmailSender,
	passwords ports.PasswordHasher,
	publicURL string,
) *SubscriptionService {
	return &SubscriptionService{
		pages:         pages,
		subscribers:   subscribers,
		notifications: notifications,
		tokens:        tokens,
		mailer:        mailer,
		passwords:     passwords,
		publicURL:     strings.TrimRight(strings.TrimSpace(publicURL), "/"),
	}
}

// SubscriptionsAvailable reports whether a page can accept new subscribers.
// Exposed on the public status payload as subscriptions_available only.
func (s *SubscriptionService) SubscriptionsAvailable(ctx context.Context, statusPageID int64) bool {
	if s.publicURL == "" || s.mailer == nil || s.tokens == nil {
		return false
	}
	cfg, err := s.loadSMTPChannel(ctx, statusPageID)
	return err == nil && cfg != nil
}

// Subscribe starts or refreshes a double-opt-in subscription. After
// configuration and syntax validation it always attempts SMTP and returns
// nil on the happy path so callers can answer 202 for new, pending, and
// active addresses alike (enumeration resistance).
func (s *SubscriptionService) Subscribe(ctx context.Context, slug, email, accessCode string) error {
	sp, err := s.pages.GetBySlug(ctx, slug)
	if err != nil {
		return fmt.Errorf("subscribe: %w", err)
	}
	if !sp.Published {
		return fmt.Errorf("subscribe: %w", domain.ErrNotFound)
	}
	if sp.PasswordHash != "" {
		if s.passwords == nil {
			return fmt.Errorf("subscribe: %w: password hasher is not configured", domain.ErrInternal)
		}
		if err := s.passwords.Verify(sp.PasswordHash, accessCode); err != nil {
			return fmt.Errorf("subscribe: %w", domain.ErrUnauthorized)
		}
	}

	norm, err := normalizeEmail(email)
	if err != nil {
		return fmt.Errorf("subscribe: %w: %s", domain.ErrValidation, err.Error())
	}

	// Configuration gate BEFORE any existence lookup so missing SMTP cannot
	// become an address oracle.
	if s.publicURL == "" || s.mailer == nil || s.tokens == nil {
		return ErrSubscriptionsUnavailable
	}
	smtpCfg, err := s.loadSMTPChannel(ctx, sp.ID)
	if err != nil {
		// The caller still gets the same opaque answer — enumeration
		// resistance depends on that — but a real fault is no longer
		// invisible to the operator diagnosing why subscribing fails.
		if !errors.Is(err, errNoSubscriptionChannel) {
			slog.Error("subscription: smtp channel unusable",
				"status_page_id", sp.ID, "slug", slug, "error", err)
		}
		return ErrSubscriptionsUnavailable
	}

	existing, err := s.subscribers.GetByPageAndEmail(ctx, sp.ID, norm)
	if err != nil && !errors.Is(err, ports.ErrNotFound) && !errors.Is(err, domain.ErrNotFound) {
		return fmt.Errorf("subscribe: lookup: %w", err)
	}

	if existing != nil && existing.Active {
		// Already confirmed — send a generic management email (same 202 path).
		if err := s.sendAlreadySubscribed(ctx, sp, existing, smtpCfg); err != nil {
			slog.Error("subscription: already-subscribed email failed", "status_page_id", sp.ID, "error", err)
			return ErrSubscriptionsUnavailable
		}
		return nil
	}

	var sub *domain.StatusPageSubscriber
	if existing != nil {
		sub = existing
	} else {
		sub = &domain.StatusPageSubscriber{
			StatusPageID: sp.ID,
			Email:        norm,
			Active:       false,
		}
		if err := s.subscribers.Create(ctx, sub); err != nil {
			// Race: another request created the same email — re-fetch.
			if errors.Is(err, ports.ErrConflict) || errors.Is(err, domain.ErrConflict) {
				existing, getErr := s.subscribers.GetByPageAndEmail(ctx, sp.ID, norm)
				if getErr != nil {
					return fmt.Errorf("subscribe: create race re-fetch: %w", getErr)
				}
				if existing.Active {
					if sendErr := s.sendAlreadySubscribed(ctx, sp, existing, smtpCfg); sendErr != nil {
						return ErrSubscriptionsUnavailable
					}
					return nil
				}
				sub = existing
			} else {
				return fmt.Errorf("subscribe: create: %w", err)
			}
		}
	}

	if err := s.sendConfirmation(ctx, sp, sub, smtpCfg); err != nil {
		slog.Error("subscription: confirmation email failed", "status_page_id", sp.ID, "error", err)
		return ErrSubscriptionsUnavailable
	}
	return nil
}

// Confirm activates a pending subscriber from a confirmation token.
func (s *SubscriptionService) Confirm(ctx context.Context, token string) error {
	if s.tokens == nil {
		return fmt.Errorf("confirm: %w", ports.ErrSubscriberToken)
	}
	id, err := s.tokens.Verify(token, ports.SubscriberTokenConfirm)
	if err != nil {
		return fmt.Errorf("confirm: %w", err)
	}
	sub, err := s.subscribers.GetByID(ctx, id)
	if err != nil {
		return fmt.Errorf("confirm: %w", domain.ErrNotFound)
	}
	if sub.Active {
		return nil // idempotent
	}
	now := time.Now().UTC()
	sub.Active = true
	sub.ConfirmedAt = &now
	if err := s.subscribers.Update(ctx, sub); err != nil {
		return fmt.Errorf("confirm: update: %w", err)
	}
	return nil
}

// Unsubscribe deletes a subscriber identified by an unsubscribe token.
func (s *SubscriptionService) Unsubscribe(ctx context.Context, token string) error {
	if s.tokens == nil {
		return fmt.Errorf("unsubscribe: %w", ports.ErrSubscriberToken)
	}
	id, err := s.tokens.Verify(token, ports.SubscriberTokenUnsubscribe)
	if err != nil {
		return fmt.Errorf("unsubscribe: %w", err)
	}
	if err := s.subscribers.Delete(ctx, id); err != nil {
		return fmt.Errorf("unsubscribe: %w", err)
	}
	return nil
}

// ListSubscribers returns the admin-safe subscriber list for a status page.
func (s *SubscriptionService) ListSubscribers(ctx context.Context, statusPageID int64) ([]*domain.StatusPageSubscriber, error) {
	if _, err := s.pages.GetByID(ctx, statusPageID); err != nil {
		return nil, fmt.Errorf("list subscribers: %w", err)
	}
	return s.subscribers.ListByStatusPage(ctx, statusPageID)
}

// DeleteSubscriber removes one subscriber belonging to the given status page.
func (s *SubscriptionService) DeleteSubscriber(ctx context.Context, statusPageID, subscriberID int64) error {
	sub, err := s.subscribers.GetByID(ctx, subscriberID)
	if err != nil {
		return fmt.Errorf("delete subscriber: %w", err)
	}
	if sub.StatusPageID != statusPageID {
		return fmt.Errorf("delete subscriber: %w", domain.ErrNotFound)
	}
	return s.subscribers.Delete(ctx, subscriberID)
}

// GetChannel returns the selected SMTP notification binding, or nil when unset.
func (s *SubscriptionService) GetChannel(ctx context.Context, statusPageID int64) (*domain.StatusPageSubscriptionChannel, error) {
	if _, err := s.pages.GetByID(ctx, statusPageID); err != nil {
		return nil, fmt.Errorf("get channel: %w", err)
	}
	ch, err := s.subscribers.GetChannel(ctx, statusPageID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) || errors.Is(err, domain.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("get channel: %w", err)
	}
	return ch, nil
}

// SetChannel validates and binds an active SMTP notification to the page.
func (s *SubscriptionService) SetChannel(ctx context.Context, statusPageID, notificationID int64) (*domain.StatusPageSubscriptionChannel, error) {
	if _, err := s.pages.GetByID(ctx, statusPageID); err != nil {
		return nil, fmt.Errorf("set channel: %w", err)
	}
	n, err := s.notifications.GetByID(ctx, notificationID)
	if err != nil {
		return nil, fmt.Errorf("set channel: %w", err)
	}
	if !n.Active {
		return nil, fmt.Errorf("set channel: %w: notification is not active", domain.ErrValidation)
	}
	if !strings.EqualFold(n.Type, "smtp") {
		return nil, fmt.Errorf("set channel: %w: notification must be type smtp", domain.ErrValidation)
	}
	ch := &domain.StatusPageSubscriptionChannel{
		StatusPageID:   statusPageID,
		NotificationID: notificationID,
	}
	if err := s.subscribers.SetChannel(ctx, ch); err != nil {
		return nil, fmt.Errorf("set channel: %w", err)
	}
	return ch, nil
}

// DeleteChannel removes the SMTP binding for a status page.
func (s *SubscriptionService) DeleteChannel(ctx context.Context, statusPageID int64) error {
	if _, err := s.pages.GetByID(ctx, statusPageID); err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	return s.subscribers.DeleteChannel(ctx, statusPageID)
}

// NotifyIncidentCreated emails confirmed subscribers about a new active incident.
// Delivery errors are logged only — callers' CRUD must not fail.
func (s *SubscriptionService) NotifyIncidentCreated(ctx context.Context, inc *domain.Incident) error {
	if inc == nil || !inc.Active {
		return nil
	}
	return s.fanOutIncident(ctx, inc, true)
}

// NotifyIncidentResolved emails confirmed subscribers when an incident becomes inactive.
func (s *SubscriptionService) NotifyIncidentResolved(ctx context.Context, inc *domain.Incident) error {
	if inc == nil {
		return nil
	}
	return s.fanOutIncident(ctx, inc, false)
}

// NotifyIncidentUpdated emails confirmed subscribers when an incident timeline update is posted.
func (s *SubscriptionService) NotifyIncidentUpdated(
	ctx context.Context,
	inc *domain.Incident,
	update *domain.IncidentUpdate,
) error {
	if inc == nil || update == nil {
		return nil
	}
	return s.fanOutIncidentUpdate(ctx, inc, update)
}

// NotifyMaintenanceScheduled emails confirmed subscribers on status pages that
// include any of the linked monitors. Zero monitors suppresses all mail.
// Page IDs are de-duplicated.
func (s *SubscriptionService) NotifyMaintenanceScheduled(
	ctx context.Context,
	window *domain.MaintenanceWindow,
	monitorIDs []int64,
) error {
	if window == nil || len(monitorIDs) == 0 {
		return nil
	}
	pageIDs, err := s.subscribers.ListStatusPageIDsForMonitors(ctx, monitorIDs)
	if err != nil {
		slog.Error("subscription: list pages for maintenance failed", "error", err)
		return nil
	}
	// Deduplicate (repo should already return DISTINCT, but be safe).
	seen := make(map[int64]struct{}, len(pageIDs))
	for _, pageID := range pageIDs {
		if _, ok := seen[pageID]; ok {
			continue
		}
		seen[pageID] = struct{}{}
		sp, err := s.pages.GetByID(ctx, pageID)
		if err != nil {
			continue
		}
		if err := s.fanOutToPage(ctx, sp, func(pageTitle string) (string, string, string) {
			title := html.EscapeString(window.Title)
			page := html.EscapeString(pageTitle)
			subject := fmt.Sprintf("[%s] Maintenance scheduled: %s", pageTitle, window.Title)
			text := fmt.Sprintf(
				"Scheduled maintenance on %s\n\nTitle: %s\nDescription: %s\nStrategy: %s\n\n",
				pageTitle, window.Title, window.Description, window.Strategy,
			)
			htmlBody := fmt.Sprintf(
				"<p>Scheduled maintenance on <strong>%s</strong></p><p><strong>%s</strong></p><p>%s</p><p>Strategy: %s</p>",
				page, title, html.EscapeString(window.Description), html.EscapeString(window.Strategy),
			)
			return subject, text, htmlBody
		}); err != nil {
			slog.Error("subscription: maintenance fan-out failed", "status_page_id", pageID, "error", err)
		}
	}
	return nil
}

func (s *SubscriptionService) fanOutIncident(ctx context.Context, inc *domain.Incident, created bool) error {
	sp, err := s.pages.GetByID(ctx, inc.StatusPageID)
	if err != nil {
		slog.Error("subscription: load page for incident mail failed", "status_page_id", inc.StatusPageID, "error", err)
		return nil
	}
	kind := "resolved"
	if created {
		kind = "created"
	}
	if err := s.fanOutToPage(ctx, sp, func(pageTitle string) (string, string, string) {
		title := html.EscapeString(inc.Title)
		content := html.EscapeString(inc.Content)
		page := html.EscapeString(pageTitle)
		var subject, text, htmlBody string
		if created {
			subject = fmt.Sprintf("[%s] Incident: %s", pageTitle, inc.Title)
			text = fmt.Sprintf("A new incident was reported on %s\n\nTitle: %s\n\n%s\n", pageTitle, inc.Title, inc.Content)
			htmlBody = fmt.Sprintf(
				"<p>A new incident was reported on <strong>%s</strong></p><h2>%s</h2><p>%s</p>",
				page, title, content,
			)
		} else {
			subject = fmt.Sprintf("[%s] Resolved: %s", pageTitle, inc.Title)
			text = fmt.Sprintf("An incident was resolved on %s\n\nTitle: %s\n\n%s\n", pageTitle, inc.Title, inc.Content)
			htmlBody = fmt.Sprintf(
				"<p>An incident was resolved on <strong>%s</strong></p><h2>%s</h2><p>%s</p>",
				page, title, content,
			)
		}
		return subject, text, htmlBody
	}); err != nil {
		slog.Error("subscription: incident fan-out failed",
			"kind", kind, "status_page_id", inc.StatusPageID, "incident_id", inc.ID, "error", err)
	}
	return nil
}

func (s *SubscriptionService) fanOutIncidentUpdate(ctx context.Context, inc *domain.Incident, update *domain.IncidentUpdate) error {
	sp, err := s.pages.GetByID(ctx, inc.StatusPageID)
	if err != nil {
		slog.Error("subscription: load page for incident update mail failed", "status_page_id", inc.StatusPageID, "error", err)
		return nil
	}
	if err := s.fanOutToPage(ctx, sp, func(pageTitle string) (string, string, string) {
		title := html.EscapeString(inc.Title)
		content := html.EscapeString(update.Content)
		page := html.EscapeString(pageTitle)
		status := string(update.Status)
		subjectPrefix := "Incident update"
		if update.Status == domain.IncidentStatusResolved {
			subjectPrefix = "Resolved"
		}
		subject := fmt.Sprintf("[%s] %s: %s", pageTitle, subjectPrefix, inc.Title)
		text := fmt.Sprintf(
			"Incident update on %s\n\nTitle: %s\nStatus: %s\n\n%s\n",
			pageTitle, inc.Title, status, update.Content,
		)
		htmlBody := fmt.Sprintf(
			"<p>Incident update on <strong>%s</strong></p><h2>%s</h2><p><strong>Status:</strong> %s</p><p>%s</p>",
			page, title, html.EscapeString(status), content,
		)
		return subject, text, htmlBody
	}); err != nil {
		slog.Error("subscription: incident update fan-out failed",
			"status_page_id", inc.StatusPageID, "incident_id", inc.ID, "incident_update_id", update.ID, "error", err)
	}
	return nil
}

type bodyBuilder func(pageTitle string) (subject, text, htmlBody string)

func (s *SubscriptionService) fanOutToPage(ctx context.Context, sp *domain.StatusPage, build bodyBuilder) error {
	if s.publicURL == "" || s.mailer == nil || s.tokens == nil {
		return nil
	}
	smtpCfg, err := s.loadSMTPChannel(ctx, sp.ID)
	if err != nil {
		if errors.Is(err, errNoSubscriptionChannel) {
			return nil // nothing configured for this page — nothing to send
		}
		return fmt.Errorf("subscription fan-out: status page %d: %w", sp.ID, err)
	}
	subs, err := s.subscribers.ListConfirmedByStatusPage(ctx, sp.ID)
	if err != nil {
		return err
	}
	subject, text, htmlBody := build(sp.Title)
	for _, sub := range subs {
		unsubToken, err := s.tokens.IssueUnsubscribe(sub.ID)
		if err != nil {
			slog.Error("subscription: issue unsubscribe token failed", "error", err)
			continue
		}
		unsubURL := s.publicURL + "/status/" + url.PathEscape(sp.Slug) + "?unsubscribe=" + url.QueryEscape(unsubToken)
		fullText := text + "\nUnsubscribe: " + unsubURL + "\n"
		fullHTML := htmlBody + fmt.Sprintf(
			`<p style="margin-top:2em;font-size:12px;color:#666"><a href="%s">Unsubscribe</a></p>`,
			html.EscapeString(unsubURL),
		)
		msg := ports.EmailMessage{
			To:       sub.Email,
			Subject:  subject,
			TextBody: fullText,
			HTMLBody: fullHTML,
		}
		if err := s.mailer.Send(ctx, smtpCfg, msg); err != nil {
			// Never log the address.
			slog.Error("subscription: send failed", "status_page_id", sp.ID, "error", err)
		}
	}
	return nil
}

func (s *SubscriptionService) sendConfirmation(
	ctx context.Context,
	sp *domain.StatusPage,
	sub *domain.StatusPageSubscriber,
	smtpCfg map[string]any,
) error {
	expires := time.Now().UTC().Add(confirmationTokenTTL)
	token, err := s.tokens.IssueConfirmation(sub.ID, expires)
	if err != nil {
		return err
	}
	confirmURL := s.publicURL + "/status/" + url.PathEscape(sp.Slug) + "?confirm=" + url.QueryEscape(token)
	unsubToken, err := s.tokens.IssueUnsubscribe(sub.ID)
	if err != nil {
		return err
	}
	unsubURL := s.publicURL + "/status/" + url.PathEscape(sp.Slug) + "?unsubscribe=" + url.QueryEscape(unsubToken)

	page := html.EscapeString(sp.Title)
	subject := fmt.Sprintf("Confirm your subscription to %s", sp.Title)
	text := fmt.Sprintf(
		"Please confirm your subscription to %s status updates.\n\nConfirm: %s\n\nIf you did not request this, ignore this email or unsubscribe: %s\n",
		sp.Title, confirmURL, unsubURL,
	)
	htmlBody := fmt.Sprintf(
		`<p>Please confirm your subscription to <strong>%s</strong> status updates.</p>`+
			`<p><a href="%s">Confirm subscription</a></p>`+
			`<p style="font-size:12px;color:#666">If you did not request this, ignore this email or <a href="%s">unsubscribe</a>.</p>`,
		page, html.EscapeString(confirmURL), html.EscapeString(unsubURL),
	)
	return s.mailer.Send(ctx, smtpCfg, ports.EmailMessage{
		To:       sub.Email,
		Subject:  subject,
		TextBody: text,
		HTMLBody: htmlBody,
	})
}

func (s *SubscriptionService) sendAlreadySubscribed(
	ctx context.Context,
	sp *domain.StatusPage,
	sub *domain.StatusPageSubscriber,
	smtpCfg map[string]any,
) error {
	unsubToken, err := s.tokens.IssueUnsubscribe(sub.ID)
	if err != nil {
		return err
	}
	unsubURL := s.publicURL + "/status/" + url.PathEscape(sp.Slug) + "?unsubscribe=" + url.QueryEscape(unsubToken)
	page := html.EscapeString(sp.Title)
	subject := fmt.Sprintf("You are already subscribed to %s", sp.Title)
	text := fmt.Sprintf(
		"You are already subscribed to status updates for %s.\n\nUnsubscribe: %s\n",
		sp.Title, unsubURL,
	)
	htmlBody := fmt.Sprintf(
		`<p>You are already subscribed to status updates for <strong>%s</strong>.</p>`+
			`<p style="font-size:12px;color:#666"><a href="%s">Unsubscribe</a></p>`,
		page, html.EscapeString(unsubURL),
	)
	return s.mailer.Send(ctx, smtpCfg, ports.EmailMessage{
		To:       sub.Email,
		Subject:  subject,
		TextBody: text,
		HTMLBody: htmlBody,
	})
}

func (s *SubscriptionService) loadSMTPChannel(ctx context.Context, statusPageID int64) (map[string]any, error) {
	ch, err := s.subscribers.GetChannel(ctx, statusPageID)
	if err != nil {
		if errors.Is(err, ports.ErrNotFound) {
			return nil, errNoSubscriptionChannel
		}
		return nil, fmt.Errorf("load subscription channel: %w", err)
	}
	// A channel row exists but its notification does not resolve: that is a
	// dangling reference, not an unconfigured page, and must not be mistaken
	// for one even though the repository reports it as not-found.
	n, err := s.notifications.GetByID(ctx, ch.NotificationID)
	if err != nil {
		return nil, fmt.Errorf("subscription channel references notification %d: %w", ch.NotificationID, err)
	}
	if !n.Active || !strings.EqualFold(n.Type, "smtp") {
		return nil, fmt.Errorf("notification %d is not an active smtp channel (active=%t type=%q)", n.ID, n.Active, n.Type)
	}
	if n.Config == nil {
		return nil, fmt.Errorf("notification %d has no smtp config", n.ID)
	}
	return n.Config, nil
}

func normalizeEmail(raw string) (string, error) {
	email := strings.ToLower(strings.TrimSpace(raw))
	if email == "" {
		return "", fmt.Errorf("email is required")
	}
	if len(email) > 320 {
		return "", fmt.Errorf("email is too long")
	}
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return "", fmt.Errorf("invalid email address")
	}
	// Reject display-name forms; only bare addresses.
	if !strings.EqualFold(addr.Address, email) && addr.Name != "" {
		// ParseAddress accepts "Name <a@b.c>" — require the raw input to be the address.
		if strings.Contains(raw, "<") {
			return "", fmt.Errorf("invalid email address")
		}
	}
	normalized := strings.ToLower(addr.Address)
	if !strings.Contains(normalized, "@") {
		return "", fmt.Errorf("invalid email address")
	}
	return normalized, nil
}
