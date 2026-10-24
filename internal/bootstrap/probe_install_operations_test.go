package bootstrap

import (
	"context"
	"strings"
	"testing"
)

func TestValidateProbeInstallConfig(t *testing.T) {
	ok := Config{ProbeSSHInstallEnabled: true, ProbesEnabled: true, ProbeStageAuthorityKeyFile: "/k", ProbeInstallArtifactDir: "/a"}
	if err := validateProbeInstallConfig(ok); err != nil {
		t.Fatalf("complete config refused: %v", err)
	}
	if err := validateProbeInstallConfig(Config{}); err != nil {
		t.Fatalf("disabled default refused: %v", err)
	}
	for want, mutate := range map[string]func(*Config){
		"PROBES_ENABLED":                 func(c *Config) { c.ProbesEnabled = false },
		"PROBE_STAGE_AUTHORITY_KEY_FILE": func(c *Config) { c.ProbeStageAuthorityKeyFile = "" },
		"PROBE_INSTALL_ARTIFACT_DIR":     func(c *Config) { c.ProbeInstallArtifactDir = "" },
	} {
		cfg := ok
		mutate(&cfg)
		if err := validateProbeInstallConfig(cfg); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("missing %s: %v", want, err)
		}
	}
}

// The default deployment (and every worker) builds no install service and
// loads no key, policy or artifact directory.
func TestNewProbeInstallOperationsDisabledLoadsNothing(t *testing.T) {
	for _, cfg := range []Config{
		{},
		{Mode: "worker", ProbeSSHInstallEnabled: true, ProbesEnabled: true, ProbeStageAuthorityKeyFile: "/absent/key", ProbeInstallArtifactDir: "/absent/dir"},
	} {
		ops, err := newProbeInstallOperations(context.Background(), cfg, nil)
		if err != nil || ops != nil || ops.service() != nil {
			t.Fatalf("cfg %+v: ops=%v err=%v", cfg.Mode, ops, err)
		}
		if err := ops.Close(context.Background()); err != nil {
			t.Fatalf("nil close: %v", err)
		}
	}
	// Enabled with an unreadable key fails fast at startup.
	cfg := Config{ProbeSSHInstallEnabled: true, ProbesEnabled: true, ProbeStageAuthorityKeyFile: "/absent/key", ProbeInstallArtifactDir: t.TempDir()}
	if ops, err := newProbeInstallOperations(context.Background(), cfg, nil); err == nil || ops != nil {
		t.Fatalf("missing key accepted: ops=%v err=%v", ops, err)
	}
}
