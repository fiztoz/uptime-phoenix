package probe

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// exactRegionalAssignmentRevision is 2^53+1: past float64 precision, so any
// floating-point round trip through the read path would corrupt the immutable
// assignment-set binding instead of reporting it exactly.
const exactRegionalAssignmentRevision = 9007199254740993

// regionalReadHarness runs the real edge stack around EdgeConfigService:
// real private SQLite store, real single-use enrollment, real accepted
// connection-generation fence, real AES-256-GCM protector and the production
// config service. Nothing in the read path is faked.
type regionalReadHarness struct {
	t          *testing.T
	dir        string
	id         *RuntimeIdentity
	store      *edge.Store
	protector  *auth.ProbeConfigProtector
	configs    *services.EdgeConfigService
	hubID      string
	generation int64
	closeStore func()
}

func newRegionalReadHarness(t *testing.T) *regionalReadHarness {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id, err := InitializeRuntimeIdentity(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = id.Close() })
	store, err := edge.Open(t.Context(), dir, domain.EdgeIdentity{ProbeID: id.ProbeID, StreamID: id.StreamID, Fingerprint: id.Fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	closeStore := func() { once.Do(func() { _ = store.Close() }) }
	t.Cleanup(closeStore)
	at := time.Now().UTC()
	enrollment := services.NewEdgeEnrollmentService(store)
	token, err := enrollment.Issue(t.Context(), at)
	if err != nil {
		t.Fatal(err)
	}
	hubID := uuid.NewString()
	runtimeToken := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	if err := enrollment.Accept(t.Context(), token, runtimeToken, domain.EdgeEnrollment{HubID: hubID, ProbeID: id.ProbeID, EnrollmentID: uuid.NewString(), CredentialVersion: 1}, at); err != nil {
		t.Fatal(err)
	}
	const generation = 7
	if err := store.AcceptConnectionGeneration(t.Context(), hubID, generation); err != nil {
		t.Fatal(err)
	}
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	harness := &regionalReadHarness{t: t, dir: dir, id: id, store: store, protector: protector, hubID: hubID, generation: generation, closeStore: closeStore}
	harness.configs = services.NewEdgeConfigService(store, store, NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	return harness
}

// service wires the real decoder and protector around caller-selected ports so
// bounded interleaving doubles can replace only identity/storage reads.
func (h *regionalReadHarness) service(identity ports.EdgeIdentityRepository, store ports.EdgeConfigRepository) *services.EdgeConfigService {
	return services.NewEdgeConfigService(identity, store, NewEdgeConfigDecoder(checker.Get, notifier.Get), h.protector)
}

// document builds the fixture config bound to this harness's enrolled identity.
// A zero assignment revision is the legacy unbound document shape.
func (h *regionalReadHarness) document(mode domain.AlertDelivery, assignmentRevision int64) []byte {
	h.t.Helper()
	snapshot := m2Config(h.t)
	snapshot.HubID, snapshot.ProbeID = h.hubID, h.id.ProbeID
	snapshot.Assignments[0].AlertDelivery = mode
	snapshot.Assignments[0].AssignmentRevision = Decimal(assignmentRevision)
	return configBytes(h.t, snapshot)
}

func (h *regionalReadHarness) apply(document []byte) *domain.EdgeResolvedConfig {
	h.t.Helper()
	resolved, err := h.configs.Apply(h.t.Context(), document, h.generation, time.Now().UTC())
	if err != nil {
		h.t.Fatal(err)
	}
	return resolved
}

func (h *regionalReadHarness) read(metadata domain.ProbeConfigMetadata) ([]domain.ProbeDeliveryModeApplication, error) {
	return h.configs.ReadRegionalApplications(h.t.Context(), metadata, h.generation, h.id.StreamID)
}

// expectedFact derives the exact fact tuple the retained document must report:
// effective regional delivery regardless of the desired mode it carries.
func (h *regionalReadHarness) expectedFact(resolved *domain.EdgeResolvedConfig) domain.ProbeDeliveryModeApplication {
	assignment := resolved.Assignments[0]
	return domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget:  domain.ProbeConfigTarget{HubID: h.hubID, ProbeID: h.id.ProbeID},
		MonitorID:          assignment.Monitor.ID,
		AssignmentRevision: assignment.AssignmentRevision,
		Generation:         assignment.Generation,
		ConfigRevision:     resolved.Metadata.Revision,
		SHA256:             resolved.Metadata.SHA256,
		StreamID:           h.id.StreamID,
		AlertDelivery:      domain.AlertDeliveryRegional,
	}
}

// regionalReadIdentityMock replays a fixed identity sequence so a test can
// prove the post-storage identity recheck refuses a session that changed while
// the active tuple was being authenticated.
type regionalReadIdentityMock struct {
	calls    int
	sequence []domain.EdgeIdentity
}

var _ ports.EdgeIdentityRepository = (*regionalReadIdentityMock)(nil)

func (m *regionalReadIdentityMock) ReadIdentity(context.Context) (domain.EdgeIdentity, error) {
	if len(m.sequence) == 0 {
		return domain.EdgeIdentity{}, errors.New("no identity configured")
	}
	index := m.calls
	m.calls++
	if index >= len(m.sequence) {
		index = len(m.sequence) - 1
	}
	return m.sequence[index], nil
}

func (*regionalReadIdentityMock) AcceptConnectionGeneration(context.Context, string, int64) error {
	return errors.New("unexpected connection generation acceptance")
}

// regionalReadConfigMock serves one captured active tuple and a poison tuple on
// repeat reads so a second storage read is detectable instead of invisible.
type regionalReadConfigMock struct {
	reads     int
	primary   domain.EdgeActiveConfig
	secondary *domain.EdgeActiveConfig
}

var _ ports.EdgeConfigRepository = (*regionalReadConfigMock)(nil)

func (*regionalReadConfigMock) ActivateConfig(context.Context, domain.EdgeActiveConfig) error {
	return errors.New("unexpected activation")
}

func (m *regionalReadConfigMock) ReadActiveConfig(context.Context) (domain.EdgeActiveConfig, error) {
	m.reads++
	if m.secondary != nil && m.reads > 1 {
		return *m.secondary, nil
	}
	return m.primary, nil
}

// regionalReadLegacyWriteStore persists activations exactly like a build that
// predates durable regional facts: the real store commits the encrypted
// document and assignment index but receives no fact rows at all.
type regionalReadLegacyWriteStore struct{ *edge.Store }

func (s *regionalReadLegacyWriteStore) ActivateConfig(ctx context.Context, config domain.EdgeActiveConfig) error {
	config.RegionalDeliveryApplications = nil
	return s.Store.ActivateConfig(ctx, config)
}

// regionalReadOpen opens a second connection to the harness database so a test
// can tamper with durable rows the way on-disk corruption or a hostile writer
// would, without going through any service or store write path.
func regionalReadOpen(t *testing.T, dir string) *sql.DB {
	t.Helper()
	target := url.URL{Scheme: "file", Path: filepath.Join(dir, "edge.db")}
	query := target.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	target.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", target.String())
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func regionalReadExec(t *testing.T, dir, statement string, args ...any) {
	t.Helper()
	db := regionalReadOpen(t, dir)
	defer func() { _ = db.Close() }()
	if _, err := db.ExecContext(t.Context(), statement, args...); err != nil {
		t.Fatal(err)
	}
}

// regionalReadFlipActivePayload flips the last byte of the retained ciphertext,
// tampering the AEAD tag without changing any stored length.
func regionalReadFlipActivePayload(t *testing.T, dir string, revision int64) {
	t.Helper()
	db := regionalReadOpen(t, dir)
	defer func() { _ = db.Close() }()
	var payload []byte
	if err := db.QueryRowContext(t.Context(), "SELECT protected_payload FROM edge_config WHERE revision = ?", revision).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload) == 0 {
		t.Fatal("no protected payload to tamper")
	}
	payload[len(payload)-1] ^= 0xff
	if _, err := db.ExecContext(t.Context(), "UPDATE edge_config SET protected_payload = ? WHERE revision = ?", payload, revision); err != nil {
		t.Fatal(err)
	}
}

// TestRegionalApplicationReadReportsHonestRegionalFacts proves the read reports
// the effective regional facts of the exact retained document even when that
// document desires aggregate or both paging, and that the immutable
// assignment-set binding crosses the read with full int64 precision.
func TestRegionalApplicationReadReportsHonestRegionalFacts(t *testing.T) {
	for _, desired := range []domain.AlertDelivery{domain.AlertDeliveryAggregate, domain.AlertDeliveryBoth} {
		t.Run(string(desired), func(t *testing.T) {
			h := newRegionalReadHarness(t)
			resolved := h.apply(h.document(desired, exactRegionalAssignmentRevision))
			if resolved.Assignments[0].DesiredAlertDelivery != desired || resolved.Assignments[0].AssignmentRevision != exactRegionalAssignmentRevision {
				t.Fatal("the exact document lost its desired mode or immutable binding")
			}
			facts, err := h.read(resolved.Metadata)
			if err != nil {
				t.Fatal(err)
			}
			want := []domain.ProbeDeliveryModeApplication{h.expectedFact(resolved)}
			if len(facts) != 1 || !slices.Equal(facts, want) {
				t.Fatalf("reported facts %+v do not match the authenticated document %+v", facts, want)
			}
			if facts[0].AssignmentRevision != exactRegionalAssignmentRevision {
				t.Fatal("assignment revision lost exact int64 precision")
			}
			if facts[0].AlertDelivery != domain.AlertDeliveryRegional {
				t.Fatal("the read reported the desired mode instead of effective regional paging")
			}
			if !domain.ValidProbeDeliveryModeApplication(facts[0]) {
				t.Fatal("reported fact is not a valid regional application")
			}
		})
	}
}

// TestRegionalApplicationReadLegacyUnboundDocumentStaysUnreported proves a
// legacy document without an assignment-set binding activates and executes but
// reports zero facts: absence is unreported, never synthesized from the
// assignment index or the document.
func TestRegionalApplicationReadLegacyUnboundDocumentStaysUnreported(t *testing.T) {
	h := newRegionalReadHarness(t)
	resolved := h.apply(h.document("", 0))
	if len(resolved.Assignments) != 1 || resolved.Assignments[0].AssignmentRevision != 0 {
		t.Fatal("the fixture is not a legacy unbound document")
	}
	facts, err := h.read(resolved.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 0 {
		t.Fatalf("legacy unbound assignment synthesized %d facts", len(facts))
	}
	active, err := h.store.ReadActiveConfig(t.Context())
	if err != nil || len(active.Assignments) != 1 {
		t.Fatal("the legacy document is not durably active", err)
	}
}

// TestRegionalApplicationReadNeverSynthesizesMissingFacts proves a bound,
// active document whose activation predates durable facts (the real store
// committed no fact rows) reads back empty instead of deriving facts from the
// desired mode or the assignment index.
func TestRegionalApplicationReadNeverSynthesizesMissingFacts(t *testing.T) {
	h := newRegionalReadHarness(t)
	legacy := h.service(h.store, &regionalReadLegacyWriteStore{Store: h.store})
	resolved, err := legacy.Apply(h.t.Context(), h.document(domain.AlertDeliveryBoth, exactRegionalAssignmentRevision), h.generation, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Assignments[0].AssignmentRevision != exactRegionalAssignmentRevision {
		t.Fatal("the tested document is not assignment-bound")
	}
	active, err := h.store.ReadActiveConfig(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(active.Assignments) != 1 || len(active.RegionalDeliveryApplications) != 0 {
		t.Fatal("the pre-upgrade activation shape was not persisted as intended", active)
	}
	facts, err := legacy.ReadRegionalApplications(h.t.Context(), resolved.Metadata, h.generation, h.id.StreamID)
	if err != nil || len(facts) != 0 {
		t.Fatalf("missing facts were synthesized instead of staying unreported: %+v %v", facts, err)
	}
}

// TestRegionalApplicationReadSurvivesColdReopenAndRetry proves the facts are
// durable SQLite state, not session memory: a same-document retry leaves them
// byte-identical and a cold reopen with a fresh service reports the same exact
// immutable tuple.
func TestRegionalApplicationReadSurvivesColdReopenAndRetry(t *testing.T) {
	h := newRegionalReadHarness(t)
	document := h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision)
	resolved := h.apply(document)
	first, err := h.read(resolved.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].AssignmentRevision != exactRegionalAssignmentRevision {
		t.Fatalf("unexpected initial facts %+v %v", first, err)
	}
	if _, err := h.configs.Apply(h.t.Context(), document, h.generation, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	retry, err := h.read(resolved.Metadata)
	if err != nil || !slices.Equal(retry, first) {
		t.Fatalf("same-document retry rewrote retained facts: %+v %v", retry, err)
	}
	h.closeStore()
	reopenedStore, err := edge.Open(h.t.Context(), h.dir, domain.EdgeIdentity{ProbeID: h.id.ProbeID, StreamID: h.id.StreamID, Fingerprint: h.id.Fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopenedStore.Close() })
	reopened := h.service(reopenedStore, reopenedStore)
	facts, err := reopened.ReadRegionalApplications(h.t.Context(), resolved.Metadata, h.generation, h.id.StreamID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(facts, first) {
		t.Fatalf("cold reopen changed the retained facts: %+v want %+v", facts, first)
	}
	if facts[0].AssignmentRevision != exactRegionalAssignmentRevision {
		t.Fatal("cold reopen lost the immutable assignment revision")
	}
}

// TestRegionalApplicationReadRefusesMismatchedSessionInputs proves every
// document/generation/stream mismatch is refused with no facts returned, while
// the exact accepted session still reads successfully afterwards.
func TestRegionalApplicationReadRefusesMismatchedSessionInputs(t *testing.T) {
	h := newRegionalReadHarness(t)
	resolved := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
	metadata := resolved.Metadata
	shift := func(change func(*domain.ProbeConfigMetadata)) domain.ProbeConfigMetadata {
		m := metadata
		change(&m)
		return m
	}
	for _, test := range []struct {
		name       string
		metadata   domain.ProbeConfigMetadata
		generation int64
		streamID   string
		wantErr    error
	}{
		{name: "revision selects another document", metadata: shift(func(m *domain.ProbeConfigMetadata) { m.Revision++ }), generation: h.generation, streamID: h.id.StreamID, wantErr: ports.ErrConflict},
		{name: "digest does not match the retained document", metadata: shift(func(m *domain.ProbeConfigMetadata) { m.SHA256 = strings.Repeat("f", 64) }), generation: h.generation, streamID: h.id.StreamID, wantErr: ports.ErrConflict},
		{name: "creation time does not match the retained document", metadata: shift(func(m *domain.ProbeConfigMetadata) { m.CreatedAt = m.CreatedAt.Add(time.Microsecond) }), generation: h.generation, streamID: h.id.StreamID, wantErr: ports.ErrConflict},
		{name: "foreign installation target", metadata: shift(func(m *domain.ProbeConfigMetadata) { m.HubID = uuid.NewString() }), generation: h.generation, streamID: h.id.StreamID, wantErr: ports.ErrConflict},
		{name: "invalid metadata shape", metadata: shift(func(m *domain.ProbeConfigMetadata) { m.Revision = 0 }), generation: h.generation, streamID: h.id.StreamID, wantErr: domain.ErrValidation},
		{name: "zero session generation", metadata: metadata, generation: 0, streamID: h.id.StreamID, wantErr: domain.ErrValidation},
		{name: "generation from another session", metadata: metadata, generation: h.generation + 1, streamID: h.id.StreamID, wantErr: ports.ErrConflict},
		{name: "local stream is not a remote report stream", metadata: metadata, generation: h.generation, streamID: domain.LocalStreamID, wantErr: domain.ErrValidation},
		{name: "foreign stream", metadata: metadata, generation: h.generation, streamID: uuid.NewString(), wantErr: ports.ErrConflict},
		{name: "malformed stream", metadata: metadata, generation: h.generation, streamID: "not-a-stream-uuid", wantErr: domain.ErrValidation},
	} {
		t.Run(test.name, func(t *testing.T) {
			facts, err := h.configs.ReadRegionalApplications(t.Context(), test.metadata, test.generation, test.streamID)
			if !errors.Is(err, test.wantErr) || facts != nil {
				t.Fatalf("mismatched input answered %+v %v, want %v", facts, err, test.wantErr)
			}
		})
	}
	if facts, err := h.read(metadata); err != nil || len(facts) != 1 {
		t.Fatalf("the exact accepted session stopped reading after refusals: %+v %v", facts, err)
	}
}

// TestRegionalApplicationReadRefusesCorruptPresentFacts proves present durable
// facts are validated against the authenticated document on every read: a
// tampered stored digest or generation refuses the whole read, and restoring
// the row restores the exact report (falsifying any unrelated failure).
func TestRegionalApplicationReadRefusesCorruptPresentFacts(t *testing.T) {
	h := newRegionalReadHarness(t)
	resolved := h.apply(h.document(domain.AlertDeliveryBoth, exactRegionalAssignmentRevision))
	want := []domain.ProbeDeliveryModeApplication{h.expectedFact(resolved)}
	keys := []any{resolved.Metadata.Revision, want[0].MonitorID, h.id.StreamID}
	where := " WHERE config_revision = ? AND monitor_id = ? AND stream_id = ?"
	regionalReadExec(t, h.dir, "UPDATE edge_regional_delivery_applications SET sha256 = ?"+where, append([]any{strings.Repeat("f", 64)}, keys...)...)
	if facts, err := h.read(resolved.Metadata); !errors.Is(err, ports.ErrConflict) || facts != nil {
		t.Fatalf("corrupt stored digest was accepted: %+v %v", facts, err)
	}
	regionalReadExec(t, h.dir, "UPDATE edge_regional_delivery_applications SET sha256 = ?"+where, append([]any{resolved.Metadata.SHA256}, keys...)...)
	if facts, err := h.read(resolved.Metadata); err != nil || !slices.Equal(facts, want) {
		t.Fatalf("restored facts are not readable again: %+v %v", facts, err)
	}
	regionalReadExec(t, h.dir, "UPDATE edge_regional_delivery_applications SET generation = ?"+where, append([]any{want[0].Generation + 1}, keys...)...)
	if facts, err := h.read(resolved.Metadata); !errors.Is(err, domain.ErrValidation) || facts != nil {
		t.Fatalf("fact conflicting with the authenticated document was accepted: %+v %v", facts, err)
	}
}

// TestRegionalApplicationReadRefusesAuthenticationFailure proves neither a
// foreign installation key nor tampered retained ciphertext ever authenticates
// the active tuple, and that only the tamper causes the refusal.
func TestRegionalApplicationReadRefusesAuthenticationFailure(t *testing.T) {
	h := newRegionalReadHarness(t)
	resolved := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
	foreign, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{9}, 32))
	if err != nil {
		t.Fatal(err)
	}
	rekeyed := services.NewEdgeConfigService(h.store, h.store, NewEdgeConfigDecoder(checker.Get, notifier.Get), foreign)
	if facts, err := rekeyed.ReadRegionalApplications(h.t.Context(), resolved.Metadata, h.generation, h.id.StreamID); !errors.Is(err, domain.ErrValidation) || facts != nil {
		t.Fatalf("foreign key authenticated the retained document: %+v %v", facts, err)
	}
	regionalReadFlipActivePayload(t, h.dir, resolved.Metadata.Revision)
	if facts, err := h.read(resolved.Metadata); err == nil || facts != nil {
		t.Fatalf("tampered retained ciphertext was authenticated: %+v", facts)
	}
	regionalReadFlipActivePayload(t, h.dir, resolved.Metadata.Revision)
	if facts, err := h.read(resolved.Metadata); err != nil || len(facts) != 1 {
		t.Fatalf("untampered ciphertext stopped authenticating: %+v %v", facts, err)
	}
}

// TestRegionalApplicationReadHonorsCanceledContext proves a canceled caller
// context aborts the read with the context error and no facts.
func TestRegionalApplicationReadHonorsCanceledContext(t *testing.T) {
	h := newRegionalReadHarness(t)
	resolved := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
	if _, err := h.read(resolved.Metadata); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	facts, err := h.configs.ReadRegionalApplications(ctx, resolved.Metadata, h.generation, h.id.StreamID)
	if !errors.Is(err, context.Canceled) || facts != nil {
		t.Fatalf("canceled context answered %+v %v", facts, err)
	}
}

// TestRegionalApplicationReadAuthenticatesOneCapturedActiveTuple proves the
// read authenticates exactly one captured active tuple: a poison tuple on a
// hypothetical second storage read can never reach the caller.
func TestRegionalApplicationReadAuthenticatesOneCapturedActiveTuple(t *testing.T) {
	h := newRegionalReadHarness(t)
	resolved := h.apply(h.document(domain.AlertDeliveryBoth, exactRegionalAssignmentRevision))
	identity, err := h.store.ReadIdentity(h.t.Context())
	if err != nil {
		t.Fatal(err)
	}
	active, err := h.store.ReadActiveConfig(h.t.Context())
	if err != nil {
		t.Fatal(err)
	}
	poison := active
	poison.RegionalDeliveryApplications = []domain.ProbeDeliveryModeApplication{h.expectedFact(resolved)}
	poison.RegionalDeliveryApplications[0].AssignmentRevision = exactRegionalAssignmentRevision - 1
	identityMock := &regionalReadIdentityMock{sequence: []domain.EdgeIdentity{identity, identity}}
	configMock := &regionalReadConfigMock{primary: active, secondary: &poison}
	facts, err := h.service(identityMock, configMock).ReadRegionalApplications(h.t.Context(), resolved.Metadata, h.generation, h.id.StreamID)
	if err != nil {
		t.Fatal(err)
	}
	want := []domain.ProbeDeliveryModeApplication{h.expectedFact(resolved)}
	if !slices.Equal(facts, want) {
		t.Fatalf("returned facts %+v are not the single authenticated tuple %+v", facts, want)
	}
	if configMock.reads != 1 {
		t.Fatalf("active tuple was read %d times, want exactly 1", configMock.reads)
	}
	if identityMock.calls != 2 {
		t.Fatalf("identity was captured %d times, want capture plus recheck", identityMock.calls)
	}
}

// TestRegionalApplicationReadRefusesChangedSessionIdentity proves the
// post-storage identity recheck refuses a session that changed while the
// captured tuple was being authenticated, even though that tuple is coherent.
func TestRegionalApplicationReadRefusesChangedSessionIdentity(t *testing.T) {
	h := newRegionalReadHarness(t)
	resolved := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
	identity, err := h.store.ReadIdentity(h.t.Context())
	if err != nil {
		t.Fatal(err)
	}
	active, err := h.store.ReadActiveConfig(h.t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		change func(*domain.EdgeIdentity)
	}{
		{name: "newer session generation", change: func(i *domain.EdgeIdentity) { i.ConnectionGeneration++ }},
		{name: "stream reset", change: func(i *domain.EdgeIdentity) { i.StreamID = uuid.NewString() }},
		{name: "newer active document", change: func(i *domain.EdgeIdentity) { i.ConfigRevision++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := identity
			test.change(&changed)
			identityMock := &regionalReadIdentityMock{sequence: []domain.EdgeIdentity{identity, changed}}
			configMock := &regionalReadConfigMock{primary: active}
			facts, err := h.service(identityMock, configMock).ReadRegionalApplications(t.Context(), resolved.Metadata, h.generation, h.id.StreamID)
			if !errors.Is(err, ports.ErrConflict) || facts != nil {
				t.Fatalf("changed session identity answered %+v %v", facts, err)
			}
			if configMock.reads != 1 || identityMock.calls != 2 {
				t.Fatalf("refusal did not follow one authenticated capture and a recheck: reads=%d captures=%d", configMock.reads, identityMock.calls)
			}
		})
	}
}

// TestRegionalApplicationReadBorrowsNoMutableSlices proves the read neither
// mutates its inputs nor the storage-owned fact slice it copies, and never
// hands the caller an alias of either.
func TestRegionalApplicationReadBorrowsNoMutableSlices(t *testing.T) {
	h := newRegionalReadHarness(t)
	resolved := h.apply(h.document(domain.AlertDeliveryAggregate, exactRegionalAssignmentRevision))
	identity, err := h.store.ReadIdentity(h.t.Context())
	if err != nil {
		t.Fatal(err)
	}
	active, err := h.store.ReadActiveConfig(h.t.Context())
	if err != nil {
		t.Fatal(err)
	}
	borrowed := active.RegionalDeliveryApplications
	if len(borrowed) != 1 {
		t.Fatalf("captured %d facts, want 1", len(borrowed))
	}
	pristine := slices.Clone(borrowed)
	identityMock := &regionalReadIdentityMock{sequence: []domain.EdgeIdentity{identity, identity}}
	configMock := &regionalReadConfigMock{primary: active}
	service := h.service(identityMock, configMock)
	metadata := resolved.Metadata
	input := metadata
	facts, err := service.ReadRegionalApplications(h.t.Context(), metadata, h.generation, h.id.StreamID)
	if err != nil || !slices.Equal(facts, pristine) {
		t.Fatalf("read returned %+v %v, want %+v", facts, err, pristine)
	}
	if metadata.Revision != input.Revision || metadata.SHA256 != input.SHA256 || metadata.HubID != input.HubID ||
		metadata.ProbeID != input.ProbeID || !domain.SameProbeConfigMetadata(metadata, input) {
		t.Fatal("the input metadata was mutated")
	}
	if !slices.Equal(borrowed, pristine) {
		t.Fatal("the storage-owned fact slice was mutated before copying")
	}
	facts[0].MonitorID = 999
	facts[0].AssignmentRevision = 1
	if !slices.Equal(borrowed, pristine) {
		t.Fatal("the returned facts alias the storage-owned slice")
	}
	second, err := service.ReadRegionalApplications(h.t.Context(), metadata, h.generation, h.id.StreamID)
	if err != nil || !slices.Equal(second, pristine) {
		t.Fatalf("a second read observed the caller's mutations: %+v %v", second, err)
	}
	if configMock.reads != 2 {
		t.Fatalf("storage was read %d times across two reads, want 2", configMock.reads)
	}
}
