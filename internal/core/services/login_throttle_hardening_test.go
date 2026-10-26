package services

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// collationUserRepo answers GetByUsername the way MariaDB's default
// utf8mb4_uca1400_ai_ci collation does for these spellings: case, accents,
// full-width letters and trailing spaces all match the same row.
type collationUserRepo struct{ *inMemUserRepo }

func (r collationUserRepo) GetByUsername(ctx context.Context, username string) (*domain.User, error) {
	var b strings.Builder
	for _, c := range strings.ToLower(strings.TrimRight(username, " ")) {
		switch {
		case c >= 0xFF01 && c <= 0xFF5E: // full-width ASCII
			c -= 0xFEE0
		case strings.ContainsRune("áàâä", c):
			c = 'a'
		case strings.ContainsRune("éèêë", c):
			c = 'e'
		}
		b.WriteRune(c)
	}
	return r.inMemUserRepo.GetByUsername(ctx, strings.ToLower(b.String()))
}

// lockedUserRepo serializes the in-memory repo's reads, which count calls,
// for the concurrency tests.
type lockedUserRepo struct {
	*inMemUserRepo
	mu sync.Mutex
}

func (r *lockedUserRepo) GetByID(ctx context.Context, id int64) (*domain.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inMemUserRepo.GetByID(ctx, id)
}

func (r *lockedUserRepo) GetByUsername(ctx context.Context, username string) (*domain.User, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inMemUserRepo.GetByUsername(ctx, username)
}

func newCollationThrottleService(t *testing.T) (*AuthService, *fakeClock) {
	t.Helper()
	clock := &fakeClock{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	auth := newFakeAuthenticator()
	svc := NewAuthService(collationUserRepo{newInMemUserRepo()}, newInMemAPIKeyRepo(), auth, newFakeTOTP(), WithClock(clock))
	if _, err := svc.Register(context.Background(), "alice", "supersecret"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	auth.seedHash("alice", "bcrypt$supersecret")
	return svc, clock
}

// Issue #90 follow-up: the per-account budget was keyed by the lower-cased
// input, so on MariaDB 'álice', 'ａｌｉｃｅ' and 'alice ' each got a fresh
// five guesses at the same row. Known users are now keyed by stored ID.
func TestLogin_CollationVariantsShareOneBudget(t *testing.T) {
	svc, _ := newCollationThrottleService(t)
	ctx := context.Background()
	variants := []string{"alice", "ALICE", "álice", "ａｌｉｃｅ", "alice ", "Álice"}
	for i := 0; i < LoginFailuresBeforeBackoff; i++ {
		name := variants[i%len(variants)]
		if _, err := svc.Login(ctx, name, "wrong-password"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d (%q) = %v; want ErrInvalidCredentials", i+1, name, err)
		}
	}
	for _, name := range variants {
		if _, err := svc.Login(ctx, name, "wrong-password"); !errors.Is(err, ErrTooManyAttempts) {
			t.Fatalf("Login(%q) after %d failures across spellings = %v; want ErrTooManyAttempts",
				name, LoginFailuresBeforeBackoff, err)
		}
	}
	// Unknown names keep their own normalized budget and do not touch alice's
	// from a different client.
	other := WithLoginClientIP(ctx, "198.51.100.7")
	if _, err := svc.Login(other, "mallory", "x"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user = %v; want ErrInvalidCredentials", err)
	}
}

// The strict budget belongs to one client: a remote guesser cannot lock the
// owner out from their own address. The account-wide budget is much larger.
func TestLogin_StrictBackoffIsPerClientIP(t *testing.T) {
	svc, _, _ := newThrottleTestService(t)
	attacker := WithLoginClientIP(context.Background(), "203.0.113.9")
	owner := WithLoginClientIP(context.Background(), "192.0.2.10")

	for i := 0; i < LoginFailuresBeforeBackoff; i++ {
		_, _ = svc.Login(attacker, "alice", "guess")
	}
	if _, err := svc.Login(attacker, "alice", "supersecret"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("attacker after %d failures = %v; want ErrTooManyAttempts", LoginFailuresBeforeBackoff, err)
	}
	if _, err := svc.Login(owner, "alice", "supersecret"); err != nil {
		t.Fatalf("owner from another IP = %v; want success (strict backoff is per client)", err)
	}
}

// Many clients together still hit the account-wide cap, which then applies
// to every client until it expires or an admin clears it.
func TestLogin_AccountWideBackoffAcrossClients(t *testing.T) {
	svc, _, clock := newThrottleTestService(t)
	ctx := context.Background()
	for i := 0; i < LoginAccountFailuresBeforeBackoff; i++ {
		ip := WithLoginClientIP(ctx, "203.0.113."+itoa(i%250+1)+"-"+itoa(i))
		if _, err := svc.Login(ip, "alice", "guess"); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("distributed attempt %d = %v; want ErrInvalidCredentials", i+1, err)
		}
	}
	fresh := WithLoginClientIP(ctx, "192.0.2.10")
	if _, err := svc.Login(fresh, "alice", "supersecret"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("after %d distributed failures = %v; want the account-wide ErrTooManyAttempts", LoginAccountFailuresBeforeBackoff, err)
	}
	clock.now = clock.now.Add(LoginBackoffBase + time.Second)
	if _, err := svc.Login(fresh, "alice", "supersecret"); err != nil {
		t.Fatalf("after the account backoff = %v; want success", err)
	}
}

// Unlock procedure (docs/RUNBOOK.md): an admin setting a new password
// clears every lockout on the account at once.
func TestUpdateUserPasswordClearsLoginLockout(t *testing.T) {
	svc, _, _ := newThrottleTestService(t)
	ctx := context.Background()
	failLogins(t, svc, "alice", LoginFailuresBeforeBackoff)
	if _, err := svc.Login(ctx, "alice", "supersecret"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("precondition: Login = %v; want locked", err)
	}
	// Same value as before: the fake authenticator's session issuer only
	// knows the seeded hash. What matters is that the reset clears the lock.
	pw := "supersecret"
	if _, err := svc.UpdateUser(ctx, 1, nil, nil, nil, nil, &pw, CapabilityUpdate{}); err != nil {
		t.Fatalf("UpdateUser: %v", err)
	}
	if _, err := svc.Login(ctx, "alice", pw); err != nil {
		t.Fatalf("Login after admin password reset = %v; want success", err)
	}
}

// blockingAuthenticator holds every password check until released, so the
// test can line up many attempts that are all past the throttle check at
// once — the window the old check-then-record order left open.
type blockingAuthenticator struct {
	*fakeAuthenticator
	entered atomic.Int32
	arrived chan struct{}
	release chan struct{}
}

func (a *blockingAuthenticator) VerifyPassword(hashed, pw string) error {
	a.entered.Add(1)
	a.arrived <- struct{}{}
	<-a.release
	return a.fakeAuthenticator.VerifyPassword(hashed, pw)
}

func TestLogin_ConcurrentAttemptsCannotExceedTheCap(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)}
	base := newFakeAuthenticator()
	auth := &blockingAuthenticator{fakeAuthenticator: base, arrived: make(chan struct{}, 64), release: make(chan struct{})}
	svc := NewAuthService(&lockedUserRepo{inMemUserRepo: newInMemUserRepo()}, newInMemAPIKeyRepo(), base, newFakeTOTP(), WithClock(clock))
	if _, err := svc.Register(context.Background(), "alice", "supersecret"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	base.seedHash("alice", "bcrypt$supersecret")
	svc.auth = auth // Register used the plain fake; from here every check blocks

	const parallel = 40
	ctx := WithLoginClientIP(context.Background(), "203.0.113.50")
	var wg sync.WaitGroup
	var refused, invalid atomic.Int32
	done := make(chan struct{}, parallel)
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { done <- struct{}{} }()
			_, err := svc.Login(ctx, "alice", "wrong-password")
			switch {
			case errors.Is(err, ErrTooManyAttempts):
				refused.Add(1)
			case errors.Is(err, ErrInvalidCredentials):
				invalid.Add(1)
			default:
				t.Errorf("Login = %v", err)
			}
		}()
	}
	// Wait until every goroutine is either refused or parked in bcrypt.
	parked, finished := 0, 0
	for parked+finished < parallel {
		select {
		case <-auth.arrived:
			parked++
		case <-done:
			finished++
		case <-time.After(10 * time.Second):
			t.Fatalf("stalled: %d parked, %d finished", parked, finished)
		}
	}
	close(auth.release)
	wg.Wait()

	if got := auth.entered.Load(); got > LoginFailuresBeforeBackoff {
		t.Fatalf("%d concurrent attempts reached the password check; the cap is %d", got, LoginFailuresBeforeBackoff)
	}
	if invalid.Load() != auth.entered.Load() || refused.Load() != parallel-auth.entered.Load() {
		t.Fatalf("invalid=%d refused=%d checked=%d", invalid.Load(), refused.Load(), auth.entered.Load())
	}
	if _, err := svc.Login(ctx, "alice", "supersecret"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("after the burst = %v; want the client locked out", err)
	}
}

// Parallel wrong codes on one 2FA ticket cannot exceed the per-ticket cap.
func TestComplete2FALogin_ConcurrentCodesRespectTicketCap(t *testing.T) {
	svc, totp, _ := newThrottleTestService(t)
	ctx := context.Background()
	enableTOTPForAlice(t, svc, totp)
	svc.users = &lockedUserRepo{inMemUserRepo: svc.users.(*inMemUserRepo)}
	ticket, _, err := svc.Begin2FALogin(ctx, "alice", "supersecret")
	if err != nil {
		t.Fatalf("Begin2FALogin: %v", err)
	}
	var checked atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct client IPs: only the ticket cap can stop these.
			c := WithLoginClientIP(ctx, "198.51.100."+itoa(i))
			if _, _, err := svc.Complete2FALogin(c, ticket, "000000"); errors.Is(err, ErrTOTPInvalid) {
				checked.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if got := checked.Load(); got > Max2FAAttemptsPerTicket {
		t.Fatalf("%d codes were checked on one ticket; the cap is %d", got, Max2FAAttemptsPerTicket)
	}
}

func itoa(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{digits[i%10]}, b...)
	}
	return string(b)
}
