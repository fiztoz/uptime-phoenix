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
	connections     ports.ProbeConnectionRepository
	leases          ports.ProbeConnectorLeaseRepository
	runtimes        ports.ProbeRuntimeLeaseRepository
	renewInterval   time.Duration
	protector       ports.ProbeCredentialProtector
	configs         *ProbeConfigService
	transport       ports.ProbeConnectionTransport
	configSync      ports.RemoteProbeConfigSyncRepository
	replayIngest    ports.ProbeReplayService
	rotations       ports.ProbeCredentialRotationRepository
	certificates    ports.ProbeCertificateRotationRepository
	watchdogFactory func(context.Context, domain.ProbeRuntimeLease) (*ProbeWatchdogRuntime, error)
	hubID, ownerID  string
	delay           func(int, time.Duration) time.Duration
}

// SetWatchdogFactory wires one source owner for each acquired runtime lease.
// Configure before Run. A transport without admission support is rejected.
func (s *ProbeConnectorService) SetWatchdogFactory(factory func(context.Context, domain.ProbeRuntimeLease) (*ProbeWatchdogRuntime, error)) error {
	if factory != nil {
		if _, ok := s.transport.(ports.ProbeWatchdogConnectionTransport); !ok {
			return domain.ErrValidation
		}
	}
	s.watchdogFactory = factory
	return nil
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

// SetCredentialRotation enables fenced candidate selection and authentication
// confirmation. Configure before Run; source receipts alone promote candidates.
func (s *ProbeConnectorService) SetCredentialRotation(rotations ports.ProbeCredentialRotationRepository) {
	s.rotations = rotations
}

// SetCertificateRotation wires fenced dial-pin selection separately from credential
// encryption metadata. Configure before Run.
func (s *ProbeConnectorService) SetCertificateRotation(certificates ports.ProbeCertificateRotationRepository) {
	s.certificates = certificates
}

// NewProbeConnectorService requires verified installation authority and a unique
// worker owner. Backoff is supplied by the transport composition root.
func NewProbeConnectorService(connections ports.ProbeConnectionRepository, leases ports.ProbeConnectorLeaseRepository, runtimes ports.ProbeRuntimeLeaseRepository, protector ports.ProbeCredentialProtector, configs *ProbeConfigService, transport ports.ProbeConnectionTransport, hubID, ownerID string, delay func(int, time.Duration) time.Duration) (*ProbeConnectorService, error) {
	if connections == nil || leases == nil || runtimes == nil || protector == nil || configs == nil || transport == nil || !domain.ValidHubID(hubID) || !domain.ValidHubID(ownerID) || delay == nil {
		return nil, domain.ErrValidation
	}
	return &ProbeConnectorService{connections: connections, leases: leases, runtimes: runtimes, renewInterval: 15 * time.Second, protector: protector, configs: configs, transport: transport, hubID: hubID, ownerID: ownerID, delay: delay}, nil
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
	// Revalidate enabled registration and the immutable prepared credential at
	// the DB boundary. Enrollment is a separate, one-use operator exchange; it
	// must remain possible while a worker owns runtime retries awaiting it.
	c, err = s.connections.PrepareConnection(ctx, *c)
	if err != nil {
		return err
	}
	if c.State == "active" {
		return nil
	}
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
	for ctx.Err() == nil {
		err := s.withRuntime(ctx, probeID, func(ownedCtx context.Context, runtime domain.ProbeRuntimeLease, watchdog *ProbeWatchdogRuntime) error {
			s.ownedConnectLoop(ownedCtx, runtime, watchdog, report)
			return ownedCtx.Err()
		})
		if ctx.Err() != nil {
			return
		}
		if err != nil && !errors.Is(err, ports.ErrConflict) && report != nil {
			report(errors.New("probe runtime ownership unavailable"))
		}
		if !waitProbeConnector(ctx, 5*time.Second) {
			return
		}
	}
}

func (s *ProbeConnectorService) ownedConnectLoop(ctx context.Context, runtime domain.ProbeRuntimeLease, watchdog *ProbeWatchdogRuntime, report func(error)) {
	failures := 0
	for ctx.Err() == nil {
		healthy, err := s.connectOnce(ctx, runtime, watchdog)
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

func (s *ProbeConnectorService) connectOnce(ctx context.Context, runtime domain.ProbeRuntimeLease, watchdog *ProbeWatchdogRuntime) (time.Duration, error) {
	probeID := runtime.ProbeID
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	lease, err := s.runtimes.AcquireRuntimeConnector(opCtx, runtime)
	cancel()
	if err != nil {
		return 0, err
	}
	defer s.release(lease)
	if watchdog != nil {
		if err := watchdog.Begin(lease.Generation); err != nil {
			return 0, err
		}
		defer watchdog.End(lease.Generation)
	}
	readCtx, stopRead := context.WithTimeout(ctx, 5*time.Second)
	defer stopRead()
	c, err := s.connections.GetConnection(readCtx, probeID)
	if err != nil {
		return 0, err
	}
	if c.HubID != s.hubID {
		return 0, ports.ErrConflict
	}
	replaySession := domain.ProbeReplaySession{HubID: s.hubID, ProbeID: probeID, StreamID: c.StreamID, ConnectionGeneration: lease.Generation, OwnerID: s.ownerID}
	selection := domain.ProbeCredentialSelection{Current: *c}
	if s.rotations != nil {
		selection, err = s.rotations.SelectCredentialConnection(readCtx, replaySession)
		if err != nil {
			return 0, err
		}
		if selection.Current.HubID != s.hubID || selection.Current.ProbeID != probeID || selection.Current.StreamID != c.StreamID {
			return 0, ports.ErrConflict
		}
	}
	certificateSelection := domain.ProbeCertificateSelection{Current: selection.Current.ProbeCredentialMetadata}
	if s.certificates != nil {
		certificateSelection, err = s.certificates.SelectCertificateConnection(readCtx, replaySession)
		if err != nil {
			return 0, err
		}
		if certificateSelection.Current != selection.Current.ProbeCredentialMetadata || (certificateSelection.CandidateFingerprint != "" && selection.Candidate != nil) {
			return 0, ports.ErrConflict
		}
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
		ticker := time.NewTicker(s.renewInterval)
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
		ingestFunc = func(callbackCtx context.Context, batch domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error) {
			return s.replayIngest.ProcessBatch(callbackCtx, replaySession, batch)
		}
	}
	run := s.transport.Run
	if watchdog != nil {
		aware, ok := s.transport.(ports.ProbeWatchdogConnectionTransport)
		if !ok {
			stop()
			<-renewed
			return 0, domain.ErrValidation
		}
		run = func(ctx context.Context, input domain.ProbeSessionInput, established func(context.Context) error, applied func(context.Context, domain.ProbeActiveConfig) error, ingest func(context.Context, domain.ProbeReplayBatch) (*domain.ProbeReplayResult, error)) error {
			return aware.RunWithWatchdog(ctx, input, watchdog, established, applied, ingest)
		}
	}
	connect := func(connectCtx context.Context, connection domain.ProbeConnection, pin string) error {
		openCtx, cancel := context.WithTimeout(connectCtx, 5*time.Second)
		token, openErr := s.protector.OpenCredential(openCtx, connection.ProbeCredentialMetadata, connection.ProtectedCredential)
		cancel()
		if openErr != nil {
			return openErr
		}
		return run(connectCtx, domain.ProbeSessionInput{DialFingerprint: pin, OwnerID: s.ownerID, Connection: connection.ProbeCredentialMetadata, Token: token, Generation: lease.Generation, CommittedSeq: cursor, ConfigDocument: document}, func(callbackCtx context.Context) error {
			if err := s.leases.SetConnectorConnected(callbackCtx, lease, true); err != nil {
				return err
			}
			var confirmErr error
			if s.rotations != nil {
				confirmErr = s.rotations.ConfirmCredentialConnection(callbackCtx, replaySession, connection.ProbeCredentialMetadata)
			} else {
				confirmErr = s.connections.ActivateConnection(callbackCtx, probeID, connection.EnrollmentID, connection.CredentialVersion, time.Now().UTC())
			}
			if confirmErr != nil {
				return confirmErr
			}
			if s.certificates != nil {
				if err := s.certificates.ConfirmCertificateConnection(callbackCtx, replaySession, connection.ProbeCredentialMetadata, pin); err != nil {
					return err
				}
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
	}
	if certificateSelection.CandidateFingerprint != "" {
		err = connect(sessionCtx, selection.Current, certificateSelection.CandidateFingerprint)
		// TLS pin rejection happens before HTTP or a source generation exists.
		// All other errors retain ordinary reconnect/backoff and never downgrade.
		if certificateSelection.AllowCurrentFallback && errors.Is(err, domain.ErrProbeCertificateMismatch) && connectedAt.IsZero() && sessionCtx.Err() == nil {
			fresh, selectionErr := s.certificates.SelectCertificateConnection(sessionCtx, replaySession)
			if selectionErr != nil {
				err = selectionErr
			} else if fresh.Current != certificateSelection.Current || fresh.CandidateFingerprint != certificateSelection.CandidateFingerprint || !fresh.AllowCurrentFallback {
				err = ports.ErrConflict
			} else {
				fallbackCtx, endFallback := context.WithDeadline(sessionCtx, fresh.FallbackUntil)
				err = connect(fallbackCtx, selection.Current, selection.Current.Fingerprint)
				endFallback()
			}
		}
	} else if selection.Candidate != nil {
		err = connect(sessionCtx, *selection.Candidate, selection.Candidate.Fingerprint)
		// Only the pinned HTTP rejection guarantees no source generation was
		// admitted. Reusing this lease after any other error could revive a
		// stale session or conceal a storage/protocol failure.
		if errors.Is(err, domain.ErrProbeCredentialRejected) && connectedAt.IsZero() && sessionCtx.Err() == nil {
			err = connect(sessionCtx, selection.Current, selection.Current.Fingerprint)
		}
	} else {
		err = connect(sessionCtx, selection.Current, selection.Current.Fingerprint)
	}
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
