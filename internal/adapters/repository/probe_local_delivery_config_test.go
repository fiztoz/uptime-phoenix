package repository_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TestLocalDeliveryConfigContract covers LocalProbeDeliveryConfigStore.ReadAcknowledgedLocalDelivery:
// exact receipt-backed local document selection with immutable retained bytes,
// never current desired rows, execution-source reconstruction or applied-mode /
// send / suppression authority. Both engines are authored; MariaDB skips when
// TEST_MARIADB_DSN is unset and requires separate engine acceptance.
func TestLocalDeliveryConfigContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("ImmutableReceiptSelection", func(t *testing.T) { testLocalDeliveryConfigSelection(t, newProbeRegistryFixture(t, engine)) })
			t.Run("ConcurrentReceiptRead", func(t *testing.T) { testLocalDeliveryConfigConcurrent(t, newProbeRegistryFixture(t, engine)) })
			for name, mutate := range map[string]string{
				"MissingReceipt":      "DELETE FROM probe_config_applied_receipts",
				"ReceiptHash":         "UPDATE probe_config_applied_receipts SET sha256 = '" + strings.Repeat("0", 64) + "'",
				"ReceiptCount":        "UPDATE probe_config_applied_receipts SET assignment_count = 2",
				"ReceiptTime":         "UPDATE probe_config_applied_receipts SET applied_at = '2000-01-01 00:00:00'",
				"SnapshotHash":        "UPDATE probe_config_snapshots SET sha256 = '" + strings.Repeat("0", 64) + "'",
				"ForeignActiveTarget": "UPDATE probe_active_configs SET hub_id = '" + probeRegistryID3 + "'",
			} {
				t.Run(name, func(t *testing.T) { testLocalDeliveryConfigMismatch(t, newProbeRegistryFixture(t, engine), mutate) })
			}
			t.Run("AuthenticationAndValidation", func(t *testing.T) { testLocalDeliveryConfigValidation(t, newProbeRegistryFixture(t, engine)) })
		})
	}
}

// localDeliveryHubID is the trusted installation seeded by seedInstallation; the
// real builder must publish for the same hub the activation authority binds.
const localDeliveryHubID = "11111111-2222-4333-8444-555555555555"

// localDeliveryKeySeed matches the fixture protector shared by seedInstallation
// and protectedConfigService so sealed documents open under the installed key.
const localDeliveryKeySeed = 37

// localDeliveryTarget is the trusted local destination of the seeded installation.
func localDeliveryTarget() domain.ProbeConfigTarget {
	return domain.ProbeConfigTarget{HubID: localDeliveryHubID, ProbeID: domain.LocalProbeID}
}

// localDeliveryProtector returns the fixture installation-key protector.
func localDeliveryProtector(t *testing.T) ports.ProbeConfigProtector {
	t.Helper()
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{localDeliveryKeySeed}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return protector
}

// newLocalDeliveryStore binds the production local reader exactly as the
// composition root does: real local decoder and protector, never fake authority.
func newLocalDeliveryStore(db *bun.DB, decoder ports.LocalProbeDeliveryConfigDecoder, protector ports.ProbeConfigProtector) *repository.LocalProbeDeliveryConfigStore {
	if decoder == nil {
		decoder = probe.NewLocalDeliveryConfigDecoder(checker.Get, notifier.Get)
	}
	return repository.NewLocalProbeDeliveryConfigStore(db, decoder, protector)
}

// seedLocalDelivery seeds the trusted installation and one local assignment with
// the legacy desired regional mode, before any prepared or active document exists.
func seedLocalDelivery(t *testing.T, f probeRegistryFixture) int64 {
	t.Helper()
	seedInstallation(t, f)
	monitor := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(t.Context(), monitor); err != nil {
		t.Fatal(err)
	}
	return monitor
}

// prepareLocalDeliveryRevision prepares expected+1 from the current mutable source
// through the real builder/protector path, exactly like production publication.
func prepareLocalDeliveryRevision(t *testing.T, f probeRegistryFixture, db *bun.DB, expected int64, at time.Time) domain.ProbeConfigMetadata {
	t.Helper()
	builder, _ := sourceConfigBuilder(t, f, db)
	meta, err := builder.Prepare(t.Context(), localDeliveryHubID, expected, at, at)
	if err != nil {
		t.Fatal(err)
	}
	return meta
}

// activateLocalDelivery performs the real local activation (source freshness
// recheck, installation authority, atomic pointer plus receipt commit).
func activateLocalDelivery(t *testing.T, f probeRegistryFixture, db *bun.DB, meta domain.ProbeConfigMetadata, expectedActive int64) {
	t.Helper()
	if _, err := activationRepo(f, db).ActivateLocal(t.Context(), ports.LocalActivationParams{
		Target: meta.ProbeConfigTarget, Revision: meta.Revision, SHA256: meta.SHA256,
		ExpectedActiveRevision: expectedActive, AppliedAt: time.Now().UTC().Truncate(time.Microsecond), AssignmentCount: -1,
	}); err != nil {
		t.Fatal(err)
	}
}

// editLocalDeliveryMode changes only the desired paging mode of the retained local
// membership. Desired-row edits must never rewrite acknowledged document bytes.
func editLocalDeliveryMode(t *testing.T, f probeRegistryFixture, monitor int64) {
	t.Helper()
	if _, err := repository.NewProbeAssignmentStore(f.db).ReplaceWithBindings(t.Context(), monitor, 1,
		[]string{domain.LocalProbeID}, domain.HealthPolicyAnyDown, domain.AlertDeliveryAggregate, nil); err != nil {
		t.Fatal(err)
	}
}

// readLocalDelivery returns one projected nonsecret local document or fails.
func readLocalDelivery(t *testing.T, store *repository.LocalProbeDeliveryConfigStore) *domain.AcknowledgedProbeDeliveryConfig {
	t.Helper()
	config, err := store.ReadAcknowledgedLocalDelivery(t.Context(), localDeliveryTarget())
	if err != nil {
		t.Fatal(err)
	}
	return config
}

// assertLocalDeliveryMode asserts the exact receipt-backed document identity and
// desired document mode. It proves document acceptance only, never applied-mode,
// lease, send or suppression authority.
func assertLocalDeliveryMode(t *testing.T, config *domain.AcknowledgedProbeDeliveryConfig, meta domain.ProbeConfigMetadata, monitor int64, mode domain.AlertDelivery) {
	t.Helper()
	if config == nil || config.Target != meta.ProbeConfigTarget || config.Revision != meta.Revision || len(config.Assignments) != 1 {
		t.Fatal("read did not select the exact receipt-backed local document", config)
	}
	a := config.Assignments[0]
	if a.MonitorID != monitor || a.Generation != 1 || a.AlertDelivery != mode {
		t.Fatal("receipt-backed local assignment identity or desired mode changed", a)
	}
}

// testLocalDeliveryConfigSelection proves the exact acknowledged local document is
// retained across desired/source edits and newer preparation, on a cold reader too,
// and that the new desired mode appears only after real activation of that revision.
func testLocalDeliveryConfigSelection(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	monitor := seedLocalDelivery(t, f)
	at := time.Now().UTC().Truncate(time.Microsecond)
	first := prepareLocalDeliveryRevision(t, f, f.db, 0, at)
	store := newLocalDeliveryStore(f.db, nil, localDeliveryProtector(t))
	// A prepared snapshot alone is never receipt-backed evidence.
	if got, err := store.ReadAcknowledgedLocalDelivery(t.Context(), localDeliveryTarget()); got != nil || !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("prepared snapshot was treated as receipt-backed", got, err)
	}
	activateLocalDelivery(t, f, f.db, first, 0)
	assertLocalDeliveryMode(t, readLocalDelivery(t, store), first, monitor, domain.AlertDeliveryRegional)
	// Desired/source edits and a newer preparation do not replace acknowledged bytes.
	editLocalDeliveryMode(t, f, monitor)
	if _, err := f.db.ExecContext(t.Context(), "UPDATE monitors SET name = ? WHERE id = ?", "edited desired monitor", monitor); err != nil {
		t.Fatal(err)
	}
	second := prepareLocalDeliveryRevision(t, f, f.db, 1, at.Add(time.Second))
	if second.Revision != first.Revision+1 {
		t.Fatalf("newer preparation revision = %d, want %d", second.Revision, first.Revision+1)
	}
	assertLocalDeliveryMode(t, readLocalDelivery(t, store), first, monitor, domain.AlertDeliveryRegional)
	// A cold reader on a fresh connection selects the same exact acknowledged bytes.
	cold := newLocalDeliveryStore(reopenConfigDB(t, f), nil, localDeliveryProtector(t))
	assertLocalDeliveryMode(t, readLocalDelivery(t, cold), first, monitor, domain.AlertDeliveryRegional)
	// Only real activation of the newer revision projects its desired mode.
	activateLocalDelivery(t, f, f.db, second, 1)
	assertLocalDeliveryMode(t, readLocalDelivery(t, store), second, monitor, domain.AlertDeliveryAggregate)
}

// localDeliveryReadBarrier pauses the production reader after its active-pointer
// SELECT so a real local activation can commit through a separate connection
// between the stored-tuple SELECTs.
type localDeliveryReadBarrier struct {
	once    sync.Once
	reached chan struct{}
	done    chan struct{}
}

func (*localDeliveryReadBarrier) BeforeQuery(ctx context.Context, _ *bun.QueryEvent) context.Context {
	return ctx
}

func (h *localDeliveryReadBarrier) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	query := strings.NewReplacer("`", "", `"`, "").Replace(event.Query)
	if event.Err != nil || !strings.HasPrefix(query, "SELECT ") || !strings.Contains(query, "FROM probe_active_configs AS active") {
		return
	}
	h.once.Do(func() {
		close(h.reached)
		select {
		case <-h.done:
		case <-ctx.Done():
		}
	})
}

// testLocalDeliveryConfigConcurrent coordinates a real production local activation
// between the reader's stored-tuple SELECTs: one complete old document is returned
// for the interrupted read and the new document only on the next read.
func testLocalDeliveryConfigConcurrent(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	monitor := seedLocalDelivery(t, f)
	first := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC().Truncate(time.Microsecond))
	activateLocalDelivery(t, f, f.db, first, 0)
	editLocalDeliveryMode(t, f, monitor)
	second := prepareLocalDeliveryRevision(t, f, f.db, 1, time.Now().UTC().Truncate(time.Microsecond))
	if second.Revision != first.Revision+1 {
		t.Fatalf("newer preparation revision = %d, want %d", second.Revision, first.Revision+1)
	}
	f.db.SetMaxOpenConns(1)
	if f.engine == "sqlite" {
		if _, err := f.db.ExecContext(ctx, "PRAGMA journal_mode=WAL"); err != nil {
			t.Fatal(err)
		}
	} else {
		// The reader must override a weaker deployment/session default; the stored
		// tuple SELECTs run in their own explicit repeatable-read transaction.
		if _, err := f.db.ExecContext(ctx, "SET SESSION TRANSACTION ISOLATION LEVEL READ COMMITTED"); err != nil {
			t.Fatal(err)
		}
	}
	reader := newLocalDeliveryStore(f.db, nil, localDeliveryProtector(t))
	peer := reopenConfigDB(t, f)
	writerRepo := activationRepo(f, peer)
	barrier := &localDeliveryReadBarrier{reached: make(chan struct{}), done: make(chan struct{})}
	attachInPlaceQueryHook(f.db, barrier)
	writer := make(chan error, 1)
	go func() {
		defer close(barrier.done)
		select {
		case <-barrier.reached:
		case <-ctx.Done():
			writer <- ctx.Err()
			return
		}
		_, err := writerRepo.ActivateLocal(ctx, ports.LocalActivationParams{
			Target: localDeliveryTarget(), Revision: second.Revision, SHA256: second.SHA256,
			ExpectedActiveRevision: first.Revision, AppliedAt: time.Now().UTC().Truncate(time.Microsecond), AssignmentCount: -1,
		})
		writer <- err
	}()
	config, readErr := reader.ReadAcknowledgedLocalDelivery(ctx, localDeliveryTarget())
	writeErr := <-writer
	if readErr != nil || writeErr != nil {
		t.Fatalf("concurrent local receipt read/write failed: read %v write %v", readErr, writeErr)
	}
	assertLocalDeliveryMode(t, config, first, monitor, domain.AlertDeliveryRegional)
	assertLocalDeliveryMode(t, readLocalDelivery(t, reader), second, monitor, domain.AlertDeliveryAggregate)
}

// testLocalDeliveryConfigMismatch corrupts one stored tuple member after a real
// preparation and activation (with a newer unacknowledged revision retained) and
// requires a typed conflict: mismatched evidence never falls back to another
// revision and never projects a document.
func testLocalDeliveryConfigMismatch(t *testing.T, f probeRegistryFixture, mutation string) {
	t.Helper()
	monitor := seedLocalDelivery(t, f)
	first := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC().Truncate(time.Microsecond))
	activateLocalDelivery(t, f, f.db, first, 0)
	// A newer prepared revision is retained but never acknowledged; mismatched
	// receipt/pointer/snapshot evidence must fail instead of falling back to it.
	editLocalDeliveryMode(t, f, monitor)
	if next := prepareLocalDeliveryRevision(t, f, f.db, 1, time.Now().UTC().Truncate(time.Microsecond)); next.Revision != first.Revision+1 {
		t.Fatalf("newer preparation revision = %d, want %d", next.Revision, first.Revision+1)
	}
	if _, err := f.db.ExecContext(t.Context(), mutation); err != nil {
		t.Fatal(err)
	}
	if got, err := newLocalDeliveryStore(f.db, nil, localDeliveryProtector(t)).ReadAcknowledgedLocalDelivery(t.Context(), localDeliveryTarget()); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("inconsistent local receipt/pointer/snapshot returned a document", got, err)
	}
}

type failingLocalDeliveryDecoder struct{}

var _ ports.LocalProbeDeliveryConfigDecoder = failingLocalDeliveryDecoder{}

func (failingLocalDeliveryDecoder) DecodeLocalDelivery(context.Context, []byte, domain.ProbeConfigMetadata) (*domain.AcknowledgedProbeDeliveryConfig, error) {
	return nil, errors.New("private-local-fixture: validator details")
}

type mutatedLocalDeliveryDecoder struct {
	decoder ports.LocalProbeDeliveryConfigDecoder
	mutate  func(*domain.AcknowledgedProbeDeliveryConfig) *domain.AcknowledgedProbeDeliveryConfig
}

var _ ports.LocalProbeDeliveryConfigDecoder = mutatedLocalDeliveryDecoder{}

func (d mutatedLocalDeliveryDecoder) DecodeLocalDelivery(ctx context.Context, document []byte, expected domain.ProbeConfigMetadata) (*domain.AcknowledgedProbeDeliveryConfig, error) {
	config, err := d.decoder.DecodeLocalDelivery(ctx, document, expected)
	if err != nil {
		return nil, err
	}
	return d.mutate(config), nil
}

// captureLocalDeliveryDecoder retains the decrypted slice header (not a copy) so
// the test can observe the store's deferred plaintext clearing in place.
type captureLocalDeliveryDecoder struct {
	inner     ports.LocalProbeDeliveryConfigDecoder
	plaintext []byte
}

var _ ports.LocalProbeDeliveryConfigDecoder = (*captureLocalDeliveryDecoder)(nil)

func (d *captureLocalDeliveryDecoder) DecodeLocalDelivery(ctx context.Context, document []byte, expected domain.ProbeConfigMetadata) (*domain.AcknowledgedProbeDeliveryConfig, error) {
	d.plaintext = document
	return d.inner.DecodeLocalDelivery(ctx, document, expected)
}

// assertCleared requires every decrypted byte to have been zeroed after the read.
func assertCleared(t *testing.T, plaintext []byte) {
	t.Helper()
	if len(plaintext) == 0 {
		t.Fatal("decoder never received the decrypted document")
	}
	for i, b := range plaintext {
		if b != 0 {
			t.Fatalf("decrypted byte %d survived plaintext clearing", i)
		}
	}
}

// testLocalDeliveryConfigValidation covers ciphertext authentication, semantic
// error redaction, decoder output binding (metadata identity and assignment count),
// plaintext clearing, trusted target rejection and cancellation.
func testLocalDeliveryConfigValidation(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := t.Context()
	seedLocalDelivery(t, f)
	other := f.monitor(t)
	if _, err := f.assignments.InitializeLocal(ctx, other); err != nil {
		t.Fatal(err)
	}
	meta := prepareLocalDeliveryRevision(t, f, f.db, 0, time.Now().UTC().Truncate(time.Microsecond))
	activateLocalDelivery(t, f, f.db, meta, 0)
	real := probe.NewLocalDeliveryConfigDecoder(checker.Get, notifier.Get)

	// Ciphertext that authenticates only under a foreign key is never projected.
	wrong, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{localDeliveryKeySeed + 1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := newLocalDeliveryStore(f.db, nil, wrong).ReadAcknowledgedLocalDelivery(ctx, localDeliveryTarget()); got != nil || !errors.Is(err, domain.ErrInternal) {
		t.Fatal("foreign key authenticated the retained local snapshot", got, err)
	}
	// Semantic decoder failures fail closed with a fixed sentinel only.
	if got, err := newLocalDeliveryStore(f.db, failingLocalDeliveryDecoder{}, localDeliveryProtector(t)).ReadAcknowledgedLocalDelivery(ctx, localDeliveryTarget()); got != nil ||
		!errors.Is(err, domain.ErrInternal) || strings.Contains(err.Error(), "private-local-fixture") {
		t.Fatal("semantic validation failed open or exposed confidential details", got, err)
	}
	// Decoder output must stay bound to the exact stored tuple and its count.
	for name, mutate := range map[string]func(*domain.AcknowledgedProbeDeliveryConfig) *domain.AcknowledgedProbeDeliveryConfig{
		"nil": func(*domain.AcknowledgedProbeDeliveryConfig) *domain.AcknowledgedProbeDeliveryConfig { return nil },
		"target": func(c *domain.AcknowledgedProbeDeliveryConfig) *domain.AcknowledgedProbeDeliveryConfig {
			c.Target.HubID = probeRegistryID3
			return c
		},
		"revision": func(c *domain.AcknowledgedProbeDeliveryConfig) *domain.AcknowledgedProbeDeliveryConfig {
			c.Revision++
			return c
		},
		"count": func(c *domain.AcknowledgedProbeDeliveryConfig) *domain.AcknowledgedProbeDeliveryConfig {
			c.Assignments = c.Assignments[:1]
			return c
		},
		"monitor": func(c *domain.AcknowledgedProbeDeliveryConfig) *domain.AcknowledgedProbeDeliveryConfig {
			c.Assignments[0].MonitorID = 0
			return c
		},
		"generation": func(c *domain.AcknowledgedProbeDeliveryConfig) *domain.AcknowledgedProbeDeliveryConfig {
			c.Assignments[0].Generation = 0
			return c
		},
		"mode": func(c *domain.AcknowledgedProbeDeliveryConfig) *domain.AcknowledgedProbeDeliveryConfig {
			c.Assignments[0].AlertDelivery = "invalid"
			return c
		},
		"duplicate": func(c *domain.AcknowledgedProbeDeliveryConfig) *domain.AcknowledgedProbeDeliveryConfig {
			c.Assignments[1].MonitorID = c.Assignments[0].MonitorID
			return c
		},
	} {
		decoder := mutatedLocalDeliveryDecoder{decoder: real, mutate: mutate}
		if got, err := newLocalDeliveryStore(f.db, decoder, localDeliveryProtector(t)).ReadAcknowledgedLocalDelivery(ctx, localDeliveryTarget()); got != nil || !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("decoded %s was not bound to the exact acknowledged local document: %v %v", name, got, err)
		}
	}
	// Decrypted plaintext is cleared after every read, success or failure.
	captured := &captureLocalDeliveryDecoder{inner: real}
	config, err := newLocalDeliveryStore(f.db, captured, localDeliveryProtector(t)).ReadAcknowledgedLocalDelivery(ctx, localDeliveryTarget())
	if err != nil || config == nil || len(config.Assignments) != 2 {
		t.Fatal("authenticated local document was not projected", config, err)
	}
	assertCleared(t, captured.plaintext)
	failed := &captureLocalDeliveryDecoder{inner: failingLocalDeliveryDecoder{}}
	if got, err := newLocalDeliveryStore(f.db, failed, localDeliveryProtector(t)).ReadAcknowledgedLocalDelivery(ctx, localDeliveryTarget()); got != nil || err == nil {
		t.Fatal("semantic failure returned a local document", got, err)
	}
	assertCleared(t, failed.plaintext)
	// Trusted target rejection: only the local dialect of the installed hub.
	remoteTarget := localDeliveryTarget()
	remoteTarget.ProbeID = probeRegistryID1
	if got, err := newLocalDeliveryStore(f.db, nil, localDeliveryProtector(t)).ReadAcknowledgedLocalDelivery(ctx, remoteTarget); got != nil || !errors.Is(err, domain.ErrValidation) {
		t.Fatal("local reader accepted a remote target", got, err)
	}
	foreign := localDeliveryTarget()
	foreign.HubID = probeRegistryID3
	if got, err := newLocalDeliveryStore(f.db, nil, localDeliveryProtector(t)).ReadAcknowledgedLocalDelivery(ctx, foreign); got != nil || !errors.Is(err, ports.ErrConflict) {
		t.Fatal("foreign installation accepted the active local pointer", got, err)
	}
	if got, err := newLocalDeliveryStore(f.db, nil, localDeliveryProtector(t)).ReadAcknowledgedLocalDelivery(ctx, domain.ProbeConfigTarget{HubID: "not-a-uuid", ProbeID: domain.LocalProbeID}); got != nil || !errors.Is(err, domain.ErrValidation) {
		t.Fatal("untrusted target identity was accepted", got, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := newLocalDeliveryStore(f.db, nil, localDeliveryProtector(t)).ReadAcknowledgedLocalDelivery(canceled, localDeliveryTarget()); got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("canceled read returned a local document", got, err)
	}
	// Corrupt protected bytes must fail authentication, never decode.
	if _, err := f.db.ExecContext(ctx, "UPDATE probe_config_snapshots SET protected_payload = ? WHERE probe_id = ?", bytes.Repeat([]byte{1}, 80), domain.LocalProbeID); err != nil {
		t.Fatal(err)
	}
	if got, err := newLocalDeliveryStore(f.db, nil, localDeliveryProtector(t)).ReadAcknowledgedLocalDelivery(ctx, localDeliveryTarget()); got != nil || !errors.Is(err, domain.ErrInternal) {
		t.Fatal("corrupt protected bytes authenticated", got, err)
	}
}
