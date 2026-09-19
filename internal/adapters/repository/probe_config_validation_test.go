package repository_test

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestLocalConfigValidationProtectedRevisions(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			ctx := context.Background()
			monitorID := f.monitor(t)
			if _, err := f.assignments.InitializeLocal(ctx, monitorID); err != nil {
				t.Fatal(err)
			}
			setURL := func(value string) {
				t.Helper()
				if _, err := f.db.NewUpdate().Table("monitors").Set("config = ?", repository.JSONField{"url": value}).Where("id = ?", monitorID).Exec(ctx); err != nil {
					t.Fatal(err)
				}
			}
			setURL("https://example.test")
			builder, prepared := sourceConfigBuilder(t, f, f.db)
			at := time.Date(2026, 9, 19, 1, 0, 0, 0, time.UTC)
			meta, err := builder.Prepare(ctx, probeRegistryID2, 0, at, at)
			if err != nil {
				t.Fatal(err)
			}
			validator := probe.NewLocalConfigValidator(checker.Get, notifier.Get)
			s := services.NewLocalProbeConfigValidationService(prepared, validator)
			if got, err := s.ValidatePrepared(ctx, meta.ProbeConfigTarget, 1); err != nil || !domain.SameProbeConfigMetadata(meta, got) {
				t.Fatal("valid protected snapshot did not validate")
			}
			before, err := configRepo(f, f.db).Get(ctx, domain.LocalProbeID, 1)
			if err != nil {
				t.Fatal(err)
			}
			// Shape-valid but runtime-invalid content can be staged for diagnostics.
			// Validation must not choose older credentials or overwrite either row.
			setURL("ftp://fixture-secret-value@example.test")
			if _, err := builder.Prepare(ctx, probeRegistryID2, 1, at, at); err != nil {
				t.Fatal(err)
			}
			_, reopened := sourceConfigBuilder(t, f, reopenConfigDB(t, f))
			s = services.NewLocalProbeConfigValidationService(reopened, validator)
			if got, err := s.ValidatePrepared(ctx, meta.ProbeConfigTarget, 2); !errors.Is(err, domain.ErrValidation) || got.Revision != 0 {
				t.Fatal("invalid latest revision fell back or succeeded")
			}
			if got, err := s.ValidatePrepared(ctx, meta.ProbeConfigTarget, 1); err != nil || !domain.SameProbeConfigMetadata(meta, got) {
				t.Fatal("historical exact validation failed")
			}
			after, err := configRepo(f, f.db).Get(ctx, domain.LocalProbeID, 1)
			if err != nil || !bytes.Equal(before.ProtectedPayload, after.ProtectedPayload) || !before.StoredAt.Equal(after.StoredAt) {
				t.Fatal("validation changed original protected history")
			}
			latest, err := configRepo(f, f.db).Latest(ctx, domain.LocalProbeID)
			if err != nil || latest.Revision != 2 {
				t.Fatal("validation rewound the prepared revision")
			}
		})
	}
}
