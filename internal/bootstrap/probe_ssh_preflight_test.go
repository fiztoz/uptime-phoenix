package bootstrap

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeSSHPreflightPrivateInputAndPolicy(t *testing.T) {
	dir := t.TempDir()
	identity := filepath.Join(dir, "key")
	if err := os.WriteFile(identity, []byte("sensitive-key-canary"), 0644); err != nil {
		t.Fatal(err)
	}
	base := []string{"--address", "edge.example:22", "--user", "deploy", "--host-key-sha256", "SHA256:invalid"}
	for _, scenario := range []string{"public_key_file", "symlink", "directory", "bad_policy", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			path := identity
			args := append([]string{}, base...)
			ctx := t.Context()
			switch scenario {
			case "symlink":
				path = filepath.Join(dir, "key-link")
				if err := os.Symlink(identity, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				path = dir
			case "bad_policy":
				policy := filepath.Join(dir, "policy")
				if err := os.WriteFile(policy, []byte(`{"allowed_cidrs":["sensitive-policy-canary"]}`), 0600); err != nil {
					t.Fatal(err)
				}
				args = append(args, "--endpoint-policy", policy)
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			args = append(args, "--identity-file", path)
			var out, stderr bytes.Buffer
			if code := RunProbeSSHPreflight(ctx, args, &out, &stderr); code == 0 || out.Len() != 0 || stderr.Len() == 0 || bytes.Contains(stderr.Bytes(), []byte("canary")) {
				t.Fatal("preflight input failure succeeded, emitted a result, or leaked input")
			}
		})
	}
}
