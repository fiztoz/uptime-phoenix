package bootstrap

import (
	"net"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/logger"
)

// #163: boot waits for MariaDB by default instead of exiting on the first
// refused connection.
func TestConfigDBConnectTimeoutDefault(t *testing.T) {
	t.Setenv("JWT_SECRET", "0123456789abcdef0123456789abcdef0123456789abcdef")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DBConnectTimeoutSeconds != 60 {
		t.Fatalf("DBConnectTimeoutSeconds = %d, want 60", cfg.DBConnectTimeoutSeconds)
	}
}

func TestOpenDBRetriesMariaDBUntilBudget(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
	cfg := Config{DBEngine: "mariadb", DBDSN: "phoenix:phoenix@tcp(" + addr + ")/phoenix?timeout=200ms", DBMaxOpenConns: 2, DBConnectTimeoutSeconds: 2}
	start := time.Now()
	if _, err := openDB(cfg, logger.New("error")); err == nil {
		t.Fatal("unreachable MariaDB must still fail once the budget is spent")
	}
	if elapsed := time.Since(start); elapsed < 1500*time.Millisecond || elapsed > 8*time.Second {
		t.Fatalf("openDB returned after %s; want it to keep retrying for the ~2s budget", elapsed)
	}
}
