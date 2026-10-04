package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type activationBarrierEncoder struct{ afterRead func() }

func (e activationBarrierEncoder) EncodeLocal(def domain.LocalProbeConfigDefinition) ([]byte, error) {
	e.afterRead()
	return (probe.LocalConfigEncoder{}).EncodeLocal(def)
}

// The encoder is called after the source graph has been read, before receipt commit.
// An ordinary source writer must be unable to change the graph in that interval.
func TestProbeActivationLocksSourceThroughCommit(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		for _, operation := range []string{"monitor_update", "tag_link_insert"} {
			t.Run(engine+"/"+operation, func(t *testing.T) {
				f := newProbeRegistryFixture(t, engine)
				ctx := context.Background()
				hub := "11111111-2222-4333-8444-555555555555"
				seedInstallation(t, f)
				id := localMonitor(t, f)
				tag := &repository.TagModel{Name: "activation-race", Color: "#123456"}
				insertConfigModel(t, f.db, tag)
				builder, _ := sourceConfigBuilder(t, f, f.db)
				at := time.Now().UTC()
				meta, err := builder.Prepare(ctx, hub, 0, at, at)
				if err != nil {
					t.Fatal(err)
				}
				db2 := reopenConfigDB(t, f)
				db2.SetMaxOpenConns(1)
				timeout := "SET SESSION innodb_lock_wait_timeout = 1"
				if engine == "sqlite" {
					timeout = "PRAGMA busy_timeout = 100"
				}
				if _, err := db2.ExecContext(ctx, timeout); err != nil {
					t.Fatal(err)
				}
				edit := func(ctx context.Context) error {
					var err error
					if operation == "monitor_update" {
						_, err = db2.NewUpdate().Table("monitors").Set("check_interval = check_interval + 1").Where("id = ?", id).Exec(ctx)
					} else {
						_, err = db2.NewInsert().Model(&repository.MonitorTagModel{MonitorID: id, TagID: tag.ID}).Exec(ctx)
					}
					return err
				}
				calls := 0
				store := repository.NewProbeActivationStore(f.db, activationBarrierEncoder{afterRead: func() {
					calls++
					writeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
					defer cancel()
					if err := edit(writeCtx); err == nil {
						t.Error("source changed between freshness read and activation commit")
					}
				}})
				_, err = store.ActivateLocal(ctx, ports.LocalActivationParams{Target: meta.ProbeConfigTarget, Revision: meta.Revision, SHA256: meta.SHA256, AppliedAt: at, AssignmentCount: -1})
				if err != nil {
					t.Fatal(err)
				}
				if calls != 1 {
					t.Fatalf("barrier calls=%d, want 1", calls)
				}
				// The same valid SQL succeeds once activation has committed and released locks.
				if err := edit(ctx); err != nil {
					t.Fatalf("source edit after commit: %v", err)
				}
				reader := repository.NewProbeActivationStore(f.db, probe.LocalConfigEncoder{})
				if _, err := reader.ReadAppliedLocal(ctx); err != ports.ErrConflict {
					t.Fatalf("edited source remained executable: %v", err)
				}
			})
		}
	}
}
