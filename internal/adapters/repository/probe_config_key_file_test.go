//go:build linux || darwin

package repository_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestPreparedProbeConfigFileKeyRestart(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "snapshot.key")
			if err := auth.CreateProbeSecretKeyFile(ctx, path); err != nil {
				t.Fatal(err)
			}
			protector, err := auth.NewProbeConfigProtectorFromFile(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			svc := services.NewProbeConfigService(configRepo(f, f.db), probe.ConfigInspector{}, protector)
			document, target := configDocument(t, domain.LocalProbeID, 1, "file-key-restart-secret")
			if _, err := svc.Prepare(ctx, target, document, 0); err != nil {
				t.Fatal(err)
			}
			stored, err := configRepo(f, f.db).Get(ctx, domain.LocalProbeID, 1)
			if err != nil {
				t.Fatal(err)
			}
			protector, err = auth.NewProbeConfigProtectorFromFile(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			reopened := configRepo(f, reopenConfigDB(t, f))
			svc = services.NewProbeConfigService(reopened, probe.ConfigInspector{}, protector)
			got, _, err := svc.Read(ctx, target, 1)
			if err != nil || !bytes.Equal(document, got) {
				t.Fatal("prepared bytes did not survive DB/key reopen")
			}
			// A well-formed wrong key cannot authenticate the retained revision.
			// This is why the standalone file check never claims DB readiness.
			wrongPath := filepath.Join(dir, "other-installation.key")
			if err := auth.CreateProbeSecretKeyFile(ctx, wrongPath); err != nil {
				t.Fatal(err)
			}
			wrong, err := auth.NewProbeConfigProtectorFromFile(ctx, wrongPath)
			if err != nil {
				t.Fatal(err)
			}
			svc = services.NewProbeConfigService(reopened, probe.ConfigInspector{}, wrong)
			if got, _, err := svc.Read(ctx, target, 0); err == nil || got != nil {
				t.Fatal("wrong file key returned latest snapshot plaintext")
			}
			after, err := reopened.Get(ctx, domain.LocalProbeID, 1)
			if err != nil || !bytes.Equal(stored.ProtectedPayload, after.ProtectedPayload) || !stored.StoredAt.Equal(after.StoredAt) {
				t.Fatal("failed decryption changed stored credentials")
			}
		})
	}
}
