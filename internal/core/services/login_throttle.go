package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Login brute-force limits (issue #90). The HTTP layer throttles by client
// IP; these limits follow the account and the 2FA ticket, so rotating IPs
// does not buy an attacker unlimited guesses.
//
// Two budgets apply to every password or TOTP attempt:
//
//   - a strict one per (account, client IP): a handful of failures from one
//     address locks that address out of that account, with growing backoff;
//   - a much larger one per account across all addresses, which caps a
//     distributed guess rate without letting a single remote client lock the
//     real owner (often the sole admin) out from everywhere.
//
// Attempts are reserved before the password hash is checked and settled
// afterwards, so parallel requests cannot all pass the check before any of
// them records its failure. Counters are per process and in memory; see
// docs/RUNBOOK.md ("Sign-in lockout") for how to clear one.
const (
	// LoginFailuresBeforeBackoff is how many consecutive failed password or
	// TOTP attempts one client IP may make against one account before that
	// pair is locked out.
	LoginFailuresBeforeBackoff = 5
	// LoginAccountFailuresBeforeBackoff is how many failures an account
	// absorbs across all client IPs before it is locked for everyone.
	LoginAccountFailuresBeforeBackoff = 50
	// LoginBackoffBase is the first lockout; each further failure doubles it.
	LoginBackoffBase = 30 * time.Second
	// LoginBackoffMax caps the lockout so a legitimate user is never locked
	// out for long by someone else guessing at their username.
	LoginBackoffMax = 15 * time.Minute
	// Max2FAAttemptsPerTicket is how many TOTP codes one login ticket
	// accepts. After that the ticket is spent even for the right code, and
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

type loginClientIPKey struct{}

// WithLoginClientIP returns ctx carrying the client address a sign-in
// attempt came from. The HTTP layer sets it; the per-(account, IP) backoff
// keys on it. Without it every caller shares one empty-address bucket.
func WithLoginClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, loginClientIPKey{}, strings.TrimSpace(ip))
}

func loginClientIP(ctx context.Context) string {
	ip, _ := ctx.Value(loginClientIPKey{}).(string)
	return ip
}

// attemptCounter tracks consecutive failures for one throttle key.
type attemptCounter struct {
	failures    int
	pending     int // reserved attempts not yet settled
	lockedUntil time.Time
	lastFailure time.Time
}

type ticketFailures struct {
	failures  int
	expiresAt time.Time
}

// loginAttempt is one reserved attempt. Exactly one of fail, succeed or
// release settles it; later calls are no-ops.
type loginAttempt struct {
	account string
	pair    string
	settled bool
}

// loginThrottle is per-process. With several API replicas each counts on its
// own, so the effective account limit scales with the replica count; the
// Redis-backed per-IP limiter in front of the auth routes is shared.
type loginThrottle struct {
	mu       sync.Mutex
	pairs    map[string]*attemptCounter // account + client IP
	accounts map[string]*attemptCounter // account, every client IP
	tickets  map[string]*ticketFailures
}

func newLoginThrottle() *loginThrottle {
	return &loginThrottle{
		pairs:    make(map[string]*attemptCounter),
		accounts: make(map[string]*attemptCounter),
		tickets:  make(map[string]*ticketFailures),
	}
}

// userAccountKey identifies a stored user. Known users are keyed by ID, not by
// the typed name: MariaDB's default collation matches 'ádmin', 'ＡＤＭＩＮ' and
// 'admin ' to the same row, and each spelling must not get its own budget.
func userAccountKey(userID int64) string { return "id:" + strconv.FormatInt(userID, 10) }

// unknownAccountKey identifies a username with no account behind it, so
// guessing at one is throttled like guessing at a real one.
func unknownAccountKey(username string) string {
	return "name:" + strings.ToLower(strings.TrimSpace(username))
}

func pairKey(account, ip string) string { return account + "|" + ip }

func ticketKey(ticket string) string {
	sum := sha256.Sum256([]byte(ticket))
	return hex.EncodeToString(sum[:])
}

// reserve claims one attempt against account from ip. It refuses while
// either budget is locked, or when the attempts already in flight would use
// up what is left of it, and reports how long the caller should wait.
func (t *loginThrottle) reserve(account, ip string, now time.Time) (*loginAttempt, time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	pk := pairKey(account, ip)
	pair := t.counterLocked(t.pairs, pk, now)
	acct := t.counterLocked(t.accounts, account, now)
	for _, c := range []struct {
		ctr       *attemptCounter
		threshold int
	}{{pair, LoginFailuresBeforeBackoff}, {acct, LoginAccountFailuresBeforeBackoff}} {
		if c.ctr == nil {
			continue // table full of live entries: untracked, the IP limiter still applies
		}
		if now.Before(c.ctr.lockedUntil) {
			return nil, c.ctr.lockedUntil.Sub(now), false
		}
		if c.ctr.pending >= attemptBudget(c.ctr.failures, c.threshold) {
			return nil, LoginBackoffBase, false
		}
	}
	att := &loginAttempt{account: account, pair: pk}
	if pair != nil {
		pair.pending++
	}
	if acct != nil {
		acct.pending++
	}
	return att, 0, true
}

// attemptBudget is how many attempts may be in flight for a counter: what is
// left before the threshold, or a single probe once a lockout has expired.
func attemptBudget(failures, threshold int) int {
	if failures >= threshold {
		return 1
	}
	return threshold - failures
}

// counterLocked returns the counter for key, creating it, or nil when the
// table is full of live entries. A counter idle past the failure window
// starts over.
func (t *loginThrottle) counterLocked(table map[string]*attemptCounter, key string, now time.Time) *attemptCounter {
	c, ok := table[key]
	if !ok {
		if len(table) >= loginThrottleMaxKeys {
			t.pruneLocked(now)
		}
		if len(table) >= loginThrottleMaxKeys {
			return nil
		}
		c = &attemptCounter{}
		table[key] = c
		return c
	}
	if c.pending == 0 && c.failures > 0 && !now.Before(c.lockedUntil) && now.Sub(c.lastFailure) > loginFailureWindow {
		c.failures = 0
	}
	return c
}

// fail settles att as a failed credential check and returns the pair's and
// the account's consecutive failure counts.
func (t *loginThrottle) fail(att *loginAttempt, now time.Time) (pairFailures, accountFailures int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if att == nil || att.settled {
		return 0, 0
	}
	att.settled = true
	record := func(c *attemptCounter, threshold int) int {
		if c == nil {
			return 0
		}
		if c.pending > 0 {
			c.pending--
		}
		c.failures++
		c.lastFailure = now
		if c.failures >= threshold {
			c.lockedUntil = now.Add(loginBackoff(c.failures, threshold))
		}
		return c.failures
	}
	return record(t.pairs[att.pair], LoginFailuresBeforeBackoff),
		record(t.accounts[att.account], LoginAccountFailuresBeforeBackoff)
}

// succeed settles att as a completed sign-in: the client's pair history and
// the account's failure count start over.
func (t *loginThrottle) succeed(att *loginAttempt) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if att == nil || att.settled {
		return
	}
	att.settled = true
	delete(t.pairs, att.pair)
	if c := t.accounts[att.account]; c != nil {
		if c.pending > 0 {
			c.pending--
		}
		c.failures = 0
		c.lockedUntil = time.Time{}
	}
}

// release settles att without a verdict on the credential (inactive account,
// TOTP still required, a storage error): the reservation is returned.
func (t *loginThrottle) release(att *loginAttempt) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if att == nil || att.settled {
		return
	}
	att.settled = true
	for _, c := range []*attemptCounter{t.pairs[att.pair], t.accounts[att.account]} {
		if c != nil && c.pending > 0 {
			c.pending--
		}
	}
}

// clearAccount forgets every lockout and failure for a stored user. An admin
// resetting the user's password or (re)activating the account calls it.
func (t *loginThrottle) clearAccount(userID int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	account := userAccountKey(userID)
	if c := t.accounts[account]; c != nil {
		c.failures, c.lockedUntil = 0, time.Time{}
	}
	prefix := account + "|"
	for k, c := range t.pairs {
		if strings.HasPrefix(k, prefix) {
			c.failures, c.lockedUntil = 0, time.Time{}
		}
	}
}

// reserveTicket claims one TOTP attempt on a 2FA ticket before the code is
// checked, so parallel guesses on one ticket cannot exceed the cap.
func (t *loginThrottle) reserveTicket(ticket string, now time.Time) bool {
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
	if e.failures >= Max2FAAttemptsPerTicket {
		return false
	}
	e.failures++
	return true
}

// spendTicket marks a ticket used so it cannot be replayed after a success.
func (t *loginThrottle) spendTicket(ticket string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.tickets[ticketKey(ticket)] = &ticketFailures{failures: Max2FAAttemptsPerTicket, expiresAt: now.Add(ticketAttemptTTL)}
}

func (t *loginThrottle) pruneLocked(now time.Time) {
	for _, table := range []map[string]*attemptCounter{t.pairs, t.accounts} {
		for k, c := range table {
			if c.pending == 0 && !now.Before(c.lockedUntil) && now.Sub(c.lastFailure) > loginThrottleIdleTTL {
				delete(table, k)
			}
		}
	}
	for k, e := range t.tickets {
		if !now.Before(e.expiresAt) {
			delete(t.tickets, k)
		}
	}
}

// loginBackoff is LoginBackoffBase doubled for each failure past threshold,
// capped at LoginBackoffMax.
func loginBackoff(failures, threshold int) time.Duration {
	d := LoginBackoffBase
	for i := threshold; i < failures; i++ {
		d *= 2
		if d >= LoginBackoffMax {
			return LoginBackoffMax
		}
	}
	return d
}
