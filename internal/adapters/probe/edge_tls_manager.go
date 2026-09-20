package probe

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"net"
	"sync/atomic"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type edgeTLSSelection struct {
	certificate tls.Certificate
	fingerprint string
	version     int64
}

type edgeTLSContextKey struct{}
type edgeTLSConnection struct {
	selection atomic.Pointer[edgeTLSSelection]
}

// EdgeTLSManager selects only authenticated durable material. Its immutable TLS
// cache is invalidated before activation and reconstructed after commit or error.
// The repository remains authoritative across process and response loss.
type EdgeTLSManager struct {
	anchor    *RuntimeIdentity
	repo      ports.EdgeCertificateRepository
	protector ports.EdgeCertificateProtector
	gate      chan struct{}
	active    atomic.Pointer[edgeTLSSelection]
}

var _ ports.EdgeCertificateRepository = (*EdgeTLSManager)(nil)

// NewEdgeTLSManager validates active TLS material before any listener is opened.
// An expired bootstrap anchor is usable only with a valid protected active row.
func NewEdgeTLSManager(ctx context.Context, anchor *RuntimeIdentity, repo ports.EdgeCertificateRepository, protector ports.EdgeCertificateProtector) (*EdgeTLSManager, error) {
	if anchor == nil || repo == nil || protector == nil || !domain.ValidHubID(anchor.ProbeID) || !domain.ValidHubID(anchor.StreamID) || len(anchor.Certificate.Certificate) != 1 || anchor.Certificate.Leaf == nil {
		return nil, domain.ErrValidation
	}
	sum := sha256.Sum256(anchor.Certificate.Certificate[0])
	if hex.EncodeToString(sum[:]) != anchor.Fingerprint {
		return nil, domain.ErrValidation
	}
	m := &EdgeTLSManager{anchor: anchor, repo: repo, protector: protector, gate: make(chan struct{}, 1)}
	if err := m.Reload(ctx); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *EdgeTLSManager) lock(ctx context.Context) error {
	select {
	case m.gate <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *EdgeTLSManager) unlock() { <-m.gate }

// Reload discards any prior cache and validates the current durable selection.
// Failure leaves serving unavailable instead of silently selecting an old key.
func (m *EdgeTLSManager) Reload(ctx context.Context) error {
	if err := m.lock(ctx); err != nil {
		return err
	}
	defer m.unlock()
	m.active.Store(nil)
	return m.reload(ctx)
}

func (m *EdgeTLSManager) reload(ctx context.Context) error {
	state, err := m.repo.ReadActiveCertificate(ctx)
	if err != nil {
		return err
	}
	if state.ActiveVersion < 1 || state.HighestVersion < state.ActiveVersion {
		return domain.ErrValidation
	}
	selected := &edgeTLSSelection{version: state.ActiveVersion}
	if state.ActiveVersion == 1 {
		if state.Certificate != nil {
			return domain.ErrValidation
		}
		selected.certificate, selected.fingerprint = m.anchor.Certificate, m.anchor.Fingerprint
	} else {
		c := state.Certificate
		if c == nil || c.Version != state.ActiveVersion || c.ProbeID != m.anchor.ProbeID || c.StreamID != m.anchor.StreamID {
			return domain.ErrValidation
		}
		selected.certificate, err = OpenEdgeCertificate(ctx, m.protector, *c, time.Now().UTC())
		if err != nil {
			return err
		}
		selected.fingerprint = c.Fingerprint
	}
	now := time.Now().UTC()
	if selected.certificate.Leaf == nil || now.Before(selected.certificate.Leaf.NotBefore) || !now.Before(selected.certificate.Leaf.NotAfter) {
		return errors.New("active probe certificate is outside its validity interval")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.active.Store(selected)
	return nil
}

// ApplyCertificateCommand fences cache publication around durable activation.
// It reloads after ambiguous storage errors too; socket/result loss is not success.
func (m *EdgeTLSManager) ApplyCertificateCommand(ctx context.Context, a domain.EdgeCommandAuthority, c domain.ProbeCertificateCommand) (domain.ProbeCommandOutcome, error) {
	if err := m.lock(ctx); err != nil {
		return domain.ProbeCommandOutcome{}, err
	}
	defer m.unlock()
	activating := c.Kind == CommandCertificateActivate
	if activating {
		m.active.Store(nil)
	}
	out, err := m.repo.ApplyCertificateCommand(ctx, a, c)
	if activating {
		// A canceled sender must not leave a committed activation using an old
		// certificate. This local reconciliation is independently bounded.
		reloadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		reloadErr := m.reload(reloadCtx)
		cancel()
		if reloadErr != nil {
			return domain.ProbeCommandOutcome{}, errors.New("active certificate reconciliation unavailable")
		}
	}
	return out, err
}

// ReadActiveCertificate delegates durable selection; callers never read the TLS
// cache as evidence of a committed source command result.
func (m *EdgeTLSManager) ReadActiveCertificate(ctx context.Context) (domain.EdgeCertificateState, error) {
	return m.repo.ReadActiveCertificate(ctx)
}

func (m *EdgeTLSManager) selection(ctx context.Context) (*edgeTLSSelection, error) {
	selected := m.active.Load()
	if selected == nil {
		// Normal handshakes are memory-only. A failed prior reconciliation may
		// retry a bounded durable read; it can never use stale cached material.
		reloadCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := m.lock(reloadCtx)
		if err == nil {
			// Another waiting handshake may already have repaired the cache.
			// Unlike an explicit Reload, recovery must not invalidate that work.
			selected = m.active.Load()
			if selected == nil {
				err = m.reload(reloadCtx)
				selected = m.active.Load()
			}
			m.unlock()
		}
		cancel()
		if err != nil {
			return nil, err
		}
	}
	now := time.Now().UTC()
	if selected == nil || now.Before(selected.certificate.Leaf.NotBefore) || !now.Before(selected.certificate.Leaf.NotAfter) {
		return nil, errors.New("active probe certificate unavailable")
	}
	return selected, nil
}

// CurrentIdentity supplies only the active public fingerprint and expiry to
// explicit operator/enrollment DTOs. It exposes neither key nor protected PEM.
func (m *EdgeTLSManager) CurrentIdentity(ctx context.Context) (string, time.Time, error) {
	selected, err := m.selection(ctx)
	if err != nil {
		return "", time.Time{}, err
	}
	return selected.fingerprint, selected.certificate.Leaf.NotAfter.UTC(), nil
}

// ConnContext binds one server connection to the certificate its TLS handshake
// actually selects. Install it alongside TLSConfig on the same HTTP server.
func (m *EdgeTLSManager) ConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, edgeTLSContextKey{}, &edgeTLSConnection{})
}

// TLSConfig requires TLS 1.3 and an observed certificate selection on every
// connection. Resumption is disabled so it cannot bypass the handshake binding.
func (m *EdgeTLSManager) TLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, SessionTicketsDisabled: true, GetCertificate: func(info *tls.ClientHelloInfo) (*tls.Certificate, error) {
		binding, ok := info.Context().Value(edgeTLSContextKey{}).(*edgeTLSConnection)
		if !ok {
			return nil, errors.New("missing TLS connection binding")
		}
		selected, err := m.selection(info.Context())
		if err != nil {
			return nil, err
		}
		binding.selection.Store(selected)
		return &selected.certificate, nil
	}}
}

func (m *EdgeTLSManager) bindEnrollment(ctx context.Context, binding domain.EdgeEnrollment) (domain.EdgeEnrollment, error) {
	connection, ok := ctx.Value(edgeTLSContextKey{}).(*edgeTLSConnection)
	if !ok {
		return domain.EdgeEnrollment{}, errors.New("missing TLS admission binding")
	}
	selected := connection.selection.Load()
	if selected == nil {
		return domain.EdgeEnrollment{}, errors.New("TLS admission certificate unavailable")
	}
	binding.CertificateFingerprint = selected.fingerprint
	binding.CertificateNotBefore = selected.certificate.Leaf.NotBefore.UTC()
	binding.CertificateNotAfter = selected.certificate.Leaf.NotAfter.UTC()
	return binding, nil
}
