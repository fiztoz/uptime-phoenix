package services

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// --- fakes ----------------------------------------------------------------

type subFakePageRepo struct {
	mu     sync.Mutex
	byID   map[int64]*domain.StatusPage
	bySlug map[string]*domain.StatusPage
	next   int64
}

func newSubFakePageRepo() *subFakePageRepo {
	return &subFakePageRepo{
		byID:   make(map[int64]*domain.StatusPage),
		bySlug: make(map[string]*domain.StatusPage),
	}
}

func (r *subFakePageRepo) seed(sp *domain.StatusPage) *domain.StatusPage {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	sp.ID = r.next
	cp := *sp
	r.byID[sp.ID] = &cp
	r.bySlug[sp.Slug] = &cp
	return &cp
}

func (r *subFakePageRepo) Create(_ context.Context, sp *domain.StatusPage) error {
	r.seed(sp)
	return nil
}
func (r *subFakePageRepo) GetByID(_ context.Context, id int64) (*domain.StatusPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sp, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *sp
	return &cp, nil
}
func (r *subFakePageRepo) GetBySlug(_ context.Context, slug string) (*domain.StatusPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	sp, ok := r.bySlug[slug]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *sp
	return &cp, nil
}
func (r *subFakePageRepo) List(context.Context) ([]*domain.StatusPage, error) { return nil, nil }
func (r *subFakePageRepo) Update(context.Context, *domain.StatusPage) error   { return nil }
func (r *subFakePageRepo) Delete(context.Context, int64) error                { return nil }

type subFakeSubRepo struct {
	mu      sync.Mutex
	byID    map[int64]*domain.StatusPageSubscriber
	byKey   map[string]*domain.StatusPageSubscriber
	channel map[int64]*domain.StatusPageSubscriptionChannel
	pageMon map[int64][]int64 // pageID -> monitorIDs
	next    int64
	// channelErr, when set, is returned by GetChannel instead of consulting
	// the map — used to simulate a repository failure as opposed to an
	// unconfigured page. Those two must not behave the same way.
	channelErr error
}

func newSubFakeSubRepo() *subFakeSubRepo {
	return &subFakeSubRepo{
		byID:    make(map[int64]*domain.StatusPageSubscriber),
		byKey:   make(map[string]*domain.StatusPageSubscriber),
		channel: make(map[int64]*domain.StatusPageSubscriptionChannel),
		pageMon: make(map[int64][]int64),
	}
}

func subKey(pageID int64, email string) string {
	return strings.ToLower(email) + "@" + string(rune(pageID)) // simplified; use fmt below
}

func (r *subFakeSubRepo) key(pageID int64, email string) string {
	return strings.ToLower(email) + "|" + itoa64(pageID)
}

func itoa64(n int64) string {
	return strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(
		// cheap int64 → string without strconv import noise in helpers
		func() string {
			if n == 0 {
				return "0"
			}
			neg := n < 0
			if neg {
				n = -n
			}
			var b [20]byte
			i := len(b)
			for n > 0 {
				i--
				b[i] = byte('0' + n%10)
				n /= 10
			}
			if neg {
				i--
				b[i] = '-'
			}
			return string(b[i:])
		}(), " ", ""), "\t", ""))
}

func (r *subFakeSubRepo) Create(_ context.Context, sub *domain.StatusPageSubscriber) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := r.key(sub.StatusPageID, sub.Email)
	if _, ok := r.byKey[k]; ok {
		return ports.ErrConflict
	}
	r.next++
	sub.ID = r.next
	now := time.Now().UTC()
	sub.CreatedAt = now
	sub.UpdatedAt = now
	cp := *sub
	if sub.ConfirmedAt != nil {
		t := *sub.ConfirmedAt
		cp.ConfirmedAt = &t
	}
	r.byID[sub.ID] = &cp
	r.byKey[k] = &cp
	return nil
}

func (r *subFakeSubRepo) GetByID(_ context.Context, id int64) (*domain.StatusPageSubscriber, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *s
	if s.ConfirmedAt != nil {
		t := *s.ConfirmedAt
		cp.ConfirmedAt = &t
	}
	return &cp, nil
}

func (r *subFakeSubRepo) GetByPageAndEmail(_ context.Context, statusPageID int64, email string) (*domain.StatusPageSubscriber, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byKey[r.key(statusPageID, email)]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *s
	if s.ConfirmedAt != nil {
		t := *s.ConfirmedAt
		cp.ConfirmedAt = &t
	}
	return &cp, nil
}

func (r *subFakeSubRepo) Update(_ context.Context, sub *domain.StatusPageSubscriber) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[sub.ID]; !ok {
		return ports.ErrNotFound
	}
	sub.UpdatedAt = time.Now().UTC()
	cp := *sub
	if sub.ConfirmedAt != nil {
		t := *sub.ConfirmedAt
		cp.ConfirmedAt = &t
	}
	r.byID[sub.ID] = &cp
	r.byKey[r.key(sub.StatusPageID, sub.Email)] = &cp
	return nil
}

func (r *subFakeSubRepo) Delete(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.byID[id]
	if !ok {
		return nil
	}
	delete(r.byID, id)
	delete(r.byKey, r.key(s.StatusPageID, s.Email))
	return nil
}

func (r *subFakeSubRepo) ListByStatusPage(_ context.Context, statusPageID int64) ([]*domain.StatusPageSubscriber, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.StatusPageSubscriber
	for _, s := range r.byID {
		if s.StatusPageID == statusPageID {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *subFakeSubRepo) ListConfirmedByStatusPage(_ context.Context, statusPageID int64) ([]*domain.StatusPageSubscriber, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.StatusPageSubscriber
	for _, s := range r.byID {
		if s.StatusPageID == statusPageID && s.Active {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *subFakeSubRepo) GetChannel(_ context.Context, statusPageID int64) (*domain.StatusPageSubscriptionChannel, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.channelErr != nil {
		return nil, r.channelErr
	}
	ch, ok := r.channel[statusPageID]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *ch
	return &cp, nil
}

func (r *subFakeSubRepo) SetChannel(_ context.Context, channel *domain.StatusPageSubscriptionChannel) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *channel
	r.channel[channel.StatusPageID] = &cp
	return nil
}

func (r *subFakeSubRepo) DeleteChannel(_ context.Context, statusPageID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.channel, statusPageID)
	return nil
}

func (r *subFakeSubRepo) ListStatusPageIDsForMonitors(_ context.Context, monitorIDs []int64) ([]int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	want := make(map[int64]struct{}, len(monitorIDs))
	for _, id := range monitorIDs {
		want[id] = struct{}{}
	}
	seen := map[int64]struct{}{}
	var out []int64
	for pageID, mids := range r.pageMon {
		for _, mid := range mids {
			if _, ok := want[mid]; ok {
				if _, dup := seen[pageID]; !dup {
					seen[pageID] = struct{}{}
					out = append(out, pageID)
				}
				break
			}
		}
	}
	return out, nil
}

type subFakeNotifRepo struct {
	byID map[int64]*domain.Notification
}

func (r *subFakeNotifRepo) Create(context.Context, *domain.Notification) error { return nil }
func (r *subFakeNotifRepo) GetByID(_ context.Context, id int64) (*domain.Notification, error) {
	n, ok := r.byID[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	cp := *n
	return &cp, nil
}
func (r *subFakeNotifRepo) GetByMonitorID(context.Context, int64) ([]*domain.Notification, error) {
	return nil, nil
}
func (r *subFakeNotifRepo) List(context.Context, int64) ([]*domain.Notification, error) {
	return nil, nil
}
func (r *subFakeNotifRepo) ListAll(context.Context) ([]*domain.Notification, error) { return nil, nil }
func (r *subFakeNotifRepo) Update(context.Context, *domain.Notification) error      { return nil }
func (r *subFakeNotifRepo) Delete(context.Context, int64) error                     { return nil }

type subFakeTokens struct {
	mu     sync.Mutex
	tokens map[string]struct {
		id      int64
		purpose string
		exp     time.Time
	}
	seq int
}

func newSubFakeTokens() *subFakeTokens {
	return &subFakeTokens{tokens: make(map[string]struct {
		id      int64
		purpose string
		exp     time.Time
	})}
}

func (t *subFakeTokens) IssueConfirmation(subscriberID int64, expiresAt time.Time) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	tok := "confirm-" + itoa64(int64(t.seq)) + "-" + itoa64(subscriberID)
	t.tokens[tok] = struct {
		id      int64
		purpose string
		exp     time.Time
	}{id: subscriberID, purpose: ports.SubscriberTokenConfirm, exp: expiresAt}
	return tok, nil
}

func (t *subFakeTokens) IssueUnsubscribe(subscriberID int64) (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seq++
	tok := "unsub-" + itoa64(int64(t.seq)) + "-" + itoa64(subscriberID)
	t.tokens[tok] = struct {
		id      int64
		purpose string
		exp     time.Time
	}{id: subscriberID, purpose: ports.SubscriberTokenUnsubscribe, exp: time.Now().Add(365 * 24 * time.Hour)}
	return tok, nil
}

func (t *subFakeTokens) Verify(token string, expectedPurpose string) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	info, ok := t.tokens[token]
	if !ok {
		return 0, ports.ErrSubscriberToken
	}
	if info.purpose != expectedPurpose {
		return 0, ports.ErrSubscriberToken
	}
	if !info.exp.IsZero() && time.Now().After(info.exp) {
		return 0, ports.ErrSubscriberToken
	}
	return info.id, nil
}

type capturedMail struct {
	To      string
	Subject string
	Text    string
	HTML    string
	Config  map[string]any
}

type subFakeMailer struct {
	mu      sync.Mutex
	sent    []capturedMail
	failAll bool
}

func (m *subFakeMailer) Send(_ context.Context, smtpConfig map[string]any, msg ports.EmailMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failAll {
		return errors.New("smtp down")
	}
	m.sent = append(m.sent, capturedMail{
		To: msg.To, Subject: msg.Subject, Text: msg.TextBody, HTML: msg.HTMLBody, Config: smtpConfig,
	})
	return nil
}

func (m *subFakeMailer) last() *capturedMail {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.sent) == 0 {
		return nil
	}
	cp := m.sent[len(m.sent)-1]
	return &cp
}

func (m *subFakeMailer) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sent)
}

type subFakePasswords struct {
	// hash -> plaintext accepted
	ok map[string]string
}

func (p *subFakePasswords) Hash(password string) (string, error) { return "hash:" + password, nil }
func (p *subFakePasswords) Verify(hashed, password string) error {
	if p.ok != nil {
		if want, ok := p.ok[hashed]; ok && want == password {
			return nil
		}
	}
	if hashed == "hash:"+password {
		return nil
	}
	return errors.New("mismatch")
}

func newTestSubscriptionService(
	pages *subFakePageRepo,
	subs *subFakeSubRepo,
	notifs *subFakeNotifRepo,
	tokens *subFakeTokens,
	mailer *subFakeMailer,
	publicURL string,
) *SubscriptionService {
	return NewSubscriptionService(pages, subs, notifs, tokens, mailer, &subFakePasswords{}, publicURL)
}

func seedSMTPPage(pages *subFakePageRepo, subs *subFakeSubRepo, notifs *subFakeNotifRepo) *domain.StatusPage {
	sp := pages.seed(&domain.StatusPage{
		Slug: "acme", Title: "Acme Status", Published: true,
	})
	notifs.byID = map[int64]*domain.Notification{
		10: {
			ID: 10, Name: "SMTP", Type: "smtp", Active: true,
			Config: map[string]any{
				"host": "smtp.example.com", "port": float64(587),
				"from": "alerts@example.com", "to": "ops@example.com",
			},
		},
	}
	_ = subs.SetChannel(context.Background(), &domain.StatusPageSubscriptionChannel{
		StatusPageID: sp.ID, NotificationID: 10,
	})
	return sp
}

// --- tests ----------------------------------------------------------------

func TestSubscribe_Confirm_Unsubscribe_Flow(t *testing.T) {
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	notifs := &subFakeNotifRepo{}
	tokens := newSubFakeTokens()
	mailer := &subFakeMailer{}
	seedSMTPPage(pages, subs, notifs)
	svc := newTestSubscriptionService(pages, subs, notifs, tokens, mailer, "https://status.example.com")

	ctx := context.Background()
	if err := svc.Subscribe(ctx, "acme", "User@Example.COM", ""); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if mailer.count() != 1 {
		t.Fatalf("want 1 confirmation mail, got %d", mailer.count())
	}
	mail := mailer.last()
	if mail.To != "user@example.com" {
		t.Fatalf("email not normalized: %q", mail.To)
	}
	if !strings.Contains(mail.Text, "Confirm:") || !strings.Contains(mail.HTML, "Confirm subscription") {
		t.Fatalf("confirmation body missing links: text=%q html=%q", mail.Text, mail.HTML)
	}
	if strings.Contains(mail.HTML, "<script>") {
		t.Fatal("unexpected raw script in HTML")
	}
	// Extract confirm token from text body.
	confirmTok := extractConfirmTokenFromURL(mail.Text)
	if confirmTok == "" {
		t.Fatalf("no confirm token in body: %s", mail.Text)
	}

	// Pending, not active yet.
	list, _ := svc.ListSubscribers(ctx, 1)
	if len(list) != 1 || list[0].Active {
		t.Fatalf("expected one pending subscriber, got %+v", list)
	}

	if err := svc.Confirm(ctx, confirmTok); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	got, err := subs.GetByPageAndEmail(ctx, 1, "user@example.com")
	if err != nil || !got.Active || got.ConfirmedAt == nil {
		t.Fatalf("subscriber not activated: %+v err=%v", got, err)
	}

	// Idempotent confirm.
	if err := svc.Confirm(ctx, confirmTok); err != nil {
		t.Fatalf("re-confirm: %v", err)
	}

	unsubTok, err := tokens.IssueUnsubscribe(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Unsubscribe(ctx, unsubTok); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if _, err := subs.GetByID(ctx, got.ID); !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("subscriber should be deleted, err=%v", err)
	}
}

func TestSubscribe_DuplicateReturns202PathAndSendsMail(t *testing.T) {
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	notifs := &subFakeNotifRepo{}
	tokens := newSubFakeTokens()
	mailer := &subFakeMailer{}
	seedSMTPPage(pages, subs, notifs)
	svc := newTestSubscriptionService(pages, subs, notifs, tokens, mailer, "https://status.example.com")
	ctx := context.Background()

	_ = svc.Subscribe(ctx, "acme", "a@b.co", "")
	confirmTok := extractConfirmTokenFromURL(mailer.last().Text)
	_ = svc.Confirm(ctx, confirmTok)

	before := mailer.count()
	if err := svc.Subscribe(ctx, "acme", "a@b.co", ""); err != nil {
		t.Fatalf("duplicate subscribe should succeed (202 path): %v", err)
	}
	if mailer.count() != before+1 {
		t.Fatalf("active duplicate must still send management mail")
	}
	if !strings.Contains(mailer.last().Subject, "already subscribed") {
		t.Fatalf("expected already-subscribed subject, got %q", mailer.last().Subject)
	}
}

func TestSubscribe_WrongPurposeTokenRejected(t *testing.T) {
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	notifs := &subFakeNotifRepo{}
	tokens := newSubFakeTokens()
	mailer := &subFakeMailer{}
	seedSMTPPage(pages, subs, notifs)
	svc := newTestSubscriptionService(pages, subs, notifs, tokens, mailer, "https://status.example.com")
	ctx := context.Background()
	_ = svc.Subscribe(ctx, "acme", "a@b.co", "")
	list, _ := svc.ListSubscribers(ctx, 1)
	unsub, _ := tokens.IssueUnsubscribe(list[0].ID)

	if err := svc.Confirm(ctx, unsub); !errors.Is(err, ports.ErrSubscriberToken) {
		t.Fatalf("confirm with unsub token: want ErrSubscriberToken, got %v", err)
	}
}

func TestSubscribe_ExpiredConfirmRejected(t *testing.T) {
	tokens := newSubFakeTokens()
	// Issue an already-expired token for the created subscriber.
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	_ = subs.Create(context.Background(), &domain.StatusPageSubscriber{StatusPageID: 1, Email: "x@y.z"})
	tok, _ := tokens.IssueConfirmation(1, time.Now().Add(-time.Minute))
	svc := newTestSubscriptionService(pages, subs, &subFakeNotifRepo{}, tokens, &subFakeMailer{}, "https://x")
	if err := svc.Confirm(context.Background(), tok); !errors.Is(err, ports.ErrSubscriberToken) {
		t.Fatalf("expired confirm: want ErrSubscriberToken, got %v", err)
	}
}

func TestSubscribe_AccessCodeRequired(t *testing.T) {
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	notifs := &subFakeNotifRepo{}
	tokens := newSubFakeTokens()
	mailer := &subFakeMailer{}
	sp := seedSMTPPage(pages, subs, notifs)
	// Protect the page.
	pages.mu.Lock()
	pages.byID[sp.ID].PasswordHash = "hash:secret"
	pages.bySlug[sp.Slug].PasswordHash = "hash:secret"
	pages.mu.Unlock()

	svc := newTestSubscriptionService(pages, subs, notifs, tokens, mailer, "https://status.example.com")
	ctx := context.Background()
	if err := svc.Subscribe(ctx, "acme", "a@b.co", ""); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("want unauthorized without code, got %v", err)
	}
	if err := svc.Subscribe(ctx, "acme", "a@b.co", "wrong"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("want unauthorized wrong code, got %v", err)
	}
	if err := svc.Subscribe(ctx, "acme", "a@b.co", "secret"); err != nil {
		t.Fatalf("correct code: %v", err)
	}
}

func TestSubscribe_NoPublicURLUnavailable(t *testing.T) {
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	notifs := &subFakeNotifRepo{}
	seedSMTPPage(pages, subs, notifs)
	svc := newTestSubscriptionService(pages, subs, notifs, newSubFakeTokens(), &subFakeMailer{}, "")
	if err := svc.Subscribe(context.Background(), "acme", "a@b.co", ""); !errors.Is(err, ErrSubscriptionsUnavailable) {
		t.Fatalf("want unavailable, got %v", err)
	}
}

func TestSubscribe_SMTPFailureUnavailable(t *testing.T) {
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	notifs := &subFakeNotifRepo{}
	seedSMTPPage(pages, subs, notifs)
	mailer := &subFakeMailer{failAll: true}
	svc := newTestSubscriptionService(pages, subs, notifs, newSubFakeTokens(), mailer, "https://x")
	if err := svc.Subscribe(context.Background(), "acme", "a@b.co", ""); !errors.Is(err, ErrSubscriptionsUnavailable) {
		t.Fatalf("want unavailable on SMTP fail, got %v", err)
	}
}

func TestSubscribe_HTMLEscapesPageTitle(t *testing.T) {
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	notifs := &subFakeNotifRepo{}
	sp := seedSMTPPage(pages, subs, notifs)
	pages.mu.Lock()
	pages.byID[sp.ID].Title = `<img src=x onerror=alert(1)>`
	pages.bySlug[sp.Slug].Title = `<img src=x onerror=alert(1)>`
	pages.mu.Unlock()
	mailer := &subFakeMailer{}
	svc := newTestSubscriptionService(pages, subs, notifs, newSubFakeTokens(), mailer, "https://x")
	_ = svc.Subscribe(context.Background(), "acme", "a@b.co", "")
	htmlBody := mailer.last().HTML
	// Escaped form still contains the characters of "onerror=alert" as text;
	// the load-bearing check is that the angle brackets were escaped so the
	// browser cannot reconstitute a tag.
	if strings.Contains(htmlBody, "<img") {
		t.Fatalf("raw <img tag in HTML: %s", htmlBody)
	}
	if !strings.Contains(htmlBody, "&lt;img") {
		t.Fatalf("expected escaped title, got %s", htmlBody)
	}
}

// A fan-out that cannot load its SMTP channel for a REAL reason must surface
// that error — a database error, a dangling notification reference or a
// deactivated channel must not silently stop all subscriber email. Only a page
// with no channel configured at all is allowed to skip quietly.
func TestNotifyIncident_ChannelFailureIsReportedNotSwallowed(t *testing.T) {
	seed := func() (*subFakePageRepo, *subFakeSubRepo, *subFakeNotifRepo, *subFakeMailer, *domain.StatusPage) {
		pages := newSubFakePageRepo()
		subs := newSubFakeSubRepo()
		notifs := &subFakeNotifRepo{}
		mailer := &subFakeMailer{}
		sp := seedSMTPPage(pages, subs, notifs)
		return pages, subs, notifs, mailer, sp
	}

	// NotifyIncidentCreated deliberately returns nil on a mail failure so that a
	// broken SMTP channel cannot fail incident creation itself. The contract it
	// must honor is therefore VISIBILITY: the failure has to reach the log.
	// Before the fix it reached nothing at all.
	captureLogs := func(t *testing.T) *strings.Builder {
		t.Helper()
		var buf strings.Builder
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
		t.Cleanup(func() { slog.SetDefault(prev) })
		return &buf
	}

	t.Run("repository failure is logged", func(t *testing.T) {
		pages, subs, notifs, mailer, sp := seed()
		svc := newTestSubscriptionService(pages, subs, notifs, newSubFakeTokens(), mailer, "https://status.example.com")
		logs := captureLogs(t)
		subs.channelErr = errors.New("database is down")

		_ = svc.NotifyIncidentCreated(context.Background(),
			&domain.Incident{ID: 5, StatusPageID: sp.ID, Title: "DB down", Active: true})

		if logs.Len() == 0 {
			t.Fatal("channel lookup failed but nothing was logged: subscriber email " +
				"stopped silently, which is exactly the regression this guards")
		}
		if !strings.Contains(logs.String(), "database is down") {
			t.Errorf("log should name the underlying cause, got: %s", logs.String())
		}
		if mailer.count() != 0 {
			t.Errorf("no mail should have been sent, got %d", mailer.count())
		}
	})

	t.Run("deactivated channel is logged", func(t *testing.T) {
		pages, subs, notifs, mailer, sp := seed()
		svc := newTestSubscriptionService(pages, subs, notifs, newSubFakeTokens(), mailer, "https://status.example.com")
		logs := captureLogs(t)
		notifs.byID[10].Active = false

		_ = svc.NotifyIncidentCreated(context.Background(),
			&domain.Incident{ID: 6, StatusPageID: sp.ID, Title: "DB down", Active: true})

		if logs.Len() == 0 {
			t.Fatal("SMTP channel was deactivated but the fan-out reported nothing")
		}
	})

	t.Run("unconfigured page still skips quietly", func(t *testing.T) {
		pages := newSubFakePageRepo()
		subs := newSubFakeSubRepo()
		notifs := &subFakeNotifRepo{byID: map[int64]*domain.Notification{}}
		mailer := &subFakeMailer{}
		sp := pages.seed(&domain.StatusPage{Slug: "bare", Title: "Bare", Published: true})
		svc := newTestSubscriptionService(pages, subs, notifs, newSubFakeTokens(), mailer, "https://status.example.com")

		if err := svc.NotifyIncidentCreated(context.Background(),
			&domain.Incident{ID: 7, StatusPageID: sp.ID, Title: "DB down", Active: true}); err != nil {
			t.Fatalf("a page with no subscription channel must skip without error, got: %v", err)
		}
		if mailer.count() != 0 {
			t.Fatalf("unconfigured page sent %d mails", mailer.count())
		}
	})
}

func TestNotifyIncident_OpenAndResolveOnce(t *testing.T) {
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	notifs := &subFakeNotifRepo{}
	tokens := newSubFakeTokens()
	mailer := &subFakeMailer{}
	sp := seedSMTPPage(pages, subs, notifs)
	svc := newTestSubscriptionService(pages, subs, notifs, tokens, mailer, "https://status.example.com")
	ctx := context.Background()

	// Confirmed subscriber.
	_ = svc.Subscribe(ctx, "acme", "fan@out.test", "")
	_ = svc.Confirm(ctx, extractConfirmTokenFromURL(mailer.last().Text))
	before := mailer.count()

	inc := &domain.Incident{ID: 5, StatusPageID: sp.ID, Title: "DB down", Content: "Primary offline", Active: true}
	if err := svc.NotifyIncidentCreated(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if mailer.count() != before+1 {
		t.Fatalf("want one create mail, got %d", mailer.count()-before)
	}
	if !strings.Contains(mailer.last().Subject, "Incident") {
		t.Fatalf("create subject: %s", mailer.last().Subject)
	}
	if !strings.Contains(mailer.last().Text, "Unsubscribe:") {
		t.Fatal("create mail missing unsubscribe")
	}

	inc.Active = false
	if err := svc.NotifyIncidentResolved(ctx, inc); err != nil {
		t.Fatal(err)
	}
	if mailer.count() != before+2 {
		t.Fatalf("want one resolve mail, got %d", mailer.count()-before)
	}
	if !strings.Contains(mailer.last().Subject, "Resolved") {
		t.Fatalf("resolve subject: %s", mailer.last().Subject)
	}
}

func TestNotifyIncidentUpdated_IncludesStatusAndContent(t *testing.T) {
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	notifs := &subFakeNotifRepo{}
	tokens := newSubFakeTokens()
	mailer := &subFakeMailer{}
	sp := seedSMTPPage(pages, subs, notifs)
	svc := newTestSubscriptionService(pages, subs, notifs, tokens, mailer, "https://status.example.com")
	ctx := context.Background()

	_ = svc.Subscribe(ctx, "acme", "fan@out.test", "")
	_ = svc.Confirm(ctx, extractConfirmTokenFromURL(mailer.last().Text))
	before := mailer.count()

	inc := &domain.Incident{ID: 9, StatusPageID: sp.ID, Title: "API latency", Content: "Investigating", Active: true}
	update := &domain.IncidentUpdate{
		ID: 3, IncidentID: inc.ID, StatusPageID: sp.ID,
		Status: domain.IncidentStatusIdentified, Content: "Root cause **found**.",
	}
	if err := svc.NotifyIncidentUpdated(ctx, inc, update); err != nil {
		t.Fatal(err)
	}
	if mailer.count() != before+1 {
		t.Fatalf("want one update mail, got %d", mailer.count()-before)
	}
	msg := mailer.last()
	if !strings.Contains(msg.Subject, "Incident update") {
		t.Fatalf("update subject: %s", msg.Subject)
	}
	if !strings.Contains(msg.Text, "Status: identified") {
		t.Fatalf("update text missing status: %s", msg.Text)
	}
	if !strings.Contains(msg.Text, "Root cause **found**.") {
		t.Fatalf("update text missing markdown body: %s", msg.Text)
	}
	if !strings.Contains(msg.Text, "Unsubscribe:") {
		t.Fatal("update mail missing unsubscribe")
	}
}

func TestNotifyMaintenance_DedupeAndZeroMonitors(t *testing.T) {
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	notifs := &subFakeNotifRepo{}
	tokens := newSubFakeTokens()
	mailer := &subFakeMailer{}
	sp1 := seedSMTPPage(pages, subs, notifs)
	// Second page sharing monitors.
	sp2 := pages.seed(&domain.StatusPage{Slug: "beta", Title: "Beta", Published: true})
	notifs.byID[11] = &domain.Notification{
		ID: 11, Type: "smtp", Active: true,
		Config: map[string]any{"host": "smtp.example.com", "port": 587, "from": "a@b.c"},
	}
	_ = subs.SetChannel(ctxBG(), &domain.StatusPageSubscriptionChannel{StatusPageID: sp2.ID, NotificationID: 11})
	subs.pageMon[sp1.ID] = []int64{100, 200}
	subs.pageMon[sp2.ID] = []int64{200}

	svc := newTestSubscriptionService(pages, subs, notifs, tokens, mailer, "https://status.example.com")
	ctx := context.Background()

	// Confirmed on both pages.
	_ = svc.Subscribe(ctx, "acme", "a@b.co", "")
	_ = svc.Confirm(ctx, extractConfirmTokenFromURL(mailer.last().Text))
	// Manually add confirmed sub on sp2.
	sub2 := &domain.StatusPageSubscriber{StatusPageID: sp2.ID, Email: "a@b.co", Active: true}
	now := time.Now().UTC()
	sub2.ConfirmedAt = &now
	_ = subs.Create(ctx, sub2)

	before := mailer.count()
	// Zero monitors → nothing.
	_ = svc.NotifyMaintenanceScheduled(ctx, &domain.MaintenanceWindow{Title: "x", Strategy: "single"}, nil)
	if mailer.count() != before {
		t.Fatal("zero monitors must not send")
	}

	// Monitors spanning both pages → one mail per page (deduped page IDs).
	_ = svc.NotifyMaintenanceScheduled(ctx, &domain.MaintenanceWindow{
		Title: "Nightly", Description: "reboot", Strategy: "single",
	}, []int64{100, 200, 200})
	// 2 pages × 1 subscriber each = 2 mails (sp1 has fan@? wait acme has a@b.co, sp2 has a@b.co)
	if mailer.count() != before+2 {
		t.Fatalf("want 2 maintenance mails (deduped pages), got %d", mailer.count()-before)
	}
}

func TestSetChannel_RequiresActiveSMTP(t *testing.T) {
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	notifs := &subFakeNotifRepo{byID: map[int64]*domain.Notification{
		1: {ID: 1, Type: "discord", Active: true, Config: map[string]any{}},
		2: {ID: 2, Type: "smtp", Active: false, Config: map[string]any{"host": "h", "from": "f"}},
		3: {ID: 3, Type: "smtp", Active: true, Config: map[string]any{"host": "h", "from": "f"}},
	}}
	sp := pages.seed(&domain.StatusPage{Slug: "p", Title: "P", Published: true})
	svc := newTestSubscriptionService(pages, subs, notifs, newSubFakeTokens(), &subFakeMailer{}, "https://x")
	ctx := context.Background()

	if _, err := svc.SetChannel(ctx, sp.ID, 1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("discord: want validation, got %v", err)
	}
	if _, err := svc.SetChannel(ctx, sp.ID, 2); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("inactive smtp: want validation, got %v", err)
	}
	ch, err := svc.SetChannel(ctx, sp.ID, 3)
	if err != nil || ch.NotificationID != 3 {
		t.Fatalf("active smtp: %+v err=%v", ch, err)
	}
}

func TestNoTokenLeakInAdminList(t *testing.T) {
	pages := newSubFakePageRepo()
	subs := newSubFakeSubRepo()
	notifs := &subFakeNotifRepo{}
	seedSMTPPage(pages, subs, notifs)
	svc := newTestSubscriptionService(pages, subs, notifs, newSubFakeTokens(), &subFakeMailer{}, "https://x")
	ctx := context.Background()
	_ = svc.Subscribe(ctx, "acme", "a@b.co", "")
	list, err := svc.ListSubscribers(ctx, 1)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %+v", err, list)
	}
	// Domain subscriber has no token fields — wire View is built in handlers.
	if list[0].Email != "a@b.co" {
		t.Fatalf("email: %s", list[0].Email)
	}
}

func extractConfirmTokenFromURL(body string) string {
	key := "confirm="
	idx := strings.Index(body, key)
	if idx < 0 {
		return ""
	}
	rest := body[idx+len(key):]
	end := strings.IndexAny(rest, " \n\r\t\"'<>")
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func ctxBG() context.Context { return context.Background() }

// Silence unused helper.
var _ = subKey
