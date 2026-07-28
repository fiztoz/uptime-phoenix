package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// webAuthnSessionTTL bounds how long a passkey ceremony (begin → finish) may
// stay open. WebAuthn challenges are single-use and short-lived; two minutes
// is generous for a human completing an authenticator prompt while keeping the
// pending-challenge window small.
const webAuthnSessionTTL = 2 * time.Minute

// webAuthnSessionStore is a tiny in-memory, TTL-bounded store for WebAuthn
// ceremony state.
//
// CHALLENGE-STORE CHOICE: we keep ceremony state server-side in memory keyed by
// a random session id handed to the client, rather than in a transient DB
// table. WebAuthn challenges are inherently single-process and single-use, live
// for seconds, and never need to survive a restart (the client simply restarts
// the ceremony). An in-memory map is the simplest correct option and avoids a
// migration + write amplification for ephemeral data. The trade-off is that
// ceremonies do not survive a process restart and are not shared across
// replicas — acceptable because a restarted/other replica just returns
// ErrWebAuthnChallenge and the browser retries the (idempotent) begin step.
type webAuthnSessionStore struct {
	mu       sync.Mutex
	sessions map[string]webAuthnSession
}

type webAuthnSession struct {
	userID  int64
	data    []byte
	expires time.Time
}

func newWebAuthnSessionStore() *webAuthnSessionStore {
	return &webAuthnSessionStore{sessions: make(map[string]webAuthnSession)}
}

// put stores ceremony data for userID and returns the opaque session id.
func (s *webAuthnSessionStore) put(userID int64, data []byte, now time.Time) (string, error) {
	id, err := randomSessionID()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	s.sessions[id] = webAuthnSession{userID: userID, data: data, expires: now.Add(webAuthnSessionTTL)}
	return id, nil
}

// take returns and removes the ceremony data for id, enforcing TTL and that
// the session belongs to the expected user. A single-use design: a session is
// always consumed on lookup.
func (s *webAuthnSessionStore) take(id string, userID int64, now time.Time) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	delete(s.sessions, id)
	if now.After(sess.expires) || sess.userID != userID {
		return nil, false
	}
	return sess.data, true
}

func (s *webAuthnSessionStore) pruneLocked(now time.Time) {
	for id, sess := range s.sessions {
		if now.After(sess.expires) {
			delete(s.sessions, id)
		}
	}
}

func randomSessionID() (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("webauthn session id: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// webAuthnUserFor builds the port view of a user plus their stored credentials.
func (s *AuthService) webAuthnUserFor(ctx context.Context, u *domain.User) (ports.WebAuthnUser, error) {
	creds, err := s.webAuthnCreds.ListByUser(ctx, u.ID)
	if err != nil {
		return ports.WebAuthnUser{}, fmt.Errorf("auth service: list passkeys: %w", err)
	}
	return ports.WebAuthnUser{
		ID:          u.ID,
		Username:    u.Username,
		DisplayName: u.Username,
		Credentials: creds,
	}, nil
}

// BeginPasskeyRegistration starts a passkey registration ceremony for an
// already-authenticated user. It returns the creation options JSON to forward
// to navigator.credentials.create() and a session id the client must echo back
// to FinishPasskeyRegistration.
func (s *AuthService) BeginPasskeyRegistration(ctx context.Context, userID int64) (options []byte, sessionID string, err error) {
	if s.webAuthn == nil || s.webAuthnCreds == nil {
		return nil, "", fmt.Errorf("%w", ErrWebAuthnNotConfigured)
	}
	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return nil, "", fmt.Errorf("auth service: begin passkey registration: %w", err)
	}
	wu, err := s.webAuthnUserFor(ctx, user)
	if err != nil {
		return nil, "", err
	}
	opts, session, err := s.webAuthn.BeginRegistration(wu)
	if err != nil {
		return nil, "", fmt.Errorf("auth service: begin passkey registration: %w", err)
	}
	sessionID, err = s.webAuthnSessions.put(userID, session, s.clock.Now())
	if err != nil {
		return nil, "", fmt.Errorf("auth service: store passkey challenge: %w", err)
	}
	return opts, sessionID, nil
}

// FinishPasskeyRegistration validates the authenticator response against the
// stored ceremony session and persists the new credential under the given
// name/label.
func (s *AuthService) FinishPasskeyRegistration(ctx context.Context, userID int64, sessionID, name string, response []byte) (*domain.WebAuthnCredential, error) {
	if s.webAuthn == nil || s.webAuthnCreds == nil {
		return nil, fmt.Errorf("%w", ErrWebAuthnNotConfigured)
	}
	session, ok := s.webAuthnSessions.take(sessionID, userID, s.clock.Now())
	if !ok {
		return nil, fmt.Errorf("%w", ErrWebAuthnChallenge)
	}
	user, err := s.users.GetByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("auth service: finish passkey registration: %w", err)
	}
	wu, err := s.webAuthnUserFor(ctx, user)
	if err != nil {
		return nil, err
	}
	cred, err := s.webAuthn.FinishRegistration(wu, session, response)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrWebAuthnChallenge, err)
	}
	cred.UserID = userID
	cred.Name = name
	cred.CreatedAt = s.clock.Now()
	if err := s.webAuthnCreds.Create(ctx, cred); err != nil {
		return nil, fmt.Errorf("auth service: save passkey: %w", err)
	}
	return cred, nil
}

// ListPasskeys returns the user's registered passkeys.
func (s *AuthService) ListPasskeys(ctx context.Context, userID int64) ([]*domain.WebAuthnCredential, error) {
	if s.webAuthnCreds == nil {
		return nil, fmt.Errorf("%w", ErrWebAuthnNotConfigured)
	}
	creds, err := s.webAuthnCreds.ListByUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("auth service: list passkeys: %w", err)
	}
	return creds, nil
}

// DeletePasskey removes one of the user's passkeys.
func (s *AuthService) DeletePasskey(ctx context.Context, userID, credID int64) error {
	if s.webAuthnCreds == nil {
		return fmt.Errorf("%w", ErrWebAuthnNotConfigured)
	}
	if err := s.webAuthnCreds.Delete(ctx, credID, userID); err != nil {
		return fmt.Errorf("auth service: delete passkey: %w", err)
	}
	return nil
}

// BeginPasskeyLogin starts a passwordless passkey assertion ceremony for the
// named user. The username → assertion flow is a first-factor (passwordless)
// login: a valid assertion alone yields a session, so TOTP is not consulted.
// TOTP remains fully available for the password login path.
func (s *AuthService) BeginPasskeyLogin(ctx context.Context, username string) (options []byte, sessionID string, err error) {
	if s.webAuthn == nil || s.webAuthnCreds == nil {
		return nil, "", fmt.Errorf("%w", ErrWebAuthnNotConfigured)
	}
	user, err := s.users.GetByUsername(ctx, username)
	if err != nil {
		if isNotFound(err) {
			return nil, "", fmt.Errorf("%w", ErrInvalidCredentials)
		}
		return nil, "", fmt.Errorf("auth service: begin passkey login: %w", err)
	}
	if !user.Active {
		return nil, "", fmt.Errorf("%w", ErrUserInactive)
	}
	wu, err := s.webAuthnUserFor(ctx, user)
	if err != nil {
		return nil, "", err
	}
	if len(wu.Credentials) == 0 {
		return nil, "", fmt.Errorf("%w", ErrWebAuthnNoCredentials)
	}
	opts, session, err := s.webAuthn.BeginLogin(wu)
	if err != nil {
		return nil, "", fmt.Errorf("auth service: begin passkey login: %w", err)
	}
	sessionID, err = s.webAuthnSessions.put(user.ID, session, s.clock.Now())
	if err != nil {
		return nil, "", fmt.Errorf("auth service: store passkey challenge: %w", err)
	}
	return opts, sessionID, nil
}

// FinishPasskeyLogin validates the assertion, updates the credential's
// signature counter, and issues a session JWT via the existing session path.
func (s *AuthService) FinishPasskeyLogin(ctx context.Context, username, sessionID string, response []byte) (string, *domain.User, error) {
	if s.webAuthn == nil || s.webAuthnCreds == nil {
		return "", nil, fmt.Errorf("%w", ErrWebAuthnNotConfigured)
	}
	user, err := s.users.GetByUsername(ctx, username)
	if err != nil {
		if isNotFound(err) {
			return "", nil, fmt.Errorf("%w", ErrInvalidCredentials)
		}
		return "", nil, fmt.Errorf("auth service: finish passkey login: %w", err)
	}
	if !user.Active {
		return "", nil, fmt.Errorf("%w", ErrUserInactive)
	}
	session, ok := s.webAuthnSessions.take(sessionID, user.ID, s.clock.Now())
	if !ok {
		return "", nil, fmt.Errorf("%w", ErrWebAuthnChallenge)
	}
	wu, err := s.webAuthnUserFor(ctx, user)
	if err != nil {
		return "", nil, err
	}
	cred, err := s.webAuthn.FinishLogin(wu, session, response)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrWebAuthnChallenge, err)
	}
	// Persist every mutable authenticator field needed by future assertions.
	stored, err := s.webAuthnCreds.GetByCredentialID(ctx, cred.CredentialID)
	if err != nil {
		return "", nil, fmt.Errorf("auth service: lookup passkey: %w", err)
	}
	if updateErr := s.webAuthnCreds.UpdateUsage(ctx, stored.ID, cred.SignCount, cred.Flags, cred.CloneWarning, cred.Attachment, s.clock.Now()); updateErr != nil {
		return "", nil, fmt.Errorf("auth service: update passkey usage: %w", updateErr)
	}
	token, err := s.auth.IssueSession(ctx, user.ID)
	if err != nil {
		return "", nil, fmt.Errorf("auth service: issue session: %w", err)
	}
	return token, user, nil
}
