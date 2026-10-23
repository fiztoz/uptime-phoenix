//go:build linux || darwin

package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"
	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// This file proves the M8 systemd artifact_directory path through production
// entry points only, on a real on-disk SQLite database:
//
//	ProbeInstallRunner.Run -> journal reserves artifact_directory ->
//	SSHArtifactDirectoryExecutor.ApplyResource -> SSHBinaryStageSender.SendAuthorized ->
//	loopback Go SSH server running the real runAuthorizedProbeBinaryStageHelper ->
//	ProbeBinaryStageAuthorizationService (real ProbeBinaryStageAdmissionStore on the
//	same database, real Ed25519ProbeStageGrantSigner) -> real files in a temp parent.
//
// Evidence limits: SQLite only. Nothing here exercises MariaDB/InnoDB locking,
// timestamp precision or isolation, so no MariaDB claim follows from it. The
// helper runs in-process behind a Go SSH server: no real sshd, no /opt, no uid 0
// and no systemd. The staged ELF is never executed.

// lifecycleAttempts are the four canonical, unique attempt UUIDs the admitting
// composition root supplies to Run, one per resource in layout order.
var lifecycleAttempts = []string{
	"aaaa0001-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
	"aaaa0002-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
	"aaaa0003-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
	"aaaa0004-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
}

// lifecycleKinds is the frozen systemd layout v1 order.
var lifecycleKinds = []string{"data_directory", "artifact_directory", "unit_file", "service"}

var (
	lifecycleApplied    = domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceApplied}
	lifecycleNotStarted = domain.ProbeInstallResourceOutcome{State: domain.ProbeInstallResourceNotStarted, ErrorCode: "not_started"}
)

// lifecycleDispatcher is the single executor the runner calls for every kind.
//   - data_directory is an explicit TEST DOUBLE that reports applied. No host data
//     directory is created or claimed anywhere by this test; it only lets the
//     production journal advance to artifact_directory. This mirrors m8StagePrepare
//     in the repository tests.
//   - artifact_directory is the REAL SSHArtifactDirectoryExecutor.
//   - unit_file and service are not implemented in this slice and report
//     not_started, which is the truthful report for an action never invoked.
//
// The hooks run synchronously on the goroutine that called Run (the runner calls
// the executor inline), so they may use the test's t.
type lifecycleDispatcher struct {
	artifact ports.ProbeInstallResourceExecutor

	// before runs on entry for every kind, with the acknowledged reservation receipt.
	before func(ctx context.Context, kind string, receipt domain.ProbeInstallOperation)
	// afterArtifact runs after the real executor joined and before the report is
	// returned to (and therefore before it is persisted by) the runner.
	afterArtifact func(ctx context.Context, receipt domain.ProbeInstallOperation, outcome domain.ProbeInstallResourceOutcome)

	mu       sync.Mutex
	calls    []string
	receipts map[string]domain.ProbeInstallOperation
}

var _ ports.ProbeInstallResourceExecutor = (*lifecycleDispatcher)(nil)

func (d *lifecycleDispatcher) ApplyResource(ctx context.Context, operation domain.ProbeInstallOperation, kind string) (domain.ProbeInstallResourceOutcome, error) {
	receipt := operation
	receipt.Resources = slices.Clone(operation.Resources)
	attempt := ""
	for _, r := range operation.Resources {
		if r.Spec.Kind == kind {
			attempt = r.AttemptID
		}
	}
	d.mu.Lock()
	d.calls = append(d.calls, kind+":"+attempt)
	if d.receipts == nil {
		d.receipts = map[string]domain.ProbeInstallOperation{}
	}
	d.receipts[kind] = receipt
	d.mu.Unlock()
	if d.before != nil {
		d.before(ctx, kind, receipt)
	}
	switch kind {
	case "data_directory":
		return lifecycleApplied, nil
	case "artifact_directory":
		outcome, err := d.artifact.ApplyResource(ctx, operation, kind)
		if d.afterArtifact != nil {
			d.afterArtifact(ctx, receipt, outcome)
		}
		return outcome, err
	case "unit_file", "service":
		return lifecycleNotStarted, nil
	}
	return domain.ProbeInstallResourceOutcome{}, errors.New("unexpected resource kind in lifecycle dispatcher")
}

func (d *lifecycleDispatcher) dispatched() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func (d *lifecycleDispatcher) receipt(t *testing.T, kind string) domain.ProbeInstallOperation {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	receipt, ok := d.receipts[kind]
	if !ok {
		t.Fatalf("no %s receipt was ever dispatched", kind)
	}
	return receipt
}

// lifecycleAdmissionRow is a test-only raw view of the stored one-shot admission.
type lifecycleAdmissionRow struct {
	bun.BaseModel  `bun:"table:probe_binary_stage_admissions"`
	OperationID    string `bun:"operation_id"`
	Kind           string `bun:"kind"`
	OwnerID        string `bun:"owner_id"`
	AttemptID      string `bun:"attempt_id"`
	AuthorityKeyID string `bun:"authority_key_id"`
	MarkerSHA256   string `bun:"marker_sha256"`
}

// lifecycleVariant selects the one fault a fixture injects.
type lifecycleVariant struct {
	// helperTrustsOtherAuthority makes the host trust a different authority key
	// than the one the hub signs with.
	helperTrustsOtherAuthority bool
	// helperForeignHostKeys makes the helper's own sshd host-key set exclude the
	// host key the operation pinned (and the SSH server really presents).
	helperForeignHostKeys bool
	// preparedArch, when set, prepares a binary of that architecture instead of
	// the pinned amd64 one, so the executor's own pin check refuses.
	preparedArch string
}

// artifactLifecycle is the complete production composition under test.
type artifactLifecycle struct {
	dsn        string
	db         *bun.DB
	peer       *bun.DB
	journal    *services.ProbeInstallJournalService
	peerView   *services.ProbeInstallJournalService
	runner     *services.ProbeInstallRunner
	dispatcher *lifecycleDispatcher
	artifact   *provisioning.SSHArtifactDirectoryExecutor

	server   *authzE2EServer
	parent   string
	scratch  string
	data     []byte
	identity domain.ProbeInstallIdentity
	owner    string
	// intent is the exact stage intent of the reserved artifact_directory attempt.
	intent domain.ProbeBinaryStageIntent
	// authorityKeyID names the authority the HUB signs with.
	authorityKeyID string
}

func lifecycleOpenDB(t *testing.T, dsn string, migrate bool) *bun.DB {
	t.Helper()
	db, err := sqlite.NewDB(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if migrate {
		if err := repository.RunMigrations(db.DB, "sqlite"); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func lifecycleJournal(t *testing.T, db *bun.DB) *services.ProbeInstallJournalService {
	t.Helper()
	store := repository.NewProbeInstallJournalStore(db)
	journal, err := services.NewProbeInstallJournalService(store, store)
	if err != nil {
		t.Fatal(err)
	}
	return journal
}

func newArtifactLifecycle(t *testing.T, variant lifecycleVariant) *artifactLifecycle {
	t.Helper()
	data := helperEffectFixture(t, "amd64")
	parent, _, scratch := helperEffectParent(t)
	authority, authorityKey := authzE2EAuthority(t)
	hostTrusted := authorityKey
	if variant.helperTrustsOtherAuthority {
		_, hostTrusted = authzE2EAuthority(t)
	}
	var server *authzE2EServer
	if variant.helperForeignHostKeys {
		server = serveLifecycleForeignHostKeys(t, hostTrusted, parent)
	} else {
		server = serveAuthzE2E(t, hostTrusted, parent)
	}

	base := helperEffectIntent(t, data, "amd64")
	identity := base.Identity
	identity.Host = domain.ProbeSSHHost{Address: server.address, User: "root"}
	identity.HostKeyFingerprint = ssh.FingerprintSHA256(server.hostSigner.PublicKey())
	identity.ClientKeyFingerprint = ssh.FingerprintSHA256(server.clientPub)
	intent := domain.ProbeBinaryStageIntent{Identity: identity, OwnerID: base.OwnerID, AttemptID: lifecycleAttempts[1]}
	if !identity.Valid() || !intent.Valid() {
		t.Fatal("lifecycle identity/intent invalid")
	}
	specs := identity.Resources()
	if len(specs) != len(lifecycleKinds) || specs[1].Kind != "artifact_directory" || intent.Directory() != specs[1].Reference {
		t.Fatal("lifecycle layout does not match the frozen systemd layout")
	}

	dsn := "file:" + filepath.Join(t.TempDir(), "lifecycle.db") + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)"
	db := lifecycleOpenDB(t, dsn, true)
	peer := lifecycleOpenDB(t, dsn, false)
	journal := lifecycleJournal(t, db)

	signer, err := provisioning.NewEd25519ProbeStageGrantSigner(authority)
	if err != nil {
		t.Fatal(err)
	}
	authorizer, err := services.NewProbeBinaryStageAuthorizationService(repository.NewProbeBinaryStageAdmissionStore(db), signer)
	if err != nil {
		t.Fatal(err)
	}
	policy := probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	sender, err := provisioning.NewSSHBinaryStageSender(t.Context(), server.clientPEM, identity.HostKeyFingerprint, identity.ClientKeyFingerprint, policy)
	if err != nil {
		t.Fatal(err)
	}
	preparedData, preparedPin := data, identity.Binary
	if variant.preparedArch != "" {
		preparedData = helperEffectFixture(t, variant.preparedArch)
		preparedPin = helperEffectIntent(t, preparedData, variant.preparedArch).Identity.Binary
	}
	prepared, err := provisioning.NewProbeBinaryPreparer(t.TempDir()).PrepareBinary(t.Context(), preparedPin, io.NopCloser(bytes.NewReader(preparedData)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close(context.Background()) })
	artifact, err := provisioning.NewSSHArtifactDirectoryExecutor(sender, prepared, authorizer)
	if err != nil {
		t.Fatal(err)
	}
	dispatcher := &lifecycleDispatcher{artifact: artifact}
	runner, err := services.NewProbeInstallRunner(journal, dispatcher)
	if err != nil {
		t.Fatal(err)
	}

	f := &artifactLifecycle{
		dsn: dsn, db: db, peer: peer, journal: journal, peerView: lifecycleJournal(t, peer), runner: runner,
		dispatcher: dispatcher, artifact: artifact, server: server, parent: parent, scratch: scratch, data: data,
		identity: identity, owner: base.OwnerID, intent: intent,
		authorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(authorityKey),
	}
	// The admitting composition root records the intent before the runner starts.
	created, fresh, err := journal.Create(t.Context(), identity, f.owner)
	if err != nil || !fresh || created.State != domain.ProbeInstallQueued {
		t.Fatalf("admit install intent: fresh=%v err=%v", fresh, err)
	}
	return f
}

// run executes the production coordinator once with the canonical attempt plan.
func (f *artifactLifecycle) run(t *testing.T) (*domain.ProbeInstallOperation, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	return f.runner.Run(ctx, f.identity, f.owner, slices.Clone(lifecycleAttempts))
}

// stored re-reads the operation through a freshly opened SQLite handle, so the
// assertion cannot be served from any state the runner's own connection cached.
func (f *artifactLifecycle) stored(t *testing.T) *domain.ProbeInstallOperation {
	t.Helper()
	db, err := sqlite.NewDB(f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	op, err := lifecycleJournal(t, db).Observe(t.Context(), f.identity.OperationID)
	if err != nil {
		t.Fatal("cold re-read of stored operation:", err)
	}
	return op
}

// observePeer reads through an independent connection while a call is in flight.
func (f *artifactLifecycle) observePeer(t *testing.T) *domain.ProbeInstallOperation {
	t.Helper()
	op, err := f.peerView.Observe(t.Context(), f.identity.OperationID)
	if err != nil {
		t.Fatal("independent read of stored operation:", err)
	}
	return op
}

func (f *artifactLifecycle) admissions(t *testing.T) []lifecycleAdmissionRow {
	t.Helper()
	var rows []lifecycleAdmissionRow
	if err := f.peer.NewSelect().Model(&rows).Order("operation_id").Scan(t.Context()); err != nil {
		t.Fatal("read stored admissions:", err)
	}
	return rows
}

// wantAdmitted asserts exactly one consumed admission bound to this attempt,
// authority and marker.
func (f *artifactLifecycle) wantAdmitted(t *testing.T) {
	t.Helper()
	rows := f.admissions(t)
	if len(rows) != 1 {
		t.Fatalf("stored admissions: got %d want 1", len(rows))
	}
	got := rows[0]
	if got.OperationID != f.identity.OperationID || got.Kind != "artifact_directory" || got.OwnerID != f.owner ||
		got.AttemptID != lifecycleAttempts[1] || got.AuthorityKeyID != f.authorityKeyID || got.MarkerSHA256 != f.intent.MarkerSHA256() {
		t.Fatalf("admission row binds the wrong operation/owner/attempt/authority/marker: %+v", got)
	}
}

func (f *artifactLifecycle) wantNoAdmission(t *testing.T) {
	t.Helper()
	if rows := f.admissions(t); len(rows) != 0 {
		t.Fatalf("stored admissions: got %d want 0 (an attempt was consumed)", len(rows))
	}
}

// lifecycleRes is the expected shape of one stored resource.
type lifecycleRes struct{ state, attempt, code string }

func lifecycleShape(op *domain.ProbeInstallOperation) []string {
	out := make([]string, 0, len(op.Resources))
	for _, r := range op.Resources {
		out = append(out, fmt.Sprintf("%s/%s/%s/%s", r.Spec.Kind, r.State, r.AttemptID, r.ErrorCode))
	}
	return out
}

func lifecycleWant(res ...lifecycleRes) []string {
	out := make([]string, 0, len(res))
	for i, r := range res {
		out = append(out, fmt.Sprintf("%s/%s/%s/%s", lifecycleKinds[i], r.state, r.attempt, r.code))
	}
	return out
}

func lifecycleWantResources(t *testing.T, op *domain.ProbeInstallOperation, label string, res ...lifecycleRes) {
	t.Helper()
	if got, want := lifecycleShape(op), lifecycleWant(res...); !slices.Equal(got, want) {
		t.Fatalf("%s resources:\n got %q\nwant %q", label, got, want)
	}
}

// awaitHelperExits returns the loopback server's record once it holds n helper
// exits. A sender that refuses before any stdin byte abandons the transport and
// returns without waiting for the remote process (joining local resources is
// documented not to claim remote termination), so this fixture's own exit
// record can trail SendAuthorized's return. Reading it immediately was observed
// to flake under load (codes=[0] where [0 1] was expected for a refused replay).
// The caller still asserts exact equality, so extra or missing runs are caught.
func (f *artifactLifecycle) awaitHelperExits(t *testing.T, n int) (commands []string, codes []int, stdinLen int, stderr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		commands, codes, stdinLen, stderr = f.server.snapshot()
		if len(codes) >= n || time.Now().After(deadline) {
			return commands, codes, stdinLen, stderr
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wantStagedOnce asserts the real staged graph for the reserved attempt, an
// empty private scratch area, and the helper-side facts of exactly one
// successful authorized stage.
func (f *artifactLifecycle) wantStagedOnce(t *testing.T) {
	t.Helper()
	helperEffectGraph(t, f.parent, f.intent, f.data)
	helperEffectEmpty(t, f.scratch)
}

// wantNothingStaged asserts the parent and the private scratch area are empty.
func (f *artifactLifecycle) wantNothingStaged(t *testing.T) {
	t.Helper()
	helperEffectEmpty(t, f.parent, f.scratch)
}

func (f *artifactLifecycle) stdinOfSuccess(t *testing.T) int {
	t.Helper()
	header, err := provisioning.EncodeProbeBinaryStageHeader(f.intent)
	if err != nil {
		t.Fatal(err)
	}
	return len(header) + provisioning.ProbeBinaryStageGrantFrameSize + len(f.data)
}

// serveLifecycleForeignHostKeys is serveAuthzE2E with one difference: the host
// key set the real helper reports as "this host's sshd keys" excludes the key
// the SSH server actually presents (and the operation pins). serveAuthzE2E's
// helper callback is not configurable, so the accept/serve loop is repeated here.
func serveLifecycleForeignHostKeys(t *testing.T, trusted [32]byte, parentPath string) *authzE2EServer {
	t.Helper()
	clientPEM, clientSigner := authzE2EKey(t)
	_, hostSigner := authzE2EKey(t)
	_, foreignSigner := authzE2EKey(t)
	s := &authzE2EServer{hostSigner: hostSigner, clientPEM: clientPEM, clientPub: clientSigner.PublicKey(), trusted: trusted, parentPath: parentPath}
	foreign := []string{ssh.FingerprintSHA256(foreignSigner.PublicKey())}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.address = listener.Addr().String()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				lifecycleServeConn(s, conn, foreign)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		done := make(chan struct{})
		go func() { s.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("lifecycle SSH fixture did not stop")
		}
	})
	return s
}

// lifecycleServeConn mirrors authzE2EServer.serve with an injectable helper
// host-key set. The helper itself is the unmodified production function.
func lifecycleServeConn(s *authzE2EServer, conn net.Conn, helperHostKeys []string) {
	defer func() { _ = conn.Close() }()
	config := &ssh.ServerConfig{PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if meta.User() != "root" || !bytes.Equal(key.Marshal(), s.clientPub.Marshal()) {
			return nil, errors.New("rejected")
		}
		return nil, nil
	}}
	config.AddHostKey(s.hostSigner)
	server, channels, requests, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return
	}
	defer func() { _ = server.Close() }()
	go ssh.DiscardRequests(requests)
	for newCh := range channels {
		ch, reqs, err := newCh.Accept()
		if err != nil {
			return
		}
		for req := range reqs {
			var exec struct{ Command string }
			if req.Type != "exec" || ssh.Unmarshal(req.Payload, &exec) != nil {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			s.mu.Lock()
			s.commands = append(s.commands, exec.Command)
			s.mu.Unlock()
			code := 127
			var stderr bytes.Buffer
			counter := &authzE2ECounter{r: authzE2EInput{ch: ch}}
			if exec.Command == provisioning.SSHProbeBinaryStageAuthorizedCommand {
				code = runAuthorizedProbeBinaryStageHelper(context.Background(), nil, counter, ch, &stderr,
					func(context.Context) ([32]byte, error) { return s.trusted, nil },
					func(context.Context) (*os.File, error) { return os.Open(s.parentPath) },
					rand.Reader,
					func(context.Context) ([]string, error) { return slices.Clone(helperHostKeys), nil })
			}
			s.mu.Lock()
			s.codes = append(s.codes, code)
			s.stdinLen += counter.n
			s.stderr.Write(stderr.Bytes())
			s.mu.Unlock()
			_, _ = ch.Stderr().Write(stderr.Bytes())
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
			_ = ch.Close()
			break
		}
		return
	}
}

// TestProbeInstallRunnerStagesArtifactDirectoryEndToEnd is the happy path: the
// runner drives data_directory (test double), the REAL artifact_directory stage
// over SSH under a real one-shot DB admission, then stops honestly at the
// unimplemented unit_file, and a replay of the consumed attempt is refused.
func TestProbeInstallRunnerStagesArtifactDirectoryEndToEnd(t *testing.T) {
	f := newArtifactLifecycle(t, lifecycleVariant{})
	a := lifecycleAttempts

	// Ordering evidence, read through an independent connection at dispatch time.
	f.dispatcher.before = func(_ context.Context, kind string, receipt domain.ProbeInstallOperation) {
		stored := f.observePeer(t)
		switch kind {
		case "artifact_directory":
			// The reservation is committed in_flight, its predecessor applied,
			// dependents untouched, and no admission exists before dispatch.
			lifecycleWantResources(t, stored, "stored at artifact dispatch",
				lifecycleRes{"applied", a[0], ""}, lifecycleRes{"in_flight", a[1], ""}, lifecycleRes{"planned", "", ""}, lifecycleRes{"planned", "", ""})
			lifecycleWantResources(t, &receipt, "receipt at artifact dispatch",
				lifecycleRes{"applied", a[0], ""}, lifecycleRes{"in_flight", a[1], ""}, lifecycleRes{"planned", "", ""}, lifecycleRes{"planned", "", ""})
			f.wantNoAdmission(t)
			f.wantNothingStaged(t)
		case "unit_file":
			// The runner persisted the joined artifact report BEFORE it reserved
			// the dependent action; the stage and the admission already exist.
			lifecycleWantResources(t, stored, "stored at unit_file dispatch",
				lifecycleRes{"applied", a[0], ""}, lifecycleRes{"applied", a[1], ""}, lifecycleRes{"in_flight", a[2], ""}, lifecycleRes{"planned", "", ""})
			f.wantAdmitted(t)
			f.wantStagedOnce(t)
		}
	}

	op, err := f.run(t)
	if t.Failed() {
		t.FailNow()
	}
	// not_started for the current action is an honest nil-error end: the runner
	// closes the operation failed/not_started and earlier effects stay recorded.
	if err != nil || op == nil {
		t.Fatalf("Run: op=%v err=%v", op != nil, err)
	}
	if op.State != domain.ProbeInstallFailed || op.ErrorCode != "not_started" || op.OwnerID != f.owner || op.Identity != f.identity {
		t.Fatalf("Run receipt state=%q code=%q", op.State, op.ErrorCode)
	}
	if got, want := f.dispatcher.dispatched(), []string{"data_directory:" + a[0], "artifact_directory:" + a[1], "unit_file:" + a[2]}; !slices.Equal(got, want) {
		t.Fatalf("dispatch order/attempts:\n got %q\nwant %q", got, want)
	}

	want := []lifecycleRes{{"applied", a[0], ""}, {"applied", a[1], ""}, {"not_started", a[2], "not_started"}, {"planned", "", ""}}
	lifecycleWantResources(t, op, "Run receipt", want...)

	// The durable record, re-read cold, agrees with the receipt.
	stored := f.stored(t)
	lifecycleWantResources(t, stored, "stored", want...)
	if stored.State != domain.ProbeInstallFailed || stored.ErrorCode != "not_started" || !stored.Terminal() {
		t.Fatalf("stored operation state=%q code=%q", stored.State, stored.ErrorCode)
	}
	if stored.Resources[1].FinishedAt.IsZero() || stored.Resources[1].FinishedAt.After(stored.UpdatedAt) {
		t.Fatal("stored artifact_directory has no joined finish time")
	}

	// Real effects: the exact staged graph, a clean scratch area, and one helper
	// run that received header + grant frame + payload.
	f.wantStagedOnce(t)
	commands, codes, stdinLen, stderr := f.awaitHelperExits(t, 1)
	if !slices.Equal(commands, []string{provisioning.SSHProbeBinaryStageAuthorizedCommand}) || !slices.Equal(codes, []int{0}) ||
		stdinLen != f.stdinOfSuccess(t) || stderr != "" {
		t.Fatalf("helper commands=%q codes=%v stdin=%d stderr=%q", commands, codes, stdinLen, stderr)
	}
	// Exactly one admission was consumed, for exactly this attempt.
	f.wantAdmitted(t)
	// A second AdmitBinaryStage for the same attempt is refused by the store.
	_, err = repository.NewProbeBinaryStageAdmissionStore(f.peer).AdmitBinaryStage(t.Context(), domain.ProbeBinaryStageAdmissionRequest{
		Intent: f.intent,
		Challenge: domain.ProbeBinaryStageChallenge{
			AuthorityKeyID: f.authorityKeyID, Nonce: [32]byte{7}, MarkerSHA256: f.intent.MarkerSHA256(),
		},
	})
	if !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("re-admitting the consumed attempt: %v", err)
	}

	// A fresh helper invocation replaying the same acknowledged receipt is
	// refused before any stdin byte: not_started, no new files, no new admission.
	// (The journal is already terminal here, so this proves the operation can not
	// be re-driven; TestProbeInstallRunnerArtifactDirectoryReplayRefusedByConsumedAdmission
	// isolates the admission itself while the journal still says in_flight.)
	receipt := f.dispatcher.receipt(t, "artifact_directory")
	replay, err := f.artifact.ApplyResource(t.Context(), receipt, "artifact_directory")
	if err != nil || replay != lifecycleNotStarted {
		t.Fatalf("replay outcome=%+v err=%v", replay, err)
	}
	f.wantStagedOnce(t)
	_, codes, stdinLen, _ = f.awaitHelperExits(t, 2)
	if len(codes) != 2 || codes[0] != 0 || codes[1] == 0 || stdinLen != f.stdinOfSuccess(t) {
		t.Fatalf("replay reached helper input: codes=%v stdin=%d", codes, stdinLen)
	}
	f.wantAdmitted(t)
	lifecycleWantResources(t, f.stored(t), "stored after replay", want...)
}

// TestProbeInstallRunnerArtifactDirectoryReplayRefusedByConsumedAdmission replays
// the acknowledged receipt while the journal still shows the operation running
// with artifact_directory in_flight on that very attempt, i.e. the only thing
// left to refuse a second stage is the consumed one-shot admission itself.
func TestProbeInstallRunnerArtifactDirectoryReplayRefusedByConsumedAdmission(t *testing.T) {
	f := newArtifactLifecycle(t, lifecycleVariant{})
	a := lifecycleAttempts
	replayed := false
	f.dispatcher.afterArtifact = func(ctx context.Context, receipt domain.ProbeInstallOperation, outcome domain.ProbeInstallResourceOutcome) {
		replayed = true
		if outcome != lifecycleApplied {
			t.Errorf("first stage outcome=%+v", outcome)
			return
		}
		// Journal state at replay time: running, artifact_directory in_flight on
		// the same attempt. State alone would therefore allow another stage.
		stored := f.observePeer(t)
		if stored.State != domain.ProbeInstallRunning {
			t.Errorf("stored state at replay=%q", stored.State)
		}
		lifecycleWantResources(t, stored, "stored at replay",
			lifecycleRes{"applied", a[0], ""}, lifecycleRes{"in_flight", a[1], ""}, lifecycleRes{"planned", "", ""}, lifecycleRes{"planned", "", ""})
		f.wantAdmitted(t)
		f.wantStagedOnce(t)

		got, err := f.artifact.ApplyResource(ctx, receipt, "artifact_directory")
		if err != nil || got != lifecycleNotStarted {
			t.Errorf("replay outcome=%+v err=%v", got, err)
		}
		// No new files; the second helper process refused before reading stdin.
		f.wantStagedOnce(t)
		_, codes, stdinLen, _ := f.awaitHelperExits(t, 2)
		if len(codes) != 2 || codes[0] != 0 || codes[1] == 0 || stdinLen != f.stdinOfSuccess(t) {
			t.Errorf("replay reached helper input: codes=%v stdin=%d", codes, stdinLen)
		}
		f.wantAdmitted(t)
		lifecycleWantResources(t, f.observePeer(t), "stored after replay",
			lifecycleRes{"applied", a[0], ""}, lifecycleRes{"in_flight", a[1], ""}, lifecycleRes{"planned", "", ""}, lifecycleRes{"planned", "", ""})
	}

	op, err := f.run(t)
	if t.Failed() {
		t.FailNow()
	}
	if !replayed {
		t.Fatal("the replay hook never ran")
	}
	// The refused replay never touched the first attempt's applied report.
	if err != nil || op == nil || op.State != domain.ProbeInstallFailed || op.ErrorCode != "not_started" {
		t.Fatalf("Run: op=%+v err=%v", op, err)
	}
	lifecycleWantResources(t, f.stored(t), "stored",
		lifecycleRes{"applied", a[0], ""}, lifecycleRes{"applied", a[1], ""}, lifecycleRes{"not_started", a[2], "not_started"}, lifecycleRes{"planned", "", ""})
	f.wantStagedOnce(t)
	f.wantAdmitted(t)
}

// TestProbeInstallRunnerArtifactDirectoryUntrustedAuthorityNotStarted has the
// host trust a different authority key than the hub signs with. The authorization
// service refuses the challenge before it consumes anything, so nothing reached
// the helper's stdin and the report is a truthful not_started.
func TestProbeInstallRunnerArtifactDirectoryUntrustedAuthorityNotStarted(t *testing.T) {
	f := newArtifactLifecycle(t, lifecycleVariant{helperTrustsOtherAuthority: true})
	a := lifecycleAttempts

	op, err := f.run(t)
	if err != nil || op == nil {
		t.Fatalf("Run: op=%v err=%v", op != nil, err)
	}
	if op.State != domain.ProbeInstallFailed || op.ErrorCode != "not_started" {
		t.Fatalf("Run receipt state=%q code=%q", op.State, op.ErrorCode)
	}
	want := []lifecycleRes{{"applied", a[0], ""}, {"not_started", a[1], "not_started"}, {"planned", "", ""}, {"planned", "", ""}}
	lifecycleWantResources(t, op, "Run receipt", want...)
	stored := f.stored(t)
	lifecycleWantResources(t, stored, "stored", want...)
	if stored.State != domain.ProbeInstallFailed || stored.ErrorCode != "not_started" {
		t.Fatalf("stored operation state=%q code=%q", stored.State, stored.ErrorCode)
	}
	// No dependent action was ever reserved or dispatched.
	if got, wantCalls := f.dispatcher.dispatched(), []string{"data_directory:" + a[0], "artifact_directory:" + a[1]}; !slices.Equal(got, wantCalls) {
		t.Fatalf("dispatch order/attempts:\n got %q\nwant %q", got, wantCalls)
	}
	// The helper ran, refused the unfamiliar authority and received no stdin; the
	// attempt was NOT consumed and nothing was staged.
	f.wantNothingStaged(t)
	commands, codes, stdinLen, _ := f.awaitHelperExits(t, 1)
	if !slices.Equal(commands, []string{provisioning.SSHProbeBinaryStageAuthorizedCommand}) || len(codes) != 1 || codes[0] == 0 || stdinLen != 0 {
		t.Fatalf("helper commands=%q codes=%v stdin=%d", commands, codes, stdinLen)
	}
	f.wantNoAdmission(t)
}

// TestProbeInstallRunnerArtifactDirectoryForeignHostKeyOutcomeUnknown has a helper
// whose own sshd host keys do not include the host key the operation pinned. The
// helper checks that only after it decoded the header, which the sender writes
// only AFTER it consumed the one-shot admission. The helper's refusal is definite
// but the sender cannot know that, so the honest report is unknown: the runner
// then atomically interrupts the operation and never continues.
func TestProbeInstallRunnerArtifactDirectoryForeignHostKeyOutcomeUnknown(t *testing.T) {
	f := newArtifactLifecycle(t, lifecycleVariant{helperForeignHostKeys: true})
	a := lifecycleAttempts

	op, err := f.run(t)
	// Unknown interrupts the graph: Run returns the recorded receipt together
	// with the fixed internal sentinel, never an executor/remote diagnostic.
	if !errors.Is(err, domain.ErrInternal) || op == nil {
		t.Fatalf("Run: op=%v err=%v", op != nil, err)
	}
	want := []lifecycleRes{{"applied", a[0], ""}, {"unknown", a[1], "outcome_unknown"}, {"planned", "", ""}, {"planned", "", ""}}
	lifecycleWantResources(t, op, "Run receipt", want...)
	if op.State != domain.ProbeInstallInterrupted || op.ErrorCode != "outcome_unknown" {
		t.Fatalf("Run receipt state=%q code=%q", op.State, op.ErrorCode)
	}
	stored := f.stored(t)
	lifecycleWantResources(t, stored, "stored", want...)
	if stored.State != domain.ProbeInstallInterrupted || stored.ErrorCode != "outcome_unknown" || !stored.Terminal() {
		t.Fatalf("stored operation state=%q code=%q", stored.State, stored.ErrorCode)
	}
	// Dependents are never reserved or dispatched over an unknown.
	if got, wantCalls := f.dispatcher.dispatched(), []string{"data_directory:" + a[0], "artifact_directory:" + a[1]}; !slices.Equal(got, wantCalls) {
		t.Fatalf("dispatch order/attempts:\n got %q\nwant %q", got, wantCalls)
	}
	// The helper refused with the definite pre-creation Unauthorized error after
	// the header arrived: nothing was staged, yet the admission is consumed and
	// stays consumed (never replenished).
	f.wantNothingStaged(t)
	commands, codes, stdinLen, stderr := f.awaitHelperExits(t, 1)
	if !slices.Equal(commands, []string{provisioning.SSHProbeBinaryStageAuthorizedCommand}) || !slices.Equal(codes, []int{1}) ||
		stdinLen == 0 || stderr != domain.ErrProbeBinaryStageWriteUnauthorized.Error()+"\n" {
		t.Fatalf("helper commands=%q codes=%v stdin=%d stderr=%q", commands, codes, stdinLen, stderr)
	}
	f.wantAdmitted(t)
}

// TestProbeInstallRunnerArtifactDirectoryBinaryPinMismatchNotStarted binds an
// executor to a prepared binary that is not the pinned one. The pair must refuse
// before any SSH connection exists: no helper run, no admission. Two layers
// refuse independently (the executor's own pin comparison and the sender's), so
// this proves the composed outcome, not that either layer alone is sufficient.
func TestProbeInstallRunnerArtifactDirectoryBinaryPinMismatchNotStarted(t *testing.T) {
	f := newArtifactLifecycle(t, lifecycleVariant{preparedArch: "arm64"})
	a := lifecycleAttempts

	op, err := f.run(t)
	if err != nil || op == nil || op.State != domain.ProbeInstallFailed || op.ErrorCode != "not_started" {
		t.Fatalf("Run: op=%+v err=%v", op, err)
	}
	want := []lifecycleRes{{"applied", a[0], ""}, {"not_started", a[1], "not_started"}, {"planned", "", ""}, {"planned", "", ""}}
	lifecycleWantResources(t, f.stored(t), "stored", want...)
	f.wantNothingStaged(t)
	if commands, codes, stdinLen, _ := f.server.snapshot(); len(commands) != 0 || len(codes) != 0 || stdinLen != 0 {
		t.Fatalf("pin mismatch reached the host: commands=%q codes=%v stdin=%d", commands, codes, stdinLen)
	}
	f.wantNoAdmission(t)
}
