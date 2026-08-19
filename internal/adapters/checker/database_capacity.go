package checker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Fixed capacity queries. Never interpolate operator text — these are constants only.
const (
	sqlPostgresSessions = `SELECT COALESCE(SUM(numbackends), 0)::float8 AS used,
       current_setting('max_connections')::float8 AS max
FROM pg_stat_database`

	// Database size only (CONNECT is enough). Do not use pg_stat_file / superuser disk APIs.
	sqlPostgresStorageUsed = `SELECT pg_database_size(current_database())::float8`

	// Sum every non-template database. Needs CONNECT on each database or pg_read_all_stats.
	// Still logical database size — not WAL, logs, temp, backups, or filesystem free space.
	sqlPostgresStorageUsedInstance = `SELECT COALESCE(SUM(pg_database_size(datname)), 0)::float8
FROM pg_database
WHERE NOT datistemplate`

	sqlMySQLSessions = `SELECT
  CAST((SELECT VARIABLE_VALUE FROM performance_schema.global_status
        WHERE VARIABLE_NAME = 'Threads_connected') AS DECIMAL(20,2)) AS used,
  CAST((SELECT VARIABLE_VALUE FROM performance_schema.global_variables
        WHERE VARIABLE_NAME = 'max_connections') AS DECIMAL(20,2)) AS max`

	sqlMySQLStatusThreads = `SHOW GLOBAL STATUS LIKE 'Threads_connected'`
	sqlMySQLVarMaxConn    = `SHOW GLOBAL VARIABLES LIKE 'max_connections'`

	sqlMySQLStorage = `SELECT COALESCE(SUM(data_length + index_length), 0)
FROM information_schema.tables
WHERE table_schema = DATABASE()`

	// Visible schemas only — information_schema hides tables the user cannot see.
	sqlMySQLStorageInstance = `SELECT COALESCE(SUM(data_length + index_length), 0)
FROM information_schema.tables
WHERE table_schema NOT IN ('information_schema', 'performance_schema')`

	// DISKS emits one row per mount path; the same block device is repeated.
	// Sum unique devices or GiB is multiplied by the bind-mount count.
	sqlMariaDBDisks = `SELECT COALESCE(SUM(used_kib), 0), COALESCE(SUM(total_kib), 0)
FROM (
  SELECT MAX(USED) AS used_kib, MAX(TOTAL) AS total_kib
  FROM information_schema.DISKS
  GROUP BY DISK
) AS disks`

	sqlMSSQLSessions = `SELECT CAST(COUNT(*) AS FLOAT) AS used,
       CAST(@@MAX_CONNECTIONS AS FLOAT) AS max
FROM sys.dm_exec_sessions
WHERE is_user_process = 1`

	sqlMSSQLStorage = `SELECT
  CAST(SUM(CAST(FILEPROPERTY(name, 'SpaceUsed') AS bigint)) * 8.0 * 1024 AS FLOAT) AS used,
  CAST(SUM(CAST(size AS bigint)) * 8.0 * 1024 AS FLOAT) AS allocated
FROM sys.database_files`
)

const (
	defaultCapacityThreshold = 80.0
	bytesPerGiB              = 1024 * 1024 * 1024

	storageKindStorage = "storage"
	storageKindMemory  = "memory"

	storageScopeDatabase = "database"
	storageScopeInstance = "instance"

	msgStorageCapacityUnknown   = "storage capacity unknown; set storage_max_gb (GiB) to compare against measured database size"
	msgRedisMaxmemoryUnlimited  = "storage capacity unknown; Redis maxmemory is 0 (unlimited) — set maxmemory or storage_max_gb"
	hintMSSQLSessionPool        = "SQL Server needs VIEW SERVER STATE for sys.dm_exec_sessions"
	hintMongoSessionPool        = "MongoDB needs clusterMonitor for serverStatus connections"
	hintRedisInfo               = "Redis needs +info ACL"
	hintPostgresInstanceStorage = "PostgreSQL instance storage needs CONNECT on each database or GRANT pg_read_all_stats"
	hintMySQLInstanceStorage    = "MySQL instance storage only includes schemas the user can see — GRANT SELECT on each database or SELECT ON *.*"
	errSessionPoolNoMeasurement = "session pool check failed: no measurement"
	errStorageNoMeasurement     = "storage check failed: no measurement"
)

var errCapacityUnknown = errors.New("capacity unknown")

// usage is used/max in one unit (connections, or bytes for storage/memory).
type usage struct {
	Used float64
	Max  float64
}

// dbCapacityOpts is parsed from the monitor JSON config map.
type dbCapacityOpts struct {
	CheckSessionPool     bool
	SessionPoolThreshold float64
	CheckStorage         bool
	StorageThreshold     float64
	StorageMaxGB         float64
	HasStorageMaxGB      bool
	StorageScope         string // "database" (default) or "instance"
	StorageKind          string // "storage" (default) or "memory" (Redis)
	Engine               string
}

func parseDBCapacityOpts(config map[string]any) dbCapacityOpts {
	opts := dbCapacityOpts{
		SessionPoolThreshold: defaultCapacityThreshold,
		StorageThreshold:     defaultCapacityThreshold,
		StorageKind:          storageKindStorage,
		StorageScope:         storageScopeDatabase,
	}
	if config == nil {
		return opts
	}
	opts.CheckSessionPool = configBool(config, "check_session_pool")
	opts.CheckStorage = configBool(config, "check_storage")

	if v, ok, err := configFloat(config, "session_pool_threshold"); err == nil && ok && v > 0 {
		opts.SessionPoolThreshold = v
	}
	if v, ok, err := configFloat(config, "storage_threshold"); err == nil && ok && v > 0 {
		opts.StorageThreshold = v
	}
	if v, ok, err := configFloat(config, "storage_max_gb"); err == nil && ok && v > 0 {
		opts.StorageMaxGB = v
		opts.HasStorageMaxGB = true
	}
	opts.StorageScope = parseStorageScope(config)
	return opts
}

func validateDBCapacityConfig(config map[string]any) error {
	if err := validateCapacityThreshold(config, "session_pool_threshold"); err != nil {
		return err
	}
	if err := validateCapacityThreshold(config, "storage_threshold"); err != nil {
		return err
	}
	v, ok, err := configFloat(config, "storage_max_gb")
	if err != nil {
		return err
	}
	if ok && v <= 0 {
		return fmt.Errorf("storage_max_gb must be greater than 0")
	}
	return validateStorageScope(config)
}

func parseStorageScope(config map[string]any) string {
	if config == nil {
		return storageScopeDatabase
	}
	raw, _ := config["storage_scope"].(string)
	if strings.EqualFold(strings.TrimSpace(raw), storageScopeInstance) {
		return storageScopeInstance
	}
	return storageScopeDatabase
}

func validateStorageScope(config map[string]any) error {
	if config == nil {
		return nil
	}
	raw, ok := config["storage_scope"]
	if !ok || raw == nil {
		return nil
	}
	s, ok := raw.(string)
	if !ok {
		return fmt.Errorf("storage_scope must be %q or %q", storageScopeDatabase, storageScopeInstance)
	}
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", storageScopeDatabase, storageScopeInstance:
		return nil
	default:
		return fmt.Errorf("storage_scope must be %q or %q", storageScopeDatabase, storageScopeInstance)
	}
}

func validateCapacityThreshold(config map[string]any, key string) error {
	v, ok, err := configFloat(config, key)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if v < 1 {
		return fmt.Errorf("%s must be greater than or equal to 1", key)
	}
	if v > 100 {
		return fmt.Errorf("%s must be <= 100", key)
	}
	return nil
}

func configBool(config map[string]any, key string) bool {
	if config == nil {
		return false
	}
	raw, ok := config[key]
	if !ok || raw == nil {
		return false
	}
	switch v := raw.(type) {
	case bool:
		return v
	case string:
		s := strings.TrimSpace(strings.ToLower(v))
		return s == "true" || s == "1" || s == "yes"
	case int:
		return v != 0
	case int8:
		return v != 0
	case int16:
		return v != 0
	case int32:
		return v != 0
	case int64:
		return v != 0
	case uint:
		return v != 0
	case uint32:
		return v != 0
	case uint64:
		return v != 0
	case float32:
		return v != 0
	case float64:
		return v != 0
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n != 0
		}
		f, err := v.Float64()
		return err == nil && f != 0
	default:
		return false
	}
}

// configFloat returns (value, present, err). present is false when the key is
// missing or nil. Unparseable values return an error with present=true.
func configFloat(config map[string]any, key string) (float64, bool, error) {
	if config == nil {
		return 0, false, nil
	}
	raw, ok := config[key]
	if !ok || raw == nil {
		return 0, false, nil
	}
	switch v := raw.(type) {
	case float64:
		return v, true, nil
	case float32:
		return float64(v), true, nil
	case int:
		return float64(v), true, nil
	case int8:
		return float64(v), true, nil
	case int16:
		return float64(v), true, nil
	case int32:
		return float64(v), true, nil
	case int64:
		return float64(v), true, nil
	case uint:
		return float64(v), true, nil
	case uint32:
		return float64(v), true, nil
	case uint64:
		return float64(v), true, nil
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return 0, true, fmt.Errorf("%s is not a number: %w", key, err)
		}
		return f, true, nil
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return 0, true, fmt.Errorf("%s is not a number", key)
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, true, fmt.Errorf("%s is not a number: %w", key, err)
		}
		return f, true, nil
	default:
		return 0, true, fmt.Errorf("%s is not a number", key)
	}
}

func gibToBytes(gb float64) float64 {
	return gb * bytesPerGiB
}

func bytesToGiB(b float64) float64 {
	return b / bytesPerGiB
}

func usagePercent(u usage) (float64, error) {
	if u.Max <= 0 {
		return 0, errCapacityUnknown
	}
	return (u.Used / u.Max) * 100, nil
}

func thresholdReached(pct, threshold float64) bool {
	return pct+1e-9 >= threshold
}

func formatConn(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func formatThreshold(v float64) string {
	if v == math.Trunc(v) {
		return strconv.FormatFloat(v, 'f', 0, 64)
	}
	return strconv.FormatFloat(v, 'f', 1, 64)
}

func formatUsage(kind string, u usage, pct float64, asBytes bool) string {
	if asBytes {
		return fmt.Sprintf("%s %.1f/%.1f GiB (%.1f%%)", kind, bytesToGiB(u.Used), bytesToGiB(u.Max), pct)
	}
	return fmt.Sprintf("%s %s/%s (%.1f%%)", kind, formatConn(u.Used), formatConn(u.Max), pct)
}

func formatBreach(kind string, u usage, pct, threshold float64, asBytes bool) string {
	return fmt.Sprintf("%s exceeds threshold %s%%", formatUsage(kind, u, pct, asBytes), formatThreshold(threshold))
}

func storageKind(opts dbCapacityOpts) string {
	if opts.StorageKind == storageKindMemory {
		return storageKindMemory
	}
	return storageKindStorage
}

func applyCapacityChecks(base ports.CheckResult, sessions *usage, sessionErr error, storage *usage, storageErr error, opts dbCapacityOpts) ports.CheckResult {
	if base.Status != domain.StatusUp {
		return base
	}
	if !opts.CheckSessionPool && !opts.CheckStorage {
		return base
	}

	var extras []string
	conditions := make([]domain.ConditionObservation, 0, 2)

	if opts.CheckSessionPool {
		condition, extra := evaluateSession(sessions, sessionErr, opts)
		if extra != "" {
			extras = append(extras, extra)
		}
		conditions = append(conditions, condition)
	}
	if opts.CheckStorage {
		condition, extra := evaluateStorage(storage, storageErr, opts)
		if extra != "" {
			extras = append(extras, extra)
		}
		conditions = append(conditions, condition)
	}

	out := base
	out.Conditions = conditions
	if len(extras) > 0 {
		out.Message = base.Message + "; " + strings.Join(extras, "; ")
	}
	return out
}

func evaluateSession(u *usage, err error, opts dbCapacityOpts) (domain.ConditionObservation, string) {
	return evaluateUsage(u, err, baseConditionObservation(
		domain.MonitorConditionSessionPool,
		"Session pool",
		"connections",
		sessionScope(opts.Engine),
		sessionSource(opts.Engine),
		opts.SessionPoolThreshold,
	), "session pool", "session pool", "sessions", errSessionPoolNoMeasurement, false, opts.SessionPoolThreshold)
}

func evaluateStorage(u *usage, err error, opts dbCapacityOpts) (domain.ConditionObservation, string) {
	resource := storageResource(opts)
	kind := storageKind(opts)
	return evaluateUsage(u, err, baseConditionObservation(
		domain.MonitorConditionStorage,
		resource,
		"bytes",
		storageScope(opts),
		storageSource(opts),
		opts.StorageThreshold,
	), strings.ToLower(resource), kind, kind, errStorageNoMeasurement, true, opts.StorageThreshold)
}

func evaluateUsage(
	u *usage,
	queryErr error,
	condition domain.ConditionObservation,
	failName, warnName, okName, noMeasurement string,
	asBytes bool,
	threshold float64,
) (domain.ConditionObservation, string) {
	if queryErr != nil {
		condition.State = domain.ConditionStateError
		condition.Message = fmt.Sprintf("%s check failed: %s", failName, queryErr.Error())
		return condition, condition.Message
	}
	if u == nil {
		condition.State = domain.ConditionStateError
		condition.Message = noMeasurement
		return condition, condition.Message
	}
	pct, perr := usagePercent(*u)
	if perr != nil {
		condition.State = domain.ConditionStateError
		condition.Message = fmt.Sprintf("%s check failed: %s", failName, perr.Error())
		return condition, condition.Message
	}
	condition.Used = floatPtr(u.Used)
	condition.Limit = floatPtr(u.Max)
	condition.Percent = floatPtr(pct)
	if thresholdReached(pct, threshold) {
		condition.State = domain.ConditionStateWarning
		condition.Message = formatBreach(warnName, *u, pct, threshold, asBytes)
		return condition, condition.Message
	}
	condition.State = domain.ConditionStateOK
	condition.Message = formatUsage(okName, *u, pct, asBytes)
	return condition, condition.Message
}

func baseConditionObservation(kind, resource, unit, scope, source string, threshold float64) domain.ConditionObservation {
	return domain.ConditionObservation{
		Kind:       kind,
		Threshold:  floatPtr(threshold),
		Unit:       unit,
		Resource:   resource,
		Scope:      scope,
		Source:     source,
		ObservedAt: time.Now().UTC(),
	}
}

func floatPtr(value float64) *float64 { return &value }

func sessionScope(engine string) string {
	if engine == "postgres" || engine == "mongodb" {
		return "cluster"
	}
	return "server"
}

func sessionSource(engine string) string {
	switch engine {
	case "postgres":
		return "pg_stat_database / max_connections"
	case "mysql", "mariadb":
		return "Threads_connected / max_connections"
	case "mssql":
		return "dm_exec_sessions / @@MAX_CONNECTIONS"
	case "mongodb":
		return "serverStatus.connections"
	case "redis":
		return "INFO clients"
	default:
		return "database session statistics"
	}
}

func storageResource(opts dbCapacityOpts) string {
	switch opts.Engine {
	case "redis":
		return "Redis memory"
	case "postgres", "mysql":
		if usesInstanceStorage(opts) {
			return "Instance database size"
		}
		return "Database size"
	case "mssql":
		return "Database file utilization"
	default:
		return "Storage"
	}
}

func usesInstanceStorage(opts dbCapacityOpts) bool {
	if opts.StorageScope != storageScopeInstance {
		return false
	}
	switch opts.Engine {
	case "postgres", "mysql", "mariadb":
		return true
	default:
		return false
	}
}

func storageScope(opts dbCapacityOpts) string {
	if usesInstanceStorage(opts) && (opts.Engine == "postgres" || opts.Engine == "mysql") {
		return storageScopeInstance
	}
	switch opts.Engine {
	case "mariadb", "mongodb":
		return "database-or-filesystem"
	case "redis":
		return "server-memory"
	default:
		return storageScopeDatabase
	}
}

func storageSource(opts dbCapacityOpts) string {
	switch opts.Engine {
	case "postgres":
		if usesInstanceStorage(opts) {
			return "pg_database_size (all databases) / configured limit"
		}
		return "pg_database_size / configured limit"
	case "mysql":
		if usesInstanceStorage(opts) {
			return "information_schema.tables (all schemas) / configured limit"
		}
		return "information_schema.tables / configured limit"
	case "mariadb":
		return "information_schema.DISKS or database size / configured limit"
	case "mssql":
		return "sys.database_files"
	case "mongodb":
		return "dbStats filesystem or database size / configured limit"
	case "redis":
		return "INFO memory / maxmemory"
	default:
		return "database capacity statistics"
	}
}

func runCapacityChecks(ctx context.Context, base ports.CheckResult, opts dbCapacityOpts, sessFn, storFn func(context.Context) (*usage, error)) ports.CheckResult {
	var sessions *usage
	var sessionErr error
	var storage *usage
	var storageErr error
	if opts.CheckSessionPool {
		sessions, sessionErr = sessFn(ctx)
	}
	if opts.CheckStorage {
		storage, storageErr = storFn(ctx)
	}
	return applyCapacityChecks(base, sessions, sessionErr, storage, storageErr, opts)
}

func queryPostgresSessions(ctx context.Context, pool *pgxpool.Pool) (*usage, error) {
	// Phoenix's own check connection counts as +1 session; that is expected.
	var u usage
	if err := pool.QueryRow(ctx, sqlPostgresSessions).Scan(&u.Used, &u.Max); err != nil {
		return nil, err
	}
	return &u, nil
}

func postgresStorageSQL(opts dbCapacityOpts) string {
	if usesInstanceStorage(opts) {
		return sqlPostgresStorageUsedInstance
	}
	return sqlPostgresStorageUsed
}

func mysqlStorageSQL(opts dbCapacityOpts) string {
	if usesInstanceStorage(opts) {
		return sqlMySQLStorageInstance
	}
	return sqlMySQLStorage
}

func queryPostgresStorage(ctx context.Context, pool *pgxpool.Pool, opts dbCapacityOpts) (*usage, error) {
	var used float64
	if err := pool.QueryRow(ctx, postgresStorageSQL(opts)).Scan(&used); err != nil {
		if usesInstanceStorage(opts) {
			return nil, fmt.Errorf("%w (%s)", err, hintPostgresInstanceStorage)
		}
		return nil, err
	}
	if !opts.HasStorageMaxGB {
		return nil, errors.New(msgStorageCapacityUnknown)
	}
	return &usage{Used: used, Max: gibToBytes(opts.StorageMaxGB)}, nil
}

func queryMySQLSessions(ctx context.Context, db *sql.DB) (*usage, error) {
	// Phoenix's own check connection counts as +1 session; that is expected.
	var u usage
	if err := db.QueryRowContext(ctx, sqlMySQLSessions).Scan(&u.Used, &u.Max); err == nil {
		return &u, nil
	}
	used, err := mysqlShowFloat(ctx, db, sqlMySQLStatusThreads)
	if err != nil {
		return nil, err
	}
	max, err := mysqlShowFloat(ctx, db, sqlMySQLVarMaxConn)
	if err != nil {
		return nil, err
	}
	return &usage{Used: used, Max: max}, nil
}

func mysqlShowFloat(ctx context.Context, db *sql.DB, query string) (float64, error) {
	var name, value string
	if err := db.QueryRowContext(ctx, query).Scan(&name, &value); err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", name, err)
	}
	return f, nil
}

func queryMySQLStorage(ctx context.Context, db *sql.DB, opts dbCapacityOpts) (*usage, error) {
	var used float64
	if err := db.QueryRowContext(ctx, mysqlStorageSQL(opts)).Scan(&used); err != nil {
		if usesInstanceStorage(opts) {
			return nil, fmt.Errorf("%w (%s)", err, hintMySQLInstanceStorage)
		}
		return nil, err
	}
	if !opts.HasStorageMaxGB {
		return nil, errors.New(msgStorageCapacityUnknown)
	}
	return &usage{Used: used, Max: gibToBytes(opts.StorageMaxGB)}, nil
}

func queryMariaDBStorage(ctx context.Context, db *sql.DB, opts dbCapacityOpts) (*usage, error) {
	var usedKiB, totalKiB float64
	// information_schema.DISKS USED/TOTAL are KiB (1024 bytes). Convert before GiB formatting.
	if err := db.QueryRowContext(ctx, sqlMariaDBDisks).Scan(&usedKiB, &totalKiB); err == nil && totalKiB > 0 {
		return &usage{Used: usedKiB * 1024, Max: totalKiB * 1024}, nil
	}
	return queryMySQLStorage(ctx, db, opts)
}

func queryMSSQLSessions(ctx context.Context, db *sql.DB) (*usage, error) {
	// Phoenix's own check connection counts as +1 session; that is expected.
	var u usage
	if err := db.QueryRowContext(ctx, sqlMSSQLSessions).Scan(&u.Used, &u.Max); err != nil {
		return nil, fmt.Errorf("%w (%s)", err, hintMSSQLSessionPool)
	}
	return &u, nil
}

func queryMSSQLStorage(ctx context.Context, db *sql.DB, opts dbCapacityOpts) (*usage, error) {
	var used, allocated float64
	if err := db.QueryRowContext(ctx, sqlMSSQLStorage).Scan(&used, &allocated); err != nil {
		return nil, err
	}
	max := allocated
	if opts.HasStorageMaxGB {
		max = gibToBytes(opts.StorageMaxGB)
	}
	if max <= 0 {
		return nil, errors.New(msgStorageCapacityUnknown)
	}
	return &usage{Used: used, Max: max}, nil
}

func queryMongoSessions(ctx context.Context, client *mongo.Client) (*usage, error) {
	var result bson.M
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Decode(&result); err != nil {
		return nil, fmt.Errorf("%w (%s)", err, hintMongoSessionPool)
	}
	conns, ok := lookupDoc(result, "connections")
	if !ok {
		return nil, fmt.Errorf("connections missing from serverStatus (%s)", hintMongoSessionPool)
	}
	current, currentOK := lookupFloat(conns, "current")
	available, availOK := lookupFloat(conns, "available")
	if !currentOK {
		return nil, fmt.Errorf("connections.current missing (%s)", hintMongoSessionPool)
	}
	if !availOK {
		return nil, fmt.Errorf("connections.available missing; cannot compute max (%s)", hintMongoSessionPool)
	}
	return &usage{Used: current, Max: current + available}, nil
}

func queryMongoStorage(ctx context.Context, client *mongo.Client, connStr string, opts dbCapacityOpts) (*usage, error) {
	dbName := mongoDBNameFromURI(connStr)
	var result bson.M
	if err := client.Database(dbName).RunCommand(ctx, bson.D{{Key: "dbStats", Value: 1}}).Decode(&result); err != nil {
		return nil, err
	}
	fsUsed, hasUsed := lookupFloat(result, "fsUsedSize")
	fsTotal, hasTotal := lookupFloat(result, "fsTotalSize")
	if hasTotal && fsTotal > 0 {
		if !hasUsed {
			fsUsed = 0
		}
		return &usage{Used: fsUsed, Max: fsTotal}, nil
	}
	used, ok := lookupFloat(result, "storageSize")
	if !ok {
		used, ok = lookupFloat(result, "dataSize")
	}
	if !ok {
		return nil, errors.New("dbStats missing storageSize/dataSize")
	}
	if !opts.HasStorageMaxGB {
		return nil, errors.New(msgStorageCapacityUnknown)
	}
	return &usage{Used: used, Max: gibToBytes(opts.StorageMaxGB)}, nil
}

func mongoDBNameFromURI(connStr string) string {
	u, err := url.Parse(connStr)
	if err != nil {
		return "admin"
	}
	name := strings.Trim(u.Path, "/")
	if name == "" {
		return "admin"
	}
	if i := strings.IndexByte(name, '/'); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return "admin"
	}
	return name
}

func lookupDoc(doc any, key string) (any, bool) {
	v, ok := lookupAny(doc, key)
	if !ok || v == nil {
		return nil, false
	}
	switch v.(type) {
	case bson.M, map[string]any, bson.D:
		return v, true
	default:
		return nil, false
	}
}

func lookupAny(doc any, key string) (any, bool) {
	switch d := doc.(type) {
	case bson.M:
		v, ok := d[key]
		return v, ok
	case map[string]any:
		v, ok := d[key]
		return v, ok
	case bson.D:
		for _, e := range d {
			if e.Key == key {
				return e.Value, true
			}
		}
		return nil, false
	default:
		return nil, false
	}
}

func lookupFloat(doc any, key string) (float64, bool) {
	v, ok := lookupAny(doc, key)
	if !ok {
		return 0, false
	}
	return anyToFloat(v)
}

func anyToFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int8:
		return float64(n), true
	case int16:
		return float64(n), true
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint32:
		return float64(n), true
	case uint64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		// BSON Decimal128 and similar stringify to a numeric decimal.
		f, err := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(n)), 64)
		return f, err == nil
	}
}

func queryRedisSessions(ctx context.Context, rdb *redis.Client) (*usage, error) {
	// Phoenix's own check connection counts as +1 session; that is expected.
	raw, err := rdb.Info(ctx, "clients").Result()
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", err, hintRedisInfo)
	}
	m := parseRedisInfoMap(raw)
	used, ok := parseRedisInfoFloat(m, "connected_clients")
	if !ok {
		return nil, fmt.Errorf("connected_clients missing from INFO (%s)", hintRedisInfo)
	}
	max, ok := parseRedisInfoFloat(m, "maxclients")
	if !ok {
		return nil, errors.New("maxclients missing; grant +info ACL or use a newer Redis")
	}
	return &usage{Used: used, Max: max}, nil
}

func queryRedisStorage(ctx context.Context, rdb *redis.Client, opts dbCapacityOpts) (*usage, error) {
	raw, err := rdb.Info(ctx, "memory").Result()
	if err != nil {
		return nil, fmt.Errorf("%w (%s)", err, hintRedisInfo)
	}
	m := parseRedisInfoMap(raw)
	used, ok := parseRedisInfoFloat(m, "used_memory")
	if !ok {
		return nil, fmt.Errorf("used_memory missing from INFO (%s)", hintRedisInfo)
	}
	max, hasMax := parseRedisInfoFloat(m, "maxmemory")
	if hasMax && max == 0 {
		if opts.HasStorageMaxGB {
			return &usage{Used: used, Max: gibToBytes(opts.StorageMaxGB)}, nil
		}
		return nil, errors.New(msgRedisMaxmemoryUnlimited)
	}
	if !hasMax {
		if opts.HasStorageMaxGB {
			return &usage{Used: used, Max: gibToBytes(opts.StorageMaxGB)}, nil
		}
		return nil, fmt.Errorf("maxmemory missing; grant +info ACL or set storage_max_gb")
	}
	return &usage{Used: used, Max: max}, nil
}

func parseRedisInfoMap(info string) map[string]string {
	out := make(map[string]string)
	for _, line := range strings.Split(info, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(val)
	}
	return out
}

func parseRedisInfoFloat(m map[string]string, key string) (float64, bool) {
	s, ok := m[key]
	if !ok || s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}
