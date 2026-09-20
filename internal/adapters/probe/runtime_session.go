package probe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// EdgeRuntimeConfig advertises only execution capabilities implemented by this build.
type EdgeRuntimeConfig struct {
	AgentVersion string
	Capabilities []string
}

// EdgeRuntimeState reads one coherent durable identity/retained-sequence snapshot.
type EdgeRuntimeState func(context.Context) (domain.EdgeIdentity, int64, error)

// EdgeRuntime owns authenticated negotiations and one current established session.
// Configuration commits remain protected by the durable connection generation.
type EdgeRuntime struct {
	state       EdgeRuntimeState
	identity    ports.EdgeIdentityRepository
	configs     *services.EdgeConfigService
	cfg         EdgeRuntimeConfig
	health      func(context.Context) (Health, error)
	replayRepo  ports.EdgeReplayRepository
	stateRepo   ports.EdgeStateRepository
	mu          sync.Mutex
	closed      bool
	active      *Session
	connections map[*websocket.Conn]context.CancelFunc
	handlers    sync.WaitGroup
}

// SetReplayRepository configures the edge replay repository before accepting sessions.
// Setup-only before accepting sessions. Nil repo preserves existing health/config-only behavior.
func (r *EdgeRuntime) SetReplayRepository(repo ports.EdgeReplayRepository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replayRepo = repo
}

// SetStateRepository enables complete source snapshots before historical replay.
// Configure it before accepting connections.
func (r *EdgeRuntime) SetStateRepository(repo ports.EdgeStateRepository) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stateRepo = repo
}

// NewEdgeRuntime validates and copies the local capability inventory.
func NewEdgeRuntime(state EdgeRuntimeState, identity ports.EdgeIdentityRepository, configs *services.EdgeConfigService, cfg EdgeRuntimeConfig, health func(context.Context) (Health, error)) (*EdgeRuntime, error) {
	if state == nil || identity == nil || configs == nil || health == nil || !validAgentVersion(cfg.AgentVersion) || validateCapabilities(cfg.Capabilities) != nil || !slices.Contains(cfg.Capabilities, "snapshot.v1") {
		return nil, domain.ErrValidation
	}
	cfg.Capabilities = slices.Clone(cfg.Capabilities)
	return &EdgeRuntime{state: state, identity: identity, configs: configs, cfg: cfg, health: health, connections: make(map[*websocket.Conn]context.CancelFunc)}, nil
}

// Handle negotiates an already header-authenticated socket under one deadline,
// persists a new fence, then starts the sole session reader/writer.
func (r *EdgeRuntime) Handle(ctx context.Context, conn *websocket.Conn, binding domain.EdgeEnrollment) error {
	if conn == nil {
		return domain.ErrValidation
	}
	runCtx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		cancel()
		return errors.New("edge runtime closed")
	}
	r.connections[conn] = cancel
	r.handlers.Add(1)
	r.mu.Unlock()
	defer func() {
		cancel()
		_ = conn.CloseNow()
		r.mu.Lock()
		delete(r.connections, conn)
		r.mu.Unlock()
		r.handlers.Done()
	}()
	conn.SetReadLimit(MaxFrameBytes)
	handshakeCtx, stop := context.WithTimeout(runCtx, 10*time.Second)
	defer stop()
	i, first, err := r.state(handshakeCtx)
	if err != nil {
		return errors.New("read local handshake state failed")
	}
	if i.HubID != binding.HubID || i.ProbeID != binding.ProbeID || !domain.ValidEdgeIdentity(i) || !domain.ValidEdgeEnrollment(binding) {
		return ErrHandshakeIdentity
	}
	hello := Hello{SessionIdentity: SessionIdentity{HubID: i.HubID, ProbeID: i.ProbeID, StreamID: i.StreamID}, AgentVersion: r.cfg.AgentVersion, ProtocolMin: 1, ProtocolMax: 1, ConfigRevision: Decimal(i.ConfigRevision), FirstRetainedSeq: Decimal(first), LastCreatedSeq: Decimal(i.LastCreatedSeq), Capabilities: r.cfg.Capabilities, ResourceBindings: []ResourceBinding{}}
	helloFrame, err := encodeFrame("hello", 0, hello)
	if err != nil {
		return err
	}
	if _, _, err := DecodeHello(helloFrame); err != nil {
		return errors.New("invalid local handshake state")
	}
	if err := conn.Write(handshakeCtx, websocket.MessageText, helloFrame); err != nil {
		return errors.New("write probe handshake failed")
	}
	kind, welcomeFrame, err := conn.Read(handshakeCtx)
	if err != nil || kind != websocket.MessageText {
		return errors.New("read hub handshake failed")
	}
	_, welcome, err := DecodeWelcome(welcomeFrame)
	if err != nil {
		return errors.New("invalid hub welcome")
	}
	expect := HandshakeExpectation{HubID: i.HubID, ProbeID: i.ProbeID, StreamID: i.StreamID, ConnectionGeneration: welcome.ConnectionGeneration, RequiredCapabilities: []string{"snapshot.v1"}}
	if _, err := ValidateHandshake(helloFrame, welcomeFrame, expect); err != nil {
		return errors.New("hub handshake comparison failed")
	}
	if welcome.CommittedSeq < Decimal(i.CommittedSeq) {
		return errors.New("hub welcome cursor precedes local committed sequence")
	}
	session, err := NewSession(conn, SessionConfig{Generation: welcome.ConnectionGeneration, PeerRole: "hub"})
	if err != nil {
		return err
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		_ = session.Close()
		return errors.New("edge runtime closed")
	}
	// Storage atomically rejects equal generations too. This also fences a
	// duplicate runtime object; a mutex alone would not protect persistent state.
	err = r.identity.AcceptConnectionGeneration(handshakeCtx, i.HubID, int64(welcome.ConnectionGeneration))
	if err != nil {
		r.mu.Unlock()
		_ = session.Close()
		return ErrHandshakeGeneration
	}
	previous := r.active
	r.active = session
	r.mu.Unlock()
	if previous != nil {
		_ = previous.Close()
	}
	stop()
	defer func() {
		_ = session.Close()
		r.mu.Lock()
		if r.active == session {
			r.active = nil
		}
		r.mu.Unlock()
	}()
	establishedCtx, end := context.WithCancel(runCtx)
	defer end()
	healthDone := make(chan struct{})
	go func() { defer close(healthDone); r.sendHealth(establishedCtx, session, welcome.ConnectionGeneration) }()
	var pump *edgeReplayPump
	replayDone := make(chan struct{})
	var pumpErr error
	r.mu.Lock()
	replayRepo := r.replayRepo
	stateRepo := r.stateRepo
	r.mu.Unlock()
	if replayRepo != nil {
		pump = newEdgeReplayPump(replayRepo, session, i.HubID, i.ProbeID, i.StreamID, welcome.ConnectionGeneration)
	}
	var statePump *edgeStatePump
	stateDone := make(chan struct{})
	var stateErr error
	if stateRepo != nil {
		statePump = newEdgeStatePump(stateRepo, session, domain.EdgeReplayFence{HubID: i.HubID, ProbeID: i.ProbeID, StreamID: i.StreamID, ConnectionGeneration: int64(welcome.ConnectionGeneration)}, int64(welcome.DesiredConfigRevision), pump)
		go func() {
			defer close(stateDone)
			stateErr = statePump.run(establishedCtx)
			if stateErr != nil {
				_ = session.Close()
			}
		}()
	} else {
		close(stateDone)
	}
	if pump != nil {
		go func() { defer close(replayDone); pumpErr = pump.run(establishedCtx) }()
	} else {
		close(replayDone)
	}

	var transfer *ConfigTransfer
	var deadline time.Time
	defer func() {
		if transfer != nil {
			transfer.Discard()
		}
	}()
	err = session.Run(establishedCtx, func(frameCtx context.Context, envelope Envelope) error {
		if transfer != nil && !time.Now().Before(deadline) {
			transfer.Discard()
			transfer = nil
		}
		frame, err := json.Marshal(envelope)
		if err != nil {
			return errors.New("invalid runtime frame")
		}
		reject := func(id ConfigTransferIdentity) error {
			if transfer != nil {
				transfer.Discard()
				transfer = nil
			}
			response, err := encodeFrame("config.rejected", welcome.ConnectionGeneration, ConfigRejected{ConfigTransferIdentity: id, Errors: []ConfigError{{Path: "/", Code: "invalid_config", Message: "Configuration could not be applied"}}})
			if err != nil {
				return err
			}
			return session.SendControl(frameCtx, response)
		}
		switch envelope.Type {
		case "health":
			var h Health
			if err := json.Unmarshal(envelope.Payload, &h); err == nil {
				if statePump != nil {
					statePump.updateHubHealth(h)
				}
				if pump != nil {
					pump.updateHubHealth(h)
				}
			}
			return nil // Session validates exact role and payload.
		case "state.applied":
			if statePump == nil {
				return errors.New("unsupported current state receipt")
			}
			_, receipt, err := DecodeStateApplied(frame)
			if err != nil {
				return err
			}
			return statePump.handleApplied(envelope.ConnectionGeneration, receipt)
		case "telemetry.ack":
			if pump == nil {
				return errors.New("unsupported edge runtime operation")
			}
			_, ack, err := DecodeTelemetryACK(frame)
			if err != nil {
				return err
			}
			return pump.handleACK(frameCtx, envelope.ConnectionGeneration, ack)
		case "telemetry.retry":
			if pump == nil {
				return errors.New("unsupported edge runtime operation")
			}
			_, retry, err := DecodeTelemetryRetry(frame)
			if err != nil {
				return err
			}
			return pump.handleRetry(frameCtx, envelope.ConnectionGeneration, retry)
		case "config.begin":
			_, begin, err := DecodeConfigBegin(frame)
			if err != nil {
				return errors.New("invalid config begin")
			}
			if transfer != nil {
				transfer.Discard()
			}
			transfer, err = NewConfigTransfer(frame, time.Now(), ConfigTarget{HubID: i.HubID, ProbeID: i.ProbeID, ConnectionGeneration: welcome.ConnectionGeneration, Capabilities: r.cfg.Capabilities})
			if err != nil {
				return reject(begin.ConfigTransferIdentity)
			}
			deadline = time.Now().Add(ConfigTransferTimeout)
			return nil
		case "config.chunk":
			_, chunk, err := DecodeConfigChunk(frame)
			if err != nil {
				return errors.New("invalid config chunk")
			}
			if transfer == nil {
				return reject(chunk.ConfigTransferIdentity)
			}
			if err := transfer.AddChunk(frame, time.Now()); err != nil {
				return reject(chunk.ConfigTransferIdentity)
			}
			return nil
		case "config.commit":
			_, commit, err := DecodeConfigCommit(frame)
			if err != nil {
				return errors.New("invalid config commit")
			}
			if transfer == nil {
				return reject(commit.ConfigTransferIdentity)
			}
			_, document, err := transfer.CommitDocument(frame, time.Now())
			transfer = nil
			if err != nil {
				return reject(commit.ConfigTransferIdentity)
			}
			defer clear(document)
			at := time.Now().UTC()
			applied, err := r.configs.Apply(frameCtx, document, int64(welcome.ConnectionGeneration), at)
			if err != nil {
				return reject(commit.ConfigTransferIdentity)
			}
			response, err := encodeFrame("config.applied", welcome.ConnectionGeneration, ConfigApplied{ConfigTransferIdentity: commit.ConfigTransferIdentity, SHA256: applied.Metadata.SHA256, AppliedAt: Timestamp(at), AssignmentCount: len(applied.Assignments)})
			if err != nil {
				return err
			}
			return session.SendControl(frameCtx, response)
		default:
			return errors.New("unsupported edge runtime operation")
		}
	})
	end()
	<-healthDone
	<-replayDone
	<-stateDone
	if runCtx.Err() != nil {
		return runCtx.Err()
	}
	if stateErr != nil && !errors.Is(stateErr, context.Canceled) {
		return errors.New("edge current state transfer failed")
	}
	if pumpErr != nil && !errors.Is(pumpErr, context.Canceled) {
		return fmt.Errorf("edge replay pump failed: %w", pumpErr)
	}
	if err != nil {
		return errors.New("edge runtime session ended")
	}
	return nil
}

func (r *EdgeRuntime) sendHealth(ctx context.Context, s *Session, generation Decimal) {
	ticker := time.NewTicker(HeartbeatSeconds * time.Second)
	defer ticker.Stop()
	for {
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		h, err := r.health(opCtx)
		if err == nil && h.Role == "probe" {
			var frame []byte
			frame, err = encodeFrame("health", generation, h)
			if err == nil {
				_, _, err = DecodeHealth(frame)
			}
			if err == nil {
				err = s.SendControl(opCtx, frame)
			}
		} else {
			err = errors.New("local health unavailable")
		}
		cancel()
		if err != nil {
			_ = s.Close()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Close permanently stops admissions, closes all handshakes/sessions and waits
// for handlers to release storage callbacks before the composition root closes DB.
func (r *EdgeRuntime) Close() error {
	r.mu.Lock()
	r.closed = true
	connections := make(map[*websocket.Conn]context.CancelFunc, len(r.connections))
	for conn, cancel := range r.connections {
		connections[conn] = cancel
	}
	r.mu.Unlock()
	for conn, cancel := range connections {
		cancel()
		_ = conn.CloseNow()
	}
	r.handlers.Wait()
	return nil
}
