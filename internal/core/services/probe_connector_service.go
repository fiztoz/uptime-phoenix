package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeConnectorService owns hub DB leases and confidential transport inputs.
// A network session is canceled immediately when renewal fails.
type ProbeConnectorService struct {
	connections    ports.ProbeConnectionRepository
	leases         ports.ProbeConnectorLeaseRepository
	protector      ports.ProbeCredentialProtector
	configs        *ProbeConfigService
	transport      ports.ProbeConnectionTransport
	configSync     ports.RemoteProbeConfigSyncRepository
	replayIngest   ports.ProbeReplayService
	hubID, ownerID string
	delay          func(int, time.Duration) time.Duration
}

// SetConfigSync enables automatic desired-state reconciliation and durable
// application receipts. Configure it before Run; manual operator enrollment does
// not require it.
func (s *ProbeConnectorService) SetConfigSync(syncer ports.RemoteProbeConfigSyncRepository) {
	s.configSync = syncer
}

// SetReplayIngest enables durable fenced telemetry batch replay. Configure it before Run.
func (s *ProbeConnectorService) SetReplayIngest(ingest ports.ProbeReplayService) {
	s.replayIngest = ingest
}

// NewProbeConnectorService requires verified installation authority and a unique
// worker owner. Backoff is supplied by the transport composition root.
func NewProbeConnectorService(connections ports.ProbeConnectionRepository, leases ports.ProbeConnectorLeaseRepository, protector ports.ProbeCredentialProtector, configs *ProbeConfigService, transport ports.ProbeConnectionTransport, hubID, ownerID string, delay func(int, time.Duration) time.Duration) (*ProbeConnectorService, error) {
	if connections == nil || leases == nil || protector == nil || configs == nil || transport == nil || !domain.ValidHubID(hubID) || !domain.ValidHubID(ownerID) || delay == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeConnectorService{connections: connections, leases: leases, protector: protector, configs: configs, transport: transport, hubID: hubID, ownerID: ownerID, delay: delay}, nil
}

// Prepare creates recoverable runtime authorization before any network request.
// An existing matching preparation is reused, including after a lost receipt.
func (s *ProbeConnectorService) Prepare(ctx context.Context, probeID, streamID, endpoint, pin string) (*domain.ProbeConnection, error) {
	stored, err := s.connections.GetConnection(ctx, probeID)
	if err == nil {
		if stored.HubID != s.hubID || stored.StreamID != streamID || stored.Endpoint != endpoint || stored.Fingerprint != pin {
			return nil, ports.ErrConflict
		}
		return stored, nil
	}
	if !errors.Is(err, ports.ErrNotFound) {
		return nil, err
	}
	id, err := newUUIDv4()
	if err != nil {
		return nil, err
	}
	m := domain.ProbeCredentialMetadata{HubID: s.hubID, ProbeID: probeID, StreamID: streamID, EnrollmentID: id, CredentialVersion: 1, Endpoint: endpoint, Fingerprint: pin}
	if !domain.ValidProbeCredentialMetadata(m) {
		return nil, domain.ErrValidation
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, errors.New("probe credential generation failed")
	}
	token := "phx_probe_" + base64.RawURLEncoding.EncodeToString(random[:])
	clear(random[:])
	protected, err := s.protector.SealCredential(ctx, m, token)
	if err != nil {
		return nil, err
	}
	return s.connections.PrepareConnection(ctx, domain.ProbeConnection{ProbeCredentialMetadata: m, ProtectedCredential: protected, State: "prepared", PreparedAt: time.Now().UTC()})
}

// Enroll consumes local operator authorization over the pinned endpoint. The hub
// marks credentials active only after the subsequent runtime handshake/health.
func (s *ProbeConnectorService) Enroll(ctx context.Context, probeID, enrollmentToken string) error {
	c, err := s.connections.GetConnection(ctx, probeID)
	if err != nil {
		return err
	}
	if c.HubID != s.hubID {
		return ports.ErrConflict
	}
	if c.State == "active" {
		return nil
	}
	lease, err := s.leases.AcquireConnector(ctx, probeID, s.ownerID)
	if err != nil {
		return err
	}
	defer s.release(lease)
	token, err := s.protector.OpenCredential(ctx, c.ProbeCredentialMetadata, c.ProtectedCredential)
	if err != nil {
		return err
	}
	return s.transport.Enroll(ctx, c.ProbeCredentialMetadata, enrollmentToken, token)
}

// Run reconciles enabled registrations and waits for each connector to stop on
// shutdown. Registration read failures cancel existing work rather than retaining
// authority indefinitely from stale cached membership.
func (s *ProbeConnectorService) Run(ctx context.Context, report func(error)) {
	workers := make(map[string]context.CancelFunc)
	var joined sync.WaitGroup
	defer func() {
		for _, cancel := range workers {
			cancel()
		}
		joined.Wait()
	}()
	for ctx.Err() == nil {
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		connections, err := s.connections.ListConnections(readCtx)
		cancel()
		if err != nil {
			for id, cancel := range workers {
				cancel()
				delete(workers, id)
			}
			if report != nil {
				report(errors.New("probe registry unavailable"))
			}
		} else {
			present := make(map[string]bool, len(connections))
			for _, c := range connections {
				if c.HubID != s.hubID {
					if report != nil {
						report(errors.New("probe installation mismatch"))
					}
					continue
				}
				present[c.ProbeID] = true
				if _, exists := workers[c.ProbeID]; exists {
					continue
				}
				workerCtx, stop := context.WithCancel(ctx)
				workers[c.ProbeID] = stop
				joined.Add(1)
				go func(id string) { defer joined.Done(); s.connectLoop(workerCtx, id, report) }(c.ProbeID)
			}
			for id, cancel := range workers {
				if !present[id] {
					cancel()
					delete(workers, id)
				}
			}
		}
		if !waitProbeConnector(ctx, 5*time.Second) {
			return
		}
	}
}

func (s *ProbeConnectorService) connectLoop(ctx context.Context, probeID string, report func(error)) {
	failures := 0
	for ctx.Err() == nil {
		healthy, err := s.connectOnce(ctx, probeID)
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, ports.ErrConflict) && report != nil {
			report(errors.New("probe connection unavailable"))
		}
		failures++
		if healthy >= 30*time.Second {
			failures = 1
		}
		if failures > 6 {
			failures = 6
		}
		pause := s.delay(failures, healthy)
		pause = max(time.Second, min(30*time.Second, pause))
		if !waitProbeConnector(ctx, pause) {
			return
		}
	}
}

func (s *ProbeConnectorService) connectOnce(ctx context.Context, probeID string) (time.Duration, error) {
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	lease, err := s.leases.AcquireConnector(opCtx, probeID, s.ownerID)
	cancel()
	if err != nil {
		return 0, err
	}
	defer s.release(lease)
	readCtx, stopRead := context.WithTimeout(ctx, 5*time.Second)
	defer stopRead()
	c, err := s.connections.GetConnection(readCtx, probeID)
	if err != nil {
		return 0, err
	}
	if c.HubID != s.hubID {
		return 0, ports.ErrConflict
	}
	token, err := s.protector.OpenCredential(readCtx, c.ProbeCredentialMetadata, c.ProtectedCredential)
	if err != nil {
		return 0, err
	}
	if s.configSync != nil {
		if _, err := s.configSync.RefreshRemote(readCtx, domain.ProbeConfigTarget{HubID: s.hubID, ProbeID: probeID}, time.Now().UTC()); err != nil {
			return 0, err
		}
	}
	document, metadata, err := s.configs.Read(readCtx, domain.ProbeConfigTarget{HubID: s.hubID, ProbeID: probeID}, 0)
	if err != nil && !errors.Is(err, ports.ErrNotFound) {
		return 0, err
	}
	defer clear(document)
	cursor, err := s.connections.GetConnectionCursor(readCtx, probeID, c.StreamID)
	if err != nil {
		return 0, err
	}
	stopRead()
	sessionCtx, stop := context.WithCancel(ctx)
	defer stop()
	renewed := make(chan struct{})
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-sessionCtx.Done():
				return
			case <-ticker.C:
			}
			checkCtx, cancel := context.WithTimeout(sessionCtx, 5*time.Second)
			_, err := s.leases.RenewConnector(checkCtx, lease)
			if err == nil && s.configSync != nil {
				_, err = s.configSync.RefreshRemote(checkCtx, domain.ProbeConfigTarget{HubID: s.hubID, ProbeID: probeID}, time.Now().UTC())
			}
			if err == nil {
				next, readErr := s.configs.LatestMetadata(checkCtx, domain.ProbeConfigTarget{HubID: s.hubID, ProbeID: probeID})
				if readErr != nil && !errors.Is(readErr, ports.ErrNotFound) {
					err = readErr
				} else if next.Revision != metadata.Revision {
					err = ports.ErrConflict
				}
			}
			cancel()
			if err != nil {
				stop()
				return
			}
		}
	}()
	var connectedAt time.Time
	var ingestFunc func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)
	if s.replayIngest != nil {
		replaySession := domain.ProbeReplaySession{
			HubID:                s.hubID,
			ProbeID:              probeID,
			StreamID:             c.StreamID,
			ConnectionGeneration: lease.Generation,
			OwnerID:              s.ownerID,
		}
		ingestFunc = func(callbackCtx context.Context, batch domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
			return s.replayIngest.ProcessBatch(callbackCtx, replaySession, batch)
		}
	}
	err = s.transport.Run(sessionCtx, domain.ProbeSessionInput{OwnerID: s.ownerID, Connection: c.ProbeCredentialMetadata, Token: token, Generation: lease.Generation, CommittedSeq: cursor, ConfigDocument: document}, func(callbackCtx context.Context) error {
		if err := s.leases.SetConnectorConnected(callbackCtx, lease, true); err != nil {
			return err
		}
		if err := s.connections.ActivateConnection(callbackCtx, probeID, c.EnrollmentID, c.CredentialVersion, time.Now().UTC()); err != nil {
			return err
		}
		if connectedAt.IsZero() {
			connectedAt = time.Now()
		}
		return nil
	}, func(callbackCtx context.Context, receipt domain.ProbeActiveConfig) error {
		if s.configSync == nil {
			return nil
		}
		return s.configSync.RecordRemoteApplied(callbackCtx, lease, receipt)
	}, ingestFunc)
	stop()
	<-renewed
	if connectedAt.IsZero() {
		return 0, err
	}
	return time.Since(connectedAt), err
}

func (s *ProbeConnectorService) release(lease domain.ProbeConnectorLease) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = s.leases.ReleaseConnector(ctx, lease)
}
func waitProbeConnector(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
