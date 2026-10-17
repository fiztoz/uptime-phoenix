package repository_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	m8InstallOp1    = "0a1b2c3d-1111-4111-8111-111111111111"
	m8InstallOp2    = "0a1b2c3d-2222-4222-8222-222222222222"
	m8InstallOp3    = "0a1b2c3d-3333-4333-8333-333333333333"
	m8InstallOp4    = "0a1b2c3d-4444-4444-8444-444444444444"
	m8InstallOp5    = "0a1b2c3d-5555-4555-8555-555555555555"
	m8InstallOp6    = "0a1b2c3d-6666-4666-8666-666666666666"
	m8InstallOp7    = "0a1b2c3d-7777-4777-8777-777777777777"
	m8InstallOp8    = "0a1b2c3d-8888-4888-8888-888888888888"
	m8InstallOp9    = "0a1b2c3d-9999-4999-8999-999999999999"
	m8InstallOwnerA = "11111111-2222-4333-8444-555555555555"
	m8InstallOwnerB = "66666666-7777-4888-8999-aaaaaaaaaaaa"
	m8InstallActor  = int64(4242)
)

// m8InstallAttempts are canonical, globally unique attempt UUIDs per action.
const (
	m8InstallAttempt1 = "aaaa0001-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	m8InstallAttempt2 = "aaaa0002-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	m8InstallAttempt3 = "aaaa0003-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	m8InstallAttempt4 = "aaaa0004-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	m8InstallAttempt5 = "aaaa0005-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	m8InstallAttempt6 = "aaaa0006-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	m8InstallAttempt7 = "aaaa0007-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	m8InstallAttempt8 = "aaaa0008-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
)

// m8InstallFingerprint builds a canonical OpenSSH SHA256 fingerprint so the
// fixtures exercise the real domain validator instead of bypassing it.
func m8InstallFingerprint(seed byte) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{seed}, 32))
}

func m8InstallIdentity(mode, operationID string) domain.ProbeInstallIdentity {
	identity := domain.ProbeInstallIdentity{
		OperationID:          operationID,
		Host:                 domain.ProbeSSHHost{Address: "192.0.2.10:22", User: "phoenix"},
		HostKeyFingerprint:   m8InstallFingerprint('a'),
		ClientKeyFingerprint: m8InstallFingerprint('b'),
		RequestedBy:          m8InstallActor,
		LayoutVersion:        domain.ProbeInstallLayoutVersion,
		Mode:                 mode,
	}
	if mode == domain.ProbeInstallSystemd {
		identity.Binary = domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: strings.Repeat("a", 64), Size: 100}
	} else {
		identity.Image = domain.ProbeImageSelection{
			Pin:            domain.ProbeImagePin{Repository: "ghcr.io/example/probe", Version: "1.2.3", Architecture: "amd64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 100},
			ManifestSHA256: strings.Repeat("b", 64),
			ConfigSHA256:   strings.Repeat("c", 64),
			LayerCount:     1,
		}
	}
	return identity
}

func m8InstallKinds(mode string) []string {
	if mode == domain.ProbeInstallSystemd {
		return []string{"data_directory", "artifact_directory", "unit_file", "service"}
	}
	return []string{"image_cache", "data_directory", "container"}
}

func m8InstallStore(f probeRegistryFixture) *repository.ProbeInstallJournalStore {
	return repository.NewProbeInstallJournalStore(f.db)
}

// m8InstallSecondDB opens one independent connection set against the same
// engine. The extra parameter only ever carries driver options for MariaDB.
func m8InstallSecondDB(t *testing.T, f probeRegistryFixture, extra string) *bun.DB {
	t.Helper()
	var db *bun.DB
	var err error
	if f.engine == "sqlite" {
		db, err = sqlite.NewDB(f.dsn)
	} else {
		db, err = mariadb.NewDB(f.dsn + extra)
	}
	if err != nil {
		t.Fatalf("open second %s connection: %v", f.engine, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// m8InstallRawOpRow is a test-only raw view of the stored operation row, used
// to assert that refused transitions and refused reconciliations changed
// nothing durable.
type m8InstallRawOpRow struct {
	bun.BaseModel `bun:"table:probe_install_operations"`
	OperationID   string    `bun:"operation_id,pk"`
	OwnerID       string    `bun:"owner_id"`
	AdmissionID   string    `bun:"admission_id"`
	State         string    `bun:"state"`
	ErrorCode     *string   `bun:"error_code"`
	CreatedAt     time.Time `bun:"created_at"`
	UpdatedAt     time.Time `bun:"updated_at"`
	DeadlineAt    time.Time `bun:"deadline_at"`
}

// m8InstallRawResourceRow is the test-only raw view of one resource row.
type m8InstallRawResourceRow struct {
	bun.BaseModel `bun:"table:probe_install_resources"`
	OperationID   string     `bun:"operation_id"`
	Ordinal       int        `bun:"ordinal"`
	Kind          string     `bun:"kind"`
	State         string     `bun:"state"`
	AttemptID     *string    `bun:"attempt_id"`
	ErrorCode     *string    `bun:"error_code"`
	StartedAt     *time.Time `bun:"started_at"`
	FinishedAt    *time.Time `bun:"finished_at"`
}

func m8InstallRawOp(t *testing.T, f probeRegistryFixture, operationID string) m8InstallRawOpRow {
	t.Helper()
	row := new(m8InstallRawOpRow)
	if err := f.db.NewSelect().Model(row).Where("operation_id = ?", operationID).Scan(t.Context()); err != nil {
		t.Fatalf("raw read operation %s: %v", operationID, err)
	}
	return *row
}

func m8InstallRawResources(t *testing.T, f probeRegistryFixture, operationID string) []m8InstallRawResourceRow {
	t.Helper()
	var rows []m8InstallRawResourceRow
	if err := f.db.NewSelect().Model(&rows).Where("operation_id = ?", operationID).Order("ordinal ASC").Scan(t.Context()); err != nil {
		t.Fatalf("raw read resources %s: %v", operationID, err)
	}
	return rows
}

// m8InstallSnapshot renders every durable column of one operation so a refused
// transaction can prove it left nothing behind.
func m8InstallSnapshot(t *testing.T, f probeRegistryFixture, operationID string) string {
	t.Helper()
	nullText := func(v *string) string {
		if v == nil {
			return "<nil>"
		}
		return *v
	}
	timeText := func(v *time.Time) string {
		if v == nil {
			return "<nil>"
		}
		return v.String()
	}
	op := m8InstallRawOp(t, f, operationID)
	out := fmt.Sprintf("op state=%s owner=%s admission=%s error=%s created=%s updated=%s deadline=%s",
		op.State, op.OwnerID, op.AdmissionID, nullText(op.ErrorCode),
		op.CreatedAt.String(), op.UpdatedAt.String(), op.DeadlineAt.String())
	for _, res := range m8InstallRawResources(t, f, operationID) {
		out += fmt.Sprintf(" | %s state=%s attempt=%s error=%s started=%s finished=%s",
			res.Kind, res.State, nullText(res.AttemptID), nullText(res.ErrorCode),
			timeText(res.StartedAt), timeText(res.FinishedAt))
	}
	return out
}

// m8InstallClear removes journal rows (children first) between downgrade
// scenarios; the migration guard is evaluated over the whole table.
func m8InstallClear(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	if _, err := f.db.ExecContext(t.Context(), "DELETE FROM probe_install_resources"); err != nil {
		t.Fatalf("clear resources: %v", err)
	}
	if _, err := f.db.ExecContext(t.Context(), "DELETE FROM probe_install_operations"); err != nil {
		t.Fatalf("clear operations: %v", err)
	}
}

// m8InstallExpire shifts every stored stamp of one operation back together so
// the row stays internally consistent (and therefore a valid stored graph)
// while the DB clock is already past its fixed deadline.
func m8InstallExpire(t *testing.T, f probeRegistryFixture, operationID string) {
	t.Helper()
	shift := 2 * time.Hour
	ctx := t.Context()
	op := m8InstallRawOp(t, f, operationID)
	if _, err := f.db.ExecContext(ctx,
		"UPDATE probe_install_operations SET created_at = ?, updated_at = ?, deadline_at = ? WHERE operation_id = ?",
		op.CreatedAt.Add(-shift), op.UpdatedAt.Add(-shift), op.DeadlineAt.Add(-shift), operationID); err != nil {
		t.Fatalf("expire operation %s: %v", operationID, err)
	}
	for _, res := range m8InstallRawResources(t, f, operationID) {
		if res.StartedAt == nil {
			continue
		}
		started := res.StartedAt.Add(-shift)
		var finished any
		if res.FinishedAt != nil {
			finished = res.FinishedAt.Add(-shift)
		}
		if _, err := f.db.ExecContext(ctx,
			"UPDATE probe_install_resources SET started_at = ?, finished_at = ? WHERE operation_id = ? AND ordinal = ?",
			started, finished, operationID, res.Ordinal); err != nil {
			t.Fatalf("expire resource %s/%d: %v", operationID, res.Ordinal, err)
		}
	}
}

// m8InstallSame compares two receipts field by field with time equality.
func m8InstallSame(a, b *domain.ProbeInstallOperation) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Identity != b.Identity || a.OwnerID != b.OwnerID || a.State != b.State || a.ErrorCode != b.ErrorCode {
		return false
	}
	if !a.CreatedAt.Equal(b.CreatedAt) || !a.UpdatedAt.Equal(b.UpdatedAt) || !a.DeadlineAt.Equal(b.DeadlineAt) {
		return false
	}
	if len(a.Resources) != len(b.Resources) {
		return false
	}
	for i := range a.Resources {
		x, y := a.Resources[i], b.Resources[i]
		if x.Spec != y.Spec || x.State != y.State || x.AttemptID != y.AttemptID || x.ErrorCode != y.ErrorCode {
			return false
		}
		if !x.StartedAt.Equal(y.StartedAt) || !x.FinishedAt.Equal(y.FinishedAt) {
			return false
		}
	}
	return true
}

func m8InstallRequireSame(t *testing.T, got, want *domain.ProbeInstallOperation, what string) {
	t.Helper()
	if !m8InstallSame(got, want) {
		t.Fatalf("%s changed the receipt:\n got %+v\nwant %+v", what, got, want)
	}
}

// m8InstallAssertUTC proves every stored stamp crossed the boundary as UTC
// microseconds, the only shape the frozen domain accepts.
func m8InstallAssertUTC(t *testing.T, operation *domain.ProbeInstallOperation) {
	t.Helper()
	check := func(name string, stamp time.Time, allowZero bool) {
		t.Helper()
		if stamp.IsZero() {
			if allowZero {
				return
			}
			t.Fatalf("%s stamp is zero", name)
		}
		if stamp.Location() != time.UTC || stamp.Nanosecond()%1000 != 0 {
			t.Fatalf("%s stamp is not a UTC microsecond value: %v", name, stamp)
		}
	}
	check("created", operation.CreatedAt, false)
	check("updated", operation.UpdatedAt, false)
	check("deadline", operation.DeadlineAt, false)
	for i, resource := range operation.Resources {
		check(fmt.Sprintf("resource %d started", i), resource.StartedAt, true)
		check(fmt.Sprintf("resource %d finished", i), resource.FinishedAt, true)
	}
}

func m8InstallResourceOf(t *testing.T, operation *domain.ProbeInstallOperation, kind string) domain.ProbeInstallResource {
	t.Helper()
	for _, resource := range operation.Resources {
		if resource.Spec.Kind == kind {
			return resource
		}
	}
	t.Fatalf("resource %s missing from %+v", kind, operation.Resources)
	return domain.ProbeInstallResource{}
}

// m8InstallCreate admits one operation and asserts the queued shape.
func m8InstallCreate(t *testing.T, store *repository.ProbeInstallJournalStore, identity domain.ProbeInstallIdentity, ownerID string) *domain.ProbeInstallOperation {
	t.Helper()
	operation, created, err := store.CreateInstall(t.Context(), identity, ownerID)
	if err != nil || !created {
		t.Fatalf("create %s: created=%v err=%v", identity.OperationID, created, err)
	}
	if operation.State != domain.ProbeInstallQueued || operation.OwnerID != ownerID ||
		!operation.DeadlineAt.Equal(operation.CreatedAt.Add(domain.ProbeInstallLifetime)) {
		t.Fatalf("queued admission shape: %+v", operation)
	}
	m8InstallAssertUTC(t, operation)
	if !operation.Valid() {
		t.Fatalf("admitted graph is invalid: %+v", operation)
	}
	return operation
}

func m8InstallBeginOK(t *testing.T, store *repository.ProbeInstallJournalStore, operationID, ownerID string) *domain.ProbeInstallOperation {
	t.Helper()
	operation, err := store.BeginInstall(t.Context(), operationID, ownerID)
	if err != nil || operation == nil || operation.State != domain.ProbeInstallRunning {
		t.Fatalf("begin %s: %+v err=%v", operationID, operation, err)
	}
	m8InstallAssertUTC(t, operation)
	return operation
}

func m8InstallReserveOK(t *testing.T, store *repository.ProbeInstallJournalStore, operationID, ownerID, kind, attemptID string) *domain.ProbeInstallOperation {
	t.Helper()
	operation, created, err := store.ReserveInstallResource(t.Context(), operationID, ownerID, kind, attemptID)
	if err != nil || !created {
		t.Fatalf("reserve %s/%s: created=%v err=%v", operationID, kind, created, err)
	}
	resource := m8InstallResourceOf(t, operation, kind)
	if resource.State != domain.ProbeInstallResourceInFlight || resource.AttemptID != attemptID {
		t.Fatalf("fresh reservation is not a committed in-flight intent: %+v", resource)
	}
	m8InstallAssertUTC(t, operation)
	return operation
}

func m8InstallRecordOK(t *testing.T, store *repository.ProbeInstallJournalStore, operationID, ownerID, kind, attemptID string, outcome domain.ProbeInstallResourceOutcome) *domain.ProbeInstallOperation {
	t.Helper()
	operation, err := store.RecordInstallResource(t.Context(), operationID, ownerID, kind, attemptID, outcome)
	if err != nil {
		t.Fatalf("record %s/%s %s: %v", operationID, kind, outcome.State, err)
	}
	resource := m8InstallResourceOf(t, operation, kind)
	if resource.State != outcome.State || resource.ErrorCode != outcome.ErrorCode {
		t.Fatalf("recorded outcome mismatch: %+v want %+v", resource, outcome)
	}
	m8InstallAssertUTC(t, operation)
	return operation
}

func m8InstallApplied() domain.ProbeInstallResourceOutcome {
	return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}
}

func m8InstallNotStartedOutcome() domain.ProbeInstallResourceOutcome {
	return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}
}

func m8InstallUnknownOutcome() domain.ProbeInstallResourceOutcome {
	return domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceUnknown, ErrorCode: "outcome_unknown"}
}

// TestM8InstallJournalStore proves the durable M8 installation intent and
// resource journal through the real production fixtures on both engines:
// atomic graph admission, immutable exact retries, committed resource
// prerequisites, globally unique attempt identity, DB-clock owner/state/expiry
// fencing after the row lock, honest cold-expiry interruption, cancellation
// intent versus joined outcomes, corrupt-graph refusal before reconciliation,
// commit-versus-rollback discipline, guarded downgrade and UTC/monotonic
// stamps. MariaDB requires TEST_MARIADB_DSN; main owns the gate that fails
// closed when the expected real-engine execution is absent.
func TestM8InstallJournalStore(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("AtomicCreateAndImmutableRetry", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				store := m8InstallStore(f)
				identity := m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1)
				operation := m8InstallCreate(t, store, identity, m8InstallOwnerA)
				specs := identity.Resources()
				if len(operation.Resources) != len(specs) {
					t.Fatalf("atomic creation lost resources: %+v", operation.Resources)
				}
				for i, spec := range specs {
					resource := operation.Resources[i]
					if resource.Spec != spec || resource.State != domain.ProbeInstallResourcePlanned ||
						resource.AttemptID != "" || resource.ErrorCode != "" ||
						!resource.StartedAt.IsZero() || !resource.FinishedAt.IsZero() {
						t.Fatalf("planned resource %d malformed: %+v", i, resource)
					}
				}
				// Commit-before-return: a cold read sees the complete graph.
				seen, err := store.ObserveInstall(ctx, m8InstallOp1)
				if err != nil {
					t.Fatalf("committed read: %v", err)
				}
				m8InstallRequireSame(t, seen, operation, "commit-before-return read")
				// Exact identity retry from a different owner keeps the original
				// owner, deadline and history and never replenishes authority.
				again, created, err := store.CreateInstall(ctx, identity, m8InstallOwnerB)
				if err != nil || created {
					t.Fatalf("exact retry: created=%v err=%v", created, err)
				}
				m8InstallRequireSame(t, again, operation, "exact cross-owner retry")
				// Every immutable identity member is bound.
				changed := map[string]domain.ProbeInstallIdentity{}
				mutate := func(name string, fn func(*domain.ProbeInstallIdentity)) {
					next := identity
					fn(&next)
					changed[name] = next
				}
				mutate("address", func(i *domain.ProbeInstallIdentity) { i.Host.Address = "192.0.2.11:22" })
				mutate("user", func(i *domain.ProbeInstallIdentity) { i.Host.User = "other" })
				mutate("host_key", func(i *domain.ProbeInstallIdentity) { i.HostKeyFingerprint = m8InstallFingerprint('c') })
				mutate("client_key", func(i *domain.ProbeInstallIdentity) { i.ClientKeyFingerprint = m8InstallFingerprint('d') })
				mutate("actor", func(i *domain.ProbeInstallIdentity) { i.RequestedBy++ })
				mutate("mode", func(i *domain.ProbeInstallIdentity) {
					*i = m8InstallIdentity(domain.ProbeInstallDocker, m8InstallOp1)
				})
				mutate("binary_version", func(i *domain.ProbeInstallIdentity) { i.Binary.Version = "1.2.4" })
				mutate("binary_arch", func(i *domain.ProbeInstallIdentity) { i.Binary.Architecture = "arm64" })
				mutate("binary_sha", func(i *domain.ProbeInstallIdentity) { i.Binary.SHA256 = strings.Repeat("b", 64) })
				mutate("binary_size", func(i *domain.ProbeInstallIdentity) { i.Binary.Size = 101 })
				for name, request := range changed {
					if _, _, err := store.CreateInstall(ctx, request, m8InstallOwnerA); !errors.Is(err, ports.ErrConflict) {
						t.Fatalf("changed %s must conflict, got %v", name, err)
					}
				}
				// A changed layout version is refused by the frozen domain before
				// storage is ever consulted.
				wrongLayout := identity
				wrongLayout.LayoutVersion++
				if _, _, err := store.CreateInstall(ctx, wrongLayout, m8InstallOwnerA); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("wrong layout must fail validation, got %v", err)
				}
				stored, err := store.ObserveInstall(ctx, m8InstallOp1)
				if err != nil {
					t.Fatalf("read after conflicts: %v", err)
				}
				m8InstallRequireSame(t, stored, operation, "conflicting retries")
				// Invalid requests never reach the journal.
				if _, _, err := store.CreateInstall(ctx, domain.ProbeInstallIdentity{}, m8InstallOwnerA); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("invalid identity must fail validation, got %v", err)
				}
				if _, _, err := store.CreateInstall(ctx, identity, "not-a-uuid"); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("invalid owner must fail validation, got %v", err)
				}
				// Docker layout: the shared image cache is the exact selected
				// manifest reference and is never owned.
				dockerIdentity := m8InstallIdentity(domain.ProbeInstallDocker, m8InstallOp2)
				docker := m8InstallCreate(t, store, dockerIdentity, m8InstallOwnerA)
				dockerSpecs := dockerIdentity.Resources()
				if len(docker.Resources) != 3 || docker.Resources[0].Spec != dockerSpecs[0] ||
					docker.Resources[0].Spec.Owned || docker.Resources[0].Spec.Reference != dockerIdentity.Image.Reference() ||
					!docker.Resources[1].Spec.Owned {
					t.Fatalf("docker graph shape: %+v", docker.Resources)
				}
				// Image tuple members are immutable too.
				imageChanged := dockerIdentity
				imageChanged.Image.ManifestSHA256 = strings.Repeat("f", 64)
				if _, _, err := store.CreateInstall(ctx, imageChanged, m8InstallOwnerA); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("changed image manifest must conflict, got %v", err)
				}
				if engine == "mariadb" {
					// clientFoundRows=true changes driver rows-affected
					// reporting; fresh creation is decided by the stored
					// admission UUID, so behavior must be identical.
					other := repository.NewProbeInstallJournalStore(m8InstallSecondDB(t, f, "&clientFoundRows=true"))
					identity3 := m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp3)
					fresh, created, err := other.CreateInstall(ctx, identity3, m8InstallOwnerA)
					if err != nil || !created {
						t.Fatalf("clientFoundRows fresh create: created=%v err=%v", created, err)
					}
					retry, created, err := other.CreateInstall(ctx, identity3, m8InstallOwnerB)
					if err != nil || created {
						t.Fatalf("clientFoundRows exact retry: created=%v err=%v", created, err)
					}
					m8InstallRequireSame(t, retry, fresh, "clientFoundRows retry")
					m8InstallBeginOK(t, other, m8InstallOp3, m8InstallOwnerA)
					if _, err := other.BeginInstall(ctx, m8InstallOp3, m8InstallOwnerA); !errors.Is(err, ports.ErrConflict) {
						t.Fatalf("clientFoundRows second begin must conflict, got %v", err)
					}
				}
			})
			t.Run("OwnerStateExpiryFences", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				store := m8InstallStore(f)
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				if _, err := store.BeginInstall(ctx, m8InstallOp1, m8InstallOwnerB); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("foreign begin must conflict, got %v", err)
				}
				m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				if _, err := store.BeginInstall(ctx, m8InstallOp1, m8InstallOwnerA); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("second begin must conflict (no second dispatch), got %v", err)
				}
				// Every journal write is fenced by the exact stored owner.
				if _, _, err := store.ReserveInstallResource(ctx, m8InstallOp1, m8InstallOwnerB, "data_directory", m8InstallAttempt1); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("foreign reserve must conflict, got %v", err)
				}
				if _, err := store.RecordInstallResource(ctx, m8InstallOp1, m8InstallOwnerB, "data_directory", m8InstallAttempt1, m8InstallApplied()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("foreign record must conflict, got %v", err)
				}
				if _, err := store.FinishInstall(ctx, m8InstallOp1, m8InstallOwnerB, domain.ProbeInstallOutcome{State: domain.ProbeInstallFailed, ErrorCode: "internal"}); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("foreign finish must conflict, got %v", err)
				}
				// Queued work has no joined executor and cannot be finished.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp2), m8InstallOwnerA)
				if _, err := store.FinishInstall(ctx, m8InstallOp2, m8InstallOwnerA, domain.ProbeInstallOutcome{State: domain.ProbeInstallFailed, ErrorCode: "internal"}); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("finish before begin must conflict, got %v", err)
				}
				// Record requires the exact stored attempt, not just a kind.
				if _, err := store.RecordInstallResource(ctx, m8InstallOp2, m8InstallOwnerA, "data_directory", m8InstallAttempt1, m8InstallApplied()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("record without reservation must conflict, got %v", err)
				}
				// A canonical kind this layout does not contain is missing.
				if _, _, err := store.ReserveInstallResource(ctx, m8InstallOp2, m8InstallOwnerA, "container", m8InstallAttempt1); !errors.Is(err, ports.ErrNotFound) {
					t.Fatalf("absent kind must be not-found, got %v", err)
				}
				// Hard-expired work refuses the dispatch grant; the honest
				// expiry reconciliation still commits (see ConflictRollsBack).
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp3), m8InstallOwnerA)
				m8InstallExpire(t, f, m8InstallOp3)
				if _, err := store.BeginInstall(ctx, m8InstallOp3, m8InstallOwnerA); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("expired begin must conflict, got %v", err)
				}
				expired, err := store.ObserveInstall(ctx, m8InstallOp3)
				if err != nil || expired.State != domain.ProbeInstallInterrupted || expired.ErrorCode != "deadline_expired" {
					t.Fatalf("expired begin did not reconcile: %+v err=%v", expired, err)
				}
				if m8InstallResourceOf(t, expired, "data_directory").State != domain.ProbeInstallResourcePlanned {
					t.Fatalf("expiry fabricated resource state: %+v", expired.Resources)
				}
			})
			t.Run("CommittedPrerequisiteAndOneAttempt", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				store := m8InstallStore(f)
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				// Queue position is not authority: the second resource may not
				// be reserved before the first one is committed applied.
				if _, _, err := store.ReserveInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "artifact_directory", m8InstallAttempt1); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("successor reserve without predecessor must conflict, got %v", err)
				}
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
				if _, _, err := store.ReserveInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "artifact_directory", m8InstallAttempt2); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("successor reserve with in-flight predecessor must conflict, got %v", err)
				}
				// not_started never unlocks its successors.
				m8InstallRecordOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1, m8InstallNotStartedOutcome())
				if _, _, err := store.ReserveInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "artifact_directory", m8InstallAttempt2); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("successor reserve after not_started predecessor must conflict, got %v", err)
				}
				// Committed applied predecessors unlock exactly one successor.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp2), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp2, m8InstallOwnerA)
				m8InstallReserveOK(t, store, m8InstallOp2, m8InstallOwnerA, "data_directory", m8InstallAttempt3)
				m8InstallRecordOK(t, store, m8InstallOp2, m8InstallOwnerA, "data_directory", m8InstallAttempt3, m8InstallApplied())
				inFlight := m8InstallReserveOK(t, store, m8InstallOp2, m8InstallOwnerA, "artifact_directory", m8InstallAttempt4)
				// One attempt per resource per operation.
				if _, _, err := store.ReserveInstallResource(ctx, m8InstallOp2, m8InstallOwnerA, "artifact_directory", m8InstallAttempt5); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("second attempt on one resource must conflict, got %v", err)
				}
				// Exact reservation retry reads history and grants nothing.
				retry, created, err := store.ReserveInstallResource(ctx, m8InstallOp2, m8InstallOwnerA, "artifact_directory", m8InstallAttempt4)
				if err != nil || created {
					t.Fatalf("exact reservation retry: created=%v err=%v", created, err)
				}
				m8InstallRequireSame(t, retry, inFlight, "exact reservation retry")
				// Duplicate attempt UUIDs are refused anywhere in the graph.
				if _, _, err := store.ReserveInstallResource(ctx, m8InstallOp2, m8InstallOwnerA, "unit_file", m8InstallAttempt3); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("duplicated attempt in one graph must conflict, got %v", err)
				}
				m8InstallRecordOK(t, store, m8InstallOp2, m8InstallOwnerA, "artifact_directory", m8InstallAttempt4, m8InstallApplied())
				m8InstallReserveOK(t, store, m8InstallOp2, m8InstallOwnerA, "unit_file", m8InstallAttempt6)
				m8InstallRecordOK(t, store, m8InstallOp2, m8InstallOwnerA, "unit_file", m8InstallAttempt6, m8InstallApplied())
				m8InstallReserveOK(t, store, m8InstallOp2, m8InstallOwnerA, "service", m8InstallAttempt7)
				operation := m8InstallRecordOK(t, store, m8InstallOp2, m8InstallOwnerA, "service", m8InstallAttempt7, m8InstallApplied())
				if operation.State != domain.ProbeInstallResourcesReady {
					t.Fatalf("last applied must move running to resources_ready: %+v", operation)
				}
			})
			t.Run("ResourceOutcomeIdempotency", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				store := m8InstallStore(f)
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallDocker, m8InstallOp1), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "image_cache", m8InstallAttempt1)
				operation := m8InstallRecordOK(t, store, m8InstallOp1, m8InstallOwnerA, "image_cache", m8InstallAttempt1, m8InstallApplied())
				if operation.State != domain.ProbeInstallRunning {
					t.Fatalf("an applied prefix is not resources_ready: %+v", operation)
				}
				// Exact completed-outcome retries are immutable history.
				retry, err := store.RecordInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "image_cache", m8InstallAttempt1, m8InstallApplied())
				if err != nil {
					t.Fatalf("exact outcome retry: %v", err)
				}
				m8InstallRequireSame(t, retry, operation, "exact completed-outcome retry")
				// Differing outcomes on a completed attempt conflict.
				if _, err := store.RecordInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "image_cache", m8InstallAttempt1, m8InstallNotStartedOutcome()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("differing outcome must conflict, got %v", err)
				}
				if _, err := store.RecordInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "image_cache", m8InstallAttempt1, m8InstallUnknownOutcome()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("late uncertainty must conflict, got %v", err)
				}
				// not_started is immutable the same way.
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt2)
				operation = m8InstallRecordOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt2, m8InstallNotStartedOutcome())
				retry, err = store.RecordInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt2, m8InstallNotStartedOutcome())
				if err != nil {
					t.Fatalf("not_started retry: %v", err)
				}
				m8InstallRequireSame(t, retry, operation, "not_started retry")
				if _, err := store.RecordInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt2, m8InstallApplied()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("not_started may never be repaired to applied, got %v", err)
				}
				// Record validates the exact stored attempt, not just the kind.
				if _, err := store.RecordInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt8, m8InstallApplied()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("foreign attempt must conflict, got %v", err)
				}
			})
			t.Run("UnknownOutcomeStopsDependents", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				store := m8InstallStore(f)
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
				m8InstallRecordOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1, m8InstallApplied())
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "artifact_directory", m8InstallAttempt2)
				operation := m8InstallRecordOK(t, store, m8InstallOp1, m8InstallOwnerA, "artifact_directory", m8InstallAttempt2, m8InstallUnknownOutcome())
				if operation.State != domain.ProbeInstallInterrupted || operation.ErrorCode != "outcome_unknown" || !operation.Terminal() {
					t.Fatalf("unknown outcome must interrupt atomically: %+v", operation)
				}
				// No dependent work can ever start afterwards.
				if _, _, err := store.ReserveInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "unit_file", m8InstallAttempt3); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("dependent reserve after uncertainty must conflict, got %v", err)
				}
				// A late result cannot replace the preserved uncertainty.
				if _, err := store.RecordInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "artifact_directory", m8InstallAttempt2, m8InstallApplied()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("late applied must conflict, got %v", err)
				}
				// Historical reservations never replay authority.
				retry, created, err := store.ReserveInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
				if err != nil || created {
					t.Fatalf("historical reservation retry: created=%v err=%v", created, err)
				}
				if m8InstallResourceOf(t, retry, "data_directory").State != domain.ProbeInstallResourceApplied {
					t.Fatalf("historical receipt mutated: %+v", retry.Resources)
				}
				// Terminal history is immutable to every outcome.
				if _, err := store.FinishInstall(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeInstallOutcome{State: domain.ProbeInstallFailed, ErrorCode: "internal"}); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("finish over terminal must conflict, got %v", err)
				}
				canceled, err := store.RequestInstallCancellation(ctx, m8InstallOp1)
				if err != nil {
					t.Fatalf("cancel over terminal: %v", err)
				}
				m8InstallRequireSame(t, canceled, operation, "cancel over terminal")
			})
			t.Run("CancellationIntentVersusJoinedFinish", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				store := m8InstallStore(f)
				// Queued cancel is terminal canceled: no begin grant ever existed.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				canceled, err := store.RequestInstallCancellation(ctx, m8InstallOp1)
				if err != nil || canceled.State != domain.ProbeInstallCanceled || canceled.ErrorCode != "canceled" || !canceled.Terminal() {
					t.Fatalf("queued cancel: %+v err=%v", canceled, err)
				}
				if _, err := store.BeginInstall(ctx, m8InstallOp1, m8InstallOwnerA); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("begin after queued cancel must conflict, got %v", err)
				}
				// Exact terminal finish retries read immutable history.
				retry, err := store.FinishInstall(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeInstallOutcome{State: domain.ProbeInstallCanceled, ErrorCode: "canceled"})
				if err != nil {
					t.Fatalf("exact terminal finish retry: %v", err)
				}
				m8InstallRequireSame(t, retry, canceled, "exact terminal finish retry")
				// Active cancel is intent only; reservations are already committed
				// work and their reports keep landing while the intent is active.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp2), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp2, m8InstallOwnerA)
				kinds := m8InstallKinds(domain.ProbeInstallSystemd)
				for i := 0; i < len(kinds)-1; i++ {
					attempt := fmt.Sprintf("bbbb000%d-bbbb-4bbb-8bbb-bbbbbbbbbbbb", i+1)
					m8InstallReserveOK(t, store, m8InstallOp2, m8InstallOwnerA, kinds[i], attempt)
					m8InstallRecordOK(t, store, m8InstallOp2, m8InstallOwnerA, kinds[i], attempt, m8InstallApplied())
				}
				finalAttempt := fmt.Sprintf("bbbb000%d-bbbb-4bbb-8bbb-bbbbbbbbbbbb", len(kinds))
				m8InstallReserveOK(t, store, m8InstallOp2, m8InstallOwnerA, kinds[len(kinds)-1], finalAttempt)
				intent, err := store.RequestInstallCancellation(ctx, m8InstallOp2)
				if err != nil || intent.State != domain.ProbeInstallCancelRequested || intent.Terminal() {
					t.Fatalf("active cancel must stay intent: %+v err=%v", intent, err)
				}
				// Cancellation intent remains cancel_requested even if all
				// resources apply afterwards.
				m8InstallRecordOK(t, store, m8InstallOp2, m8InstallOwnerA, kinds[len(kinds)-1], finalAttempt, m8InstallApplied())
				still, err := store.ObserveInstall(ctx, m8InstallOp2)
				if err != nil || still.State != domain.ProbeInstallCancelRequested {
					t.Fatalf("cancellation intent must survive fully applied resources: %+v err=%v", still, err)
				}
				// The joined owner finish turns intent into terminal canceled.
				finished, err := store.FinishInstall(ctx, m8InstallOp2, m8InstallOwnerA, domain.ProbeInstallOutcome{State: domain.ProbeInstallCanceled, ErrorCode: "canceled"})
				if err != nil || finished.State != domain.ProbeInstallCanceled || !finished.Terminal() {
					t.Fatalf("joined cancel finish: %+v err=%v", finished, err)
				}
				// Failure and cancellation refuse unresolved actions.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp3), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp3, m8InstallOwnerA)
				m8InstallReserveOK(t, store, m8InstallOp3, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
				if _, err := store.RequestInstallCancellation(ctx, m8InstallOp3); err != nil {
					t.Fatalf("cancel intent: %v", err)
				}
				if _, err := store.FinishInstall(ctx, m8InstallOp3, m8InstallOwnerA, domain.ProbeInstallOutcome{State: domain.ProbeInstallCanceled, ErrorCode: "canceled"}); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("canceled with in-flight action must conflict, got %v", err)
				}
				if _, err := store.FinishInstall(ctx, m8InstallOp3, m8InstallOwnerA, domain.ProbeInstallOutcome{State: domain.ProbeInstallFailed, ErrorCode: "execution_failed"}); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("failed with in-flight action must conflict, got %v", err)
				}
				m8InstallRecordOK(t, store, m8InstallOp3, m8InstallOwnerA, "data_directory", m8InstallAttempt1, m8InstallNotStartedOutcome())
				resolved, err := store.FinishInstall(ctx, m8InstallOp3, m8InstallOwnerA, domain.ProbeInstallOutcome{State: domain.ProbeInstallFailed, ErrorCode: "not_started"})
				if err != nil || resolved.State != domain.ProbeInstallFailed || resolved.ErrorCode != "not_started" {
					t.Fatalf("resolved failure: %+v err=%v", resolved, err)
				}
				// A joined owner cannot fabricate cancellation without its
				// durable intent. After intent commits, joined finish is valid.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp4), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp4, m8InstallOwnerA)
				outcome := domain.ProbeInstallOutcome{State: domain.ProbeInstallCanceled, ErrorCode: "canceled"}
				if _, err := store.FinishInstall(ctx, m8InstallOp4, m8InstallOwnerA, outcome); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("joined cancel without prior intent must conflict: %v", err)
				}
				if _, err := store.RequestInstallCancellation(ctx, m8InstallOp4); err != nil {
					t.Fatal(err)
				}
				direct, err := store.FinishInstall(ctx, m8InstallOp4, m8InstallOwnerA, outcome)
				if err != nil || direct.State != domain.ProbeInstallCanceled {
					t.Fatalf("joined cancel after intent: %+v err=%v", direct, err)
				}
			})
			t.Run("ColdExpiryMakesInFlightUnknown", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				store := m8InstallStore(f)
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallDocker, m8InstallOp1), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "image_cache", m8InstallAttempt1)
				m8InstallRecordOK(t, store, m8InstallOp1, m8InstallOwnerA, "image_cache", m8InstallAttempt1, m8InstallApplied())
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt2)
				m8InstallExpire(t, f, m8InstallOp1)
				observed, err := store.ObserveInstall(ctx, m8InstallOp1)
				if err != nil {
					t.Fatalf("cold observe: %v", err)
				}
				if observed.State != domain.ProbeInstallInterrupted || observed.ErrorCode != "deadline_expired" ||
					observed.UpdatedAt.Before(observed.DeadlineAt) {
					t.Fatalf("cold expiry interruption shape: %+v", observed)
				}
				open := m8InstallResourceOf(t, observed, "data_directory")
				if open.State != domain.ProbeInstallResourceUnknown || open.ErrorCode != "deadline_expired" ||
					open.FinishedAt.Before(observed.DeadlineAt) {
					t.Fatalf("in-flight action not honestly expired: %+v", open)
				}
				if applied := m8InstallResourceOf(t, observed, "image_cache"); applied.State != domain.ProbeInstallResourceApplied {
					t.Fatalf("historical applied action must survive: %+v", applied)
				}
				if planned := m8InstallResourceOf(t, observed, "container"); planned.State != domain.ProbeInstallResourcePlanned {
					t.Fatalf("planned action was fabricated: %+v", planned)
				}
				// Late results cannot repair the expired uncertainty.
				if _, err := store.RecordInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt2, m8InstallApplied()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("late applied after expiry must conflict, got %v", err)
				}
				// An expired reservation retry reads history and never grants.
				retry, created, err := store.ReserveInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt2)
				if err != nil || created {
					t.Fatalf("expired reservation retry: created=%v err=%v", created, err)
				}
				m8InstallRequireSame(t, retry, observed, "expired reservation retry")
				// Cold expiry of never-started work never fabricates execution.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp2), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp2, m8InstallOwnerA)
				m8InstallExpire(t, f, m8InstallOp2)
				idle, err := store.ObserveInstall(ctx, m8InstallOp2)
				if err != nil || idle.State != domain.ProbeInstallInterrupted || idle.ErrorCode != "deadline_expired" {
					t.Fatalf("idle expiry: %+v err=%v", idle, err)
				}
				for _, resource := range idle.Resources {
					if resource.State != domain.ProbeInstallResourcePlanned {
						t.Fatalf("expiry fabricated execution: %+v", resource)
					}
				}
			})
			t.Run("CorruptGraphRefusalBeforeReconcile", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				store := m8InstallStore(f)
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
				m8InstallExpire(t, f, m8InstallOp1)
				// Schema-valid but domain-invalid: the attempt claims to start
				// after its fixed deadline.
				if _, err := f.db.ExecContext(ctx,
					"UPDATE probe_install_resources SET started_at = ? WHERE operation_id = ? AND kind = ?",
					time.Now().UTC().Add(time.Hour), m8InstallOp1, "data_directory"); err != nil {
					t.Fatalf("corrupt started_at: %v", err)
				}
				corrupted := m8InstallSnapshot(t, f, m8InstallOp1)
				if _, err := store.ObserveInstall(ctx, m8InstallOp1); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("corrupt graph must be refused, got %v", err)
				}
				// Nothing was reconciled or repaired behind the refusal.
				if after := m8InstallSnapshot(t, f, m8InstallOp1); after != corrupted {
					t.Fatalf("refusal mutated the corrupt graph:\nbefore %s\nafter  %s", corrupted, after)
				}
				// A causal impossibility is refused the same way.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp2), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp2, m8InstallOwnerA)
				m8InstallReserveOK(t, store, m8InstallOp2, m8InstallOwnerA, "data_directory", m8InstallAttempt2)
				op := m8InstallRawOp(t, f, m8InstallOp2)
				if _, err := f.db.ExecContext(ctx,
					"UPDATE probe_install_resources SET started_at = ? WHERE operation_id = ? AND kind = ?",
					op.CreatedAt.Add(-time.Hour), m8InstallOp2, "data_directory"); err != nil {
					t.Fatalf("corrupt started_at: %v", err)
				}
				if _, err := store.ObserveInstall(ctx, m8InstallOp2); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("pre-creation attempt must be refused, got %v", err)
				}
				// A corrupt internal admission identity is refused too.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp3), m8InstallOwnerA)
				if _, err := f.db.ExecContext(ctx,
					"UPDATE probe_install_operations SET admission_id = ? WHERE operation_id = ?",
					strings.Repeat("z", 36), m8InstallOp3); err != nil {
					t.Fatalf("corrupt admission: %v", err)
				}
				if _, err := store.ObserveInstall(ctx, m8InstallOp3); !errors.Is(err, domain.ErrValidation) {
					t.Fatalf("corrupt admission must be refused, got %v", err)
				}
			})
			t.Run("ConflictRollsBack", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				store := m8InstallStore(f)
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
				before := m8InstallSnapshot(t, f, m8InstallOp1)
				// Every refused transition leaves the journal byte-identical.
				if _, err := store.FinishInstall(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeInstallOutcome{State: domain.ProbeInstallCanceled, ErrorCode: "canceled"}); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("finish with in-flight must conflict, got %v", err)
				}
				if _, _, err := store.ReserveInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "artifact_directory", m8InstallAttempt2); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("reserve behind open frontier must conflict, got %v", err)
				}
				if _, err := store.RecordInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt2, m8InstallApplied()); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("record with foreign attempt must conflict, got %v", err)
				}
				if after := m8InstallSnapshot(t, f, m8InstallOp1); after != before {
					t.Fatalf("conflicts rolled forward state:\nbefore %s\nafter  %s", before, after)
				}
				// The expiry reconciliation commits even when the caller's
				// transition is then refused with a conflict.
				m8InstallExpire(t, f, m8InstallOp1)
				if _, err := store.FinishInstall(ctx, m8InstallOp1, m8InstallOwnerB, domain.ProbeInstallOutcome{State: domain.ProbeInstallInterrupted, ErrorCode: "owner_interrupted"}); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("late owner interruption must conflict, got %v", err)
				}
				reconciled, err := store.ObserveInstall(ctx, m8InstallOp1)
				if err != nil || reconciled.State != domain.ProbeInstallInterrupted || reconciled.ErrorCode != "deadline_expired" {
					t.Fatalf("reconciliation must commit through the conflict: %+v err=%v", reconciled, err)
				}
				if open := m8InstallResourceOf(t, reconciled, "data_directory"); open.State != domain.ProbeInstallResourceUnknown || open.ErrorCode != "deadline_expired" {
					t.Fatalf("in-flight action not reconciled through the conflict: %+v", open)
				}
				// A refused fresh grant after expiry keeps the reconciliation too.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp2), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp2, m8InstallOwnerA)
				m8InstallExpire(t, f, m8InstallOp2)
				if _, created, err := store.ReserveInstallResource(ctx, m8InstallOp2, m8InstallOwnerA, "data_directory", m8InstallAttempt3); created || !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("expired reserve: created=%v err=%v", created, err)
				}
				after, err := store.ObserveInstall(ctx, m8InstallOp2)
				if err != nil || after.State != domain.ProbeInstallInterrupted || after.ErrorCode != "deadline_expired" {
					t.Fatalf("reconciliation behind refused reserve: %+v err=%v", after, err)
				}
			})
			t.Run("GlobalAttemptIdentity", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				store := m8InstallStore(f)
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp2), m8InstallOwnerB)
				m8InstallBeginOK(t, store, m8InstallOp2, m8InstallOwnerB)
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
				// Attempt UUIDs are unique across the whole journal.
				if _, _, err := store.ReserveInstallResource(ctx, m8InstallOp2, m8InstallOwnerB, "data_directory", m8InstallAttempt1); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("cross-operation attempt reuse must conflict, got %v", err)
				}
				if _, _, err := store.ReserveInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "artifact_directory", m8InstallAttempt1); !errors.Is(err, ports.ErrConflict) {
					t.Fatalf("in-graph attempt reuse must conflict, got %v", err)
				}
				m8InstallReserveOK(t, store, m8InstallOp2, m8InstallOwnerB, "data_directory", m8InstallAttempt2)
			})
			t.Run("DowngradeGuard", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				store := m8InstallStore(f)
				down := func() error {
					return runEngineMigration(t, f.db, engine, "084_probe_install_journal", "down")
				}
				up := func() error {
					return runEngineMigration(t, f.db, engine, "084_probe_install_journal", "up")
				}
				// The resource FK restricts parent deletion.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				if _, err := f.db.ExecContext(ctx, "DELETE FROM probe_install_operations WHERE operation_id = ?", m8InstallOp1); err == nil {
					t.Fatal("parent deletion must be restricted while resources exist")
				}
				m8InstallClear(t, f)
				// Any nonterminal operation refuses the downgrade.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				if err := down(); err == nil {
					t.Fatal("downgrade must refuse nonterminal operations")
				}
				m8InstallClear(t, f)
				// Expired but not yet observed work is still nonterminal.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				m8InstallExpire(t, f, m8InstallOp1)
				if err := down(); err == nil {
					t.Fatal("downgrade must refuse expired unobserved operations")
				}
				m8InstallClear(t, f)
				// In-flight and unknown actions refuse the downgrade.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
				if err := down(); err == nil {
					t.Fatal("downgrade must refuse in-flight actions")
				}
				m8InstallRecordOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1, m8InstallUnknownOutcome())
				if err := down(); err == nil {
					t.Fatal("downgrade must refuse unknown actions")
				}
				m8InstallClear(t, f)
				// Applied OWNED resources refuse the downgrade.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt2)
				m8InstallRecordOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt2, m8InstallApplied())
				if _, err := store.FinishInstall(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeInstallOutcome{State: domain.ProbeInstallFailed, ErrorCode: "execution_failed"}); err != nil {
					t.Fatalf("terminal failure: %v", err)
				}
				if err := down(); err == nil {
					t.Fatal("downgrade must refuse applied owned resources")
				}
				m8InstallClear(t, f)
				// Shared image-cache history alone is removable.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallDocker, m8InstallOp1), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "image_cache", m8InstallAttempt3)
				m8InstallRecordOK(t, store, m8InstallOp1, m8InstallOwnerA, "image_cache", m8InstallAttempt3, m8InstallApplied())
				if _, err := store.FinishInstall(ctx, m8InstallOp1, m8InstallOwnerA, domain.ProbeInstallOutcome{State: domain.ProbeInstallFailed, ErrorCode: "resource_conflict"}); err != nil {
					t.Fatalf("terminal failure: %v", err)
				}
				if err := down(); err != nil {
					t.Fatalf("guarded downgrade of removable history: %v", err)
				}
				if err := up(); err != nil {
					t.Fatalf("guarded re-upgrade: %v", err)
				}
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp2), m8InstallOwnerA)
			})
			t.Run("UTCAndClockRollback", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				store := m8InstallStore(f)
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1), m8InstallOwnerA)
				operation := m8InstallBeginOK(t, store, m8InstallOp1, m8InstallOwnerA)
				deadline := operation.DeadlineAt
				m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1)
				m8InstallRecordOK(t, store, m8InstallOp1, m8InstallOwnerA, "data_directory", m8InstallAttempt1, m8InstallApplied())
				// A rewound DB clock never rewinds stored updated stamps.
				future := deadline.Add(-time.Minute)
				if _, err := f.db.ExecContext(ctx,
					"UPDATE probe_install_operations SET updated_at = ? WHERE operation_id = ?", future, m8InstallOp1); err != nil {
					t.Fatalf("pin future updated_at: %v", err)
				}
				operation = m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "artifact_directory", m8InstallAttempt2)
				if !operation.UpdatedAt.Equal(future) {
					t.Fatalf("updated stamp rewound under clock rollback: %v", operation.UpdatedAt)
				}
				m8InstallRecordOK(t, store, m8InstallOp1, m8InstallOwnerA, "artifact_directory", m8InstallAttempt2, m8InstallApplied())
				// Causal clamp: a successor may never start before its
				// predecessor finished, even when the clock rewound past it.
				predFinish := deadline.Add(-2 * time.Minute)
				if _, err := f.db.ExecContext(ctx,
					"UPDATE probe_install_resources SET finished_at = ? WHERE operation_id = ? AND kind = ?",
					predFinish, m8InstallOp1, "artifact_directory"); err != nil {
					t.Fatalf("pin predecessor finish: %v", err)
				}
				operation = m8InstallReserveOK(t, store, m8InstallOp1, m8InstallOwnerA, "unit_file", m8InstallAttempt3)
				successor := m8InstallResourceOf(t, operation, "unit_file")
				if !successor.StartedAt.Equal(predFinish) {
					t.Fatalf("successor start is not causal: %v want %v", successor.StartedAt, predFinish)
				}
				if !operation.Valid() {
					t.Fatalf("clamped graph invalid: %+v", operation)
				}
				// The DB clock is sampled only after the operation row lock is
				// held: a mutation blocked behind the lock records the time of
				// its own lock acquisition, not of its invocation.
				m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallDocker, m8InstallOp2), m8InstallOwnerA)
				m8InstallBeginOK(t, store, m8InstallOp2, m8InstallOwnerA)
				m8InstallReserveOK(t, store, m8InstallOp2, m8InstallOwnerA, "image_cache", m8InstallAttempt4)
				m8InstallRecordOK(t, store, m8InstallOp2, m8InstallOwnerA, "image_cache", m8InstallAttempt4, m8InstallApplied())
				holder := m8InstallSecondDB(t, f, "")
				released := make(chan error, 1)
				var holdStart time.Time
				go func() {
					released <- holder.RunInTx(context.Background(), nil, func(ctx context.Context, tx bun.Tx) error {
						if _, err := tx.ExecContext(ctx,
							"UPDATE probe_install_operations SET operation_id = operation_id WHERE operation_id = ?", m8InstallOp2); err != nil {
							return err
						}
						holdStart = time.Now()
						time.Sleep(time.Second)
						return nil
					})
				}()
				time.Sleep(150 * time.Millisecond)
				operation = m8InstallReserveOK(t, store, m8InstallOp2, m8InstallOwnerA, "data_directory", m8InstallAttempt5)
				if err := <-released; err != nil {
					t.Fatalf("lock holder: %v", err)
				}
				started := m8InstallResourceOf(t, operation, "data_directory").StartedAt
				if !started.After(holdStart.Add(500 * time.Millisecond)) {
					t.Fatalf("clock sampled before the row lock: %v within %v", started, holdStart)
				}
			})
			t.Run("SeparateConnectionConcurrentAdmission", func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := t.Context()
				// For MariaDB one of the two admission connections runs with
				// clientFoundRows=true so the race also covers that driver mode.
				extra := ""
				if engine == "mariadb" {
					extra = "&clientFoundRows=true"
				}
				storeA := m8InstallStore(f)
				storeB := repository.NewProbeInstallJournalStore(m8InstallSecondDB(t, f, extra))
				identity := m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp1)
				type admitResult struct {
					operation *domain.ProbeInstallOperation
					created   bool
					err       error
				}
				results := make([]admitResult, 2)
				admission := make(chan struct{})
				var wg sync.WaitGroup
				for i, store := range []*repository.ProbeInstallJournalStore{storeA, storeB} {
					wg.Add(1)
					go func(i int, store *repository.ProbeInstallJournalStore) {
						defer wg.Done()
						<-admission
						operation, created, err := store.CreateInstall(ctx, identity, m8InstallOwnerA)
						results[i] = admitResult{operation, created, err}
					}(i, store)
				}
				close(admission)
				wg.Wait()
				grants := 0
				var admitted *domain.ProbeInstallOperation
				for _, result := range results {
					if result.err != nil {
						t.Fatalf("concurrent admission: %v", result.err)
					}
					if result.created {
						grants++
						admitted = result.operation
					}
				}
				if grants != 1 || admitted == nil {
					t.Fatalf("exactly one fresh admission required, got %d", grants)
				}
				for _, result := range results {
					m8InstallRequireSame(t, result.operation, admitted, "concurrent admission receipt")
				}
				// Concurrent begin admits exactly one executor.
				begins := make([]error, 2)
				begin := make(chan struct{})
				for i, store := range []*repository.ProbeInstallJournalStore{storeA, storeB} {
					wg.Add(1)
					go func(i int, store *repository.ProbeInstallJournalStore) {
						defer wg.Done()
						<-begin
						_, begins[i] = store.BeginInstall(ctx, m8InstallOp1, m8InstallOwnerA)
					}(i, store)
				}
				close(begin)
				wg.Wait()
				ok := 0
				for _, err := range begins {
					if err == nil {
						ok++
					} else if !errors.Is(err, ports.ErrConflict) {
						t.Fatalf("concurrent begin: %v", err)
					}
				}
				if ok != 1 {
					t.Fatalf("exactly one begin grant required, got %d", ok)
				}
				// Concurrent reservations of one resource admit exactly one.
				reservations := make([]admitResult, 2)
				attempts := []string{m8InstallAttempt1, m8InstallAttempt2}
				reserve := make(chan struct{})
				for i, store := range []*repository.ProbeInstallJournalStore{storeA, storeB} {
					wg.Add(1)
					go func(i int, store *repository.ProbeInstallJournalStore) {
						defer wg.Done()
						<-reserve
						operation, created, err := store.ReserveInstallResource(ctx, m8InstallOp1, m8InstallOwnerA, "data_directory", attempts[i])
						reservations[i] = admitResult{operation, created, err}
					}(i, store)
				}
				close(reserve)
				wg.Wait()
				grants = 0
				for i, result := range reservations {
					switch {
					case result.err == nil && result.created:
						grants++
					case errors.Is(result.err, ports.ErrConflict):
					default:
						t.Fatalf("concurrent reservation %d: created=%v err=%v", i, result.created, result.err)
					}
				}
				if grants != 1 {
					t.Fatalf("exactly one reservation grant required, got %d", grants)
				}
			})
		})
	}
}
