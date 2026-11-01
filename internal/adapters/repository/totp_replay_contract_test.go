package repository_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// totpReplayFixture is one real database reached through independent
// connection pools, standing in for separate hub processes (or one hub before
// and after a restart) that share the database.
type totpReplayFixture struct {
	engine string
	db     *bun.DB // migration handle
	open   func(t *testing.T) *bun.DB
}

func (f totpReplayFixture) users(db *bun.DB) ports.UserRepository {
	if f.engine == "mariadb" {
		return mariadb.NewUserRepo(db)
	}
	return sqlite.NewUserRepo(db)
}

func (f totpReplayFixture) apiKeys(db *bun.DB) ports.APIKeyRepository {
	if f.engine == "mariadb" {
		return mariadb.NewAPIKeyRepo(db)
	}
	return sqlite.NewAPIKeyRepo(db)
}

func newTOTPReplayFixture(t *testing.T, engine string) totpReplayFixture {
	t.Helper()
	var open func(t *testing.T) *bun.DB
	switch engine {
	case "sqlite":
		dsn := "file:" + filepath.Join(t.TempDir(), "totp.db")
		open = func(t *testing.T) *bun.DB {
			db, err := sqlite.NewDB(dsn)
			if err != nil {
				t.Fatalf("open SQLite: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return db
		}
	case "mariadb":
		dsn := os.Getenv("TEST_MARIADB_DSN")
		if dsn == "" {
			t.Skip("TEST_MARIADB_DSN is unset; skipping MariaDB TOTP replay contract")
		}
		validateMariaDBDSN(t, dsn)
		open = func(t *testing.T) *bun.DB {
			db, err := mariadb.NewDB(dsn)
			if err != nil {
				t.Fatalf("open MariaDB from TEST_MARIADB_DSN: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return db
		}
	}
	db := open(t)
	if err := repository.RunMigrations(db.DB, engine); err != nil {
		t.Fatalf("run %s migrations: %v", engine, err)
	}
	if engine == "mariadb" {
		healMariaDBTail(t, db)
		resetMariaDB(t, db.DB)
	}
	return totpReplayFixture{engine: engine, db: db, open: open}
}

// totpTestClock is a settable clock shared by every service instance.
type totpTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *totpTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *totpTestClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// totpReplayHub is one service instance with its own connection pool.
type totpReplayHub struct {
	svc   *services.AuthService
	users ports.UserRepository
}

func (f totpReplayFixture) hub(t *testing.T, clock *totpTestClock) totpReplayHub {
	t.Helper()
	db := f.open(t)
	users := f.users(db)
	authn := auth.NewJWTAuthenticator("totp-replay-contract-signing-key-0123456789", 1, users)
	svc := services.NewAuthService(users, f.apiKeys(db), authn, auth.NewTOTPProvider("phoenix-test"), services.WithClock(clock))
	return totpReplayHub{svc: svc, users: users}
}

func totpContractCode(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := totp.GenerateCodeCustom(secret, at, totp.ValidateOpts{
		Period: auth.TOTPPeriod, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1,
	})
	if err != nil {
		t.Fatalf("generate TOTP code: %v", err)
	}
	return code
}

// enrolTOTPUser creates an active user and enables TOTP through hub a,
// confirming with the code of the clock's current step. It returns the
// user and secret.
func enrolTOTPUser(t *testing.T, ctx context.Context, a totpReplayHub, clock *totpTestClock) (*domain.User, string) {
	t.Helper()
	hash, err := auth.NewPasswordHasherWithCost(4).Hash("supersecret")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	user := &domain.User{Username: "totp-user", PasswordHash: hash, Active: true, Timezone: "UTC"}
	if err := a.users.Create(ctx, user); err != nil {
		t.Fatalf("Create user: %v", err)
	}
	secret, _, err := a.svc.SetupTOTP(ctx, user.ID)
	if err != nil {
		t.Fatalf("SetupTOTP: %v", err)
	}
	if err := a.svc.EnableTOTP(ctx, user.ID, totpContractCode(t, secret, clock.Now())); err != nil {
		t.Fatalf("EnableTOTP: %v", err)
	}
	clock.advance(auth.TOTPPeriod * time.Second)
	return user, secret
}

func beginTOTPTicket(t *testing.T, ctx context.Context, h totpReplayHub) string {
	t.Helper()
	ticket, _, err := h.svc.Begin2FALogin(ctx, "totp-user", "supersecret")
	if err != nil || ticket == "" {
		t.Fatalf("Begin2FALogin = %q, %v; want a ticket", ticket, err)
	}
	return ticket
}

func TestTOTPReplayContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			runTOTPReplayContract(t, engine)
		})
	}
}

func runTOTPReplayContract(t *testing.T, engine string) {
	start := time.Date(2026, 10, 30, 5, 0, 0, 0, time.UTC)

	t.Run("ConcurrentHubsSameStepExactlyOneWins", func(t *testing.T) {
		f := newTOTPReplayFixture(t, engine)
		ctx := context.Background()
		clock := &totpTestClock{now: start}
		_, secret := enrolTOTPUser(t, ctx, f.hub(t, clock), clock)
		for round := 0; round < 5; round++ {
			// Fresh hubs each round: the losing hub's in-process login
			// throttle must not decide the next round.
			a, b := f.hub(t, clock), f.hub(t, clock)
			code := totpContractCode(t, secret, clock.Now())
			tickets := []string{beginTOTPTicket(t, ctx, a), beginTOTPTicket(t, ctx, b)}
			hubs := []totpReplayHub{a, b}
			tokens := make([]string, 2)
			errs := make([]error, 2)
			var wg sync.WaitGroup
			gate := make(chan struct{})
			for i := range hubs {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					<-gate
					tokens[i], _, errs[i] = hubs[i].svc.Complete2FALogin(ctx, tickets[i], code)
				}(i)
			}
			close(gate)
			wg.Wait()
			wins := 0
			for i := range hubs {
				switch {
				case errs[i] == nil && tokens[i] != "":
					wins++
				case errors.Is(errs[i], services.ErrTOTPInvalid) && tokens[i] == "":
				default:
					t.Fatalf("round %d hub %d = %q, %v; want a session or ErrTOTPInvalid", round, i, tokens[i], errs[i])
				}
			}
			if wins != 1 {
				t.Fatalf("round %d: %d hubs accepted the same step; want exactly 1", round, wins)
			}
			clock.advance(auth.TOTPPeriod * time.Second)
		}
	})

	t.Run("ConcurrentConsumeSameStepExactlyOneWins", func(t *testing.T) {
		f := newTOTPReplayFixture(t, engine)
		ctx := context.Background()
		clock := &totpTestClock{now: start}
		a := f.hub(t, clock)
		user, _ := enrolTOTPUser(t, ctx, a, clock)
		current, err := a.users.GetByID(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		repos := make([]ports.UserRepository, 6)
		for i := range repos {
			repos[i] = f.users(f.open(t))
		}
		step := clock.Now().Unix() / auth.TOTPPeriod
		results := make([]bool, len(repos))
		errs := make([]error, len(repos))
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for i := range repos {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-gate
				results[i], errs[i] = repos[i].ConsumeTOTPStep(ctx, user.ID, current.TOTPGeneration, step)
			}(i)
		}
		close(gate)
		wg.Wait()
		wins := 0
		for i := range repos {
			if errs[i] != nil {
				t.Fatalf("consume %d: %v", i, errs[i])
			}
			if results[i] {
				wins++
			}
		}
		if wins != 1 {
			t.Fatalf("%d concurrent consumes of one step won; want exactly 1", wins)
		}
	})

	t.Run("ReuseAfterRestartRefusedNewStepWorks", func(t *testing.T) {
		f := newTOTPReplayFixture(t, engine)
		ctx := context.Background()
		clock := &totpTestClock{now: start}
		a := f.hub(t, clock)
		_, secret := enrolTOTPUser(t, ctx, a, clock)
		code := totpContractCode(t, secret, clock.Now())
		if tok, _, err := a.svc.Complete2FALogin(ctx, beginTOTPTicket(t, ctx, a), code); err != nil || tok == "" {
			t.Fatalf("first login = %q, %v; want a session", tok, err)
		}
		// A fresh service with a fresh pool: nothing in process memory.
		restarted := f.hub(t, clock)
		if tok, _, err := restarted.svc.Complete2FALogin(ctx, beginTOTPTicket(t, ctx, restarted), code); !errors.Is(err, services.ErrTOTPInvalid) || tok != "" {
			t.Fatalf("reuse after restart = %q, %v; want ErrTOTPInvalid and no session", tok, err)
		}
		if tok, err := restarted.svc.LoginWith2FA(ctx, "totp-user", "supersecret", code); !errors.Is(err, services.ErrTOTPInvalid) || tok != "" {
			t.Fatalf("LoginWith2FA reuse after restart = %q, %v; want ErrTOTPInvalid", tok, err)
		}
		clock.advance(auth.TOTPPeriod * time.Second)
		next := totpContractCode(t, secret, clock.Now())
		if tok, err := restarted.svc.LoginWith2FA(ctx, "totp-user", "supersecret", next); err != nil || tok == "" {
			t.Fatalf("new step after restart = %q, %v; want a session", tok, err)
		}
	})

	t.Run("SkewWindowIsDeterministic", func(t *testing.T) {
		f := newTOTPReplayFixture(t, engine)
		ctx := context.Background()
		clock := &totpTestClock{now: start}
		a := f.hub(t, clock)
		_, secret := enrolTOTPUser(t, ctx, a, clock)
		clock.advance(5 * auth.TOTPPeriod * time.Second)
		now := clock.Now()
		period := auth.TOTPPeriod * time.Second
		// Two steps behind is outside the window.
		if _, err := a.svc.LoginWith2FA(ctx, "totp-user", "supersecret", totpContractCode(t, secret, now.Add(-2*period))); !errors.Is(err, services.ErrTOTPInvalid) {
			t.Fatalf("code two steps old = %v; want ErrTOTPInvalid", err)
		}
		// One step behind is accepted, then the current step, then one ahead.
		for _, offset := range []time.Duration{-period, 0, period} {
			if tok, err := a.svc.LoginWith2FA(ctx, "totp-user", "supersecret", totpContractCode(t, secret, now.Add(offset))); err != nil || tok == "" {
				t.Fatalf("code at offset %v = %q, %v; want a session", offset, tok, err)
			}
		}
		// Once the step ahead is consumed nothing at or below it is accepted.
		if _, err := a.svc.LoginWith2FA(ctx, "totp-user", "supersecret", totpContractCode(t, secret, now)); !errors.Is(err, services.ErrTOTPInvalid) {
			t.Fatalf("current step after a later one = %v; want ErrTOTPInvalid", err)
		}
	})

	t.Run("RotationFencesStaleGeneration", func(t *testing.T) {
		f := newTOTPReplayFixture(t, engine)
		ctx := context.Background()
		clock := &totpTestClock{now: start}
		a := f.hub(t, clock)
		user, secret := enrolTOTPUser(t, ctx, a, clock)
		stale, err := a.users.GetByID(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		code := totpContractCode(t, secret, clock.Now())
		ticket := beginTOTPTicket(t, ctx, a)
		// The secret is rotated through a second hub before the ticket is used.
		b := f.hub(t, clock)
		newSecret, _, err := b.svc.SetupTOTP(ctx, user.ID)
		if err != nil {
			t.Fatalf("SetupTOTP: %v", err)
		}
		if tok, _, err := a.svc.Complete2FALogin(ctx, ticket, code); err == nil || tok != "" {
			t.Fatalf("old-secret code after rotation = %q, %v; want an error and no session", tok, err)
		}
		step := clock.Now().Unix() / auth.TOTPPeriod
		if ok, err := a.users.ConsumeTOTPStep(ctx, user.ID, stale.TOTPGeneration, step); err != nil || ok {
			t.Fatalf("consume with a stale generation = %v, %v; want false", ok, err)
		}
		if ok, err := a.users.ConfirmTOTPCredential(ctx, user.ID, stale.TOTPGeneration, step); err != nil || ok {
			t.Fatalf("confirm with a stale generation = %v, %v; want false", ok, err)
		}
		rotated, err := b.users.GetByID(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		if rotated.TOTPGeneration <= stale.TOTPGeneration || rotated.TOTPEnabled || rotated.TOTPSecret != newSecret {
			t.Fatalf("after rotation generation=%d (was %d) enabled=%v; want a newer pending credential",
				rotated.TOTPGeneration, stale.TOTPGeneration, rotated.TOTPEnabled)
		}
		// Replay state restarts for the new credential: the step consumed at
		// enrolment of the old one does not block confirming the new one.
		if err := b.svc.EnableTOTP(ctx, user.ID, totpContractCode(t, newSecret, clock.Now().Add(-auth.TOTPPeriod*time.Second))); err != nil {
			t.Fatalf("EnableTOTP on the rotated credential: %v", err)
		}
		if err := b.svc.EnableTOTP(ctx, user.ID, totpContractCode(t, newSecret, clock.Now())); !errors.Is(err, services.ErrTOTPAlreadyEnabled) && !errors.Is(err, services.ErrTOTPInvalid) {
			t.Fatalf("second EnableTOTP = %v; want a refusal", err)
		}
	})

	t.Run("ProfileUpdateCannotRewriteTOTPState", func(t *testing.T) {
		f := newTOTPReplayFixture(t, engine)
		ctx := context.Background()
		clock := &totpTestClock{now: start}
		a := f.hub(t, clock)
		user, secret := enrolTOTPUser(t, ctx, a, clock)
		code := totpContractCode(t, secret, clock.Now())
		if tok, err := a.svc.LoginWith2FA(ctx, "totp-user", "supersecret", code); err != nil || tok == "" {
			t.Fatalf("login = %q, %v", tok, err)
		}
		snapshot, err := a.users.GetByID(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		snapshot.Timezone = "Asia/Bangkok"
		snapshot.TOTPEnabled, snapshot.TOTPSecret, snapshot.TOTPGeneration = false, "", 0
		if err := a.users.Update(ctx, snapshot); err != nil {
			t.Fatalf("Update: %v", err)
		}
		got, err := a.users.GetByID(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Timezone != "Asia/Bangkok" {
			t.Fatalf("Update dropped an ordinary field: timezone=%q", got.Timezone)
		}
		if !got.TOTPEnabled || got.TOTPSecret != secret || got.TOTPGeneration == 0 {
			t.Fatalf("Update rewrote TOTP state: enabled=%v secret-kept=%v generation=%d",
				got.TOTPEnabled, got.TOTPSecret == secret, got.TOTPGeneration)
		}
		if _, err := a.svc.LoginWith2FA(ctx, "totp-user", "supersecret", code); !errors.Is(err, services.ErrTOTPInvalid) {
			t.Fatalf("reuse after a profile update = %v; want ErrTOTPInvalid", err)
		}
	})

	t.Run("DeactivatedUserCannotConsume", func(t *testing.T) {
		f := newTOTPReplayFixture(t, engine)
		ctx := context.Background()
		clock := &totpTestClock{now: start}
		a := f.hub(t, clock)
		user, secret := enrolTOTPUser(t, ctx, a, clock)
		ticket := beginTOTPTicket(t, ctx, a)
		u, err := a.users.GetByID(ctx, user.ID)
		if err != nil {
			t.Fatal(err)
		}
		u.Active = false
		if err := a.users.Update(ctx, u); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if tok, _, err := a.svc.Complete2FALogin(ctx, ticket, totpContractCode(t, secret, clock.Now())); err == nil || tok != "" {
			t.Fatalf("deactivated user = %q, %v; want an error and no session", tok, err)
		}
	})

	t.Run("StorageFailureFailsClosed", func(t *testing.T) {
		f := newTOTPReplayFixture(t, engine)
		ctx := context.Background()
		clock := &totpTestClock{now: start}
		a := f.hub(t, clock)
		_, secret := enrolTOTPUser(t, ctx, a, clock)
		ticket := beginTOTPTicket(t, ctx, a)
		// The hub's database becomes unreachable after the user was read:
		// close the pool behind a second hub's service.
		b := f.hub(t, clock)
		ticketB := beginTOTPTicket(t, ctx, b)
		closedDB := f.open(t)
		failing := &failAfterReadUsers{UserRepository: f.users(closedDB), read: b.users}
		_ = closedDB.Close()
		authn := auth.NewJWTAuthenticator("totp-replay-contract-signing-key-0123456789", 1, b.users)
		svc := services.NewAuthService(failing, f.apiKeys(f.db), authn, auth.NewTOTPProvider("phoenix-test"), services.WithClock(clock))
		code := totpContractCode(t, secret, clock.Now())
		tok, _, err := svc.Complete2FALogin(ctx, ticketB, code)
		if !errors.Is(err, services.ErrTOTPUnavailable) || tok != "" {
			t.Fatalf("Complete2FALogin with the database down = %q, %v; want ErrTOTPUnavailable and no session", tok, err)
		}
		// Nothing was consumed, so the healthy hub can still use the step once.
		if tok, _, err := a.svc.Complete2FALogin(ctx, ticket, code); err != nil || tok == "" {
			t.Fatalf("login after the failure = %q, %v; want a session", tok, err)
		}
	})

	t.Run("Migration090DownUp", func(t *testing.T) {
		f := newTOTPReplayFixture(t, engine)
		ctx := context.Background()
		if err := runEngineMigration(t, f.db, engine, "090_totp_replay_state", "down"); err != nil {
			t.Fatalf("090 down: %v", err)
		}
		for _, column := range []string{"totp_generation", "totp_last_step"} {
			if totpColumnExists(t, ctx, f, column) {
				t.Fatalf("users.%s still exists after 090 down", column)
			}
		}
		if err := runEngineMigration(t, f.db, engine, "090_totp_replay_state", "up"); err != nil {
			t.Fatalf("090 up: %v", err)
		}
		for _, column := range []string{"totp_generation", "totp_last_step"} {
			if !totpColumnExists(t, ctx, f, column) {
				t.Fatalf("users.%s missing after 090 up", column)
			}
		}
		if engine == "mariadb" {
			// MariaDB's up script is idempotent; a second run is a no-op.
			if err := runEngineMigration(t, f.db, engine, "090_totp_replay_state", "up"); err != nil {
				t.Fatalf("090 up again: %v", err)
			}
		}
		// The replayed schema still enforces replay protection.
		clock := &totpTestClock{now: start}
		a := f.hub(t, clock)
		_, secret := enrolTOTPUser(t, ctx, a, clock)
		code := totpContractCode(t, secret, clock.Now())
		if tok, err := a.svc.LoginWith2FA(ctx, "totp-user", "supersecret", code); err != nil || tok == "" {
			t.Fatalf("login after 090 down/up = %q, %v", tok, err)
		}
		if _, err := a.svc.LoginWith2FA(ctx, "totp-user", "supersecret", code); !errors.Is(err, services.ErrTOTPInvalid) {
			t.Fatalf("reuse after 090 down/up = %v; want ErrTOTPInvalid", err)
		}
	})
}

// failAfterReadUsers reads users through a healthy repository but writes TOTP
// replay state through one whose database is closed.
type failAfterReadUsers struct {
	ports.UserRepository
	read ports.UserRepository
}

func (r *failAfterReadUsers) GetByID(ctx context.Context, id int64) (*domain.User, error) {
	return r.read.GetByID(ctx, id)
}

func (r *failAfterReadUsers) GetByUsername(ctx context.Context, username string) (*domain.User, error) {
	return r.read.GetByUsername(ctx, username)
}

func totpColumnExists(t *testing.T, ctx context.Context, f totpReplayFixture, column string) bool {
	t.Helper()
	var n int
	query := `SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = ?`
	if f.engine == "mariadb" {
		query = `SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = DATABASE() AND table_name = 'users' AND column_name = ?`
	}
	if err := f.db.NewRaw(query, column).Scan(ctx, &n); err != nil {
		t.Fatalf("inspect users.%s: %v", column, err)
	}
	return n == 1
}
