package repository_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// m8ActOps are the fixture install operations. Attempt UUIDs are derived from
// the operation's position here so they stay globally unique per journal.
var m8ActOps = []string{
	m8InstallOp1, m8InstallOp2, m8InstallOp3, m8InstallOp4, m8InstallOp5,
	m8InstallOp6, m8InstallOp7, m8InstallOp8, m8InstallOp9,
}

// m8ActKinds is the frozen activation layout; a step's ordinal is index+1.
var m8ActKinds = []string{
	domain.ProbeActivationProbeInit, domain.ProbeActivationRegistration,
	domain.ProbeActivationServiceStart, domain.ProbeActivationEnrollment,
}

const m8ActForeignAttempt = "cccc9999-cccc-4ccc-8ccc-cccccccccccc"

func m8ActOpIndex(operationID string) int {
	for i, op := range m8ActOps {
		if op == operationID {
			return i + 1
		}
	}
	panic("unknown fixture operation " + operationID)
}

func m8ActKindIndex(kind string) int {
	for i, k := range m8ActKinds {
		if k == kind {
			return i
		}
	}
	panic("unknown activation kind " + kind)
}

// m8ActInstallAttempt is the install journal attempt for one fixture resource.
func m8ActInstallAttempt(operationID string, ordinal int) string {
	return fmt.Sprintf("bbbb%02d%02d-bbbb-4bbb-8bbb-bbbbbbbbbbbb", m8ActOpIndex(operationID), ordinal)
}

// m8ActAttempt is the activation attempt for one fixture step.
func m8ActAttempt(operationID, kind string) string {
	return fmt.Sprintf("cccc%02d%02d-cccc-4ccc-8ccc-cccccccccccc", m8ActOpIndex(operationID), m8ActKindIndex(kind)+1)
}

// m8ActFacts are canonical probe_init facts, distinct per operation.
func m8ActFacts(operationID string) domain.ProbeActivationFacts {
	n := m8ActOpIndex(operationID)
	return domain.ProbeActivationFacts{
		ProbeID:                fmt.Sprintf("dddd%04d-dddd-4ddd-8ddd-dddddddddddd", n),
		StreamID:               fmt.Sprintf("eeee%04d-eeee-4eee-8eee-eeeeeeeeeeee", n),
		CertificateFingerprint: strings.Repeat("ab", 32),
	}
}

// m8ActIdentity is a valid activation request over a root systemd install.
func m8ActIdentity(operationID string) domain.ProbeActivationIdentity {
	install := m8InstallIdentity(domain.ProbeInstallSystemd, operationID)
	install.Host.User = "root"
	return domain.ProbeActivationIdentity{
		Install: install, ProbeKey: "edge-eu-1", Name: "Edge EU 1", Location: "Frankfurt",
		ListenPort: domain.ProbeActivationDefaultPort, ClientKeyFingerprint: install.ClientKeyFingerprint,
		RequestedBy: install.RequestedBy,
	}
}

// m8ActRawActivation is the test-only raw view of the stored activation row.
type m8ActRawActivation struct {
	bun.BaseModel          `bun:"table:probe_install_activations"`
	OperationID            string    `bun:"operation_id,pk"`
	OwnerID                string    `bun:"owner_id"`
	State                  string    `bun:"state"`
	ErrorCode              *string   `bun:"error_code"`
	ProbeID                *string   `bun:"probe_id"`
	StreamID               *string   `bun:"stream_id"`
	CertificateFingerprint *string   `bun:"certificate_fingerprint"`
	CreatedAt              time.Time `bun:"created_at"`
	UpdatedAt              time.Time `bun:"updated_at"`
	DeadlineAt             time.Time `bun:"deadline_at"`
}

// m8ActRawStep is the test-only raw view of one stored activation step.
type m8ActRawStep struct {
	bun.BaseModel `bun:"table:probe_install_activation_steps"`
	OperationID   string     `bun:"operation_id"`
	Kind          string     `bun:"kind"`
	Ordinal       int        `bun:"ordinal"`
	State         string     `bun:"state"`
	AttemptID     *string    `bun:"attempt_id"`
	ErrorCode     *string    `bun:"error_code"`
	StartedAt     *time.Time `bun:"started_at"`
	FinishedAt    *time.Time `bun:"finished_at"`
}

// m8ActRawAdmission is the test-only raw view of one activation admission.
type m8ActRawAdmission struct {
	bun.BaseModel  `bun:"table:probe_activation_admissions"`
	OperationID    string    `bun:"operation_id"`
	Kind           string    `bun:"kind"`
	OwnerID        string    `bun:"owner_id"`
	AttemptID      string    `bun:"attempt_id"`
	AuthorityKeyID string    `bun:"authority_key_id"`
	NonceHex       string    `bun:"nonce_hex"`
	MarkerSHA256   string    `bun:"marker_sha256"`
	AdmittedAt     time.Time `bun:"admitted_at"`
}

func m8ActRawRow(t *testing.T, f probeRegistryFixture, operationID string) m8ActRawActivation {
	t.Helper()
	row := new(m8ActRawActivation)
	if err := f.db.NewSelect().Model(row).Where("operation_id = ?", operationID).Scan(t.Context()); err != nil {
		t.Fatalf("raw read activation %s: %v", operationID, err)
	}
	return *row
}

func m8ActRawSteps(t *testing.T, f probeRegistryFixture, operationID string) []m8ActRawStep {
	t.Helper()
	var rows []m8ActRawStep
	if err := f.db.NewSelect().Model(&rows).Where("operation_id = ?", operationID).Order("ordinal ASC").Scan(t.Context()); err != nil {
		t.Fatalf("raw read activation steps %s: %v", operationID, err)
	}
	return rows
}

func m8ActRawAdmissions(t *testing.T, f probeRegistryFixture) []m8ActRawAdmission {
	t.Helper()
	var rows []m8ActRawAdmission
	if err := f.db.NewSelect().Model(&rows).Order("operation_id ASC", "kind ASC").Scan(t.Context()); err != nil {
		t.Fatalf("raw read activation admissions: %v", err)
	}
	return rows
}

func m8ActCount(t *testing.T, f probeRegistryFixture, table string) int64 {
	t.Helper()
	count, err := f.db.NewSelect().Table(table).Count(t.Context())
	if err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return int64(count)
}

// m8ActSnapshot renders every durable activation column of one operation so a
// refused transaction can prove it left nothing behind.
func m8ActSnapshot(t *testing.T, f probeRegistryFixture, operationID string) string {
	t.Helper()
	text := func(v *string) string {
		if v == nil {
			return "<nil>"
		}
		return *v
	}
	stamp := func(v *time.Time) string {
		if v == nil {
			return "<nil>"
		}
		return v.String()
	}
	a := m8ActRawRow(t, f, operationID)
	out := fmt.Sprintf("activation state=%s owner=%s error=%s probe=%s stream=%s pin=%s created=%s updated=%s deadline=%s",
		a.State, a.OwnerID, text(a.ErrorCode), text(a.ProbeID), text(a.StreamID), text(a.CertificateFingerprint),
		a.CreatedAt, a.UpdatedAt, a.DeadlineAt)
	for _, s := range m8ActRawSteps(t, f, operationID) {
		out += fmt.Sprintf(" | %s state=%s attempt=%s error=%s started=%s finished=%s",
			s.Kind, s.State, text(s.AttemptID), text(s.ErrorCode), stamp(s.StartedAt), stamp(s.FinishedAt))
	}
	for _, adm := range m8ActRawAdmissions(t, f) {
		if adm.OperationID == operationID {
			out += fmt.Sprintf(" | admission %s attempt=%s nonce=%s at=%s", adm.Kind, adm.AttemptID, adm.NonceHex, adm.AdmittedAt)
		}
	}
	return out
}

// m8ActExpire shifts every stored stamp of one activation back together so the
// graph stays internally consistent (a valid stored graph) while the DB clock is
// already past its fixed deadline.
func m8ActExpire(t *testing.T, f probeRegistryFixture, operationID string) {
	t.Helper()
	shift := 2 * time.Hour
	ctx := t.Context()
	a := m8ActRawRow(t, f, operationID)
	if _, err := f.db.ExecContext(ctx,
		"UPDATE probe_install_activations SET created_at = ?, updated_at = ?, deadline_at = ? WHERE operation_id = ?",
		a.CreatedAt.Add(-shift), a.UpdatedAt.Add(-shift), a.DeadlineAt.Add(-shift), operationID); err != nil {
		t.Fatalf("expire activation %s: %v", operationID, err)
	}
	for _, s := range m8ActRawSteps(t, f, operationID) {
		if s.StartedAt == nil {
			continue
		}
		var finished any
		if s.FinishedAt != nil {
			finished = s.FinishedAt.Add(-shift)
		}
		if _, err := f.db.ExecContext(ctx,
			"UPDATE probe_install_activation_steps SET started_at = ?, finished_at = ? WHERE operation_id = ? AND kind = ?",
			s.StartedAt.Add(-shift), finished, operationID, s.Kind); err != nil {
			t.Fatalf("expire step %s/%s: %v", operationID, s.Kind, err)
		}
	}
}

// m8ActSame compares two receipts field by field with time equality.
func m8ActSame(a, b *domain.ProbeActivation) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Identity != b.Identity || a.OwnerID != b.OwnerID || a.State != b.State || a.ErrorCode != b.ErrorCode || a.Facts != b.Facts {
		return false
	}
	if !a.CreatedAt.Equal(b.CreatedAt) || !a.UpdatedAt.Equal(b.UpdatedAt) || !a.DeadlineAt.Equal(b.DeadlineAt) || len(a.Steps) != len(b.Steps) {
		return false
	}
	for i := range a.Steps {
		x, y := a.Steps[i], b.Steps[i]
		if x.Spec != y.Spec || x.State != y.State || x.AttemptID != y.AttemptID || x.ErrorCode != y.ErrorCode ||
			!x.StartedAt.Equal(y.StartedAt) || !x.FinishedAt.Equal(y.FinishedAt) {
			return false
		}
	}
	return true
}

func m8ActRequireSame(t *testing.T, got, want *domain.ProbeActivation, what string) {
	t.Helper()
	if !m8ActSame(got, want) {
		t.Fatalf("%s changed the receipt:\n got %+v\nwant %+v", what, got, want)
	}
}

// m8ActCheck asserts every returned receipt is a valid domain activation whose
// stamps crossed the DB boundary as UTC microseconds.
func m8ActCheck(t *testing.T, a *domain.ProbeActivation) *domain.ProbeActivation {
	t.Helper()
	if a == nil {
		t.Fatal("nil activation receipt")
	}
	if !a.Valid() {
		t.Fatalf("returned receipt is not a valid domain activation: %+v", a)
	}
	check := func(name string, stamp time.Time, allowZero bool) {
		t.Helper()
		if stamp.IsZero() {
			if !allowZero {
				t.Fatalf("%s stamp is zero", name)
			}
			return
		}
		if stamp.Location() != time.UTC || stamp.Nanosecond()%1000 != 0 {
			t.Fatalf("%s stamp is not a UTC microsecond value: %v", name, stamp)
		}
	}
	check("created", a.CreatedAt, false)
	check("updated", a.UpdatedAt, false)
	check("deadline", a.DeadlineAt, false)
	for i, step := range a.Steps {
		check(fmt.Sprintf("step %d started", i), step.StartedAt, true)
		check(fmt.Sprintf("step %d finished", i), step.FinishedAt, true)
	}
	return a
}

func m8ActStep(t *testing.T, a *domain.ProbeActivation, kind string) domain.ProbeInstallResource {
	t.Helper()
	for _, step := range a.Steps {
		if step.Spec.Kind == kind {
			return step
		}
	}
	t.Fatalf("step %s missing from %+v", kind, a.Steps)
	return domain.ProbeInstallResource{}
}

// m8ActEnv is one isolated engine fixture and its activation store.
type m8ActEnv struct {
	t      *testing.T
	f      probeRegistryFixture
	engine string
	store  *repository.ProbeInstallActivationStore
}

func newM8ActEnv(t *testing.T, engine string) *m8ActEnv {
	t.Helper()
	f := newProbeRegistryFixture(t, engine)
	return &m8ActEnv{t: t, f: f, engine: engine, store: repository.NewProbeInstallActivationStore(f.db)}
}

// installReady drives an install to resources_ready through the production
// install journal store: every resource reserved and recorded applied in order.
func (e *m8ActEnv) installReady(operationID string) {
	e.t.Helper()
	istore := m8InstallStore(e.f)
	m8InstallCreate(e.t, istore, m8ActIdentity(operationID).Install, m8InstallOwnerA)
	m8InstallBeginOK(e.t, istore, operationID, m8InstallOwnerA)
	var last *domain.ProbeInstallOperation
	for i, kind := range m8InstallKinds(domain.ProbeInstallSystemd) {
		attempt := m8ActInstallAttempt(operationID, i+1)
		m8InstallReserveOK(e.t, istore, operationID, m8InstallOwnerA, kind, attempt)
		last = m8InstallRecordOK(e.t, istore, operationID, m8InstallOwnerA, kind, attempt, m8InstallApplied())
	}
	if last.State != domain.ProbeInstallResourcesReady {
		e.t.Fatalf("install did not reach resources_ready: %+v", last)
	}
}

// create admits one queued activation over an already resources_ready install.
func (e *m8ActEnv) create(operationID, ownerID string) *domain.ProbeActivation {
	e.t.Helper()
	a, created, err := e.store.CreateActivation(e.t.Context(), m8ActIdentity(operationID), ownerID)
	if err != nil || !created {
		e.t.Fatalf("create activation %s: created=%v err=%v", operationID, created, err)
	}
	m8ActCheck(e.t, a)
	if a.State != domain.ProbeActivationQueued || a.OwnerID != ownerID || a.Facts != (domain.ProbeActivationFacts{}) ||
		!a.DeadlineAt.Equal(a.CreatedAt.Add(domain.ProbeActivationLifetime)) {
		e.t.Fatalf("queued activation shape: %+v", a)
	}
	return a
}

// fresh builds a ready install plus its queued activation under owner A.
func (e *m8ActEnv) fresh(operationID string) *domain.ProbeActivation {
	e.t.Helper()
	e.installReady(operationID)
	return e.create(operationID, m8InstallOwnerA)
}

func (e *m8ActEnv) begin(operationID, ownerID string) *domain.ProbeActivation {
	e.t.Helper()
	a, err := e.store.BeginActivation(e.t.Context(), operationID, ownerID)
	if err != nil || a == nil || a.State != domain.ProbeActivationRunning {
		e.t.Fatalf("begin %s: %+v err=%v", operationID, a, err)
	}
	return m8ActCheck(e.t, a)
}

func (e *m8ActEnv) reserve(operationID, ownerID, kind string) *domain.ProbeActivation {
	e.t.Helper()
	attempt := m8ActAttempt(operationID, kind)
	a, created, err := e.store.ReserveActivationStep(e.t.Context(), operationID, ownerID, kind, attempt)
	if err != nil || !created {
		e.t.Fatalf("reserve %s/%s: created=%v err=%v", operationID, kind, created, err)
	}
	m8ActCheck(e.t, a)
	if step := m8ActStep(e.t, a, kind); step.State != domain.ProbeInstallResourceInFlight || step.AttemptID != attempt {
		e.t.Fatalf("fresh reservation is not a committed in-flight intent: %+v", step)
	}
	return a
}

// facts returns the facts a record call must carry for this outcome.
func (e *m8ActEnv) facts(operationID, kind string, outcome domain.ProbeInstallResourceOutcome) domain.ProbeActivationFacts {
	if kind == domain.ProbeActivationProbeInit && outcome.State == domain.ProbeInstallResourceApplied {
		return m8ActFacts(operationID)
	}
	return domain.ProbeActivationFacts{}
}

func (e *m8ActEnv) record(operationID, ownerID, kind string, outcome domain.ProbeInstallResourceOutcome) *domain.ProbeActivation {
	e.t.Helper()
	a, err := e.store.RecordActivationStep(e.t.Context(), operationID, ownerID, kind, m8ActAttempt(operationID, kind), outcome, e.facts(operationID, kind, outcome))
	if err != nil {
		e.t.Fatalf("record %s/%s %s: %v", operationID, kind, outcome.State, err)
	}
	m8ActCheck(e.t, a)
	if step := m8ActStep(e.t, a, kind); step.State != outcome.State || step.ErrorCode != outcome.ErrorCode {
		e.t.Fatalf("recorded outcome mismatch: %+v want %+v", step, outcome)
	}
	return a
}

// advance reserves and records the first n steps applied, in order.
func (e *m8ActEnv) advance(operationID, ownerID string, n int) *domain.ProbeActivation {
	e.t.Helper()
	var last *domain.ProbeActivation
	for _, kind := range m8ActKinds[:n] {
		e.reserve(operationID, ownerID, kind)
		last = e.record(operationID, ownerID, kind, m8InstallApplied())
	}
	return last
}

func (e *m8ActEnv) observe(operationID string) *domain.ProbeActivation {
	e.t.Helper()
	a, err := e.store.ObserveActivation(e.t.Context(), operationID)
	if err != nil {
		e.t.Fatalf("observe %s: %v", operationID, err)
	}
	return m8ActCheck(e.t, a)
}

func (e *m8ActEnv) conflicts(what string, err error) {
	e.t.Helper()
	if !errors.Is(err, ports.ErrConflict) {
		e.t.Fatalf("%s must conflict, got %v", what, err)
	}
}

func m8ActFinish(state, code string) domain.ProbeInstallOutcome {
	return domain.ProbeInstallOutcome{State: state, ErrorCode: code}
}

// TestM8InstallActivationStore proves the durable M8 probe activation journal
// through the real production fixtures on both engines: creation only over a
// resources_ready install, immutable exact retries, committed step
// prerequisites, probe_init facts stored atomically with the applied step,
// owner/state/expiry fencing after the shared row lock, honest cold expiry,
// cancellation intent versus joined outcomes, corrupt-graph refusal before
// reconciliation, rollback discipline, guarded downgrade and UTC stamps.
// MariaDB leaves require TEST_MARIADB_DSN; a skip establishes nothing about
// InnoDB, so the real-engine gate must execute and count them.
func TestM8InstallActivationStore(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("CreateRequiresResourcesReadyInstall", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				istore := m8InstallStore(e.f)
				// An install that does not exist.
				if _, created, err := e.store.CreateActivation(ctx, m8ActIdentity(m8InstallOp9), m8InstallOwnerA); created || !errors.Is(err, ports.ErrNotFound) {
					t.Fatalf("missing install must be not-found: created=%v err=%v", created, err)
				}
				// queued
				m8InstallCreate(t, istore, m8ActIdentity(m8InstallOp1).Install, m8InstallOwnerA)
				// running
				m8InstallCreate(t, istore, m8ActIdentity(m8InstallOp2).Install, m8InstallOwnerA)
				m8InstallBeginOK(t, istore, m8InstallOp2, m8InstallOwnerA)
				// running with a fully applied prefix short of resources_ready
				m8InstallCreate(t, istore, m8ActIdentity(m8InstallOp3).Install, m8InstallOwnerA)
				m8InstallBeginOK(t, istore, m8InstallOp3, m8InstallOwnerA)
				for i, kind := range m8InstallKinds(domain.ProbeInstallSystemd)[:3] {
					attempt := m8ActInstallAttempt(m8InstallOp3, i+1)
					m8InstallReserveOK(t, istore, m8InstallOp3, m8InstallOwnerA, kind, attempt)
					m8InstallRecordOK(t, istore, m8InstallOp3, m8InstallOwnerA, kind, attempt, m8InstallApplied())
				}
				// failed
				m8InstallCreate(t, istore, m8ActIdentity(m8InstallOp4).Install, m8InstallOwnerA)
				m8InstallBeginOK(t, istore, m8InstallOp4, m8InstallOwnerA)
				m8InstallReserveOK(t, istore, m8InstallOp4, m8InstallOwnerA, "data_directory", m8ActInstallAttempt(m8InstallOp4, 1))
				m8InstallRecordOK(t, istore, m8InstallOp4, m8InstallOwnerA, "data_directory", m8ActInstallAttempt(m8InstallOp4, 1), m8InstallApplied())
				if _, err := istore.FinishInstall(ctx, m8InstallOp4, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallFailed, "execution_failed")); err != nil {
					t.Fatal(err)
				}
				// canceled
				m8InstallCreate(t, istore, m8ActIdentity(m8InstallOp5).Install, m8InstallOwnerA)
				if _, err := istore.RequestInstallCancellation(ctx, m8InstallOp5); err != nil {
					t.Fatal(err)
				}
				// interrupted by an unknown outcome
				m8InstallCreate(t, istore, m8ActIdentity(m8InstallOp6).Install, m8InstallOwnerA)
				m8InstallBeginOK(t, istore, m8InstallOp6, m8InstallOwnerA)
				m8InstallReserveOK(t, istore, m8InstallOp6, m8InstallOwnerA, "data_directory", m8ActInstallAttempt(m8InstallOp6, 1))
				m8InstallRecordOK(t, istore, m8InstallOp6, m8InstallOwnerA, "data_directory", m8ActInstallAttempt(m8InstallOp6, 1), m8InstallUnknownOutcome())
				// resources_ready, then install cancellation intent
				e.installReady(m8InstallOp7)
				if cancel, err := istore.RequestInstallCancellation(ctx, m8InstallOp7); err != nil || cancel.State != domain.ProbeInstallCancelRequested {
					t.Fatalf("install cancel intent: %+v err=%v", cancel, err)
				}
				for _, op := range []string{m8InstallOp1, m8InstallOp2, m8InstallOp3, m8InstallOp4, m8InstallOp5, m8InstallOp6, m8InstallOp7} {
					before := m8InstallSnapshot(t, e.f, op)
					if a, created, err := e.store.CreateActivation(ctx, m8ActIdentity(op), m8InstallOwnerA); created || a != nil || !errors.Is(err, ports.ErrConflict) {
						t.Fatalf("install %s must refuse activation: created=%v err=%v", op, created, err)
					}
					if after := m8InstallSnapshot(t, e.f, op); after != before {
						t.Fatalf("refused create changed install %s:\nbefore %s\nafter  %s", op, before, after)
					}
				}
				if n := m8ActCount(t, e.f, "probe_install_activations"); n != 0 {
					t.Fatalf("refused creates left %d activation rows", n)
				}
				if n := m8ActCount(t, e.f, "probe_install_activation_steps"); n != 0 {
					t.Fatalf("refused creates left %d step rows", n)
				}
				// A resources_ready install whose stored identity differs conflicts.
				e.installReady(m8InstallOp8)
				for name, change := range map[string]func(*domain.ProbeActivationIdentity){
					"host_key": func(i *domain.ProbeActivationIdentity) { i.Install.HostKeyFingerprint = m8InstallFingerprint('z') },
					"binary":   func(i *domain.ProbeActivationIdentity) { i.Install.Binary.Version = "9.9.9" },
					"address":  func(i *domain.ProbeActivationIdentity) { i.Install.Host.Address = "192.0.2.77:22" },
					"actor":    func(i *domain.ProbeActivationIdentity) { i.Install.RequestedBy++ },
				} {
					bad := m8ActIdentity(m8InstallOp8)
					change(&bad)
					if !bad.Valid() {
						t.Fatalf("fixture %s must stay domain-valid", name)
					}
					if _, created, err := e.store.CreateActivation(ctx, bad, m8InstallOwnerA); created || !errors.Is(err, ports.ErrConflict) {
						t.Fatalf("install identity %s mismatch must conflict: created=%v err=%v", name, created, err)
					}
				}
				// Domain-invalid requests never reach storage.
				nonRoot := m8ActIdentity(m8InstallOp8)
				nonRoot.Install.Host.User = "phoenix"
				docker := domain.ProbeActivationIdentity{Install: m8InstallIdentity(domain.ProbeInstallDocker, m8InstallOp8), ProbeKey: "edge", Name: "Edge", ListenPort: 8443, ClientKeyFingerprint: m8InstallFingerprint('b'), RequestedBy: m8InstallActor}
				for name, bad := range map[string]domain.ProbeActivationIdentity{"non_root": nonRoot, "docker": docker, "zero": {}} {
					if _, _, err := e.store.CreateActivation(ctx, bad, m8InstallOwnerA); !errors.Is(err, domain.ErrValidation) {
						t.Fatalf("%s identity must fail validation, got %v", name, err)
					}
				}
				if n := m8ActCount(t, e.f, "probe_install_activations"); n != 0 {
					t.Fatalf("conflicting creates left %d activation rows", n)
				}
				// The positive control over the ready install succeeds exactly once.
				e.create(m8InstallOp8, m8InstallOwnerA)
			})
			t.Run("AtomicCreateAndImmutableRetry", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.installReady(m8InstallOp1)
				installBefore := m8InstallSnapshot(t, e.f, m8InstallOp1)
				identity := m8ActIdentity(m8InstallOp1)
				created := e.create(m8InstallOp1, m8InstallOwnerA)
				if installAfter := m8InstallSnapshot(t, e.f, m8InstallOp1); installAfter != installBefore {
					t.Fatalf("activation creation changed the install journal:\nbefore %s\nafter  %s", installBefore, installAfter)
				}
				specs := identity.Steps()
				if created.Identity != identity || len(created.Steps) != len(specs) || len(specs) != 4 {
					t.Fatalf("atomic creation lost identity or steps: %+v", created)
				}
				for i, spec := range specs {
					step := created.Steps[i]
					if step.Spec != spec || step.State != domain.ProbeInstallResourcePlanned || step.AttemptID != "" ||
						step.ErrorCode != "" || !step.StartedAt.IsZero() || !step.FinishedAt.IsZero() {
						t.Fatalf("planned step %d malformed: %+v", i, step)
					}
				}
				// Commit-before-return: a cold read sees the complete graph.
				m8ActRequireSame(t, e.observe(m8InstallOp1), created, "commit-before-return read")
				// Exact identity retry from a different owner keeps the original
				// owner, deadline and history and never replenishes authority.
				again, fresh, err := e.store.CreateActivation(ctx, identity, m8InstallOwnerB)
				if err != nil || fresh {
					t.Fatalf("exact retry: created=%v err=%v", fresh, err)
				}
				m8ActRequireSame(t, m8ActCheck(t, again), created, "exact cross-owner retry")
				// ... including after the activation has made progress.
				e.begin(m8InstallOp1, m8InstallOwnerA)
				progressed := e.observe(m8InstallOp1)
				again, fresh, err = e.store.CreateActivation(ctx, identity, m8InstallOwnerB)
				if err != nil || fresh {
					t.Fatalf("retry after begin: created=%v err=%v", fresh, err)
				}
				m8ActRequireSame(t, m8ActCheck(t, again), progressed, "retry after begin")
				// Every immutable identity member is bound.
				mutations := map[string]func(*domain.ProbeActivationIdentity){
					"probe_key":  func(i *domain.ProbeActivationIdentity) { i.ProbeKey = "edge-eu-2" },
					"name":       func(i *domain.ProbeActivationIdentity) { i.Name = "Edge EU 2" },
					"location":   func(i *domain.ProbeActivationIdentity) { i.Location = "Paris" },
					"client_key": func(i *domain.ProbeActivationIdentity) { i.ClientKeyFingerprint = m8InstallFingerprint('d') },
					"actor":      func(i *domain.ProbeActivationIdentity) { i.RequestedBy++ },
					"install":    func(i *domain.ProbeActivationIdentity) { i.Install.HostKeyFingerprint = m8InstallFingerprint('e') },
				}
				for name, mutate := range mutations {
					request := identity
					mutate(&request)
					if !request.Valid() {
						t.Fatalf("fixture %s must stay domain-valid", name)
					}
					if _, fresh, err := e.store.CreateActivation(ctx, request, m8InstallOwnerA); fresh || !errors.Is(err, ports.ErrConflict) {
						t.Fatalf("changed %s must conflict: created=%v err=%v", name, fresh, err)
					}
				}
				m8ActRequireSame(t, e.observe(m8InstallOp1), progressed, "conflicting retries")
				// Invalid requests never reach the journal.
				// The port is fixed to the installed unit's listener: any other
				// value is invalid rather than a changed identity.
				badPort := identity
				badPort.ListenPort = 80
				otherPort := identity
				otherPort.ListenPort = 9443
				badKey := identity
				badKey.ProbeKey = "Not A Slug"
				for name, bad := range map[string]domain.ProbeActivationIdentity{"port": badPort, "other port": otherPort, "key": badKey, "zero": {}} {
					if _, _, err := e.store.CreateActivation(ctx, bad, m8InstallOwnerA); !errors.Is(err, domain.ErrValidation) {
						t.Fatalf("invalid %s must fail validation, got %v", name, err)
					}
				}
				if _, _, err := e.store.CreateActivation(ctx, identity, "not-a-uuid"); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("invalid owner must fail validation, got %v", err)
				}
				if _, err := e.store.ObserveActivation(ctx, "not-a-uuid"); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("invalid observe id must fail validation, got %v", err)
				}
				// An install with no activation observes as not found.
				e.installReady(m8InstallOp2)
				if _, err := e.store.ObserveActivation(ctx, m8InstallOp2); !errors.Is(err, ports.ErrNotFound) {
					t.Fatalf("install without activation must be not-found, got %v", err)
				}
				// Multi-byte display text round-trips exactly.
				e.installReady(m8InstallOp3)
				unicode := m8ActIdentity(m8InstallOp3)
				unicode.Name = "Édge — 東京 ✓"
				unicode.Location = "Zürich/CH"
				stored, fresh, err := e.store.CreateActivation(ctx, unicode, m8InstallOwnerA)
				if err != nil || !fresh || stored.Identity.Name != unicode.Name || stored.Identity.Location != unicode.Location {
					t.Fatalf("unicode identity did not round-trip: created=%v err=%v %+v", fresh, err, stored)
				}
				m8ActCheck(t, stored)
				// An empty location is a valid, stored value.
				e.installReady(m8InstallOp4)
				empty := m8ActIdentity(m8InstallOp4)
				empty.Location = ""
				stored, fresh, err = e.store.CreateActivation(ctx, empty, m8InstallOwnerA)
				if err != nil || !fresh || stored.Identity.Location != "" {
					t.Fatalf("empty location: created=%v err=%v", fresh, err)
				}
				// The same install activated by a retry stays a single row.
				if n := m8ActCount(t, e.f, "probe_install_activations"); n != 3 {
					t.Fatalf("expected exactly three activations, got %d", n)
				}
			})
			t.Run("OwnerStateExpiryFences", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.fresh(m8InstallOp1)
				if _, err := e.store.BeginActivation(ctx, m8InstallOp1, m8InstallOwnerB); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("foreign begin must conflict, got %v", err)
				}
				e.begin(m8InstallOp1, m8InstallOwnerA)
				_, err := e.store.BeginActivation(ctx, m8InstallOp1, m8InstallOwnerA)
				e.conflicts("second begin (no second dispatch)", err)
				// Every journal write is fenced by the exact stored owner.
				_, _, err = e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerB, domain.ProbeActivationProbeInit, m8ActAttempt(m8InstallOp1, domain.ProbeActivationProbeInit))
				e.conflicts("foreign reserve", err)
				_, err = e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerB, domain.ProbeActivationProbeInit, m8ActAttempt(m8InstallOp1, domain.ProbeActivationProbeInit), m8InstallApplied(), m8ActFacts(m8InstallOp1))
				e.conflicts("foreign record", err)
				_, err = e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerB, m8ActFinish(domain.ProbeInstallFailed, "internal"))
				e.conflicts("foreign finish", err)
				// Queued work has no joined executor and cannot be finished.
				e.fresh(m8InstallOp2)
				_, err = e.store.FinishActivation(ctx, m8InstallOp2, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallFailed, "internal"))
				e.conflicts("finish before begin", err)
				// Reserve and record need a running activation.
				_, _, err = e.store.ReserveActivationStep(ctx, m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8ActAttempt(m8InstallOp2, domain.ProbeActivationProbeInit))
				e.conflicts("reserve before begin", err)
				// Record requires the exact stored attempt, not just a kind.
				_, err = e.store.RecordActivationStep(ctx, m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8ActAttempt(m8InstallOp2, domain.ProbeActivationProbeInit), m8InstallApplied(), m8ActFacts(m8InstallOp2))
				e.conflicts("record without reservation", err)
				// Unknown and install-journal kinds are refused as invalid input.
				for _, kind := range []string{"bogus", "data_directory", "unit_file", "service", "artifact_directory", ""} {
					if _, _, err := e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, kind, m8ActForeignAttempt); !errors.Is(err, domain.ErrValidation) {
						t.Fatalf("reserve kind %q must fail validation, got %v", kind, err)
					}
					if _, err := e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, kind, m8ActForeignAttempt, m8InstallApplied(), domain.ProbeActivationFacts{}); !errors.Is(err, domain.ErrValidation) {
						t.Fatalf("record kind %q must fail validation, got %v", kind, err)
					}
				}
				// Invalid owners/attempts/outcomes are refused before storage.
				if _, err := e.store.BeginActivation(ctx, m8InstallOp1, "x"); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("invalid owner begin must fail validation, got %v", err)
				}
				if _, _, err := e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration, "x"); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("invalid attempt reserve must fail validation, got %v", err)
				}
				if _, err := e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration, m8ActForeignAttempt, domain.ProbeInstallResourceOutcome{State: "bogus"}, domain.ProbeActivationFacts{}); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("invalid outcome must fail validation, got %v", err)
				}
				if _, err := e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallResourcesReady, "")); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("success-shaped finish must fail validation, got %v", err)
				}
				if _, err := e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeActivationActive, "")); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("active is never a finish outcome, got %v", err)
				}
				// Hard-expired work refuses the dispatch grant; the honest expiry
				// reconciliation still commits.
				e.fresh(m8InstallOp3)
				m8ActExpire(t, e.f, m8InstallOp3)
				_, err = e.store.BeginActivation(ctx, m8InstallOp3, m8InstallOwnerA)
				e.conflicts("expired begin", err)
				// Read the raw row BEFORE any healing read: the reconciliation must
				// have committed through the refusal.
				if raw := m8ActRawRow(t, e.f, m8InstallOp3); raw.State != domain.ProbeActivationInterrupted || raw.ErrorCode == nil || *raw.ErrorCode != "deadline_expired" {
					t.Fatalf("expired begin did not commit its reconciliation: %+v", raw)
				}
				expired := e.observe(m8InstallOp3)
				if expired.State != domain.ProbeActivationInterrupted || expired.ErrorCode != "deadline_expired" || expired.UpdatedAt.Before(expired.DeadlineAt) {
					t.Fatalf("expired begin did not reconcile: %+v", expired)
				}
				if m8ActStep(t, expired, domain.ProbeActivationProbeInit).State != domain.ProbeInstallResourcePlanned {
					t.Fatalf("expiry fabricated step state: %+v", expired.Steps)
				}
				// Unknown activations are not found, never conflicts.
				if _, err := e.store.BeginActivation(ctx, m8InstallOp9, m8InstallOwnerA); !errors.Is(err, ports.ErrNotFound) {
					t.Fatalf("unknown activation begin must be not-found, got %v", err)
				}
			})
			t.Run("CommittedPrerequisiteAndOneAttempt", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.fresh(m8InstallOp1)
				e.begin(m8InstallOp1, m8InstallOwnerA)
				// Queue position is not authority: no step may be reserved before
				// its predecessors are committed applied.
				for _, kind := range m8ActKinds[1:] {
					_, _, err := e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, kind, m8ActAttempt(m8InstallOp1, kind))
					e.conflicts("successor "+kind+" without predecessor", err)
				}
				e.reserve(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				_, _, err := e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration, m8ActAttempt(m8InstallOp1, domain.ProbeActivationRegistration))
				e.conflicts("successor with in-flight predecessor", err)
				// not_started never unlocks its successors.
				e.record(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8InstallNotStartedOutcome())
				_, _, err = e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration, m8ActAttempt(m8InstallOp1, domain.ProbeActivationRegistration))
				e.conflicts("successor after not_started predecessor", err)
				// A second attempt on the not_started step is refused too.
				_, _, err = e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8ActForeignAttempt)
				e.conflicts("second attempt on a not_started step", err)
				// Committed applied predecessors unlock exactly one successor.
				e.fresh(m8InstallOp2)
				e.begin(m8InstallOp2, m8InstallOwnerA)
				e.advance(m8InstallOp2, m8InstallOwnerA, 1)
				inFlight := e.reserve(m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationRegistration)
				// One attempt per step.
				_, _, err = e.store.ReserveActivationStep(ctx, m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationRegistration, m8ActForeignAttempt)
				e.conflicts("second attempt on one step", err)
				// Exact reservation retry reads history and grants nothing.
				retry, granted, err := e.store.ReserveActivationStep(ctx, m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationRegistration, m8ActAttempt(m8InstallOp2, domain.ProbeActivationRegistration))
				if err != nil || granted {
					t.Fatalf("exact reservation retry: created=%v err=%v", granted, err)
				}
				m8ActRequireSame(t, m8ActCheck(t, retry), inFlight, "exact reservation retry")
				// Another owner's exact attempt does not read history either.
				_, _, err = e.store.ReserveActivationStep(ctx, m8InstallOp2, m8InstallOwnerB, domain.ProbeActivationRegistration, m8ActAttempt(m8InstallOp2, domain.ProbeActivationRegistration))
				e.conflicts("foreign-owner reservation retry", err)
				e.record(m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationRegistration, m8InstallApplied())
				// Duplicate attempt UUIDs are refused anywhere in the graph, even
				// once every predecessor is applied.
				_, _, err = e.store.ReserveActivationStep(ctx, m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationServiceStart, m8ActAttempt(m8InstallOp2, domain.ProbeActivationProbeInit))
				e.conflicts("duplicated attempt in one graph", err)
				e.reserve(m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationServiceStart)
				e.record(m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationServiceStart, m8InstallApplied())
				e.reserve(m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationEnrollment)
			})
			t.Run("ProbeInitFactsRequiredAndStored", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.fresh(m8InstallOp1)
				e.begin(m8InstallOp1, m8InstallOwnerA)
				e.reserve(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				attempt := m8ActAttempt(m8InstallOp1, domain.ProbeActivationProbeInit)
				facts := m8ActFacts(m8InstallOp1)
				before := m8ActSnapshot(t, e.f, m8InstallOp1)
				// probe_init applied REQUIRES valid facts.
				badFacts := map[string]domain.ProbeActivationFacts{
					"zero":         {},
					"probe_id":     {ProbeID: "nope", StreamID: facts.StreamID, CertificateFingerprint: facts.CertificateFingerprint},
					"local_probe":  {ProbeID: domain.LocalProbeID, StreamID: facts.StreamID, CertificateFingerprint: facts.CertificateFingerprint},
					"stream_id":    {ProbeID: facts.ProbeID, StreamID: "", CertificateFingerprint: facts.CertificateFingerprint},
					"fingerprint":  {ProbeID: facts.ProbeID, StreamID: facts.StreamID, CertificateFingerprint: strings.Repeat("AB", 32)},
					"short_pin":    {ProbeID: facts.ProbeID, StreamID: facts.StreamID, CertificateFingerprint: strings.Repeat("ab", 31)},
					"upper_probe":  {ProbeID: strings.ToUpper(facts.ProbeID), StreamID: facts.StreamID, CertificateFingerprint: facts.CertificateFingerprint},
					"missing_pin":  {ProbeID: facts.ProbeID, StreamID: facts.StreamID},
					"zero_stream":  {ProbeID: facts.ProbeID, StreamID: "00000000-0000-0000-0000-000000000000", CertificateFingerprint: facts.CertificateFingerprint},
					"only_probeid": {ProbeID: facts.ProbeID},
				}
				for name, bad := range badFacts {
					if _, err := e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, attempt, m8InstallApplied(), bad); !errors.Is(err, domain.ErrValidation) {
						t.Fatalf("probe_init applied with %s facts must fail validation, got %v", name, err)
					}
				}
				if after := m8ActSnapshot(t, e.f, m8InstallOp1); after != before {
					t.Fatalf("refused facts changed storage:\nbefore %s\nafter  %s", before, after)
				}
				// Any other outcome of probe_init carries zero facts.
				for name, outcome := range map[string]domain.ProbeInstallResourceOutcome{"not_started": m8InstallNotStartedOutcome(), "unknown": m8InstallUnknownOutcome()} {
					if _, err := e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, attempt, outcome, facts); !errors.Is(err, domain.ErrValidation) {
						t.Fatalf("probe_init %s with facts must fail validation, got %v", name, err)
					}
				}
				if after := m8ActSnapshot(t, e.f, m8InstallOp1); after != before {
					t.Fatalf("refused outcome facts changed storage:\nbefore %s\nafter  %s", before, after)
				}
				// The valid report stores the facts atomically with the applied step.
				recorded, err := e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, attempt, m8InstallApplied(), facts)
				if err != nil {
					t.Fatalf("valid probe_init facts: %v", err)
				}
				m8ActCheck(t, recorded)
				if recorded.Facts != facts || m8ActStep(t, recorded, domain.ProbeActivationProbeInit).State != domain.ProbeInstallResourceApplied ||
					recorded.State != domain.ProbeActivationRunning {
					t.Fatalf("facts or applied step missing: %+v", recorded)
				}
				raw := m8ActRawRow(t, e.f, m8InstallOp1)
				if raw.ProbeID == nil || raw.StreamID == nil || raw.CertificateFingerprint == nil ||
					*raw.ProbeID != facts.ProbeID || *raw.StreamID != facts.StreamID || *raw.CertificateFingerprint != facts.CertificateFingerprint {
					t.Fatalf("stored facts differ: %+v", raw)
				}
				// The facts survive a cold read.
				if cold := e.observe(m8InstallOp1); cold.Facts != facts {
					t.Fatalf("cold read lost facts: %+v", cold)
				}
				// An exact retry reads history; different facts are a different claim.
				retry, err := e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, attempt, m8InstallApplied(), facts)
				if err != nil {
					t.Fatalf("exact facts retry: %v", err)
				}
				m8ActRequireSame(t, m8ActCheck(t, retry), recorded, "exact probe_init retry")
				other := m8ActFacts(m8InstallOp2)
				if _, err := e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, attempt, m8InstallApplied(), other); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("different facts on a completed init must conflict, got %v", err)
				}
				if _, err := e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, attempt, m8InstallNotStartedOutcome(), domain.ProbeActivationFacts{}); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("init may never be repaired to not_started, got %v", err)
				}
				m8ActRequireSame(t, e.observe(m8InstallOp1), recorded, "refused fact retries")
				// Other steps carry zero facts.
				e.reserve(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration)
				if _, err := e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration, m8ActAttempt(m8InstallOp1, domain.ProbeActivationRegistration), m8InstallApplied(), facts); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("registration applied with facts must fail validation, got %v", err)
				}
				// Facts of a failed init are never fabricated.
				e.fresh(m8InstallOp2)
				e.begin(m8InstallOp2, m8InstallOwnerA)
				e.reserve(m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				failed := e.record(m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8InstallNotStartedOutcome())
				if failed.Facts != (domain.ProbeActivationFacts{}) {
					t.Fatalf("not_started init carries facts: %+v", failed.Facts)
				}
				if raw := m8ActRawRow(t, e.f, m8InstallOp2); raw.ProbeID != nil || raw.StreamID != nil || raw.CertificateFingerprint != nil {
					t.Fatalf("not_started init stored facts: %+v", raw)
				}
			})
			t.Run("FullLifecycleActive", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.fresh(m8InstallOp1)
				running := e.begin(m8InstallOp1, m8InstallOwnerA)
				if running.State != domain.ProbeActivationRunning {
					t.Fatalf("begin: %+v", running)
				}
				for i, kind := range m8ActKinds {
					e.reserve(m8InstallOp1, m8InstallOwnerA, kind)
					a := e.record(m8InstallOp1, m8InstallOwnerA, kind, m8InstallApplied())
					last := i == len(m8ActKinds)-1
					if last && (a.State != domain.ProbeActivationActive || !a.Terminal() || a.ErrorCode != "") {
						t.Fatalf("last applied step must make the activation active: %+v", a)
					}
					if !last && a.State != domain.ProbeActivationRunning {
						t.Fatalf("an applied prefix must stay running: %+v", a)
					}
				}
				active := e.observe(m8InstallOp1)
				if active.State != domain.ProbeActivationActive || active.Facts != m8ActFacts(m8InstallOp1) {
					t.Fatalf("active receipt: %+v", active)
				}
				// Active is as immutable as any terminal outcome.
				_, err := e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallFailed, "internal"))
				e.conflicts("finish over active", err)
				_, err = e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallInterrupted, "owner_interrupted"))
				e.conflicts("owner interruption over active", err)
				canceled, err := e.store.RequestActivationCancellation(ctx, m8InstallOp1)
				if err != nil {
					t.Fatalf("cancel over active: %v", err)
				}
				m8ActRequireSame(t, m8ActCheck(t, canceled), active, "cancel over active")
				_, err = e.store.BeginActivation(ctx, m8InstallOp1, m8InstallOwnerA)
				e.conflicts("begin over active", err)
				// Exact outcome retry reads history; a late result conflicts.
				retry, err := e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationEnrollment, m8ActAttempt(m8InstallOp1, domain.ProbeActivationEnrollment), m8InstallApplied(), domain.ProbeActivationFacts{})
				if err != nil {
					t.Fatalf("exact enrollment retry: %v", err)
				}
				m8ActRequireSame(t, m8ActCheck(t, retry), active, "exact applied retry over active")
				_, err = e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationEnrollment, m8ActAttempt(m8InstallOp1, domain.ProbeActivationEnrollment), m8InstallUnknownOutcome(), domain.ProbeActivationFacts{})
				e.conflicts("late uncertainty over active", err)
				// A historical reservation never grants again.
				hist, granted, err := e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8ActAttempt(m8InstallOp1, domain.ProbeActivationProbeInit))
				if err != nil || granted {
					t.Fatalf("historical reservation retry: created=%v err=%v", granted, err)
				}
				m8ActRequireSame(t, m8ActCheck(t, hist), active, "historical reservation retry")
				// The deadline passing never turns active into an interruption.
				m8ActExpire(t, e.f, m8InstallOp1)
				still := e.observe(m8InstallOp1)
				if still.State != domain.ProbeActivationActive || still.ErrorCode != "" {
					t.Fatalf("expiry rewrote an active activation: %+v", still)
				}
				if raw := m8ActRawRow(t, e.f, m8InstallOp1); raw.State != domain.ProbeActivationActive || raw.ErrorCode != nil {
					t.Fatalf("expiry rewrote the stored active row: %+v", raw)
				}
			})
			t.Run("CancelQueuedCanceled", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.fresh(m8InstallOp1)
				canceled, err := e.store.RequestActivationCancellation(ctx, m8InstallOp1)
				if err != nil {
					t.Fatalf("queued cancel: %v", err)
				}
				m8ActCheck(t, canceled)
				if canceled.State != domain.ProbeActivationCanceled || canceled.ErrorCode != "canceled" || !canceled.Terminal() {
					t.Fatalf("queued cancel must be terminal canceled: %+v", canceled)
				}
				_, err = e.store.BeginActivation(ctx, m8InstallOp1, m8InstallOwnerA)
				e.conflicts("begin after queued cancel", err)
				// Exact terminal finish retries read immutable history.
				retry, err := e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallCanceled, "canceled"))
				if err != nil {
					t.Fatalf("exact terminal finish retry: %v", err)
				}
				m8ActRequireSame(t, m8ActCheck(t, retry), canceled, "exact terminal finish retry")
				_, err = e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerB, m8ActFinish(domain.ProbeInstallCanceled, "canceled"))
				e.conflicts("foreign terminal finish retry", err)
				again, err := e.store.RequestActivationCancellation(ctx, m8InstallOp1)
				if err != nil {
					t.Fatal(err)
				}
				m8ActRequireSame(t, m8ActCheck(t, again), canceled, "repeated cancel")
				// No step may ever run on a canceled activation.
				_, _, err = e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8ActAttempt(m8InstallOp1, domain.ProbeActivationProbeInit))
				e.conflicts("reserve on canceled", err)
			})
			t.Run("CancellationIntentVersusJoinedFinish", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				// A joined owner cannot fabricate cancellation without durable intent.
				e.fresh(m8InstallOp1)
				e.begin(m8InstallOp1, m8InstallOwnerA)
				e.advance(m8InstallOp1, m8InstallOwnerA, 1)
				_, err := e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallCanceled, "canceled"))
				e.conflicts("joined cancel without prior intent", err)
				intent, err := e.store.RequestActivationCancellation(ctx, m8InstallOp1)
				if err != nil {
					t.Fatalf("cancel intent: %v", err)
				}
				m8ActCheck(t, intent)
				if intent.State != domain.ProbeActivationCancelRequested || intent.Terminal() || intent.ErrorCode != "" {
					t.Fatalf("running cancel must stay intent: %+v", intent)
				}
				repeat, err := e.store.RequestActivationCancellation(ctx, m8InstallOp1)
				if err != nil {
					t.Fatal(err)
				}
				m8ActRequireSame(t, m8ActCheck(t, repeat), intent, "repeated cancel intent")
				// Cancellation intent stops new work.
				_, _, err = e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration, m8ActAttempt(m8InstallOp1, domain.ProbeActivationRegistration))
				e.conflicts("reserve under cancel intent", err)
				finished, err := e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallCanceled, "canceled"))
				if err != nil {
					t.Fatalf("joined cancel finish: %v", err)
				}
				m8ActCheck(t, finished)
				if finished.State != domain.ProbeActivationCanceled || finished.ErrorCode != "canceled" || !finished.Terminal() ||
					finished.Facts != m8ActFacts(m8InstallOp1) {
					t.Fatalf("joined cancel finish: %+v", finished)
				}
				// A foreign joined finish never lands.
				e.fresh(m8InstallOp2)
				e.begin(m8InstallOp2, m8InstallOwnerA)
				if _, err := e.store.RequestActivationCancellation(ctx, m8InstallOp2); err != nil {
					t.Fatal(err)
				}
				_, err = e.store.FinishActivation(ctx, m8InstallOp2, m8InstallOwnerB, m8ActFinish(domain.ProbeInstallCanceled, "canceled"))
				e.conflicts("foreign joined cancel finish", err)
				// Failure and cancellation refuse unresolved actions.
				e.fresh(m8InstallOp3)
				e.begin(m8InstallOp3, m8InstallOwnerA)
				e.reserve(m8InstallOp3, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				if _, err := e.store.RequestActivationCancellation(ctx, m8InstallOp3); err != nil {
					t.Fatal(err)
				}
				_, err = e.store.FinishActivation(ctx, m8InstallOp3, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallCanceled, "canceled"))
				e.conflicts("canceled with in-flight step", err)
				_, err = e.store.FinishActivation(ctx, m8InstallOp3, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallFailed, "execution_failed"))
				e.conflicts("failed with in-flight step", err)
				// Reports for already committed work keep landing under the intent.
				e.record(m8InstallOp3, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8InstallNotStartedOutcome())
				resolved, err := e.store.FinishActivation(ctx, m8InstallOp3, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallFailed, "not_started"))
				if err != nil || resolved.State != domain.ProbeActivationFailed || resolved.ErrorCode != "not_started" {
					t.Fatalf("resolved failure: %+v err=%v", resolved, err)
				}
				m8ActCheck(t, resolved)
			})
			t.Run("CancelDuringLastStepEndsActive", func(t *testing.T) {
				// DECISION: the domain forbids cancel_requested with every step
				// applied, so a last applied report that lands after cancellation
				// intent makes the activation active. Every step really happened;
				// calling the result canceled would be a lie.
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.fresh(m8InstallOp1)
				e.begin(m8InstallOp1, m8InstallOwnerA)
				e.advance(m8InstallOp1, m8InstallOwnerA, 3)
				e.reserve(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationEnrollment)
				if intent, err := e.store.RequestActivationCancellation(ctx, m8InstallOp1); err != nil || intent.State != domain.ProbeActivationCancelRequested {
					t.Fatalf("cancel intent: %+v err=%v", intent, err)
				}
				done := e.record(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationEnrollment, m8InstallApplied())
				if done.State != domain.ProbeActivationActive || done.ErrorCode != "" {
					t.Fatalf("last applied under cancel intent must be active: %+v", done)
				}
				_, err := e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallCanceled, "canceled"))
				e.conflicts("cancel finish over active", err)
				// A non-final applied report under cancel intent stays cancel_requested.
				e.fresh(m8InstallOp2)
				e.begin(m8InstallOp2, m8InstallOwnerA)
				e.reserve(m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				if _, err := e.store.RequestActivationCancellation(ctx, m8InstallOp2); err != nil {
					t.Fatal(err)
				}
				mid := e.record(m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8InstallApplied())
				if mid.State != domain.ProbeActivationCancelRequested || mid.Facts != m8ActFacts(m8InstallOp2) {
					t.Fatalf("non-final applied report under intent: %+v", mid)
				}
			})
			t.Run("UnknownOutcomeInterrupts", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.fresh(m8InstallOp1)
				e.begin(m8InstallOp1, m8InstallOwnerA)
				e.advance(m8InstallOp1, m8InstallOwnerA, 1)
				e.reserve(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration)
				a := e.record(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration, m8InstallUnknownOutcome())
				if a.State != domain.ProbeActivationInterrupted || a.ErrorCode != "outcome_unknown" || !a.Terminal() || a.Facts != m8ActFacts(m8InstallOp1) {
					t.Fatalf("unknown outcome must interrupt atomically: %+v", a)
				}
				// No dependent work can ever start afterwards.
				_, _, err := e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationServiceStart, m8ActAttempt(m8InstallOp1, domain.ProbeActivationServiceStart))
				e.conflicts("dependent reserve after uncertainty", err)
				// A late result cannot replace the preserved uncertainty.
				_, err = e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration, m8ActAttempt(m8InstallOp1, domain.ProbeActivationRegistration), m8InstallApplied(), domain.ProbeActivationFacts{})
				e.conflicts("late applied", err)
				// Historical reservations never replay authority.
				retry, granted, err := e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8ActAttempt(m8InstallOp1, domain.ProbeActivationProbeInit))
				if err != nil || granted || m8ActStep(t, retry, domain.ProbeActivationProbeInit).State != domain.ProbeInstallResourceApplied {
					t.Fatalf("historical reservation retry: created=%v err=%v", granted, err)
				}
				// Terminal history is immutable to every outcome.
				_, err = e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallFailed, "internal"))
				e.conflicts("finish over interruption", err)
				canceled, err := e.store.RequestActivationCancellation(ctx, m8InstallOp1)
				if err != nil {
					t.Fatal(err)
				}
				m8ActRequireSame(t, m8ActCheck(t, canceled), a, "cancel over interruption")
				// Unknown on the first step leaves no facts.
				e.fresh(m8InstallOp2)
				e.begin(m8InstallOp2, m8InstallOwnerA)
				e.reserve(m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				b := e.record(m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8InstallUnknownOutcome())
				if b.State != domain.ProbeActivationInterrupted || b.Facts != (domain.ProbeActivationFacts{}) {
					t.Fatalf("unknown init: %+v", b)
				}
			})
			t.Run("OwnerInterruptionAndFailure", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				// Owner interruption converts the in-flight step to unknown.
				e.fresh(m8InstallOp1)
				e.begin(m8InstallOp1, m8InstallOwnerA)
				e.advance(m8InstallOp1, m8InstallOwnerA, 2)
				e.reserve(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationServiceStart)
				a, err := e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallInterrupted, "owner_interrupted"))
				if err != nil {
					t.Fatalf("owner interruption: %v", err)
				}
				m8ActCheck(t, a)
				open := m8ActStep(t, a, domain.ProbeActivationServiceStart)
				if a.State != domain.ProbeActivationInterrupted || a.ErrorCode != "owner_interrupted" ||
					open.State != domain.ProbeInstallResourceUnknown || open.ErrorCode != "owner_interrupted" {
					t.Fatalf("owner interruption shape: %+v", a)
				}
				if applied := m8ActStep(t, a, domain.ProbeActivationRegistration); applied.State != domain.ProbeInstallResourceApplied {
					t.Fatalf("historical applied step disturbed: %+v", applied)
				}
				retry, err := e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallInterrupted, "owner_interrupted"))
				if err != nil {
					t.Fatalf("exact interruption retry: %v", err)
				}
				m8ActRequireSame(t, m8ActCheck(t, retry), a, "exact interruption retry")
				_, err = e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallFailed, "internal"))
				e.conflicts("late failure over interruption", err)
				// Every failure code the domain allows is storable. Each needs its own
				// activation, so they run over the remaining fixture operations.
				codes := []string{"unsupported_host", "artifact_rejected", "resource_conflict", "execution_failed", "permission_denied", "runtime_unverified", "not_started", "internal"}
				for i, code := range codes {
					op := m8ActOps[i+1]
					e.fresh(op)
					e.begin(op, m8InstallOwnerA)
					want := domain.ProbeActivationFacts{}
					if code == "internal" {
						// Failure after an applied init keeps its facts.
						e.advance(op, m8InstallOwnerA, 1)
						want = m8ActFacts(op)
					}
					failed, err := e.store.FinishActivation(ctx, op, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallFailed, code))
					if err != nil || failed.State != domain.ProbeActivationFailed || failed.ErrorCode != code || failed.Facts != want {
						t.Fatalf("failed/%s: %+v err=%v", code, failed, err)
					}
					m8ActCheck(t, failed)
				}
			})
			t.Run("ColdExpiryMakesInFlightUnknown", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.fresh(m8InstallOp1)
				e.begin(m8InstallOp1, m8InstallOwnerA)
				e.advance(m8InstallOp1, m8InstallOwnerA, 2)
				e.reserve(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationServiceStart)
				m8ActExpire(t, e.f, m8InstallOp1)
				observed := e.observe(m8InstallOp1)
				if observed.State != domain.ProbeActivationInterrupted || observed.ErrorCode != "deadline_expired" ||
					observed.UpdatedAt.Before(observed.DeadlineAt) || observed.Facts != m8ActFacts(m8InstallOp1) {
					t.Fatalf("cold expiry interruption shape: %+v", observed)
				}
				open := m8ActStep(t, observed, domain.ProbeActivationServiceStart)
				if open.State != domain.ProbeInstallResourceUnknown || open.ErrorCode != "deadline_expired" || open.FinishedAt.Before(observed.DeadlineAt) {
					t.Fatalf("in-flight step not honestly expired: %+v", open)
				}
				if applied := m8ActStep(t, observed, domain.ProbeActivationProbeInit); applied.State != domain.ProbeInstallResourceApplied {
					t.Fatalf("historical applied step must survive: %+v", applied)
				}
				if planned := m8ActStep(t, observed, domain.ProbeActivationEnrollment); planned.State != domain.ProbeInstallResourcePlanned {
					t.Fatalf("planned step was fabricated: %+v", planned)
				}
				// Late results cannot repair the expired uncertainty.
				_, err := e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationServiceStart, m8ActAttempt(m8InstallOp1, domain.ProbeActivationServiceStart), m8InstallApplied(), domain.ProbeActivationFacts{})
				e.conflicts("late applied after expiry", err)
				// An expired reservation retry reads history and never grants.
				retry, granted, err := e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationServiceStart, m8ActAttempt(m8InstallOp1, domain.ProbeActivationServiceStart))
				if err != nil || granted {
					t.Fatalf("expired reservation retry: created=%v err=%v", granted, err)
				}
				m8ActRequireSame(t, m8ActCheck(t, retry), observed, "expired reservation retry")
				// Cold expiry of never-started work never fabricates execution.
				e.fresh(m8InstallOp2)
				e.begin(m8InstallOp2, m8InstallOwnerA)
				m8ActExpire(t, e.f, m8InstallOp2)
				idle := e.observe(m8InstallOp2)
				if idle.State != domain.ProbeActivationInterrupted || idle.ErrorCode != "deadline_expired" {
					t.Fatalf("idle expiry: %+v", idle)
				}
				for _, step := range idle.Steps {
					if step.State != domain.ProbeInstallResourcePlanned {
						t.Fatalf("expiry fabricated execution: %+v", step)
					}
				}
				// A queued activation that expires before begin is interrupted too.
				e.fresh(m8InstallOp3)
				m8ActExpire(t, e.f, m8InstallOp3)
				queued := e.observe(m8InstallOp3)
				if queued.State != domain.ProbeActivationInterrupted || queued.ErrorCode != "deadline_expired" {
					t.Fatalf("queued expiry: %+v", queued)
				}
				// A cancel_requested activation that expires is interrupted, not canceled.
				e.fresh(m8InstallOp4)
				e.begin(m8InstallOp4, m8InstallOwnerA)
				e.reserve(m8InstallOp4, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				if _, err := e.store.RequestActivationCancellation(ctx, m8InstallOp4); err != nil {
					t.Fatal(err)
				}
				m8ActExpire(t, e.f, m8InstallOp4)
				cancelled := e.observe(m8InstallOp4)
				if cancelled.State != domain.ProbeActivationInterrupted || cancelled.ErrorCode != "deadline_expired" ||
					m8ActStep(t, cancelled, domain.ProbeActivationProbeInit).State != domain.ProbeInstallResourceUnknown {
					t.Fatalf("cancel_requested expiry: %+v", cancelled)
				}
			})
			t.Run("ExpiryReconciliationCommitsThroughRefusal", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.fresh(m8InstallOp1)
				e.begin(m8InstallOp1, m8InstallOwnerA)
				e.reserve(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				m8ActExpire(t, e.f, m8InstallOp1)
				_, err := e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerB, m8ActFinish(domain.ProbeInstallInterrupted, "owner_interrupted"))
				e.conflicts("late foreign owner interruption", err)
				raw := m8ActRawRow(t, e.f, m8InstallOp1)
				if raw.State != domain.ProbeActivationInterrupted || raw.ErrorCode == nil || *raw.ErrorCode != "deadline_expired" {
					t.Fatalf("reconciliation did not commit through the conflict: %+v", raw)
				}
				if steps := m8ActRawSteps(t, e.f, m8InstallOp1); steps[0].State != domain.ProbeInstallResourceUnknown || steps[0].ErrorCode == nil || *steps[0].ErrorCode != "deadline_expired" {
					t.Fatalf("in-flight step not reconciled through the conflict: %+v", steps[0])
				}
				// A refused fresh grant after expiry keeps the reconciliation too.
				e.fresh(m8InstallOp2)
				e.begin(m8InstallOp2, m8InstallOwnerA)
				m8ActExpire(t, e.f, m8InstallOp2)
				if _, granted, err := e.store.ReserveActivationStep(ctx, m8InstallOp2, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8ActAttempt(m8InstallOp2, domain.ProbeActivationProbeInit)); granted || !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("expired reserve: created=%v err=%v", granted, err)
				}
				if raw := m8ActRawRow(t, e.f, m8InstallOp2); raw.State != domain.ProbeActivationInterrupted || raw.ErrorCode == nil || *raw.ErrorCode != "deadline_expired" {
					t.Fatalf("reconciliation behind refused reserve: %+v", raw)
				}
			})
			t.Run("InstallExpiryRefusesCreateAndReconciles", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.installReady(m8InstallOp1)
				m8InstallExpire(t, e.f, m8InstallOp1)
				if _, created, err := e.store.CreateActivation(ctx, m8ActIdentity(m8InstallOp1), m8InstallOwnerA); created || !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("hard-expired install must refuse activation: created=%v err=%v", created, err)
				}
				// The honest install expiry commits even though the create is refused.
				raw := m8InstallRawOp(t, e.f, m8InstallOp1)
				if raw.State != domain.ProbeInstallInterrupted || raw.ErrorCode == nil || *raw.ErrorCode != "deadline_expired" {
					t.Fatalf("install expiry reconciliation did not commit: %+v", raw)
				}
				if n := m8ActCount(t, e.f, "probe_install_activations"); n != 0 {
					t.Fatalf("expired install left %d activation rows", n)
				}
			})
			t.Run("InstallLifecycleDoesNotBreakActivation", func(t *testing.T) {
				// Only creation requires resources_ready. Later install-journal
				// transitions (cancellation intent, terminal outcomes, hard expiry)
				// leave the activation readable and valid.
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				istore := m8InstallStore(e.f)
				e.fresh(m8InstallOp1)
				before := e.observe(m8InstallOp1)
				if _, err := istore.RequestInstallCancellation(ctx, m8InstallOp1); err != nil {
					t.Fatal(err)
				}
				m8ActRequireSame(t, e.observe(m8InstallOp1), before, "install cancel intent")
				if _, err := istore.FinishInstall(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallCanceled, "canceled")); err != nil {
					t.Fatal(err)
				}
				m8ActRequireSame(t, e.observe(m8InstallOp1), before, "install canceled")
				e.fresh(m8InstallOp2)
				m8InstallExpire(t, e.f, m8InstallOp2)
				if op, err := istore.ObserveInstall(ctx, m8InstallOp2); err != nil || op.State != domain.ProbeInstallInterrupted {
					t.Fatalf("install expiry: %+v err=%v", op, err)
				}
				// The activation keeps its own clock and begins normally.
				e.begin(m8InstallOp2, m8InstallOwnerA)
			})
			t.Run("CorruptGraphRefusalBeforeReconcile", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				// Schema-valid but domain-invalid: facts without an applied init.
				e.fresh(m8InstallOp1)
				e.begin(m8InstallOp1, m8InstallOwnerA)
				facts := m8ActFacts(m8InstallOp1)
				if _, err := e.f.db.ExecContext(ctx, "UPDATE probe_install_activations SET probe_id = ?, stream_id = ?, certificate_fingerprint = ? WHERE operation_id = ?",
					facts.ProbeID, facts.StreamID, facts.CertificateFingerprint, m8InstallOp1); err != nil {
					t.Fatalf("corrupt facts: %v", err)
				}
				m8ActExpire(t, e.f, m8InstallOp1)
				corrupted := m8ActSnapshot(t, e.f, m8InstallOp1)
				if _, err := e.store.ObserveActivation(ctx, m8InstallOp1); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("facts without applied init must be refused, got %v", err)
				}
				if _, err := e.store.BeginActivation(ctx, m8InstallOp1, m8InstallOwnerA); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("begin over a corrupt graph must be refused, got %v", err)
				}
				// Nothing was reconciled or repaired behind the refusals.
				if after := m8ActSnapshot(t, e.f, m8InstallOp1); after != corrupted {
					t.Fatalf("refusal mutated the corrupt graph:\nbefore %s\nafter  %s", corrupted, after)
				}
				// Applied init whose facts were erased.
				e.fresh(m8InstallOp2)
				e.begin(m8InstallOp2, m8InstallOwnerA)
				e.advance(m8InstallOp2, m8InstallOwnerA, 1)
				if _, err := e.f.db.ExecContext(ctx, "UPDATE probe_install_activations SET probe_id = NULL, stream_id = NULL, certificate_fingerprint = NULL WHERE operation_id = ?", m8InstallOp2); err != nil {
					t.Fatalf("erase facts: %v", err)
				}
				if _, err := e.store.ObserveActivation(ctx, m8InstallOp2); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("applied init without facts must be refused, got %v", err)
				}
				// A tampered canonical reference.
				e.fresh(m8InstallOp3)
				if _, err := e.f.db.ExecContext(ctx, "UPDATE probe_install_activation_steps SET reference = ? WHERE operation_id = ? AND kind = ?", "/foreign", m8InstallOp3, domain.ProbeActivationProbeInit); err != nil {
					t.Fatalf("tamper reference: %v", err)
				}
				if _, err := e.store.ObserveActivation(ctx, m8InstallOp3); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("tampered reference must be refused, got %v", err)
				}
				// An attempt that claims to start before the activation existed.
				e.fresh(m8InstallOp4)
				e.begin(m8InstallOp4, m8InstallOwnerA)
				e.reserve(m8InstallOp4, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				raw := m8ActRawRow(t, e.f, m8InstallOp4)
				if _, err := e.f.db.ExecContext(ctx, "UPDATE probe_install_activation_steps SET started_at = ? WHERE operation_id = ? AND kind = ?", raw.CreatedAt.Add(-time.Hour), m8InstallOp4, domain.ProbeActivationProbeInit); err != nil {
					t.Fatalf("corrupt started_at: %v", err)
				}
				if _, err := e.store.ObserveActivation(ctx, m8InstallOp4); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("pre-creation attempt must be refused, got %v", err)
				}
				// An unknown step that disagrees with the activation's error code.
				e.fresh(m8InstallOp5)
				e.begin(m8InstallOp5, m8InstallOwnerA)
				e.reserve(m8InstallOp5, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				e.record(m8InstallOp5, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8InstallUnknownOutcome())
				if _, err := e.f.db.ExecContext(ctx, "UPDATE probe_install_activation_steps SET error_code = 'owner_interrupted' WHERE operation_id = ? AND kind = ?", m8InstallOp5, domain.ProbeActivationProbeInit); err != nil {
					t.Fatalf("corrupt unknown code: %v", err)
				}
				if _, err := e.store.ObserveActivation(ctx, m8InstallOp5); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("mismatched unknown code must be refused, got %v", err)
				}
				// A corrupt install graph under a healthy activation is refused too.
				e.fresh(m8InstallOp6)
				if _, err := e.f.db.ExecContext(ctx, "UPDATE probe_install_operations SET admission_id = ? WHERE operation_id = ?", strings.Repeat("z", 36), m8InstallOp6); err != nil {
					t.Fatalf("corrupt install admission: %v", err)
				}
				if _, err := e.store.ObserveActivation(ctx, m8InstallOp6); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("corrupt install graph must be refused, got %v", err)
				}
			})
			t.Run("ConflictRollsBack", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.fresh(m8InstallOp1)
				e.begin(m8InstallOp1, m8InstallOwnerA)
				e.reserve(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				before := m8ActSnapshot(t, e.f, m8InstallOp1)
				// Every refused transition leaves the journal byte-identical.
				_, err := e.store.FinishActivation(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallFailed, "internal"))
				e.conflicts("finish with in-flight step", err)
				_, _, err = e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration, m8ActAttempt(m8InstallOp1, domain.ProbeActivationRegistration))
				e.conflicts("reserve behind open frontier", err)
				_, err = e.store.RecordActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8ActForeignAttempt, m8InstallApplied(), m8ActFacts(m8InstallOp1))
				e.conflicts("record with foreign attempt", err)
				_, err = e.store.BeginActivation(ctx, m8InstallOp1, m8InstallOwnerA)
				e.conflicts("begin over running", err)
				_, _, err = e.store.CreateActivation(ctx, func() domain.ProbeActivationIdentity {
					changed := m8ActIdentity(m8InstallOp1)
					changed.Name = "Renamed"
					return changed
				}(), m8InstallOwnerA)
				e.conflicts("changed-identity create", err)
				if after := m8ActSnapshot(t, e.f, m8InstallOp1); after != before {
					t.Fatalf("conflicts rolled forward state:\nbefore %s\nafter  %s", before, after)
				}
			})
			t.Run("GlobalAttemptIdentity", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.fresh(m8InstallOp1)
				e.begin(m8InstallOp1, m8InstallOwnerA)
				e.installReady(m8InstallOp2)
				e.create(m8InstallOp2, m8InstallOwnerB)
				e.begin(m8InstallOp2, m8InstallOwnerB)
				e.reserve(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit)
				// Attempt UUIDs are unique across the whole activation journal.
				_, _, err := e.store.ReserveActivationStep(ctx, m8InstallOp2, m8InstallOwnerB, domain.ProbeActivationProbeInit, m8ActAttempt(m8InstallOp1, domain.ProbeActivationProbeInit))
				e.conflicts("cross-operation attempt reuse", err)
				e.record(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, m8InstallApplied())
				_, _, err = e.store.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration, m8ActAttempt(m8InstallOp1, domain.ProbeActivationProbeInit))
				e.conflicts("in-graph attempt reuse", err)
				if raw := m8ActRawSteps(t, e.f, m8InstallOp2); raw[0].State != domain.ProbeInstallResourcePlanned {
					t.Fatalf("a refused foreign attempt reserved a step: %+v", raw[0])
				}
				e.reserve(m8InstallOp2, m8InstallOwnerB, domain.ProbeActivationProbeInit)
			})
			t.Run("UTCAndClockRollback", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.fresh(m8InstallOp1)
				begun := e.begin(m8InstallOp1, m8InstallOwnerA)
				deadline := begun.DeadlineAt
				e.advance(m8InstallOp1, m8InstallOwnerA, 1)
				// A rewound DB clock never rewinds stored updated stamps.
				future := deadline.Add(-time.Minute)
				if _, err := e.f.db.ExecContext(ctx, "UPDATE probe_install_activations SET updated_at = ? WHERE operation_id = ?", future, m8InstallOp1); err != nil {
					t.Fatalf("pin future updated_at: %v", err)
				}
				a := e.reserve(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration)
				if !a.UpdatedAt.Equal(future) {
					t.Fatalf("updated stamp rewound under clock rollback: %v", a.UpdatedAt)
				}
				e.record(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationRegistration, m8InstallApplied())
				// Causal clamp: a successor may never start before its predecessor
				// finished, even when the clock rewound past it.
				predecessorFinish := deadline.Add(-2 * time.Minute)
				if _, err := e.f.db.ExecContext(ctx, "UPDATE probe_install_activation_steps SET finished_at = ? WHERE operation_id = ? AND kind = ?",
					predecessorFinish, m8InstallOp1, domain.ProbeActivationRegistration); err != nil {
					t.Fatalf("pin predecessor finish: %v", err)
				}
				a = e.reserve(m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationServiceStart)
				if started := m8ActStep(t, a, domain.ProbeActivationServiceStart).StartedAt; !started.Equal(predecessorFinish) {
					t.Fatalf("successor start is not causal: %v want %v", started, predecessorFinish)
				}
				// Reads are never skewed by the server zone: every stored stamp is
				// a UTC microsecond value strictly inside the fixed lifetime.
				cold := e.observe(m8InstallOp1)
				if !cold.DeadlineAt.Equal(cold.CreatedAt.Add(domain.ProbeActivationLifetime)) || !cold.UpdatedAt.Before(cold.DeadlineAt) {
					t.Fatalf("lifetime contract: %+v", cold)
				}
			})
			t.Run("ConcurrentCreateBeginReserveSingleWinner", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				ctx := t.Context()
				e.installReady(m8InstallOp1)
				extra := ""
				if engine == "mariadb" {
					extra = "&clientFoundRows=true"
				}
				storeA := e.store
				storeB := repository.NewProbeInstallActivationStore(m8InstallSecondDB(t, e.f, extra))
				stores := []*repository.ProbeInstallActivationStore{storeA, storeB}
				identity := m8ActIdentity(m8InstallOp1)
				type result struct {
					a       *domain.ProbeActivation
					granted bool
					err     error
				}
				race := func(call func(i int, s *repository.ProbeInstallActivationStore) result) []result {
					results := make([]result, len(stores))
					start := make(chan struct{})
					var wg sync.WaitGroup
					for i, s := range stores {
						wg.Add(1)
						go func() {
							defer wg.Done()
							<-start
							results[i] = call(i, s)
						}()
					}
					close(start)
					wg.Wait()
					return results
				}
				creates := race(func(_ int, s *repository.ProbeInstallActivationStore) result {
					a, created, err := s.CreateActivation(ctx, identity, m8InstallOwnerA)
					return result{a, created, err}
				})
				fresh := 0
				var winner *domain.ProbeActivation
				for _, r := range creates {
					if r.err != nil {
						t.Fatalf("concurrent create: %v", r.err)
					}
					if r.granted {
						fresh++
						winner = r.a
					}
				}
				if fresh != 1 || winner == nil {
					t.Fatalf("exactly one fresh creation required, got %d", fresh)
				}
				for _, r := range creates {
					m8ActRequireSame(t, m8ActCheck(t, r.a), winner, "concurrent create receipt")
				}
				begins := race(func(_ int, s *repository.ProbeInstallActivationStore) result {
					a, err := s.BeginActivation(ctx, m8InstallOp1, m8InstallOwnerA)
					return result{a, false, err}
				})
				won := 0
				for _, r := range begins {
					switch {
					case r.err == nil:
						won++
					case !errors.Is(r.err, ports.ErrConflict):
						t.Fatalf("concurrent begin: %v", r.err)
					}
				}
				if won != 1 {
					t.Fatalf("exactly one begin grant required, got %d", won)
				}
				attempts := []string{m8ActAttempt(m8InstallOp1, domain.ProbeActivationProbeInit), m8ActForeignAttempt}
				reserves := race(func(i int, s *repository.ProbeInstallActivationStore) result {
					a, granted, err := s.ReserveActivationStep(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeActivationProbeInit, attempts[i])
					return result{a, granted, err}
				})
				grants := 0
				for i, r := range reserves {
					switch {
					case r.err == nil && r.granted:
						grants++
					case errors.Is(r.err, ports.ErrConflict):
					default:
						t.Fatalf("concurrent reservation %d: granted=%v err=%v", i, r.granted, r.err)
					}
				}
				if grants != 1 {
					t.Fatalf("exactly one reservation grant required, got %d", grants)
				}
			})
			t.Run("CanceledContextRefused", func(t *testing.T) {
				e := newM8ActEnv(t, engine)
				e.fresh(m8InstallOp1)
				before := m8ActSnapshot(t, e.f, m8InstallOp1)
				canceled, cancel := context.WithCancel(t.Context())
				cancel()
				op, owner := m8InstallOp1, m8InstallOwnerA
				kind := domain.ProbeActivationProbeInit
				calls := map[string]func() error{
					"create": func() error {
						_, _, err := e.store.CreateActivation(canceled, m8ActIdentity(m8InstallOp2), owner)
						return err
					},
					"observe": func() error { _, err := e.store.ObserveActivation(canceled, op); return err },
					"begin":   func() error { _, err := e.store.BeginActivation(canceled, op, owner); return err },
					"cancel":  func() error { _, err := e.store.RequestActivationCancellation(canceled, op); return err },
					"finish": func() error {
						_, err := e.store.FinishActivation(canceled, op, owner, m8ActFinish(domain.ProbeInstallFailed, "internal"))
						return err
					},
					"reserve": func() error {
						_, _, err := e.store.ReserveActivationStep(canceled, op, owner, kind, m8ActAttempt(op, kind))
						return err
					},
					"record": func() error {
						_, err := e.store.RecordActivationStep(canceled, op, owner, kind, m8ActAttempt(op, kind), m8InstallApplied(), m8ActFacts(op))
						return err
					},
				}
				for name, call := range calls {
					if err := call(); !errors.Is(err, context.Canceled) {
						t.Fatalf("%s with a canceled context must report cancellation, got %v", name, err)
					}
				}
				if after := m8ActSnapshot(t, e.f, op); after != before {
					t.Fatalf("canceled calls changed durable history:\nbefore %s\nafter  %s", before, after)
				}
				if n := m8ActCount(t, e.f, "probe_install_activations"); n != 1 {
					t.Fatalf("canceled create left %d activations", n)
				}
				// The same calls succeed with a live context.
				e.begin(op, owner)
			})
		})
	}
}

// TestM8InstallActivationNilStore proves a nil or unbound store fails closed
// without touching any database, so it is not an engine leaf.
func TestM8InstallActivationNilStore(t *testing.T) {
	var nilStore *repository.ProbeInstallActivationStore
	if _, _, err := nilStore.CreateActivation(t.Context(), m8ActIdentity(m8InstallOp1), m8InstallOwnerA); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil store create must fail closed, got %v", err)
	}
	if _, err := nilStore.ObserveActivation(t.Context(), m8InstallOp1); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil store observe must fail closed, got %v", err)
	}
	zero := &repository.ProbeInstallActivationStore{}
	if _, err := zero.BeginActivation(t.Context(), m8InstallOp1, m8InstallOwnerA); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("zero store begin must fail closed, got %v", err)
	}
	if a, err := zero.AdmitActivationAction(t.Context(), domain.ProbeHostActionAdmissionRequest{}); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("zero store admission must fail closed, got %v", err)
	}
}
