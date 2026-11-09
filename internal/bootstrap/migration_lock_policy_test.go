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
