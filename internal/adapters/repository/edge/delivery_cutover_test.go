package edge

// Effect-based verification for the operative aggregate/both cutover source
// gates. These tests drive the real SQLite store, the real AES-256-GCM config
// protector and the production EdgeConfigService executor: quiesce, apply and
// resume commit monitor-wide holds and kind-specific receipts, duplicates
// recover their exact historical outcome before any new config load, and no
// expiry, session change, configuration activation or stream reset can erase a
// hold. Only delivery.resume releases one barrier, never a replacement.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// cutoverDoc is the bounded synthetic config document this test decoder reads.
// The production document codec belongs to the probe wire domain; what matters
// here is that the real protector, real service capture and real storage fences
// all run on exact bytes.
type cutoverDoc struct {
	Revision           int64  `json:"revision"`
	MonitorID          int64  `json:"monitor_id"`
	Generation         int64  `json:"generation"`
	AssignmentRevision int64  `json:"assignment_revision"`
	Desired            string `json:"desired"`
}

var cutoverDocEpoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

type cutoverDocDecoder struct{}

func (cutoverDocDecoder) DecodeEdge(_ context.Context, document []byte, target domain.ProbeConfigTarget) (*domain.EdgeResolvedConfig, error) {
	var doc cutoverDoc
	if err := json.Unmarshal(document, &doc); err != nil {
		return nil, err
	}
	mode := domain.AlertDelivery(doc.Desired)
	if doc.Revision <= 0 || doc.MonitorID <= 0 || doc.Generation <= 0 || doc.AssignmentRevision <= 0 ||
		!domain.ValidAlertDelivery(mode) || mode == domain.AlertDeliveryRegional {
		return nil, domain.ErrValidation
	}
	sum := sha256.Sum256(document)
	metadata := domain.ProbeConfigMetadata{ProbeConfigTarget: target, Revision: doc.Revision, SchemaVersion: 1,
		SHA256: hex.EncodeToString(sum[:]), CreatedAt: cutoverDocEpoch, EffectiveAt: cutoverDocEpoch}
	return &domain.EdgeResolvedConfig{Metadata: metadata, Assignments: []domain.EdgeResolvedAssignment{{
		Monitor: &domain.Monitor{ID: doc.MonitorID, Active: true}, Generation: doc.Generation,
		AssignmentRevision: doc.AssignmentRevision, DesiredAlertDelivery: mode,
	}}}, nil
}

type cutoverHarness struct {
	t         *testing.T
	dir       string
	store     *Store
	svc       *services.EdgeConfigService
	authority domain.EdgeCommandAuthority
	active    domain.EdgeActiveConfig
	setID     int64
}

func cutoverDocument(revision int64, generation, setRevision int64, desired domain.AlertDelivery) []byte {
	return fmt.Appendf(nil, `{"revision":%d,"monitor_id":17,"generation":%d,"assignment_revision":%d,"desired":%q}`,
		revision, generation, setRevision, string(desired))
}

func newCutoverHarness(t *testing.T, desired domain.AlertDelivery) *cutoverHarness {
	t.Helper()
	s, dir := testStore(t)
	s.telemetry = probe.EdgeTelemetryEncoder{}
	enroll(t, s)
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	svc := services.NewEdgeConfigService(s, s, cutoverDocDecoder{}, protector)
	svc.SetDeliveryPreparations(s)
	h := &cutoverHarness{t: t, dir: dir, store: s, svc: svc, setID: exactSetRevision,
		authority: domain.EdgeCommandAuthority{HubID: testHubID, ProbeID: testIdentity().ProbeID,
			StreamID: testIdentity().StreamID, ConnectionGeneration: 1}}
	h.apply(1, desired)
	return h
}

// apply activates one revision through the real service so the protected
// document, regional facts and single authenticated capture are all real.
func (h *cutoverHarness) apply(revision int64, desired domain.AlertDelivery) {
	h.t.Helper()
	resolved, err := h.svc.Apply(h.t.Context(), cutoverDocument(revision, 1, h.setID, desired), 1, time.Now().UTC())
	if err != nil {
		h.t.Fatalf("apply configuration: %v", err)
	}
	if len(resolved.Assignments) != 1 || resolved.Assignments[0].AssignmentRevision != h.setID {
		h.t.Fatalf("unexpected resolved assignment: %+v", resolved.Assignments)
	}
	active, err := h.store.ReadActiveConfig(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	h.active = active
}

// prepare commits the immutable delivery.prepare prerequisite through the real
// executor and repository for one exact tuple.
func (h *cutoverHarness) prepare(commandID string, mode domain.AlertDelivery) domain.ProbeDeliveryPreparation {
	h.t.Helper()
	at := time.Now().UTC().Truncate(time.Microsecond)
	p := domain.ProbeDeliveryPreparation{
		ProbeDeliveryModeApplication: h.application(mode),
		CommandID:                    commandID,
		CreatedAt:                    at.Add(-time.Hour), ExpiresAt: at.Add(time.Hour), PayloadHash: [32]byte{9},
	}
	out, err := h.svc.PrepareDelivery(h.t.Context(), h.authority, p)
	if err != nil || out.Status != "applied" || out.Code != "delivery_prepared" {
		h.t.Fatalf("delivery preparation: %+v %v", out, err)
	}
	return p
}

func (h *cutoverHarness) application(mode domain.AlertDelivery) domain.ProbeDeliveryModeApplication {
	return domain.ProbeDeliveryModeApplication{
		ProbeConfigTarget: h.active.Snapshot.ProbeConfigTarget,
		MonitorID:         17, AssignmentRevision: h.setID, Generation: 1,
		ConfigRevision: h.active.Snapshot.Revision, SHA256: h.active.Snapshot.SHA256,
		StreamID: testIdentity().StreamID, AlertDelivery: mode,
	}
}

func (h *cutoverHarness) command(kind, commandID, barrierID string, preparation domain.ProbeDeliveryPreparation, mode domain.AlertDelivery) domain.DeliveryCutoverCommand {
	at := time.Now().UTC().Truncate(time.Microsecond)
	return domain.DeliveryCutoverCommand{
		ProbeDeliveryModeApplication: h.application(mode),
		CommandID:                    commandID, BarrierID: barrierID, PreparationCommandID: preparation.CommandID,
		Kind: kind, CreatedAt: at.Add(-time.Hour), ExpiresAt: at.Add(time.Hour), PayloadHash: [32]byte{3},
	}
}

func (h *cutoverHarness) execute(command domain.DeliveryCutoverCommand) domain.ProbeCommandOutcome {
	h.t.Helper()
	out, err := h.svc.ExecuteDeliveryCutover(h.t.Context(), h.authority, command)
	if err != nil {
		h.t.Fatalf("execute %s: %v", command.Kind, err)
	}
	return out
}

func (h *cutoverHarness) gateState(barrierID string) string {
	h.t.Helper()
	return certString(h.t, h.store, "SELECT state FROM edge_delivery_cutover_gates WHERE barrier_id = ?", barrierID)
}

func (h *cutoverHarness) gateCount() int {
	h.t.Helper()
	return certCount(h.t, h.store, "SELECT COUNT(*) FROM edge_delivery_cutover_gates")
}

func (h *cutoverHarness) outboxCount(kind string) int {
	h.t.Helper()
	return certCount(h.t, h.store, fmt.Sprintf("SELECT COUNT(*) FROM edge_delivery_outbox WHERE event_kind = '%s'", kind))
}

func requireCutover(t *testing.T, out domain.ProbeCommandOutcome, status, code string) {
	t.Helper()
	if out.Status != status || out.Code != code || out.AppliedAt == nil || out.CommandID == "" {
		t.Fatalf("unexpected cutover outcome: %+v want %s %s", out, status, code)
	}
}

// cutoverResume builds a resume against an explicit stored barrier tuple so a
// test can release an old configuration after replacement, exactly like a hub
// retrying the original barrier.
func cutoverResume(application domain.ProbeDeliveryModeApplication, commandID, barrierID, preparationID string) domain.DeliveryCutoverCommand {
	at := time.Now().UTC().Truncate(time.Microsecond)
	return domain.DeliveryCutoverCommand{ProbeDeliveryModeApplication: application,
		CommandID: commandID, BarrierID: barrierID, PreparationCommandID: preparationID,
		Kind: domain.DeliveryResumeKind, CreatedAt: at.Add(-time.Hour), ExpiresAt: at.Add(time.Hour), PayloadHash: [32]byte{3}}
}

// sameOutcome compares receipts by value: a recovered duplicate keeps the
// original identity and original AppliedAt pointer target, not its address.
func sameOutcome(a, b domain.ProbeCommandOutcome) bool {
	if a.CommandID != b.CommandID || a.Status != b.Status || a.Code != b.Code || a.Message != b.Message || (a.AppliedAt == nil) != (b.AppliedAt == nil) {
		return false
	}
	return a.AppliedAt == nil || a.AppliedAt.Equal(*b.AppliedAt)
}

// TestEdgeDeliveryCutoverOperativeLifecycle drives aggregate quiesce, apply and
// resume through the real executor against the real store: the source gate is
// monitor-wide, filtering new availability intents while recording and incident
// truth continue, and only resume releases it.
func TestEdgeDeliveryCutoverOperativeLifecycle(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	barrier := uuid.NewString()

	requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)
	if got := h.gateState(barrier); got != deliveryGateQuiesced {
		t.Fatalf("gate state after quiesce: %q", got)
	}

	// Recording continues and incident truth commits; the new availability
	// intent is filtered while the aggregate hold applies.
	down := checkRecord()
	if _, err := h.store.CommitEdgeCheck(t.Context(), down); err != nil {
		t.Fatal(err)
	}
	if n := h.outboxCount(domain.DeliveryEventStatusChange); n != 0 {
		t.Fatalf("availability intent enqueued under aggregate hold: %d", n)
	}
	if n := certCount(t, h.store, "SELECT COUNT(*) FROM edge_alerts WHERE scope = 'regional'"); n != 1 {
		t.Fatalf("incident truth did not continue: %d", n)
	}

	requireCutover(t, h.execute(h.command(domain.DeliveryApplyKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryAppliedCode)
	if got := h.gateState(barrier); got != deliveryGateApplied {
		t.Fatalf("gate state after apply: %q", got)
	}
	// Application never relabels the distinct source-local regional facts.
	if n := certCount(t, h.store, "SELECT COUNT(*) FROM edge_regional_delivery_applications WHERE alert_delivery = 'regional'"); n != 1 {
		t.Fatalf("regional facts were relabelled: %d", n)
	}

	requireCutover(t, h.execute(h.command(domain.DeliveryResumeKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryResumedCode)
	if got := h.gateState(barrier); got != deliveryGateReleased {
		t.Fatalf("gate state after resume: %q", got)
	}
	// Recovery of the same incident resumes ordinary regional availability work.
	second := down
	resolvedAt := time.Now().UTC().Truncate(time.Microsecond)
	second.ExpectedStateSeq = 1
	second.ExpectedIncidentVersion = 1
	second.Observation.Status, second.Observation.RawStatus, second.Observation.DownCount = domain.StatusUp, domain.StatusUp, 0
	second.Observation.ObservedAt, second.Observation.ReceivedAt = resolvedAt, resolvedAt
	second.Incident.Status = domain.AlertStatusResolved
	second.Incident.TransitionVersion = 2
	second.Incident.ResolvedAt = &resolvedAt
	second.DeliveryIntents[0].SourceTransitionVersion = 2
	second.DeliveryIntents[0].AvailableAt = resolvedAt
	if _, err := h.store.CommitEdgeCheck(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	if n := certCount(t, h.store, "SELECT COUNT(*) FROM edge_delivery_outbox"); n != 1 {
		t.Fatalf("released gate still filtered availability work: %d", n)
	}
}

// TestEdgeDeliveryCutoverDuplicateColdAndNewCommand proves exact receipt
// recovery (including across a cold reopen) and that a new immutable command
// reports already_applied without rewriting the original outcome.
func TestEdgeDeliveryCutoverDuplicateColdAndNewCommand(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	barrier := uuid.NewString()
	quiesce := h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)

	first := h.execute(quiesce)
	requireCutover(t, first, "applied", domain.DeliveryQuiescedCode)
	duplicate := h.execute(quiesce)
	if !sameOutcome(duplicate, first) {
		t.Fatalf("duplicate rewrote the receipt: %+v vs %+v", duplicate, first)
	}
	if err := h.store.Close(); err != nil {
		t.Fatal(err)
	}
	h.store = certStoreReopen(t, h.dir)
	h.svc = services.NewEdgeConfigService(h.store, h.store, cutoverDocDecoder{}, mustCutoverProtector(t))
	h.svc.SetDeliveryPreparations(h.store)
	cold := h.execute(quiesce)
	if !sameOutcome(cold, first) {
		t.Fatalf("cold recovery lost the receipt: %+v vs %+v", cold, first)
	}
	retry := h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate))
	requireCutover(t, retry, "already_applied", domain.DeliveryQuiescedCode)
	if retry.CommandID == first.CommandID {
		t.Fatal("retry reused the original immutable receipt")
	}
	if n := certCount(t, h.store, "SELECT COUNT(*) FROM edge_applied_commands WHERE kind = 'delivery.quiesce'"); n != 2 {
		t.Fatalf("receipts lost: %d", n)
	}
}

func mustCutoverProtector(t *testing.T) *auth.ProbeConfigProtector {
	t.Helper()
	p, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{0x42}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// TestEdgeDeliveryCutoverBarrierABA refuses to release a replacement barrier:
// an old-barrier resume stays idempotent history, and a mixed tuple is rejected
// without touching the live gate.
func TestEdgeDeliveryCutoverBarrierABA(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	first := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	barrierOne := uuid.NewString()
	requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrierOne, first, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)
	requireCutover(t, h.execute(h.command(domain.DeliveryResumeKind, uuid.NewString(), barrierOne, first, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryResumedCode)

	h.apply(2, domain.AlertDeliveryAggregate)
	second := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	barrierTwo := uuid.NewString()
	requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrierTwo, second, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)

	// A late duplicate for the old barrier is history; it cannot release the
	// replacement barrier now holding the monitor.
	late := cutoverResume(first.ProbeDeliveryModeApplication, uuid.NewString(), barrierOne, first.CommandID)
	requireCutover(t, h.execute(late), "already_applied", domain.DeliveryResumedCode)
	if got := h.gateState(barrierTwo); got != deliveryGateQuiesced {
		t.Fatalf("old barrier released the replacement: %q", got)
	}
	// A command carrying the new barrier with the old tuple releases nothing.
	mixed := cutoverResume(first.ProbeDeliveryModeApplication, uuid.NewString(), barrierTwo, first.CommandID)
	out := h.execute(mixed)
	if out.Status != "rejected" || out.Code != "delivery_barrier_mismatch" || out.AppliedAt != nil {
		t.Fatalf("replacement barrier released by mixed tuple: %+v", out)
	}
	if got := h.gateState(barrierTwo); got != deliveryGateQuiesced {
		t.Fatalf("mixed tuple altered the live gate: %q", got)
	}
	// And the exact replacement tuple still releases only its own barrier.
	requireCutover(t, h.execute(h.command(domain.DeliveryResumeKind, uuid.NewString(), barrierTwo, second, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryResumedCode)
	if n := h.gateCount(); n != 2 {
		t.Fatalf("barrier history lost: %d", n)
	}
}

// TestEdgeDeliveryCutoverNewBarrierCannotBypass proves a live gate or drain
// blocks a fresh barrier for the same monitor until resume completes.
func TestEdgeDeliveryCutoverNewBarrierCannotBypass(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	live := uuid.NewString()
	requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), live, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)
	out := h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), uuid.NewString(), preparation, domain.AlertDeliveryAggregate))
	if out.Status != "rejected" || out.Code != "delivery_barrier_conflict" {
		t.Fatalf("new barrier bypassed the live gate: %+v", out)
	}
	// Even an expired candidate command must not clear or replace the hold.
	expired := h.command(domain.DeliveryQuiesceKind, uuid.NewString(), uuid.NewString(), preparation, domain.AlertDeliveryAggregate)
	expired.CreatedAt = time.Now().UTC().Add(-2 * time.Hour)
	expired.ExpiresAt = time.Now().UTC().Add(-time.Hour)
	stale := h.execute(expired)
	if stale.Status != "expired" || stale.AppliedAt != nil {
		t.Fatalf("expired command claimed an effect: %+v", stale)
	}
	if got := h.gateState(live); got != deliveryGateQuiesced {
		t.Fatalf("expiry cleared the hold: %q", got)
	}
	if n := h.gateCount(); n != 1 {
		t.Fatalf("expiry created a barrier: %d", n)
	}
}

// TestEdgeDeliveryCutoverExpiryCannotClearHold advances the source clock far
// past every command deadline: the hold and its state are untouched and resume
// still needs the exact stored barrier tuple.
func TestEdgeDeliveryCutoverExpiryCannotClearHold(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	barrier := uuid.NewString()
	requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)

	h.store.commandNow = func() time.Time { return time.Now().UTC().Add(48 * time.Hour) }
	out := h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), uuid.NewString(), preparation, domain.AlertDeliveryAggregate))
	if out.Status != "expired" || out.AppliedAt != nil {
		t.Fatalf("late new barrier claimed an effect: %+v", out)
	}
	if got := h.gateState(barrier); got != deliveryGateQuiesced {
		t.Fatalf("time cleared the hold: %q", got)
	}
	if n := h.gateCount(); n != 1 {
		t.Fatalf("time created a barrier: %d", n)
	}
	// Resume is a fresh immutable command valid at the current source clock; it
	// still needs the exact stored barrier tuple.
	resume := h.command(domain.DeliveryResumeKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)
	resume.CreatedAt = time.Now().UTC().Add(47 * time.Hour)
	resume.ExpiresAt = time.Now().UTC().Add(49 * time.Hour)
	requireCutover(t, h.execute(resume), "applied", domain.DeliveryResumedCode)
}

// TestEdgeDeliveryCutoverSessionFence refuses stale stored session authority
// without writing any receipt.
func TestEdgeDeliveryCutoverSessionFence(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	command := h.command(domain.DeliveryQuiesceKind, uuid.NewString(), uuid.NewString(), preparation, domain.AlertDeliveryAggregate)
	stale := h.authority
	stale.ConnectionGeneration = 2
	if _, err := h.svc.ExecuteDeliveryCutover(t.Context(), stale, command); !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("stale session executed a cutover: %v", err)
	}
	if n := certCount(t, h.store, "SELECT COUNT(*) FROM edge_applied_commands"); n != 1 { // only the preparation
		t.Fatalf("stale session wrote a receipt: %d", n)
	}
	if n := h.gateCount(); n != 0 {
		t.Fatalf("stale session opened a gate: %d", n)
	}
}

// TestEdgeDeliveryCutoverBadPrerequisiteAndBinding refuses missing preparation,
// wrong tuple hashes and unauthenticated capture replacement with truthful
// rejected receipts, never a success.
func TestEdgeDeliveryCutoverBadPrerequisiteAndBinding(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
	barrier := uuid.NewString()

	missing := h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier,
		domain.ProbeDeliveryPreparation{ProbeDeliveryModeApplication: h.application(domain.AlertDeliveryAggregate), CommandID: uuid.NewString()},
		domain.AlertDeliveryAggregate)
	out := h.execute(missing)
	if out.Status != "rejected" || out.Code != "delivery_preparation_missing" {
		t.Fatalf("quiesce without preparation: %+v", out)
	}

	// A preparation for a different set revision cannot bind this command.
	wrongPrep := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	wrongPrep.AssignmentRevision = h.setID + 1
	wrongPrep.CommandID = uuid.NewString()
	if got, err := h.svc.PrepareDelivery(t.Context(), h.authority, wrongPrep); err != nil || got.Status != "rejected" {
		t.Fatalf("unbound preparation accepted: %+v %v", got, err)
	}
	out = h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, wrongPrep, domain.AlertDeliveryAggregate))
	if out.Status != "rejected" || out.Code != "delivery_preparation_missing" {
		t.Fatalf("wrong preparation bound: %+v", out)
	}

	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
	badHash := h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)
	badHash.SHA256 = "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	out = h.execute(badHash)
	if out.Status != "rejected" || out.Code != "delivery_binding_changed" {
		t.Fatalf("wrong document hash accepted: %+v", out)
	}

	// A captured tuple replaced between authentication and the transaction is
	// refused even when its metadata still parses.
	command := h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)
	poison := h.active
	poison.Snapshot.KeyConfirmation = "poison"
	repo := ports.EdgeDeliveryCutoverRepository(h.store)
	out, err := repo.ApplyDeliveryCutover(t.Context(), h.authority, command, &poison)
	if err != nil || out.Status != "rejected" || out.Code != "delivery_binding_changed" {
		t.Fatalf("unauthenticated replacement accepted: %+v %v", out, err)
	}

	// A corrupt present ciphertext fails authentication and writes nothing.
	if _, err := h.store.db.ExecContext(t.Context(), "UPDATE edge_config SET protected_payload = zeroblob(length(protected_payload))"); err != nil {
		t.Fatal(err)
	}
	fresh := h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)
	if _, err := h.svc.ExecuteDeliveryCutover(t.Context(), h.authority, fresh); err == nil {
		t.Fatal("corrupt ciphertext authenticated")
	}
	if n := certCount(t, h.store, "SELECT COUNT(*) FROM edge_applied_commands WHERE kind = 'delivery.quiesce' AND status <> 'rejected'"); n != 0 {
		t.Fatalf("corrupt ciphertext wrote a success receipt: %d", n)
	}
	if n := h.gateCount(); n != 0 {
		t.Fatalf("corrupt ciphertext opened a gate: %d", n)
	}
}

// TestEdgeDeliveryCutoverBothModeRetainsRegional proves BOTH keeps regional
// work flowing while the barrier still commits its exact quiesce/apply/resume
// receipts.
func TestEdgeDeliveryCutoverBothModeRetainsRegional(t *testing.T) {
	h := newCutoverHarness(t, domain.AlertDeliveryBoth)
	preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryBoth)
	barrier := uuid.NewString()
	requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryBoth)), "applied", domain.DeliveryQuiescedCode)
	requireCutover(t, h.execute(h.command(domain.DeliveryApplyKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryBoth)), "applied", domain.DeliveryAppliedCode)

	if _, err := h.store.CommitEdgeCheck(t.Context(), checkRecord()); err != nil {
		t.Fatal(err)
	}
	if n := h.outboxCount(domain.DeliveryEventStatusChange); n != 1 {
		t.Fatalf("both-mode hold filtered regional work: %d", n)
	}
	items, err := h.store.ClaimDeliveries(t.Context(), testIdentity().ProbeID, time.Now().UTC(), time.Minute, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("claim under both-mode hold: %v %v", items, err)
	}
	claim := domain.DeliveryClaim{DeliveryID: items[0].DeliveryID, ProbeID: items[0].ProbeID, Attempt: items[0].Attempt, LeaseToken: items[0].LeaseToken}
	authorized, err := h.store.AuthorizeEdgeDelivery(t.Context(), claim, h.active.Snapshot.ProbeConfigMetadata, 10*time.Second)
	if err != nil || authorized == nil || authorized.ProviderAttemptID == "" {
		t.Fatalf("both-mode hold blocked regional authorization: %+v %v", authorized, err)
	}
	requireCutover(t, h.execute(h.command(domain.DeliveryResumeKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryBoth)), "applied", domain.DeliveryResumedCode)
}

// TestEdgeDeliveryCutoverConfigAndResetCannotDropHold proves neither a later
// configuration activation nor an authenticated stream reset erases a hold, and
// resume still releases the exact stored old barrier.
func TestEdgeDeliveryCutoverConfigAndResetCannotDropHold(t *testing.T) {
	t.Run("new_config", func(t *testing.T) {
		h := newCutoverHarness(t, domain.AlertDeliveryAggregate)
		preparation := h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate)
		barrier := uuid.NewString()
		requireCutover(t, h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), barrier, preparation, domain.AlertDeliveryAggregate)), "applied", domain.DeliveryQuiescedCode)
		h.apply(2, domain.AlertDeliveryAggregate)
		if got := h.gateState(barrier); got != deliveryGateQuiesced {
			t.Fatalf("config activation dropped the hold: %q", got)
		}
		out := h.execute(h.command(domain.DeliveryQuiesceKind, uuid.NewString(), uuid.NewString(), h.prepare(uuid.NewString(), domain.AlertDeliveryAggregate), domain.AlertDeliveryAggregate))
		if out.Status != "rejected" || out.Code != "delivery_barrier_conflict" {
			t.Fatalf("new barrier bypassed the retained hold: %+v", out)
		}
		// Resume releases the old barrier against its stored tuple, without any
		// relation to the replaced current configuration.
		oldResume := cutoverResume(preparation.ProbeDeliveryModeApplication, uuid.NewString(), barrier, preparation.CommandID)
		requireCutover(t, h.execute(oldResume), "applied", domain.DeliveryResumedCode)
	})
	t.Run("stream_reset", func(t *testing.T) {
		s, dir := testStore(t)
		s.telemetry = probe.EdgeTelemetryEncoder{}
		enroll(t, s)
		base := protectedConfig(t, 1)
		config := regionalWithFacts(base, regionalFact(base, 17, 1, exactSetRevision))
		if err := s.ActivateConfig(t.Context(), config); err != nil {
			t.Fatal(err)
		}
		captured, err := s.ReadActiveConfig(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		authority := domain.EdgeCommandAuthority{HubID: testHubID, ProbeID: testIdentity().ProbeID, StreamID: testIdentity().StreamID, ConnectionGeneration: 1}
		at := time.Now().UTC().Truncate(time.Microsecond)
		preparation := domain.ProbeDeliveryPreparation{
			ProbeDeliveryModeApplication: domain.ProbeDeliveryModeApplication{ProbeConfigTarget: config.Snapshot.ProbeConfigTarget,
				MonitorID: 17, AssignmentRevision: exactSetRevision, Generation: 1, ConfigRevision: config.Snapshot.Revision,
				SHA256: config.Snapshot.SHA256, StreamID: testIdentity().StreamID, AlertDelivery: domain.AlertDeliveryAggregate},
			CommandID: uuid.NewString(), CreatedAt: at.Add(-time.Hour), ExpiresAt: at.Add(time.Hour), PayloadHash: [32]byte{9},
		}
		if out, err := s.PrepareDelivery(t.Context(), authority, preparation, &captured); err != nil || out.Status != "applied" {
			t.Fatalf("preparation: %+v %v", out, err)
		}
		barrier := uuid.NewString()
		quiesce := domain.DeliveryCutoverCommand{ProbeDeliveryModeApplication: preparation.ProbeDeliveryModeApplication,
			CommandID: uuid.NewString(), BarrierID: barrier, PreparationCommandID: preparation.CommandID,
			Kind: domain.DeliveryQuiesceKind, CreatedAt: preparation.CreatedAt, ExpiresAt: preparation.ExpiresAt, PayloadHash: [32]byte{3}}
		if out, err := s.ApplyDeliveryCutover(t.Context(), authority, quiesce, &captured); err != nil || out.Status != "applied" {
			t.Fatalf("quiesce: %+v %v", out, err)
		}
		if _, err := s.CommitEdgeCheck(t.Context(), checkRecord()); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		p := mustCutoverProtector(t)
		reset, err := OpenForStreamReset(t.Context(), dir, testIdentity(), WithStreamResetProtection(p))
		if err != nil {
			t.Fatal(err)
		}
		binding, _ := offer()
		plan := domain.ProbeStreamResetPlan{ResetID: uuid.NewString(), HubID: testHubID, ProbeID: testIdentity().ProbeID,
			EnrollmentID: binding.EnrollmentID, PreviousStreamID: testIdentity().StreamID, StreamID: uuid.NewString(),
			Fingerprint: testIdentity().Fingerprint, CredentialVersion: 1, CertificateVersion: 1,
			HubCommittedSeq: 5, ConnectionGeneration: 10, PreparedAt: time.Now().UTC().Truncate(time.Microsecond)}
		result, err := reset.ResetStream(t.Context(), plan)
		if err != nil || result.State != "applied" {
			t.Fatalf("stream reset: %+v %v", result, err)
		}
		if err := reset.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(t.Context(), dir, testIdentity(), WithStreamResetProtection(p), WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = reopened.Close() }()
		if got := certString(t, reopened, "SELECT state FROM edge_delivery_cutover_gates WHERE barrier_id = ?", barrier); got != deliveryGateQuiesced {
			t.Fatalf("reset dropped the hold: %q", got)
		}
		resumedAuthority := domain.EdgeCommandAuthority{HubID: testHubID, ProbeID: testIdentity().ProbeID, StreamID: plan.StreamID, ConnectionGeneration: plan.ConnectionGeneration}
		resume := domain.DeliveryCutoverCommand{ProbeDeliveryModeApplication: preparation.ProbeDeliveryModeApplication,
			CommandID: uuid.NewString(), BarrierID: barrier, PreparationCommandID: preparation.CommandID,
			Kind: domain.DeliveryResumeKind, CreatedAt: at, ExpiresAt: at.Add(time.Hour), PayloadHash: [32]byte{3}}
		out, err := reopened.ApplyDeliveryCutover(t.Context(), resumedAuthority, resume, nil)
		if err != nil || out.Status != "applied" || out.Code != domain.DeliveryResumedCode {
			t.Fatalf("old barrier resume after reset: %+v %v", out, err)
		}
	})
}
