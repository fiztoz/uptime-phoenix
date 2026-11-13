package bootstrap

import (
	"strings"
	"testing"
	"time"
)

func TestMigrationLockPolicyDefaultsAndEnv(t *testing.T) {
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef0123456789abcdef")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	p := migrationLockPolicy(cfg)
	if p.LockWait != 10*time.Second || p.Attempts != 6 {
		t.Fatalf("default policy = %+v, want 10s x 6", p)
	}
	t.Setenv("DB_MIGRATION_LOCK_WAIT_SECONDS", "3")
	t.Setenv("DB_MIGRATION_LOCK_ATTEMPTS", "2")
	if cfg, err = LoadConfig(); err != nil {
		t.Fatal(err)
	}
	if p = migrationLockPolicy(cfg); p.LockWait != 3*time.Second || p.Attempts != 2 {
		t.Fatalf("env policy = %+v, want 3s x 2", p)
	}
}

func TestMigrationLockPolicyRejectsNonPositive(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"DB_MIGRATION_LOCK_WAIT_SECONDS", "0"},
		{"DB_MIGRATION_LOCK_ATTEMPTS", "0"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef0123456789abcdef")
			t.Setenv(tc.key, tc.val)
			if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("LoadConfig with %s=%s: err = %v", tc.key, tc.val, err)
			}
		})
	}
}

func TestMigrationLockPolicyAcceptsBounds(t *testing.T) {
	t.Setenv("JWT_SECRET", testJWTSecret)
	for _, tc := range []struct{ key, val string }{
		{"DB_MIGRATION_LOCK_WAIT_SECONDS", "1"},
		{"DB_MIGRATION_LOCK_WAIT_SECONDS", "300"},
		{"DB_MIGRATION_LOCK_ATTEMPTS", "1"},
		{"DB_MIGRATION_LOCK_ATTEMPTS", "30"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := LoadConfig(); err != nil {
				t.Fatalf("LoadConfig with %s=%s: %v", tc.key, tc.val, err)
			}
		})
	}
}

func TestMigrationLockPolicyRejectsAboveMax(t *testing.T) {
	for _, tc := range []struct{ key, val, want string }{
		{"DB_MIGRATION_LOCK_WAIT_SECONDS", "301", "between 1 and 300"},
		{"DB_MIGRATION_LOCK_WAIT_SECONDS", "86400", "between 1 and 300"},
		{"DB_MIGRATION_LOCK_ATTEMPTS", "31", "between 1 and 30"},
		{"DB_MIGRATION_LOCK_ATTEMPTS", "1000000", "between 1 and 30"},
	} {
		t.Run(tc.key+"="+tc.val, func(t *testing.T) {
			t.Setenv("JWT_SECRET", testJWTSecret)
			t.Setenv(tc.key, tc.val)
			_, err := LoadConfig()
			if err == nil || !strings.Contains(err.Error(), tc.key) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadConfig with %s=%s: err = %v, want %q and the key", tc.key, tc.val, err, tc.want)
			}
		})
	}
}
