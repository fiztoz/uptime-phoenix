package repository_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type monitorMutationQueryHook struct {
	before func(context.Context, string)
}

func (h monitorMutationQueryHook) BeforeQuery(ctx context.Context, e *bun.QueryEvent) context.Context {
	h.before(ctx, e.Query)
	return ctx
}
func (monitorMutationQueryHook) AfterQuery(context.Context, *bun.QueryEvent) {}

// Exercise the production SERIALIZABLE applied-source reader, not ReadLocal's
// nonlocking REPEATABLE READ snapshot. Both local and remote FK children exist.
func TestMonitorMutationsWaitBeforeAppliedSourceGraph(t *testing.T) {
	for _, operation := range []string{"update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newProbeRegistryFixture(t, "mariadb")
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			seedInstallation(t, f)
			id := localMonitor(t, f)
			if _, err := f.db.NewUpdate().Table("monitors").Set("user_id = ?", f.user(t)).Where("id = ?", id).Exec(ctx); err != nil {
				t.Fatal(err)
			}
			remote := f.remote(t, probeRegistryID1, "mutation-lock")
			if _, err := f.assignments.Replace(ctx, id, 1, []string{domain.LocalProbeID, remote.ID}, domain.HealthPolicyAnyDown); err != nil {
				t.Fatal(err)
			}
			builder, _ := sourceConfigBuilder(t, f, f.db)
			at := time.Now().UTC()
			meta, err := builder.Prepare(ctx, "11111111-2222-4333-8444-555555555555", 0, at, at)
			if err != nil {
				t.Fatal(err)
			}
			reader := repository.NewProbeActivationStore(f.db, probe.LocalConfigEncoder{})
			if _, err := reader.ActivateLocal(ctx, ports.LocalActivationParams{Target: meta.ProbeConfigTarget, Revision: meta.Revision, SHA256: meta.SHA256, AppliedAt: at, AssignmentCount: -1}); err != nil {
				t.Fatal(err)
			}
			peer := reopenConfigDB(t, f)
			peer.SetMaxOpenConns(1)
			observer := reopenConfigDB(t, f)
			var writerConnection int64
			if err := peer.NewRaw("SELECT CONNECTION_ID()").Scan(ctx, &writerConnection); err != nil {
				t.Fatal(err)
			}
			monitor, err := mariadb.NewMonitorRepo(peer).GetByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			graph := make(chan struct{})
			release := make(chan struct{})
			var once, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			f.db.AddQueryHook(monitorMutationQueryHook{before: func(ctx context.Context, query string) {
				if strings.Contains(query, "NOT EXISTS (SELECT 1 FROM monitor_probe_assignment_sets") {
					once.Do(func() {
						close(graph)
						select {
						case <-release:
						case <-ctx.Done():
						}
					})
				}
			}})
			readResult := make(chan error, 1)
			go func() { _, err := reader.ReadAppliedLocal(ctx); readResult <- err }()
			select {
			case <-graph:
			case <-ctx.Done():
				t.Fatal("applied reader did not reach graph")
			}
			first := make(chan string, 1)
			monitorAttempted := make(chan struct{}, 1)
			var firstOnce sync.Once
			peer.AddQueryHook(monitorMutationQueryHook{before: func(_ context.Context, query string) {
				if strings.HasPrefix(query, "UPDATE `monitors`") || strings.HasPrefix(query, "DELETE FROM `monitors`") {
					select {
					case monitorAttempted <- struct{}{}:
					default:
					}
				}
				if strings.HasPrefix(query, "UPDATE `probes`") || strings.HasPrefix(query, "UPDATE `monitors`") || strings.HasPrefix(query, "DELETE FROM `monitors`") {
					firstOnce.Do(func() { first <- query })
				}
			}})
			writeResult := make(chan error, 1)
			go func() {
				repo := mariadb.NewMonitorRepo(peer)
				if operation == "delete" {
					writeResult <- repo.Delete(ctx, id)
					return
				}
				monitor.Active = !monitor.Active
				writeResult <- repo.Update(ctx, monitor)
			}()
			select {
			case query := <-first:
				if !strings.HasPrefix(query, "UPDATE `probes`") {
					t.Error("monitor mutation reached monitor locks before local source lock")
				} else {
					// Observe the dispatched registration UPDATE on a separate
					// connection while the reader holds its shared row lock. It
					// cannot complete or reach monitor SQL until that lock releases.
					ticker := time.NewTicker(10 * time.Millisecond)
					defer ticker.Stop()
					for {
						var waiting int
						if err := observer.NewRaw("SELECT COUNT(*) FROM information_schema.PROCESSLIST WHERE ID = ? AND COMMAND = 'Query' AND INFO LIKE 'UPDATE %probes%'", writerConnection).Scan(ctx, &waiting); err != nil {
							t.Fatal(err)
						}
						if waiting == 1 {
							break
						}
						select {
						case <-ticker.C:
						case <-ctx.Done():
							t.Fatal("writer never waited on source lock")
						}
					}
					select {
					case <-monitorAttempted:
						t.Error("writer reached monitor mutation while source lock was held")
					default:
					}
				}
			case <-ctx.Done():
				t.Fatal("mutation did not attempt its first lock")
			}
			unblock()
			if err := <-readResult; err != nil {
				t.Errorf("unchanged applied source could not finish: %v", err)
			}
			if err := <-writeResult; err != nil {
				t.Fatalf("mutation after source commit: %v", err)
			}
			if operation == "delete" {
				for _, table := range []string{"monitors", "monitor_probe_assignment_sets", "monitor_probe_assignments", "monitor_probe_assignment_history"} {
					column := "monitor_id"
					if table == "monitors" {
						column = "id"
					}
					n, err := f.db.NewSelect().Table(table).Where(column+" = ?", id).Count(ctx)
					if err != nil || n != 0 {
						t.Fatalf("delete did not cascade %s: rows=%d err=%v", table, n, err)
					}
				}
			} else {
				got, err := mariadb.NewMonitorRepo(peer).GetByID(ctx, id)
				if err != nil || got.Active != monitor.Active {
					t.Fatalf("activation did not persist: %v", err)
				}
			}
			if _, err := reader.ReadAppliedLocal(ctx); !errors.Is(err, ports.ErrConflict) {
				t.Fatalf("changed source remained executable: %v", err)
			}
		})
	}
}

func TestMonitorMutationsRejectMissingLocalRegistration(t *testing.T) {
	for _, operation := range []string{"update", "delete"} {
		t.Run(operation, func(t *testing.T) {
			f := newProbeRegistryFixture(t, "mariadb")
			id := f.monitor(t)
			ctx := t.Context()
			if _, err := f.db.NewDelete().Table("probes").Where("id = ?", domain.LocalProbeID).Exec(ctx); err != nil {
				t.Fatal(err)
			}
			repo := mariadb.NewMonitorRepo(f.db)
			before, err := repo.GetByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if operation == "delete" {
				err = repo.Delete(ctx, id)
			} else {
				edited := *before
				edited.Active = !edited.Active
				err = repo.Update(ctx, &edited)
			}
			if !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("missing source registration did not fail closed: %v", err)
			}
			after, err := repo.GetByID(ctx, id)
			if err != nil || after.Active != before.Active {
				t.Fatalf("failed mutation changed monitor: %v", err)
			}
		})
	}
}
