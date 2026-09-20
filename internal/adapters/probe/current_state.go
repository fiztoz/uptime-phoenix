package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func encodeCurrentSnapshot(source domain.EdgeCurrentSnapshot) (StateSnapshot, []byte, error) {
	snapshot := StateSnapshot{StreamID: source.Identity.StreamID, ConfigRevision: Decimal(source.Identity.ConfigRevision), CreatedAt: Timestamp(source.CreatedAt.UTC()), LastCreatedSeq: Decimal(source.Identity.LastCreatedSeq), States: make([]MonitorState, 0, len(source.States))}
	for _, state := range source.States {
		event, err := decodeTelemetryEvent(state.Payload)
		if err != nil {
			return snapshot, nil, err
		}
		observation, ok := event.Data.(Observation)
		if !ok || event.Kind != "observation" || event.Seq != Decimal(state.Seq) || observation.MonitorID != state.MonitorID || observation.AssignmentGeneration != Decimal(state.AssignmentGeneration) || len(observation.Conditions) > 0 || observation.TLS != nil {
			return snapshot, nil, errors.New("unsupported or inconsistent current evidence")
		}
		snapshot.States = append(snapshot.States, MonitorState{MonitorID: state.MonitorID, AssignmentGeneration: Decimal(state.AssignmentGeneration), LastObservationSeq: event.Seq, ObservedAt: event.ObservedAt, Status: observation.Status, DownCount: observation.DownCount, Ping: observation.Ping, Message: observation.Message, Conditions: []ConditionState{}, TLS: nil, ActiveSourceAlertID: state.ActiveSourceAlertID})
	}
	document, err := json.Marshal(snapshot)
	if err != nil {
		return snapshot, nil, err
	}
	if _, err := DecodeStateSnapshot(document); err != nil {
		return snapshot, nil, err
	}
	return snapshot, document, nil
}

func prepareStateReceipt(snapshot StateSnapshot, document []byte) (StateApplied, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return StateApplied{}, err
	}
	sum := sha256.Sum256(document)
	return StateApplied{StateTransferIdentity: StateTransferIdentity{SnapshotID: id.String(), StreamID: snapshot.StreamID, ConfigRevision: snapshot.ConfigRevision}, SHA256: hex.EncodeToString(sum[:]), StateCount: len(snapshot.States)}, nil
}

func sendCurrentSnapshot(ctx context.Context, session *Session, generation Decimal, receipt StateApplied, snapshot StateSnapshot, document []byte) error {
	send := func(kind string, payload any) error {
		frame, err := encodeFrame(kind, generation, payload)
		if err != nil {
			return err
		}
		return session.SendControl(ctx, frame)
	}
	chunks := (len(document) + MaxStateChunkBytes - 1) / MaxStateChunkBytes
	begin := StateBegin{StateTransferIdentity: receipt.StateTransferIdentity, StateSchemaVersion: 1, CreatedAt: snapshot.CreatedAt, LastCreatedSeq: snapshot.LastCreatedSeq, TotalBytes: len(document), ChunkCount: chunks, SHA256: receipt.SHA256}
	if err := send("state.begin", begin); err != nil {
		return err
	}
	for index := 0; index < chunks; index++ {
		from := index * MaxStateChunkBytes
		through := min(from+MaxStateChunkBytes, len(document))
		if err := send("state.chunk", StateChunk{StateTransferIdentity: receipt.StateTransferIdentity, Index: index, Data: document[from:through]}); err != nil {
			return err
		}
	}
	return send("state.commit", StateCommit{StateTransferIdentity: receipt.StateTransferIdentity, SHA256: receipt.SHA256})
}

type hubStateReceiver struct {
	transfer *StateTransfer
	ingest   ports.ProbeStateService
	session  domain.ProbeReplaySession
}

func (r *hubStateReceiver) discard() {
	if r.transfer != nil {
		r.transfer.Discard()
		r.transfer = nil
	}
}

func (r *hubStateReceiver) handle(ctx context.Context, connection *Session, frame []byte) (err error) {
	if r.ingest == nil {
		return errors.New("current state ingest unavailable")
	}
	envelope, err := DecodeEnvelope(frame)
	if err != nil {
		return err
	}
	switch envelope.Type {
	case "state.begin":
		// A second begin never silently replaces an unfinished transfer.
		if r.transfer != nil {
			r.discard()
			return errors.New("state transfer already active")
		}
		r.transfer, err = NewStateTransfer(frame, time.Now())
		if err != nil {
			return err
		}
		if r.transfer.begin.StreamID != r.session.StreamID {
			r.discard()
			return errors.New("state stream mismatch")
		}
		return nil
	case "state.chunk":
		if r.transfer == nil {
			return errors.New("state transfer missing")
		}
		if err := r.transfer.AddChunk(frame, time.Now()); err != nil {
			r.discard()
			return err
		}
		return nil
	case "state.commit":
		if r.transfer == nil {
			return errors.New("state transfer missing")
		}
		_, commit, err := DecodeStateCommit(frame)
		if err != nil {
			r.discard()
			return err
		}
		snapshot, err := r.transfer.Commit(frame, time.Now())
		r.transfer = nil
		if err != nil {
			return err
		}
		candidate := domain.ProbeCurrentSnapshot{ProbeID: r.session.ProbeID, StreamID: snapshot.StreamID, SnapshotID: commit.SnapshotID, SHA256: commit.SHA256, ConfigRevision: int64(snapshot.ConfigRevision), CreatedAt: time.Time(snapshot.CreatedAt).UTC(), LastCreatedSeq: int64(snapshot.LastCreatedSeq), States: make([]domain.ProbeCurrentState, 0, len(snapshot.States))}
		for _, state := range snapshot.States {
			if len(state.Conditions) > 0 || state.TLS != nil || state.DownCount > math.MaxInt32 || state.Ping > math.MaxInt32 {
				return errors.New("unsupported auxiliary current state")
			}
			candidate.States = append(candidate.States, domain.ProbeCurrentState{MonitorID: state.MonitorID, AssignmentGeneration: int64(state.AssignmentGeneration), Seq: int64(state.LastObservationSeq), ObservedAt: time.Time(state.ObservedAt).UTC(), Status: replayDomainStatus(state.Status), DownCount: int(state.DownCount), Ping: int(state.Ping), Message: state.Message, ActiveSourceAlertID: state.ActiveSourceAlertID})
		}
		opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		receipt, err := r.ingest.ApplySnapshot(opCtx, r.session, candidate)
		cancel()
		if err != nil {
			return errors.New("current state commit unavailable")
		}
		if receipt == nil || receipt.SnapshotID != candidate.SnapshotID || receipt.StreamID != candidate.StreamID || receipt.ConfigRevision != candidate.ConfigRevision || receipt.SHA256 != candidate.SHA256 || receipt.StateCount != len(candidate.States) || receipt.AppliedAt.IsZero() {
			return errors.New("invalid current state receipt")
		}
		response, err := encodeFrame("state.applied", Decimal(r.session.ConnectionGeneration), StateApplied{StateTransferIdentity: commit.StateTransferIdentity, SHA256: receipt.SHA256, AppliedAt: Timestamp(receipt.AppliedAt.UTC()), StateCount: receipt.StateCount})
		if err != nil {
			return err
		}
		return connection.SendControl(ctx, response)
	default:
		return errors.New("unsupported state frame")
	}
}
