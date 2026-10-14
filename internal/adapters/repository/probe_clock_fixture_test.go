package repository_test

import (
	"testing"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect"
)

// awaitProbeClock never rewrites evidence or fabricates a historical receipt.
// The local producer and disposable DB are distinct clocks: wait until the
// authoritative DB can genuinely observe the just-recorded source sample.
func awaitProbeClock(t *testing.T, p reviewPipeline) {
	t.Helper()
	awaitProbeClockFor(t, p.f.db, p.monitor.ID)
}

// awaitSourceReceiptClock retains the genuine captured source timestamp. A
// future replay observation is archived but deliberately not made current.
func awaitSourceReceiptClock(t *testing.T, db *bun.DB, observed time.Time) {
	t.Helper()
	if db.Dialect().Name() != dialect.MySQL {
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var ready bool
		if err := db.NewRaw("SELECT UTC_TIMESTAMP(6) >= ?", observed.UTC().Truncate(time.Microsecond)).Scan(t.Context(), &ready); err != nil {
			t.Fatal(err)
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("DB receipt clock has not reached the genuine source sample")
		}
		time.Sleep(time.Millisecond)
	}
}

func awaitProbeClockFor(t *testing.T, db *bun.DB, monitorID int64) {
	t.Helper()
	if db.Dialect().Name() != dialect.MySQL {
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		var future int
		if err := db.NewRaw("SELECT COUNT(*) FROM monitor_probe_state WHERE monitor_id=? AND (observed_at >= UTC_TIMESTAMP(6) OR received_at >= UTC_TIMESTAMP(6))", monitorID).Scan(t.Context(), &future); err != nil {
			t.Fatal(err)
		}
		if future == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("DB clock has not reached the actual source observation")
		}
		time.Sleep(time.Millisecond)
	}
}
