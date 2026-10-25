package bootstrap

import (
	"strings"
	"testing"
)

// TestOperativePagingConfig requires a deliberate protected connector opt-in.
func TestOperativePagingConfig(t *testing.T) {
	t.Setenv("JWT_SECRET", testJWTSecret)
	t.Setenv("PROBE_HUB_ID", "")
	t.Setenv("PROBE_SECRET_KEY_FILE", "")
	t.Setenv("PROBES_ENABLED", "false")
	t.Setenv("PROBES_AGGREGATE_PAGING_ENABLED", "false")
	cfg, err := LoadConfig()
	if err != nil || cfg.ProbesAggregatePagingEnabled {
		t.Fatal("default activated operative paging", err)
	}
	t.Setenv("PROBES_AGGREGATE_PAGING_ENABLED", "true")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "requires PROBES_ENABLED") {
		t.Fatal("unprotected opt-in accepted", err)
	}
	t.Setenv("PROBES_ENABLED", "true")
	if _, err := LoadConfig(); err == nil || !strings.Contains(err.Error(), "requires PROBE_SECRET_KEY_FILE") {
		t.Fatal("keyless connector accepted", err)
	}
	t.Setenv("PROBE_SECRET_KEY_FILE", "/fixture/generated-at-runtime")
	cfg, err = LoadConfig()
	if err != nil || !cfg.ProbesAggregatePagingEnabled || !cfg.ProbesEnabled {
		t.Fatal("explicit opt-in not loaded", err)
	}
	t.Setenv("PROBES_AGGREGATE_PAGING_ENABLED", "false")
	cfg, err = LoadConfig()
	if err != nil || cfg.ProbesAggregatePagingEnabled || !cfg.ProbesEnabled {
		t.Fatal("disable stopped required drain runtime", err)
	}
}
