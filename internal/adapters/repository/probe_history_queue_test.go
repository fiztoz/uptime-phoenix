package repository_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type historyQueueRow struct {
	bun.BaseModel `bun:"table:probe_dirty_buckets"`
	MonitorID     int64
	ProbeID       string
	Resolution    string
	Bucket        time.Time
}

type historyQueueProjector struct {
	*services.ProbeHistoryService
	seen []domain.DirtyBucket
}

func (p *historyQueueProjector) ProjectHistory(ctx context.Context, work domain.ProbeHistoryWork) (domain.ProbeHistoryProjection, error) {
	p.seen = append(p.seen, work.Bucket)
	return p.ProbeHistoryService.ProjectHistory(ctx, work)
}

func TestProbeHistoryQueuePriorityAndOpenWindows(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			r := oldHistoryFixture(t, engine)
			store := repository.NewRegionalCommitStore(r.f.db)
			projector := &historyQueueProjector{ProbeHistoryService: services.NewProbeHistoryService(store)}
			now := r.at.Add(48 * time.Hour)
			resolutions := []string{"1m", "overall", "1h", "1d"}
			rows := make([]historyQueueRow, 0, 2*len(resolutions))
			for _, resolution := range resolutions {
				probeID := r.session.ProbeID
				if resolution == "overall" {
					probeID = domain.LocalProbeID
				}
				rows = append(rows,
					historyQueueRow{MonitorID: r.monitor, ProbeID: probeID, Resolution: resolution, Bucket: r.at},
					historyQueueRow{MonitorID: r.monitor, ProbeID: probeID, Resolution: resolution, Bucket: now},
				)
			}
			if _, err := r.f.db.NewInsert().Model(&rows).Exec(t.Context()); err != nil {
				t.Fatal(err)
			}
			processed, err := store.ProcessHistoryWork(t.Context(), now, 10, projector)
			if err != nil || processed != 4 || len(projector.seen) != 4 {
				t.Fatalf("closed work: processed=%d seen=%d err=%v", processed, len(projector.seen), err)
			}
			for i, resolution := range resolutions {
				if projector.seen[i].Resolution != resolution || !projector.seen[i].Bucket.Equal(r.at) {
					t.Fatalf("priority %d: got %+v, want %s at %s", i, projector.seen[i], resolution, r.at)
				}
			}
			if remaining := replayCount(t, r.f, "probe_dirty_buckets"); remaining != 4 {
				t.Fatalf("open windows were consumed: remaining=%d", remaining)
			}
		})
	}
}

type historySelectionHook struct{ query string }

func (h *historySelectionHook) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	if strings.HasPrefix(event.Query, "SELECT ") && strings.Contains(event.Query, "probe_dirty_buckets") && strings.Contains(event.Query, "ORDER BY") && strings.HasSuffix(event.Query, "LIMIT 1") {
		h.query = event.Query
	}
	return ctx
}

func (*historySelectionHook) AfterQuery(context.Context, *bun.QueryEvent) {}

func TestProbeHistoryQueueSelectionBoundedMariaDB(t *testing.T) {
	r := oldHistoryFixture(t, "mariadb")
	rows := make([]historyQueueRow, 4096)
	for i := range rows {
		rows[i] = historyQueueRow{MonitorID: r.monitor, ProbeID: r.session.ProbeID, Resolution: "1m", Bucket: r.at.Add(time.Duration(i) * time.Minute)}
	}
	if _, err := r.f.db.NewInsert().Model(&rows).Exec(t.Context()); err != nil {
		t.Fatal(err)
	}
	hook := new(historySelectionHook)
	attachInPlaceQueryHook(r.f.db, hook)
	service := services.NewProbeHistoryService(repository.NewRegionalCommitStore(r.f.db))
	if n, err := service.ProcessBatch(t.Context(), time.Now().UTC(), 1); err != nil || n != 1 {
		t.Fatal("production history selection", n, err)
	}
	if hook.query == "" {
		t.Fatal("production queue selection was not observed")
	}
	// Execute the actual production SELECT against a populated real engine.
	// Elapsed time is machine-dependent; rows read exposes the full-backlog sort.
	var raw string
	if err := r.f.db.QueryRowContext(t.Context(), "ANALYZE FORMAT=JSON "+hook.query).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var plan map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &plan); err != nil {
		t.Fatal(err)
	}
	found := false
	var inspect func(interface{})
	inspect = func(value interface{}) {
		switch value := value.(type) {
		case map[string]interface{}:
			if value["table_name"] == "dirty" {
				count, ok := value["r_rows"].(float64)
				if !ok || count > 8 {
					t.Errorf("queue selection scanned %v rows for one item", value["r_rows"])
				}
				found = true
			}
			for _, child := range value {
				inspect(child)
			}
		case []interface{}:
			for _, child := range value {
				inspect(child)
			}
		}
	}
	inspect(plan)
	if !found {
		t.Fatal("ANALYZE did not report actual queue rows")
	}
}
