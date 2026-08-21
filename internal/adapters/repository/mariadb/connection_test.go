package mariadb

import (
	"database/sql"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func TestDefaultPool(t *testing.T) {
	t.Parallel()
	p := DefaultPool()
	if p.MaxOpenConns != 10 {
		t.Fatalf("MaxOpenConns = %d, want 10", p.MaxOpenConns)
	}
	if p.MaxIdleConns != 2 {
		t.Fatalf("MaxIdleConns = %d, want 2 (not equal to MaxOpen — idle sessions must shrink)", p.MaxIdleConns)
	}
	if p.ConnMaxIdleTime != 30*time.Second {
		t.Fatalf("ConnMaxIdleTime = %s, want 30s", p.ConnMaxIdleTime)
	}
	if p.ConnMaxLifetime != 5*time.Minute {
		t.Fatalf("ConnMaxLifetime = %s, want 5m", p.ConnMaxLifetime)
	}
}

func TestApplyPoolClampsIdleToOpen(t *testing.T) {
	t.Parallel()
	db, err := sql.Open("mysql", "phoenix:phoenix@tcp(127.0.0.1:1)/phoenix")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	applyPool(db, PoolSettings{MaxOpenConns: 4, MaxIdleConns: 99})
	if got := db.Stats().MaxOpenConnections; got != 4 {
		t.Fatalf("MaxOpenConnections = %d, want 4", got)
	}
}
