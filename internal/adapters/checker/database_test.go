package checker

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestDatabaseChecker_Type(t *testing.T) {
	c := DatabaseChecker{}
	if got := c.Type(); got != "database" {
		t.Errorf("Type() = %q, want %q", got, "database")
	}
}

func TestDatabaseChecker_Validate(t *testing.T) {
	c := DatabaseChecker{}

	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{
			name:    "missing engine",
			config:  map[string]any{"connection_string": "host:5432"},
			wantErr: true,
		},
		{
			name:    "missing connection_string",
			config:  map[string]any{"engine": "postgres"},
			wantErr: true,
		},
		{
			name:    "empty config",
			config:  map[string]any{},
			wantErr: true,
		},
		{
			name:    "unsupported engine",
			config:  map[string]any{"engine": "cassandra", "connection_string": "host:9042"},
			wantErr: true,
		},
		{
			name:    "postgres valid",
			config:  map[string]any{"engine": "postgres", "connection_string": "postgres://localhost:5432/test"},
			wantErr: false,
		},
		{
			name:    "mysql valid",
			config:  map[string]any{"engine": "mysql", "connection_string": "user:pass@tcp(localhost:3306)/db"},
			wantErr: false,
		},
		{
			name:    "mariadb valid",
			config:  map[string]any{"engine": "mariadb", "connection_string": "user:pass@tcp(localhost:3306)/db"},
			wantErr: false,
		},
		{
			name:    "mongodb valid",
			config:  map[string]any{"engine": "mongodb", "connection_string": "mongodb://localhost:27017"},
			wantErr: false,
		},
		{
			name:    "redis valid",
			config:  map[string]any{"engine": "redis", "connection_string": "localhost:6379"},
			wantErr: false,
		},
		{
			name:    "engine case insensitive",
			config:  map[string]any{"engine": "Postgres", "connection_string": "postgres://localhost:5432/test"},
			wantErr: false,
		},
		{
			name:    "mssql valid",
			config:  map[string]any{"engine": "mssql", "connection_string": "sqlserver://u:p@localhost:1433?database=master"},
			wantErr: false,
		},
		{
			name:    "sqlserver alias",
			config:  map[string]any{"engine": "sqlserver", "connection_string": "sqlserver://localhost"},
			wantErr: false,
		},
		{
			name:    "dsn alias",
			config:  map[string]any{"engine": "redis", "dsn": "localhost:6379"},
			wantErr: false,
		},
		{
			name:    "health_check select_1",
			config:  map[string]any{"engine": "postgres", "connection_string": "x", "health_check": "select_1"},
			wantErr: false,
		},
		{
			name:    "health_check invalid",
			config:  map[string]any{"engine": "postgres", "connection_string": "x", "health_check": "DROP TABLE users"},
			wantErr: true,
		},
	}

	// Ensure free-form operator SQL is never accepted as a config field that
	// would be executed — only named health_check presets.
	t.Run("no freeform query field", func(t *testing.T) {
		// Validate still succeeds if a legacy "query" key is present; Check must
		// ignore it. Presence of query alone is not a validation error.
		if err := c.Validate(map[string]any{
			"engine": "postgres", "connection_string": "x", "query": "SELECT * FROM secrets",
		}); err != nil {
			t.Fatalf("legacy query key should not fail Validate: %v", err)
		}
	})

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := c.Validate(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestDatabaseChecker_Check_UnknownEngine(t *testing.T) {
	c := DatabaseChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"engine":            "cassandra",
		"connection_string": "localhost:9042",
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN", result.Status)
	}
	if result.LatencyMs < 0 {
		t.Errorf("Check() latency = %d, want >= 0", result.LatencyMs)
	}
}

func TestDatabaseChecker_Check_Postgres_BadHost(t *testing.T) {
	c := DatabaseChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"engine":            "postgres",
		"connection_string": "postgres://192.0.2.1:5432/nonexistent?connect_timeout=2",
		"timeout":           3.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN", result.Status)
	}
	if result.LatencyMs < 0 {
		t.Errorf("Check() latency = %d, want >= 0", result.LatencyMs)
	}
}

func TestDatabaseChecker_Check_MySQL_BadHost(t *testing.T) {
	c := DatabaseChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"engine":            "mysql",
		"connection_string": "user:pass@tcp(192.0.2.1:3306)/db?timeout=2s",
		"timeout":           3.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN", result.Status)
	}
}

func TestDatabaseChecker_Check_MariaDB_BadHost(t *testing.T) {
	c := DatabaseChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"engine":            "mariadb",
		"connection_string": "user:pass@tcp(192.0.2.1:3306)/db?timeout=2s",
		"timeout":           3.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN", result.Status)
	}
}

func TestDatabaseChecker_Check_MongoDB_BadHost(t *testing.T) {
	c := DatabaseChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"engine":            "mongodb",
		"connection_string": "mongodb://192.0.2.1:27017/?connectTimeoutMS=2000&serverSelectionTimeoutMS=2000",
		"timeout":           3.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN", result.Status)
	}
}

func TestDatabaseChecker_Check_MongoDB_RealServer(t *testing.T) {
	uri := os.Getenv("UPTIME_PHOENIX_TEST_MONGODB_URI")
	if uri == "" {
		t.Skip("set UPTIME_PHOENIX_TEST_MONGODB_URI to run against a real MongoDB server")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := DatabaseChecker{}
	result, err := c.Check(ctx, map[string]any{
		"engine":            "mongodb",
		"connection_string": uri,
		"health_check":      "select_1",
		"timeout":           10.0,
	})
	if err != nil {
		t.Fatalf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Fatalf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
	if result.Message != "connected, ping ok" {
		t.Errorf("Check() message = %q, want %q", result.Message, "connected, ping ok")
	}
}

func TestDatabaseChecker_Check_Redis_BadHost(t *testing.T) {
	c := DatabaseChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"engine":            "redis",
		"connection_string": "192.0.2.1:6379",
		"timeout":           3.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN", result.Status)
	}
}

func TestDatabaseChecker_Check_Redis_URL(t *testing.T) {
	c := DatabaseChecker{}
	// Test with a redis:// URL pointing to a non-existent host.
	result, err := c.Check(context.Background(), map[string]any{
		"engine":            "redis",
		"connection_string": "redis://192.0.2.1:6379",
		"timeout":           3.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN", result.Status)
	}
}

func TestDatabaseChecker_Check_MSSQL_BadHost(t *testing.T) {
	c := DatabaseChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"engine":            "mssql",
		"connection_string": "sqlserver://u:p@192.0.2.1:1433?database=master&connection+timeout=2",
		"timeout":           3.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN", result.Status)
	}
}

func TestDatabaseChecker_Check_TimeoutDefaults(t *testing.T) {
	c := DatabaseChecker{}
	// Verify that Check completes even with no timeout configured.
	result, err := c.Check(context.Background(), map[string]any{
		"engine":            "redis",
		"connection_string": "192.0.2.1:6379",
		// No timeout — should default to 10 seconds.
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN (expected connection failure to non-routable IP)", result.Status)
	}
}
