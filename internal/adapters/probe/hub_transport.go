package probe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// HubTransport implements the manual engineering connector over pinned TLS.
// It transfers prepared configs and commits replay through trusted callbacks.
type HubTransport struct {
	policy         EndpointPolicy
	receiptTimeout time.Duration
}

var _ ports.ProbeConnectionTransport = (*HubTransport)(nil)

// NewHubTransport copies the operator destination policy for future connections.
func NewHubTransport(policy EndpointPolicy) *HubTransport {
	policy.AllowedCIDRs = slices.Clone(policy.AllowedCIDRs)
	return &HubTransport{policy: policy, receiptTimeout: ConfigTransferTimeout}
}

func (t *HubTransport) dial(ctx context.Context, endpoint, pin, token string) (*websocket.Conn, error) {
	client, err := NewPinnedHTTPClient(endpoint, pin, t.policy)
	if err != nil {
		return nil, errors.New("invalid probe management endpoint or pin")
	}
	dialCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, response, err := websocket.Dial(dialCtx, endpoint, &websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Authorization": {"Bearer " + token}}, Subprotocols: []string{"phoenix.probe.v1"}, CompressionMode: websocket.CompressionDisabled})
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		if response != nil && response.StatusCode == http.StatusUnauthorized {
			return nil, domain.ErrUnauthorized
		}
		return nil, errors.New("probe management connection failed")
	}
	conn.SetReadLimit(MaxFrameBytes)
	return conn, nil
}

// Enroll offers a credential which the caller has already durably protected.
// A lost result is recovered by Run with that same credential, never by replacing it.
func (t *HubTransport) Enroll(ctx context.Context, m domain.ProbeCredentialMetadata, enrollmentToken, runtimeToken string) error {
	if !domain.ValidProbeCredentialMetadata(m) || !validEnrollmentToken(enrollmentToken) || !validRuntimeToken(runtimeToken) {
		return domain.ErrValidation
	}
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	endpoint := strings.TrimSuffix(m.Endpoint, "/ws/probe/v1") + "/ws/probe/enroll/v1"
	conn, err := t.dial(opCtx, endpoint, m.Fingerprint, enrollmentToken)
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseNow() }()
	frame, err := encodeFrame("enroll.request", 0, EnrollRequest{HubID: m.HubID, ProbeID: m.ProbeID, EnrollmentID: m.EnrollmentID, ProtocolMin: 1, ProtocolMax: 1, Capabilities: []string{"snapshot.v1"}, CredentialVersion: Decimal(m.CredentialVersion), Token: runtimeToken})
	if err != nil {
		return err
	}
	if err := conn.Write(opCtx, websocket.MessageText, frame); err != nil {
		return errors.New("probe enrollment write failed")
	}
	kind, data, err := conn.Read(opCtx)
	if err != nil || kind != websocket.MessageText {
		return errors.New("probe enrollment receipt unavailable")
	}
	_, result, err := DecodeEnrollResult(data)
	if err != nil || result.HubID != m.HubID || result.ProbeID != m.ProbeID || result.EnrollmentID != m.EnrollmentID || result.CredentialVersion != Decimal(m.CredentialVersion) || result.Status != "applied" || result.TLSFingerprint == nil || *result.TLSFingerprint != m.Fingerprint {
		return errors.New("probe enrollment receipt rejected")
	}
	return nil
}

// Run validates the hello against trusted registration and the current lease,
// then supervises health/config traffic until cancellation or a protocol failure.
func (t *HubTransport) Run(ctx context.Context, input domain.ProbeSessionInput, established func(context.Context) error, recordApplied func(context.Context, domain.ProbeActiveConfig) error, ingest func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)) error {
	m := input.Connection
	if !domain.ValidProbeCredentialMetadata(m) || !validRuntimeToken(input.Token) || input.Generation <= 0 || input.CommittedSeq < 0 || established == nil || recordApplied == nil {
		return domain.ErrValidation
	}
	var snapshot ConfigSnapshot
	var err error
	required := []string{"snapshot.v1"}
	if len(input.ConfigDocument) > 0 {
		snapshot, err = DecodeConfigSnapshot(input.ConfigDocument)
		if err != nil || snapshot.HubID != m.HubID || snapshot.ProbeID != m.ProbeID {
			return domain.ErrValidation
		}
		required = nil
		for capability := range configCapabilities(snapshot) {
			required = append(required, capability)
		}
		sort.Strings(required)
	}
	conn, err := t.dial(ctx, m.Endpoint, m.Fingerprint, input.Token)
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseNow() }()
	handshakeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	kind, hello, err := conn.Read(handshakeCtx)
	if err != nil || kind != websocket.MessageText {
		return errors.New("probe hello unavailable")
	}
	welcome := Welcome{SessionIdentity: SessionIdentity{HubID: m.HubID, ProbeID: m.ProbeID, StreamID: m.StreamID}, SelectedProtocol: 1, ConnectionGeneration: Decimal(input.Generation), CommittedSeq: Decimal(input.CommittedSeq), DesiredConfigRevision: snapshot.Revision, HeartbeatSeconds: HeartbeatSeconds, MaxFrameBytes: MaxFrameBytes, MaxBatchEvents: MaxBatchEvents, MaxBatchBytes: MaxBatchBytes, HubTime: Timestamp(time.Now().UTC())}
	frame, err := encodeFrame("welcome", Decimal(input.Generation), welcome)
	if err != nil {
		return err
	}
	if _, err := ValidateHandshake(hello, frame, HandshakeExpectation{HubID: m.HubID, ProbeID: m.ProbeID, StreamID: m.StreamID, ConnectionGeneration: Decimal(input.Generation), RequiredCapabilities: required}); err != nil {
		return errors.New("probe handshake rejected")
	}
	if err := conn.Write(handshakeCtx, websocket.MessageText, frame); err != nil {
		return errors.New("hub welcome write failed")
	}
	cancel()
	session, err := NewSession(conn, SessionConfig{Generation: Decimal(input.Generation), PeerRole: "probe"})
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	runCtx, stop := context.WithCancel(ctx)
	defer stop()
	var senders sync.WaitGroup
	var ready atomic.Bool
	var cursor atomic.Int64
	cursor.Store(input.CommittedSeq)
	senders.Add(1)
	go func() { defer senders.Done(); t.sendHubHealth(runCtx, session, welcome, &ready, &cursor) }()
	var transferID ConfigTransferIdentity
	var hash string
	appliedDone := make(chan struct{})
	var appliedOnce sync.Once
	if len(input.ConfigDocument) > 0 {
		id, err := uuid.NewRandom()
		if err != nil {
			stop()
			_ = session.Close()
			senders.Wait()
			return errors.New("config transfer identity unavailable")
		}
		transferID = ConfigTransferIdentity{SnapshotID: id.String(), Revision: snapshot.Revision}
		digest := sha256.Sum256(input.ConfigDocument)
		hash = hex.EncodeToString(digest[:])
		senders.Add(1)
		go func() {
			defer senders.Done()
			timeout := t.receiptTimeout
			if timeout <= 0 || timeout > ConfigTransferTimeout {
				timeout = ConfigTransferTimeout
			}
			deadline := time.NewTimer(timeout)
			defer deadline.Stop()
			if err := sendPreparedConfig(runCtx, session, Decimal(input.Generation), transferID, hash, snapshot.EffectiveAt, required, input.ConfigDocument); err != nil {
				_ = session.Close()
				return
			}
			// A successful socket write does not complete durable sync work.
			// Retry the same immutable desired revision after a missing receipt.
			select {
			case <-runCtx.Done():
			case <-appliedDone:
			case <-deadline.C:
				_ = session.Close()
			}
		}()
	}
	err = session.Run(runCtx, func(frameCtx context.Context, envelope Envelope) error {
		if envelope.Type == "health" {
			// Refresh the DB lease/writability proof for each probe health.
			if err := established(frameCtx); err != nil {
				return errors.New("connector authority unavailable")
			}
			ready.Store(ingest != nil)
			return nil
		}
		data, err := json.Marshal(envelope)
		if err != nil {
			return errors.New("invalid probe response")
		}
		if envelope.Type == "telemetry.batch" {
			if ingest == nil {
				return errors.New("ingest unavailable")
			}
			batch, err := decodeReplayBatch(data, m.ProbeID)
			if err != nil || batch.StreamID != m.StreamID {
				return errors.New("invalid telemetry batch")
			}
			result, err := ingest(frameCtx, batch)
			if err != nil {
				ready.Store(false)
				return errors.New("telemetry commit unavailable")
			}
			if result == nil || result.StreamID != batch.StreamID || result.CommittedSeq != batch.LastSeq {
				return errors.New("invalid telemetry receipt")
			}
			ack := replayACK(result)
			response, err := encodeFrame("telemetry.ack", Decimal(input.Generation), ack)
			if err != nil {
				return err
			}
			if _, _, err := DecodeTelemetryACK(response); err != nil {
				return errors.New("invalid telemetry receipt")
			}
			cursor.Store(max(cursor.Load(), result.CommittedSeq))
			return session.SendControl(frameCtx, response)
		}
		if envelope.Type == "config.applied" {
			_, applied, err := DecodeConfigApplied(data)
			if err != nil || applied.ConfigTransferIdentity != transferID || applied.SHA256 != hash || applied.AssignmentCount != len(snapshot.Assignments) {
				return errors.New("invalid applied receipt")
			}
			if err := recordApplied(frameCtx, domain.ProbeActiveConfig{ProbeConfigTarget: domain.ProbeConfigTarget{HubID: m.HubID, ProbeID: m.ProbeID}, Revision: int64(applied.Revision), SHA256: applied.SHA256, AppliedAt: time.Time(applied.AppliedAt).UTC(), AssignmentCount: applied.AssignmentCount}); err != nil {
				return errors.New("configuration receipt storage unavailable")
			}
			appliedOnce.Do(func() { close(appliedDone) })
			return nil
		}
		if envelope.Type == "config.rejected" {
			return errors.New("probe rejected prepared configuration")
		}
		return errors.New("unsupported hub session operation")
	})
	stop()
	senders.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return errors.New("probe runtime connection ended")
	}
	return nil
}

func (t *HubTransport) sendHubHealth(ctx context.Context, session *Session, w Welcome, ready *atomic.Bool, cursor *atomic.Int64) {
	ticker := time.NewTicker(HeartbeatSeconds * time.Second)
	defer ticker.Stop()
	for {
		available := ready.Load()
		codes := []string{}
		if !available {
			codes = append(codes, "ingest_unavailable")
		}
		h := Health{Role: "hub", Ready: available, DBWritable: available, ConfigRevision: w.DesiredConfigRevision, CommittedSeq: Decimal(cursor.Load()), ClockTime: Timestamp(time.Now().UTC()), Errors: codes}
		frame, err := encodeFrame("health", w.ConnectionGeneration, h)
		if err == nil {
			opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = session.SendControl(opCtx, frame)
			cancel()
		}
		if err != nil {
			_ = session.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func sendPreparedConfig(ctx context.Context, session *Session, generation Decimal, id ConfigTransferIdentity, hash string, effective Timestamp, required []string, document []byte) error {
	opCtx, cancel := context.WithTimeout(ctx, ConfigTransferTimeout)
	defer cancel()
	count := (len(document) + MaxConfigChunkBytes - 1) / MaxConfigChunkBytes
	send := func(kind string, payload any) error {
		frame, err := encodeFrame(kind, generation, payload)
		if err != nil {
			return err
		}
		return session.SendControl(opCtx, frame)
	}
	if err := send("config.begin", ConfigBegin{ConfigTransferIdentity: id, ConfigSchemaVersion: 1, TotalBytes: len(document), ChunkCount: count, SHA256: hash, RequiredCapabilities: required, EffectiveAt: effective}); err != nil {
		return err
	}
	for index := 0; index < count; index++ {
		start := index * MaxConfigChunkBytes
		end := min(start+MaxConfigChunkBytes, len(document))
		if err := send("config.chunk", ConfigChunk{ConfigTransferIdentity: id, Index: index, Data: document[start:end]}); err != nil {
			return err
		}
	}
	return send("config.commit", ConfigCommit{ConfigTransferIdentity: id, SHA256: hash})
}
