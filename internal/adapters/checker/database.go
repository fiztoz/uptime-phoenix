package checker

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/microsoft/go-mssqldb"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// supportedEngines lists the database engines this checker supports.
var supportedEngines = map[string]bool{
	"postgres": true,
	"mysql":    true,
	"mariadb":  true,
	"mongodb":  true,
	"redis":    true,
	"mssql":    true,
}

// healthCheck presets — fixed server-side statements only (no free-form SQL).
// Avoids injection: the operator never supplies raw query text that Phoenix executes.
const (
	healthCheckPing    = "ping"     // driver/protocol ping only
	healthCheckSelect1 = "select_1" // SQL SELECT 1 / Redis PING / Mongo ping (after connect)
)

// DatabaseChecker checks database connectivity via engine-specific ping and
// optional fixed health presets.
// Supported engines: postgres, mysql, mariadb, mongodb, redis, mssql.
type DatabaseChecker struct{}

func init() { Register(DatabaseChecker{}) }

// Type returns the monitor type identifier.
func (DatabaseChecker) Type() string { return "database" }

// dbConnectionString resolves connection_string or the older UI key dsn.
func dbConnectionString(config map[string]any) string {
	if s, _ := config["connection_string"].(string); strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	if s, _ := config["dsn"].(string); strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s)
	}
	return ""
}

// dbEngine normalizes engine names (sqlserver → mssql, postgresql → postgres).
func dbEngine(config map[string]any) string {
	engine, _ := config["engine"].(string)
	engine = strings.ToLower(strings.TrimSpace(engine))
	switch engine {
	case "postgresql", "pg":
		return "postgres"
	case "sqlserver", "sql-server":
		return "mssql"
	case "mongo":
		return "mongodb"
	default:
		return engine
	}
}

// dbHealthCheck returns ping (default) or select_1.
func dbHealthCheck(config map[string]any) string {
	raw, _ := config["health_check"].(string)
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case healthCheckSelect1, "select1", "query":
		return healthCheckSelect1
	default:
		return healthCheckPing
	}
}

// Validate checks that engine and connection_string are present and that
// engine is one of the supported values. Optional capacity keys are type-checked
// when present; storage_max_gb is not required (some engines report capacity).
func (DatabaseChecker) Validate(config map[string]any) error {
	engine := dbEngine(config)
	if engine == "" {
		return fmt.Errorf("engine is required (supported: postgres, mysql, mariadb, mongodb, redis, mssql)")
	}
	if !supportedEngines[engine] {
		return fmt.Errorf("unsupported engine %q (supported: postgres, mysql, mariadb, mongodb, redis, mssql)", engine)
	}
	if dbConnectionString(config) == "" {
		return fmt.Errorf("connection_string is required")
	}
	if raw, ok := config["health_check"].(string); ok && strings.TrimSpace(raw) != "" {
		hc := strings.ToLower(strings.TrimSpace(raw))
		if hc != healthCheckPing && hc != healthCheckSelect1 && hc != "select1" && hc != "query" {
			return fmt.Errorf("health_check must be %q or %q", healthCheckPing, healthCheckSelect1)
		}
	}
	return validateDBCapacityConfig(config)
}

// Check performs a connectivity check against the configured database.
// Config fields:
//   - engine (required) — postgres, mysql, mariadb, mongodb, redis, mssql
//   - connection_string (required) — DSN/URI; alias: dsn
//   - health_check (optional) — "ping" (default) or "select_1" (fixed statement only)
//   - timeout (optional, float64, default 10)
//   - check_session_pool (optional, bool, default false)
//   - session_pool_threshold (optional, number 1–100, default 80)
//   - check_storage (optional, bool, default false)
//   - storage_threshold (optional, number 1–100, default 80)
//   - storage_max_gb (optional, GiB; required at check time when the engine cannot report capacity)
//
// config["query"] is never executed. Capacity query failures and over-threshold
// usage are emitted as typed auxiliary conditions; they never change a healthy
// primary connectivity check from StatusUp to StatusDown. Capacity queries run
// on the same cadence as the primary probe.
//
// Never returns an error — primary-check failures are returned as StatusDown;
// auxiliary capacity failures are represented by error conditions.
func (DatabaseChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	engine := dbEngine(config)
	connStr := dbConnectionString(config)
	health := dbHealthCheck(config)
	opts := parseDBCapacityOpts(config)
	opts.Engine = engine

	timeoutSec := 10.0
	if timeoutVal, ok := config["timeout"]; ok {
		if tf, ok := timeoutVal.(float64); ok && tf > 0 {
			timeoutSec = tf
		}
	}
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
	defer cancel()

	start := time.Now()

	var result ports.CheckResult
	switch engine {
	case "postgres":
		result = checkPostgres(ctx, connStr, health, opts)
	case "mysql":
		result = checkMySQL(ctx, connStr, health, opts, false)
	case "mariadb":
		result = checkMySQL(ctx, connStr, health, opts, true)
	case "mongodb":
		result = checkMongoDB(ctx, connStr, health, opts)
	case "redis":
		result = checkRedis(ctx, connStr, health, opts)
	case "mssql":
		result = checkMSSQL(ctx, connStr, health, opts)
	default:
		result = ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("unsupported engine: %s", engine),
		}
	}

	result.DurationMs = elapsedMillis(start)
	return result, nil
}

func checkPostgres(ctx context.Context, connStr, health string, opts dbCapacityOpts) ports.CheckResult {
	primaryStart := time.Now()
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("failed to create pool: %s", err.Error()),
		}
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("ping failed: %s", err.Error()),
		}
	}
	base := ports.CheckResult{Status: domain.StatusUp, Message: "connected successfully"}
	if health == healthCheckSelect1 {
		var one int
		if err := pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
			return ports.CheckResult{
				Status:  domain.StatusDown,
				Message: fmt.Sprintf("SELECT 1 failed: %s", err.Error()),
			}
		}
		base.Message = "connected, SELECT 1 ok"
	}
	base.LatencyMs = elapsedMillis(primaryStart)
	return runCapacityChecks(ctx, base, opts,
		func(ctx context.Context) (*usage, error) { return queryPostgresSessions(ctx, pool) },
		func(ctx context.Context) (*usage, error) { return queryPostgresStorage(ctx, pool, opts) },
	)
}

func checkMySQL(ctx context.Context, connStr, health string, opts dbCapacityOpts, mariadb bool) ports.CheckResult {
	primaryStart := time.Now()
	db, err := sql.Open("mysql", connStr)
	if err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("failed to open connection: %s", err.Error()),
		}
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("ping failed: %s", err.Error()),
		}
	}
	base := ports.CheckResult{Status: domain.StatusUp, Message: "connected successfully"}
	if health == healthCheckSelect1 {
		var one int
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			return ports.CheckResult{
				Status:  domain.StatusDown,
				Message: fmt.Sprintf("SELECT 1 failed: %s", err.Error()),
			}
		}
		base.Message = "connected, SELECT 1 ok"
	}
	base.LatencyMs = elapsedMillis(primaryStart)
	storFn := func(ctx context.Context) (*usage, error) { return queryMySQLStorage(ctx, db, opts) }
	if mariadb {
		storFn = func(ctx context.Context) (*usage, error) { return queryMariaDBStorage(ctx, db, opts) }
	}
	return runCapacityChecks(ctx, base, opts,
		func(ctx context.Context) (*usage, error) { return queryMySQLSessions(ctx, db) },
		storFn,
	)
}

func checkMSSQL(ctx context.Context, connStr, health string, opts dbCapacityOpts) ports.CheckResult {
	primaryStart := time.Now()
	// Driver name "sqlserver" is registered by microsoft/go-mssqldb.
	// Accept sqlserver:// URLs and ADO-style connection strings.
	db, err := sql.Open("sqlserver", connStr)
	if err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("failed to open connection: %s", err.Error()),
		}
	}
	defer func() { _ = db.Close() }()

	if err := db.PingContext(ctx); err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("ping failed: %s", err.Error()),
		}
	}
	base := ports.CheckResult{Status: domain.StatusUp, Message: "connected successfully"}
	if health == healthCheckSelect1 {
		var one int
		if err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
			return ports.CheckResult{
				Status:  domain.StatusDown,
				Message: fmt.Sprintf("SELECT 1 failed: %s", err.Error()),
			}
		}
		base.Message = "connected, SELECT 1 ok"
	}
	base.LatencyMs = elapsedMillis(primaryStart)
	return runCapacityChecks(ctx, base, opts,
		func(ctx context.Context) (*usage, error) { return queryMSSQLSessions(ctx, db) },
		func(ctx context.Context) (*usage, error) { return queryMSSQLStorage(ctx, db, opts) },
	)
}

func checkMongoDB(ctx context.Context, connStr, health string, opts dbCapacityOpts) ports.CheckResult {
	primaryStart := time.Now()
	client, err := mongo.Connect(options.Client().ApplyURI(connStr))
	if err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("failed to create client: %s", err.Error()),
		}
	}
	defer func() { _ = client.Disconnect(ctx) }()

	if err := client.Ping(ctx, nil); err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("ping failed: %s", err.Error()),
		}
	}
	// Mongo has no separate SELECT 1; ping is the health preset for both modes.
	base := ports.CheckResult{Status: domain.StatusUp, Message: "connected successfully"}
	if health == healthCheckSelect1 {
		base.Message = "connected, ping ok"
	}
	base.LatencyMs = elapsedMillis(primaryStart)
	return runCapacityChecks(ctx, base, opts,
		func(ctx context.Context) (*usage, error) { return queryMongoSessions(ctx, client) },
		func(ctx context.Context) (*usage, error) { return queryMongoStorage(ctx, client, connStr, opts) },
	)
}

func checkRedis(ctx context.Context, connStr, health string, capOpts dbCapacityOpts) ports.CheckResult {
	primaryStart := time.Now()
	var opts *redis.Options
	if strings.HasPrefix(connStr, "redis://") || strings.HasPrefix(connStr, "rediss://") {
		parsed, err := redis.ParseURL(connStr)
		if err != nil {
			return ports.CheckResult{
				Status:  domain.StatusDown,
				Message: fmt.Sprintf("invalid Redis URL: %s", err.Error()),
			}
		}
		opts = parsed
	} else {
		opts = &redis.Options{Addr: connStr}
	}

	rdb := redis.NewClient(opts)
	defer func() { _ = rdb.Close() }()

	if err := rdb.Ping(ctx).Err(); err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("ping failed: %s", err.Error()),
		}
	}
	base := ports.CheckResult{Status: domain.StatusUp, Message: "connected successfully"}
	if health == healthCheckSelect1 {
		base.Message = "connected, PING ok"
	}
	base.LatencyMs = elapsedMillis(primaryStart)
	capOpts.StorageKind = storageKindMemory
	return runCapacityChecks(ctx, base, capOpts,
		func(ctx context.Context) (*usage, error) { return queryRedisSessions(ctx, rdb) },
		func(ctx context.Context) (*usage, error) { return queryRedisStorage(ctx, rdb, capOpts) },
	)
}

func elapsedMillis(start time.Time) int64 {
	elapsed := time.Since(start).Milliseconds()
	if elapsed < 1 {
		return 1
	}
	return elapsed
}
