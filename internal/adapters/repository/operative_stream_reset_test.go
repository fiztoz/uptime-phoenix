package repository_test

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// operativeStreamReset drives the barrier member through the real #192 hub
// stream reset: prepare revokes its session, the operator's applied source
// receipt retires the barrier's stream and cancels the requests queued on it,
// and the probe then reconnects on the new stream.
type operativeStreamReset struct {
	o           operativeRemoteFixture
	resets      *repository.ProbeStreamResetStore
	connections *repository.ProbeConnectorStore
	plan        domain.ProbeStreamResetPlan
}

func (o operativeRemoteFixture) prepareStreamReset(t *testing.T) operativeStreamReset {
	t.Helper()
	ctx := t.Context()
	connections := repository.NewProbeConnectorStore(o.f.f.db)
	connection, err := connections.GetConnection(ctx, o.f.session.ProbeID)
	if err != nil {
		t.Fatal(err)
	}
	if connection.State != "active" {
		if err := connections.ActivateConnection(ctx, connection.ProbeID, connection.EnrollmentID, connection.CredentialVersion, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
	}
	// Reset reseals the real runtime token, so the fixture's placeholder
	// credential is replaced with genuine ciphertext first.
	token := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{19}, 32))
	protected, err := o.f.protector.SealCredential(ctx, connection.ProbeCredentialMetadata, token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.f.f.db.ExecContext(ctx, "UPDATE probe_connections SET protected_credential = ? WHERE probe_id = ?", protected, o.f.session.ProbeID); err != nil {
		t.Fatal(err)
	}
	resets := repository.NewProbeStreamResetStore(o.f.f.db, o.f.protector, o.f.protector, probe.StreamResetCodec{})
	op, err := resets.PrepareStreamReset(ctx, domain.ProbeStreamResetIssue{ResetID: "abababab-abab-4bab-8bab-abababababab",
		HubID: o.f.session.HubID, ProbeID: o.f.session.ProbeID, PreviousStreamID: o.f.session.StreamID, StreamID: "cdcdcdcd-cdcd-4dcd-8dcd-cdcdcdcdcdcd"})
	if err != nil {
		t.Fatal("prepare stream reset", err)
	}
	return operativeStreamReset{o: o, resets: resets, connections: connections, plan: op.Plan}
}

func (r operativeStreamReset) activate(t *testing.T) {
	t.Helper()
	p := r.plan
	receipt := domain.EdgeStreamResetRecord{Plan: p, InitialStreamID: p.PreviousStreamID,
		Source: domain.EdgeIdentity{ProbeID: p.ProbeID, HubID: p.HubID, StreamID: p.PreviousStreamID, Fingerprint: p.Fingerprint,
			LastCreatedSeq: 1, CommittedSeq: 0, ConnectionGeneration: p.ConnectionGeneration - 1, ConfigRevision: 1},
		State: "applied", ReservedAt: p.PreparedAt, AppliedAt: p.PreparedAt.Add(time.Second), ArchiveBytes: 4096, ArchiveSHA256: strings.Repeat("e", 64)}
	if _, err := r.resets.ActivateStreamReset(t.Context(), receipt); err != nil {
		t.Fatal("activate stream reset", err)
	}
}

// reconnect admits the probe on its new stream the way the handshake does and
// re-attests its cutover capability on that session.
func (r operativeStreamReset) reconnect(t *testing.T) domain.ProbeReplaySession {
	t.Helper()
	ctx := t.Context()
	lease, err := r.connections.AcquireConnector(ctx, r.plan.ProbeID, r.o.f.session.OwnerID)
	if err != nil {
		t.Fatal(err)
	}
	session := r.o.f.session
	session.StreamID = r.plan.StreamID
	session.ConnectionGeneration = lease.Generation
	current, err := r.connections.GetConnection(ctx, r.plan.ProbeID)
	if err != nil {
		t.Fatal(err)
	}
	commands := r.o.f.commands(t)
	if err := commands.ConfirmCredentialConnection(ctx, session, current.ProbeCredentialMetadata); err != nil {
		t.Fatal(err)
	}
	if err := commands.ConfirmCertificateConnection(ctx, session, current.ProbeCredentialMetadata, current.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := r.resets.ConfirmStreamReset(ctx, session, current.ProbeCredentialMetadata, current.Fingerprint); err != nil {
		t.Fatal(err)
	}
	if err := r.o.store.SetOperativeSourceCapability(ctx, session, true); err != nil {
		t.Fatal(err)
	}
	return session
}

// deliverResume dispatches the open resume through the production command
// service on session, records the source's applied receipt and returns the
// decoded request the source received.
func (r operativeStreamReset) deliverResume(t *testing.T, session domain.ProbeReplaySession) domain.DeliveryCutoverCommand {
	t.Helper()
	ctx := t.Context()
	svc, err := services.NewProbeCommandService(r.o.f.commands(t), r.connections, r.o.f.protector, probe.AcknowledgementCodec{}, probe.CredentialCommandCodec{}, probe.CertificateCommandCodec{})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetDeliveryCutoverCodec(probe.DeliveryCutoverCodec{})
	dispatch, err := svc.NextCommand(ctx, session, time.Second, domain.ProbeCommandCapabilities{DeliveryCutover: true})
	if err != nil || dispatch == nil || dispatch.Metadata.Kind != domain.DeliveryResumeKind || dispatch.Metadata.StreamID != session.StreamID {
		t.Fatalf("resume not dispatched on the new stream: %+v %v", dispatch, err)
	}
	command, err := probe.DeliveryCutoverCodec{}.DecodeDeliveryCutover(ctx, dispatch.Payload)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := svc.RecordCommandResult(ctx, session, domain.ProbeCommandOutcome{CommandID: command.CommandID, Status: "applied", AppliedAt: &now, Code: domain.DeliveryResumedCode}); err != nil {
		t.Fatal("resume receipt", err)
	}
	return command
}

type operativeQueuedResume struct {
	CommandID       string
	StreamID        string
	Status          string
	RemoteConfirmed bool
}

// currentResume reads the member's current resume request and how many resume
// requests were ever queued for it.
func (o operativeRemoteFixture) currentResume(t *testing.T) (operativeQueuedResume, int64) {
	t.Helper()
	var row operativeQueuedResume
	if err := o.f.f.db.NewSelect().Table("probe_commands").Column("command_id", "stream_id", "status", "remote_confirmed").
		Where("command_id = (SELECT resume_command_id FROM operative_cutover_members)").Scan(t.Context(), &row); err != nil {
		t.Fatal(err)
	}
	queued, err := o.f.f.db.NewSelect().Table("probe_commands").Where("kind = ?", domain.DeliveryResumeKind).Count(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return row, queued
}

// requireBarrierResume proves the source received a resume for the exact stored
// barrier tuple, the one its gate authenticates: still the retired stream.
func (o operativeRemoteFixture) requireBarrierResume(t *testing.T, got domain.DeliveryCutoverCommand, oldStream string) {
	t.Helper()
	var member struct {
		BarrierID            string
		StreamID             string
		PreparationCommandID string
	}
	if err := o.f.f.db.NewSelect().Table("operative_cutover_members").Column("barrier_id", "stream_id", "preparation_command_id").Scan(t.Context(), &member); err != nil {
		t.Fatal(err)
	}
	if got.BarrierID != member.BarrierID || got.PreparationCommandID != member.PreparationCommandID || got.StreamID != member.StreamID || got.StreamID != oldStream {
		t.Fatalf("resume does not name the stored barrier tuple: got %+v, member %+v", got, member)
	}
}

// TestOperativeStreamResetResume covers #228. A #192 stream reset of a barrier
// member drains the barrier, keeps the source gate, and retires the barrier's
// stream. The hub queued the member's delivery.resume on that retired stream:
// it was canceled with the stream or never claimable, no receipt ever arrived,
// the barrier stayed draining and the member's regional paging stayed held.
// The resume must reach the probe on its new stream, still naming the stored
// barrier tuple, in both orderings of the drain and the reset activation.
func TestOperativeStreamResetResume(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			t.Run("ResumeQueuedBeforeActivation", func(t *testing.T) {
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				oldStream := o.f.session.StreamID
				reset := o.prepareStreamReset(t)
				// Prepare revokes the session: the barrier drains and queues its
				// resume on the only stream the probe has.
				if got := o.run(t, true); got.Phase != "draining" {
					t.Fatalf("reset prepare did not drain: %+v", got)
				}
				for i := 0; i < 3 && o.remoteResumes(t) == 0; i++ {
					o.run(t, true)
				}
				first, queued := o.currentResume(t)
				if queued != 1 || first.StreamID != oldStream || first.Status != "pending" {
					t.Fatalf("resume before activation = %+v (%d queued)", first, queued)
				}
				reset.activate(t)
				if canceled, _ := o.currentResume(t); canceled.Status != "canceled" || canceled.RemoteConfirmed {
					t.Fatalf("activation did not cancel the old-stream resume: %+v", canceled)
				}
				// The next pass re-addresses it at once, without a rearm cooldown.
				if got := o.run(t, true); got.Phase != "draining" || !got.Changed {
					t.Fatalf("stranded resume was not reissued: %+v", got)
				}
				second, queued := o.currentResume(t)
				if queued != 2 || second.CommandID == first.CommandID || second.StreamID != reset.plan.StreamID || second.Status != "pending" {
					t.Fatalf("resume not reissued on the new stream: %+v (%d queued)", second, queued)
				}
				// A pending resume on the current stream is waited on, not churned.
				if got := o.run(t, true); got.Phase != "draining" || got.Changed {
					t.Fatalf("pending resume churned: %+v", got)
				}
				if _, queued := o.currentResume(t); queued != 2 {
					t.Fatalf("pending resume reissued again: %d queued", queued)
				}
				o.requireBarrierResume(t, reset.deliverResume(t, reset.reconnect(t)), oldStream)
				if got := o.run(t, true); got.Phase != "released" {
					t.Fatalf("resumed barrier did not release: %+v", got)
				}
			})
			t.Run("ResumeIssuedAfterActivation", func(t *testing.T) {
				o := newOperativeRemoteFixture(t, engine)
				o.activateRemote(t)
				oldStream := o.f.session.StreamID
				reset := o.prepareStreamReset(t)
				reset.activate(t)
				if got := o.run(t, true); got.Phase != "draining" || got.Reason != "configuration_changed" {
					t.Fatalf("reset did not drain: %+v", got)
				}
				for i := 0; i < 3 && o.remoteResumes(t) == 0; i++ {
					o.run(t, true)
				}
				resume, queued := o.currentResume(t)
				if queued != 1 || resume.StreamID != reset.plan.StreamID || resume.Status != "pending" {
					t.Fatalf("resume not queued on the new stream: %+v (%d queued)", resume, queued)
				}
				o.requireBarrierResume(t, reset.deliverResume(t, reset.reconnect(t)), oldStream)
				if got := o.run(t, true); got.Phase != "released" {
					t.Fatalf("resumed barrier did not release: %+v", got)
				}
				if _, queued := o.currentResume(t); queued != 1 {
					t.Fatalf("delivered resume was reissued: %d queued", queued)
				}
			})
		})
	}
}
