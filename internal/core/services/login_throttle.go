package services

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

// Login brute-force limits (issue #90). The HTTP layer throttles by client
// IP; these limits follow the account and the 2FA ticket, so rotating IPs
// does not buy an attacker more guesses.
const (
	// LoginFailuresBeforeBackoff is how many consecutive failed password or
	// TOTP attempts an account absorbs before it is locked out.
	LoginFailuresBeforeBackoff = 5
	// LoginBackoffBase is the first lockout; each further failure doubles it.
	LoginBackoffBase = 30 * time.Second
	// LoginBackoffMax caps the lockout so a legitimate user is never locked
	// out for long by someone else guessing at their username.
	LoginBackoffMax = 15 * time.Minute
	// Max2FAAttemptsPerTicket is how many wrong TOTP codes one login ticket
	// survives. After that the ticket is spent even for the right code, and
	// the user must sign in with the password again.
	Max2FAAttemptsPerTicket = 5

	// loginThrottleMaxKeys bounds memory under a username-spraying attack.
	loginThrottleMaxKeys = 10000
	// loginFailureWindow restarts the count when the previous failure is
	// older than this, so occasional typos never add up to a lockout.
	loginFailureWindow = time.Hour
	// loginThrottleIdleTTL forgets an account that has not failed recently.
	loginThrottleIdleTTL = 24 * time.Hour
	// ticketAttemptTTL outlives the 5-minute JWT ticket it tracks.
	ticketAttemptTTL = 10 * time.Minute
)

// ErrTooManyAttempts is returned while an account is in login backoff or
// when a 2FA ticket has used up its attempts. It is checked before the
// password, so it says nothing about whether the password was right.
var ErrTooManyAttempts = &AuthServiceError{Code: "too_many_attempts", Message: "too many failed attempts; try again later"}

type accountFailures struct {
	failures    int
	lockedUntil time.Time
	lastFailure time.Time
}

type ticketFailures struct {
	failures  int
	expiresAt time.Time
}

// loginThrottle is per-process. With several API replicas each counts on its
// own, so the effective account limit scales with the replica count; the
// Redis-backed per-IP limiter in front of the auth routes is shared.
type loginThrottle struct {
	mu       sync.Mutex
	accounts map[string]*accountFailures
	tickets  map[string]*ticketFailures
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{
		accounts: make(map[string]*accountFailures),
		tickets:  make(map[string]*ticketFailures),
	}
}

func accountKey(username string) string { return strings.ToLower(strings.TrimSpace(username)) }

func ticketKey(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

// locked reports whether the account is in backoff and for how long.
func (t *loginThrottle) locked(username string, now time.Time) (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	a, ok := t.accounts[accountKey(username)]
	if !ok || !now.Before(a.lockedUntil) {
		return 0, false
	}
	return a.lockedUntil.Sub(now), true
}

// fail records a failed attempt and returns the consecutive failure count.
func (t *loginThrottle) fail(username string, now time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := accountKey(username)
	a, ok := t.accounts[key]
	if !ok {
		if len(t.accounts) >= loginThrottleMaxKeys {
			t.pruneLocked(now)
		}
		if len(t.accounts) >= loginThrottleMaxKeys {
			// Still full of live entries: refuse to grow. The per-IP limiter
			// still applies, and existing lockouts stay in force.
			return 0
		}
		a = &accountFailures{}
		t.accounts[key] = a
	} else if !now.Before(a.lockedUntil) && now.Sub(a.lastFailure) > loginFailureWindow {
		a.failures = 0
	}
	a.failures++
	a.lastFailure = now
	if a.failures >= LoginFailuresBeforeBackoff {
		a.lockedUntil = now.Add(loginBackoff(a.failures))
	}
	return a.failures
}

// succeed clears the account's failure history after a completed login.
func (t *loginThrottle) succeed(username string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.accounts, accountKey(username))
}

// ticketSpent reports whether the ticket has used up its TOTP attempts.
func (t *loginThrottle) ticketSpent(ticket string, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.tickets[ticketKey(ticket)]
	return ok && now.Before(e.expiresAt) && e.failures >= Max2FAAttemptsPerTicket
}

// ticketFail records a wrong TOTP code against the ticket.
func (t *loginThrottle) ticketFail(ticket string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	key := ticketKey(ticket)
	e, ok := t.tickets[key]
	if !ok || !now.Before(e.expiresAt) {
		if len(t.tickets) >= loginThrottleMaxKeys {
			t.pruneLocked(now)
		}
		e = &ticketFailures{expiresAt: now.Add(ticketAttemptTTL)}
		t.tickets[key] = e
	}
	e.failures++
}

// spendTicket marks a ticket used so it cannot be replayed after a success.
func (t *loginThrottle) spendTicket(ticket string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tickets[ticketKey(ticket)] = &ticketFailures{failures: Max2FAAttemptsPerTicket, expiresAt: now.Add(ticketAttemptTTL)}
}

func (t *loginThrottle) pruneLocked(now time.Time) {
	for k, a := range t.accounts {
		if !now.Before(a.lockedUntil) && now.Sub(a.lastFailure) > loginThrottleIdleTTL {
			delete(t.accounts, k)
		}
	}
	for k, e := range t.tickets {
		if !now.Before(e.expiresAt) {
			delete(t.tickets, k)
		}
	}
}

// loginBackoff is LoginBackoffBase doubled for each failure past the
// threshold, capped at LoginBackoffMax.
func loginBackoff(failures int) time.Duration {
	d := LoginBackoffBase
	for i := LoginFailuresBeforeBackoff; i < failures; i++ {
		d *= 2
		if d >= LoginBackoffMax {
			return LoginBackoffMax
		}
	}
	return d
}
