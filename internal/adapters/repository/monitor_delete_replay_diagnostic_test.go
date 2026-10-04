package repository_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const requireDeleteReplayDiagnosticEnv = "REQUIRE_MARIADB_DELETE_REPLAY_DIAGNOSTIC"
const deleteReplayDiagnosticAlertID = "66666666-6666-4666-8666-666666666666"

type replayDeleteBarrierPoint int

const (
	replayCursorBarrier replayDeleteBarrierPoint = iota + 1
	currentReceiptBarrier
)

type replayDeleteBarrier struct {
	point      replayDeleteBarrierPoint
	reached    chan struct{}
	release    chan struct{}
	once       sync.Once
	releaseOne sync.Once
	timedOut   bool
	mu         sync.Mutex
}

func newReplayDeleteBarrier(point replayDeleteBarrierPoint) *replayDeleteBarrier {
	return &replayDeleteBarrier{point: point, reached: make(chan struct{}), release: make(chan struct{})}
}

func (b *replayDeleteBarrier) matches(query string) bool {
	q := strings.ToLower(query)
	if b.point == replayCursorBarrier {
		return strings.HasPrefix(strings.TrimSpace(q), "update ") && strings.Contains(q, "probe_streams")
	}
	return strings.HasPrefix(strings.TrimSpace(q), "insert into ") && strings.Contains(q, "probe_state_receipts")
}

func (b *replayDeleteBarrier) after(ctx context.Context, query string, queryErr error) {
	if !b.matches(query) {
		return
	}
	b.once.Do(func() { close(b.reached) })
	if queryErr != nil {
		return
	}
	timer := time.NewTimer(2500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-b.release:
	case <-ctx.Done():
		b.markTimedOut()
	case <-timer.C:
		b.markTimedOut()
	}
}

func (b *replayDeleteBarrier) releaseNow() {
	b.releaseOne.Do(func() { close(b.release) })
}

func (b *replayDeleteBarrier) markTimedOut() {
	b.mu.Lock()
	b.timedOut = true
	b.mu.Unlock()
}

func (b *replayDeleteBarrier) didTimeOut() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.timedOut
}

type replayDeleteDiagnosticHook struct {
	mu       sync.Mutex
	counts   map[deleteDiagnosticClass]int
	failures []deleteDiagnosticFailure
	barrier  *replayDeleteBarrier
}

func newReplayDeleteDiagnosticHook(barrier *replayDeleteBarrier) *replayDeleteDiagnosticHook {
	return &replayDeleteDiagnosticHook{counts: make(map[deleteDiagnosticClass]int), barrier: barrier}
}

// Derive the operation from SQL: Bun may report SELECT for a raw DELETE statement.
func classifyReplayDeleteQuery(query string) deleteDiagnosticClass {
	q := strings.ToLower(query)
	fields := strings.Fields(strings.TrimLeft(q, " \t\r\n"))
	if len(fields) == 0 {
		return deleteDiagnosticClass{operation: "OTHER", table: "other"}
	}
	op := strings.ToUpper(fields[0])
	if op != "SELECT" && op != "UPDATE" && op != "DELETE" && op != "INSERT" && op != "REPLACE" {
		return deleteDiagnosticClass{operation: "OTHER", table: "other"}
	}
	table := directDiagnosticTable(op, fields)
	if table != "other" {
		return deleteDiagnosticClass{operation: op, table: table}
	}
	return classifyDeleteDiagnosticQuery(op, q)
}

func directDiagnosticTable(operation string, fields []string) string {
	var token string
	switch operation {
	case "UPDATE", "REPLACE":
		if len(fields) > 1 {
			token = fields[1]
		}
	case "DELETE":
		if len(fields) > 2 && strings.EqualFold(fields[1], "from") {
			token = fields[2]
		}
	case "INSERT":
		for i := 1; i+1 < len(fields); i++ {
			if strings.EqualFold(fields[i], "into") {
				token = fields[i+1]
				break
			}
		}
	case "SELECT":
		for i := 1; i+1 < len(fields); i++ {
			if strings.EqualFold(fields[i], "from") {
				token = fields[i+1]
				break
			}
		}
	}
	token = strings.Trim(token, "`\";,()")
	switch token {
	case "probe_streams", "probe_state_receipts", "probe_telemetry_receipts":
		return token
	case "probe_delivery_events":
		return token
	default:
		return "other"
	}
}

func isMonitorDeleteStartQuery(query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if strings.HasPrefix(q, "delete from ") {
		fields := strings.Fields(q)
		return len(fields) > 2 && strings.Trim(fields[2], "`\";,()") == "monitors"
	}
	if !strings.HasPrefix(q, "select ") || !strings.HasSuffix(q, " for update") {
		return false
	}
	fields := strings.Fields(q)
	for i := 1; i+1 < len(fields); i++ {
		if strings.EqualFold(fields[i], "from") {
			return strings.Trim(fields[i+1], "`\";,()") == "monitors"
		}
	}
	return false
}

func (h *replayDeleteDiagnosticHook) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	class := classifyReplayDeleteQuery(event.Query)
	if event.Stash == nil {
		event.Stash = make(map[any]any)
	}
	event.Stash[h] = class
	h.mu.Lock()
	h.counts[class]++
	h.mu.Unlock()
	return ctx
}

func (h *replayDeleteDiagnosticHook) AfterQuery(ctx context.Context, event *bun.QueryEvent) {
	class, ok := event.Stash[h].(deleteDiagnosticClass)
	if !ok {
		class = deleteDiagnosticClass{operation: "UNKNOWN", table: "other"}
	}
	if event.Err != nil && !errors.Is(event.Err, sql.ErrNoRows) {
		failure := deleteDiagnosticFailure{class: class}
		var mysqlErr *mysql.MySQLError
		if errors.As(event.Err, &mysqlErr) {
			failure.errno = mysqlErr.Number
			failure.state = string(mysqlErr.SQLState[:])
		}
		h.mu.Lock()
		h.failures = append(h.failures, failure)
		h.mu.Unlock()
	}
	if h.barrier != nil {
		h.barrier.after(ctx, event.Query, event.Err)
	}
}

func (h *replayDeleteDiagnosticHook) report(t *testing.T, subcase, version string) {
	t.Helper()
	h.mu.Lock()
	counts := make(map[deleteDiagnosticClass]int, len(h.counts))
	for class, count := range h.counts {
		counts[class] = count
	}
	failures := append([]deleteDiagnosticFailure(nil), h.failures...)
	h.mu.Unlock()
	keys := make([]deleteDiagnosticClass, 0, len(counts))
	for class := range counts {
		keys = append(keys, class)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].operation != keys[j].operation {
			return keys[i].operation < keys[j].operation
		}
		return keys[i].table < keys[j].table
	})
	parts := make([]string, 0, len(keys))
	for _, class := range keys {
		parts = append(parts, fmt.Sprintf("%s/%s=%d", class.operation, class.table, counts[class]))
	}
	errorParts := make([]string, 0, len(failures))
	reproduced := false
	for _, failure := range failures {
		errorParts = append(errorParts, fmt.Sprintf("%s/%s errno=%d state=%s",
			failure.class.operation, failure.class.table, failure.errno, failure.state))
		if failure.errno == 1020 && failure.state == "HY000" {
			reproduced = true
		}
	}
	t.Logf("delete replay diagnostic subcase=%s mariadb_version=%q query_classes=[%s] correlated_errors=[%s] 1020_reproduced=%t",
		subcase, version, strings.Join(parts, ","), strings.Join(errorParts, ";"), reproduced)
}

func renewReplayDiagnosticLease(t *testing.T, ctx context.Context, r replayFixture) {
	t.Helper()
	lease, err := repository.NewProbeConnectorStore(r.f.db).RenewConnector(ctx, domain.ProbeConnectorLease{
		ProbeID: r.session.ProbeID, OwnerID: r.session.OwnerID, Generation: r.session.ConnectionGeneration,
	})
	if err != nil {
		t.Fatalf("renew existing connector authority failed (%s)", safeDiagnosticErrorType(err))
	}
	var dbNow int64
	if err := r.f.db.NewRaw("SELECT UNIX_TIMESTAMP(UTC_TIMESTAMP())").Scan(ctx, &dbNow); err != nil {
		t.Fatalf("read database clock failed (%s)", safeDiagnosticErrorType(err))
	}
	remaining := lease.LeaseUntil.Unix() - dbNow
	if remaining < 20 || remaining > 60 {
		t.Fatalf("renewed stored connector lease has unexpected remaining seconds: %d", remaining)
	}
}

func safeDiagnosticErrorType(err error) string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%T", err)
}

type diagnosticInnoDBWait struct {
	Table              string `bun:"table_name"`
	Index              string `bun:"index_name"`
	BlockingConnection int64  `bun:"blocking_connection_id"`
}

type diagnosticTransactionState struct {
	ConnectionID int64  `bun:"connection_id"`
	State        string `bun:"trx_state"`
}

var replayDeleteWaitTables = map[string]struct{}{
	"monitors": {}, "monitor_notification": {}, "status_page_monitors": {}, "monitor_tags": {},
	"maintenance_window_monitors": {}, "tls_info": {}, "monitor_conditions": {},
	"monitor_probe_assignment_sets": {}, "monitor_probe_assignments": {},
	"monitor_probe_assignment_history": {}, "probe_observations": {},
	"monitor_probe_state": {}, "probe_missing_state": {}, "probe_dirty_buckets": {},
	"monitor_health_state": {}, "monitor_health_history": {}, "probe_incidents": {},
	"probe_delivery_events": {}, "probe_delivery_intents": {},
	"alerts": {}, "escalation_policy_monitors": {}, "notification_throttles": {},
	"user_permissions": {}, "history_clear_watermarks": {},
}

func safeDiagnosticIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '_' && r != '$' {
			return false
		}
	}
	return true
}

func allowedReplayDeleteWait(wait diagnosticInnoDBWait) bool {
	if _, ok := replayDeleteWaitTables[wait.Table]; !ok || wait.Index == "" {
		return false
	}
	return safeDiagnosticIdentifier(wait.Index)
}

const deleteMetadataIdleInterval = 150 * time.Millisecond

// MariaDB 11.8.9 and 12.3.3 share an InnoDB metadata cache that refreshes only
// after more than 100ms without a read. Wait after each completed read, including
// before the first read, so polling cannot keep a pre-wait snapshot alive.
func readDeleteMetadataAfterIdle(ctx context.Context, read func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(deleteMetadataIdleInterval)
	defer timer.Stop()
	select {
	case <-timer.C:
		if err := ctx.Err(); err != nil {
			return err
		}
		return read(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func readDeleteTransactionStates(ctx context.Context, observer *bun.DB, deleteConnectionID, sourceConnectionID int64) ([]diagnosticTransactionState, error) {
	var states []diagnosticTransactionState
	err := readDeleteMetadataAfterIdle(ctx, func(ctx context.Context) error {
		return observer.NewRaw(`SELECT trx_mysql_thread_id AS connection_id, COALESCE(trx_state, '') AS trx_state
		FROM information_schema.INNODB_TRX
		WHERE trx_mysql_thread_id IN (?, ?)`, deleteConnectionID, sourceConnectionID).Scan(ctx, &states)
	})
	if err != nil {
		return nil, fmt.Errorf("read MariaDB transaction state metadata failed (%s)", safeDiagnosticErrorType(err))
	}
	return states, nil
}

func waitForDeleteBlockedBySource(ctx context.Context, observer *bun.DB, deleteConnectionID, sourceConnectionID int64) (diagnosticInnoDBWait, []diagnosticInnoDBWait, []diagnosticTransactionState, error) {
	observeCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	var lastWaits []diagnosticInnoDBWait
	for {
		var waits []diagnosticInnoDBWait
		err := readDeleteMetadataAfterIdle(observeCtx, func(ctx context.Context) error {
			return observer.NewRaw(`SELECT
			SUBSTRING_INDEX(SUBSTRING_INDEX(REPLACE(REPLACE(requested.lock_table, CHAR(96), ''), '"', ''), '.', -1), '/', -1) AS table_name,
			COALESCE(requested.lock_index, '') AS index_name,
			blocking.trx_mysql_thread_id AS blocking_connection_id
			FROM information_schema.INNODB_LOCK_WAITS w
			JOIN information_schema.INNODB_TRX requesting ON requesting.trx_id = w.requesting_trx_id
			JOIN information_schema.INNODB_TRX blocking ON blocking.trx_id = w.blocking_trx_id
			JOIN information_schema.INNODB_LOCKS requested ON requested.lock_id = w.requested_lock_id
			WHERE requesting.trx_mysql_thread_id = ?`, deleteConnectionID).Scan(ctx, &waits)
		})
		if err != nil {
			if observeCtx.Err() != nil {
				stateCtx, stateCancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
				states, stateErr := readDeleteTransactionStates(stateCtx, observer, deleteConnectionID, sourceConnectionID)
				stateCancel()
				if stateErr != nil {
					return diagnosticInnoDBWait{}, lastWaits, states, stateErr
				}
				if ctx.Err() != nil {
					return diagnosticInnoDBWait{}, lastWaits, states, errors.New("lock-wait observer deadline expired")
				}
				return diagnosticInnoDBWait{}, lastWaits, states, errors.New("production DELETE did not expose a wait blocked by the held source transaction")
			}
			return diagnosticInnoDBWait{}, lastWaits, nil, fmt.Errorf("read MariaDB lock-wait metadata failed (%s)", safeDiagnosticErrorType(err))
		}
		lastWaits = waits
		for _, wait := range waits {
			wait.Table = strings.Trim(wait.Table, "`\"' ./")
			if wait.BlockingConnection == sourceConnectionID {
				if allowedReplayDeleteWait(wait) {
					return wait, waits, nil, nil
				}
				return diagnosticInnoDBWait{}, waits, nil, fmt.Errorf("source-blocked delete wait targets an unrecognized schema identifier (table=%q index=%q)", safeDiagnosticValue(wait.Table), safeDiagnosticValue(wait.Index))
			}
		}
	}
}

func TestDeleteMetadataIdleAllowsCacheRefresh(t *testing.T) {
	// Model the engine cache: every completed metadata read resets its idle
	// clock. A slow reader ensures the next gap is measured from completion.
	lastRead := time.Now()
	reads := 0
	for range 2 {
		err := readDeleteMetadataAfterIdle(t.Context(), func(ctx context.Context) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if gap := time.Since(lastRead); gap < deleteMetadataIdleInterval {
				return fmt.Errorf("metadata cache has not been idle long enough: %s", gap)
			}
			reads++
			time.Sleep(30 * time.Millisecond)
			lastRead = time.Now()
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if reads != 2 {
		t.Fatalf("metadata reads = %d, want 2", reads)
	}
}

func TestDeleteMetadataIdleCancellation(t *testing.T) {
	t.Run("already-canceled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		assertDeleteMetadataReadCanceled(t, ctx, context.Canceled)
	})
	t.Run("canceled-during-gap", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		timer := time.AfterFunc(10*time.Millisecond, cancel)
		defer timer.Stop()
		assertDeleteMetadataReadCanceled(t, ctx, context.Canceled)
	})
	t.Run("deadline-during-gap", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()
		assertDeleteMetadataReadCanceled(t, ctx, context.DeadlineExceeded)
	})
}

func assertDeleteMetadataReadCanceled(t *testing.T, ctx context.Context, want error) {
	t.Helper()
	readCalled := false
	err := readDeleteMetadataAfterIdle(ctx, func(context.Context) error {
		readCalled = true
		return nil
	})
	if !errors.Is(err, want) || readCalled {
		t.Fatalf("metadata cancellation: error=%v read=%t, want error=%v without a read", err, readCalled, want)
	}
}

func safeDiagnosticValue(value string) string {
	if safeDiagnosticIdentifier(value) {
		return value
	}
	return "unrecognized"
}

func summarizeDeleteWaits(waits []diagnosticInnoDBWait) string {
	if len(waits) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(waits))
	for _, wait := range waits {
		parts = append(parts, fmt.Sprintf("blocker=%d table=%s index=%s", wait.BlockingConnection,
			safeDiagnosticValue(wait.Table), safeDiagnosticValue(wait.Index)))
	}
	return strings.Join(parts, ";")
}

func summarizeDeleteTransactions(states []diagnosticTransactionState, deleteConnectionID, sourceConnectionID int64) string {
	stateByConnection := make(map[int64]string, len(states))
	for _, state := range states {
		stateByConnection[state.ConnectionID] = safeDiagnosticTransactionState(state.State)
	}
	deleteState, deleteExists := stateByConnection[deleteConnectionID]
	sourceState, sourceExists := stateByConnection[sourceConnectionID]
	if !deleteExists {
		deleteState = "none"
	}
	if !sourceExists {
		sourceState = "none"
	}
	return fmt.Sprintf("delete={id:%d exists:%t state:%s} source={id:%d exists:%t state:%s}",
		deleteConnectionID, deleteExists, deleteState, sourceConnectionID, sourceExists, sourceState)
}

func safeDiagnosticTransactionState(state string) string {
	switch strings.ToUpper(strings.TrimSpace(state)) {
	case "RUNNING":
		return "RUNNING"
	case "LOCK WAIT":
		return "LOCK_WAIT"
	case "ROLLING BACK":
		return "ROLLING_BACK"
	case "COMMITTING":
		return "COMMITTING"
	case "":
		return "none"
	default:
		return "other"
	}
}

func diagnosticOperationState(ch <-chan error) string {
	if len(ch) > 0 {
		return "completed"
	}
	return "pending"
}

func readDiagnosticConnectionID(t *testing.T, ctx context.Context, db *bun.DB) int64 {
	t.Helper()
	// Keep one source connection pinned so the observer can prove that the
	// exact held source transaction, rather than an unrelated transaction,
	// blocks the delete request.
	db.SetMaxOpenConns(1)
	var connectionID int64
	if err := db.NewRaw("SELECT CONNECTION_ID()").Scan(ctx, &connectionID); err != nil {
		t.Fatalf("read source connection id failed (%s)", safeDiagnosticErrorType(err))
	}
	return connectionID
}

func diagnosticCount(t *testing.T, ctx context.Context, db *bun.DB, table, where string, args ...any) int {
	t.Helper()
	count, err := db.NewSelect().Table(table).Where(where, args...).Count(ctx)
	if err != nil {
		t.Fatalf("read diagnostic table %s failed (%s)", table, safeDiagnosticErrorType(err))
	}
	return count
}

// Successful DELETE must remove every seeded monitor-owned cascade row.
func assertReplayDeleteCascadeAbsent(t *testing.T, ctx context.Context, db *bun.DB, monitorID int64) {
	t.Helper()
	for _, table := range replayDeleteCascadeTables {
		if count := diagnosticCount(t, ctx, db, table.name, table.column+" = ?", monitorID); count != 0 {
			t.Fatalf("table %s rows for target differ: got=%d want=0", table.name, count)
		}
	}
	if count := diagnosticCount(t, ctx, db, "probe_delivery_events", "source_alert_id = ?", deleteReplayDiagnosticAlertID); count != 0 {
		t.Fatalf("table probe_delivery_events rows for seeded source alert differ: got=%d want=0", count)
	}
}

var replayDeleteCascadeTables = []struct{ name, column string }{
	{"monitors", "id"}, {"probe_observations", "monitor_id"}, {"monitor_probe_state", "monitor_id"},
	{"probe_missing_state", "monitor_id"}, {"probe_incidents", "monitor_id"},
	{"probe_dirty_buckets", "monitor_id"}, {"monitor_probe_assignment_sets", "monitor_id"},
	{"monitor_probe_assignments", "monitor_id"}, {"monitor_probe_assignment_history", "monitor_id"},
}

func captureReplayDeleteRows(t *testing.T, ctx context.Context, db *bun.DB, monitorID int64) map[string]int {
	t.Helper()
	counts := make(map[string]int, len(replayDeleteCascadeTables))
	for _, table := range replayDeleteCascadeTables {
		counts[table.name] = diagnosticCount(t, ctx, db, table.name, table.column+" = ?", monitorID)
	}
	counts["probe_delivery_events"] = diagnosticCount(t, ctx, db, "probe_delivery_events", "source_alert_id = ?", deleteReplayDiagnosticAlertID)
	return counts
}

func assertReplayDeleteRowsUnchanged(t *testing.T, ctx context.Context, db *bun.DB, monitorID int64, baseline map[string]int, additionalObservations int) {
	t.Helper()
	for _, table := range replayDeleteCascadeTables {
		want := baseline[table.name]
		if table.name == "probe_observations" {
			want += additionalObservations
		}
		if got := diagnosticCount(t, ctx, db, table.name, table.column+" = ?", monitorID); got != want {
			t.Fatalf("delete rollback row count for %s differs: got=%d want=%d", table.name, got, want)
		}
	}
	if got := diagnosticCount(t, ctx, db, "probe_delivery_events", "source_alert_id = ?", deleteReplayDiagnosticAlertID); got != baseline["probe_delivery_events"] {
		t.Fatalf("delete rollback row count for probe_delivery_events differs: got=%d want=%d", got, baseline["probe_delivery_events"])
	}
}

func assertReplayAuthorityAndSentinel(t *testing.T, ctx context.Context, r replayFixture, sentinelID int64) {
	t.Helper()
	if diagnosticCount(t, ctx, r.f.db, "probes", "id = ?", r.session.ProbeID) != 1 ||
		diagnosticCount(t, ctx, r.f.db, "probe_sessions", "probe_id = ? AND owner_id = ? AND generation = ? AND lease_until > UNIX_TIMESTAMP(UTC_TIMESTAMP())",
			r.session.ProbeID, r.session.OwnerID, r.session.ConnectionGeneration) != 1 ||
		diagnosticCount(t, ctx, r.f.db, "probe_connections", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 1 ||
		diagnosticCount(t, ctx, r.f.db, "probe_streams", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 1 {
		t.Fatal("remote probe registration/session/connection/stream authority changed")
	}
	if _, err := mariadb.NewMonitorRepo(r.f.db).GetByID(ctx, sentinelID); err != nil {
		t.Fatalf("unrelated sentinel monitor did not survive (%s)", safeDiagnosticErrorType(err))
	}
	assignments, err := r.f.assignments.GetByMonitorID(ctx, sentinelID)
	if err != nil || assignments == nil || assignments.Revision != 1 || len(assignments.Assignments) != 1 ||
		assignments.Assignments[0].ProbeID != domain.LocalProbeID {
		t.Fatalf("unrelated sentinel assignment changed (err=%s)", safeDiagnosticErrorType(err))
	}
}

func TestMonitorDeleteReplayDiagnostic(t *testing.T) {
	if os.Getenv("TEST_MARIADB_DSN") == "" {
		if os.Getenv(requireDeleteReplayDiagnosticEnv) == "1" {
			t.Fatal("required MariaDB replay/delete diagnostic cannot skip without TEST_MARIADB_DSN")
		}
		t.Skip("TEST_MARIADB_DSN is unset; skipping MariaDB replay/delete diagnostic")
	}

	t.Run("ReplayCommitVersusDelete", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		r := newReplayFixture(t, "mariadb")
		var version string
		if err := r.f.db.NewRaw("SELECT VERSION()").Scan(ctx, &version); err != nil {
			t.Fatalf("read MariaDB version failed (%s)", safeDiagnosticErrorType(err))
		}
		sentinelID := localMonitor(t, r.f)
		seeded, err := r.store.IngestReplayBatch(ctx, r.session, r.batch(r.observation(1), r.incident(2, 1), r.delivery(3, 1, 1, domain.DeliveryStatusSent)), &services.AccessService{})
		if err != nil || seeded.AcceptedCount != 3 || seeded.CommittedSeq != 3 {
			t.Fatalf("seed production replay failed: accepted=%d committed=%d err=%s", resultAccepted(seeded), resultCommitted(seeded), safeDiagnosticErrorType(err))
		}
		if diagnosticCount(t, ctx, r.f.db, "probe_observations", "monitor_id = ?", r.monitor) != 1 ||
			diagnosticCount(t, ctx, r.f.db, "monitor_probe_state", "monitor_id = ?", r.monitor) != 1 ||
			diagnosticCount(t, ctx, r.f.db, "probe_incidents", "monitor_id = ?", r.monitor) != 1 ||
			diagnosticCount(t, ctx, r.f.db, "probe_delivery_events", "source_alert_id = ?", deleteReplayDiagnosticAlertID) != 1 {
			t.Fatal("valid replay seed did not create expected monitor-owned rows")
		}
		baselineRows := captureReplayDeleteRows(t, ctx, r.f.db, r.monitor)
		renewReplayDiagnosticLease(t, ctx, r)
		sourceConnID := readDiagnosticConnectionID(t, ctx, r.f.db)

		barrier := newReplayDeleteBarrier(replayCursorBarrier)
		hook := newReplayDeleteDiagnosticHook(barrier)
		defer hook.report(t, "replay-commit-versus-delete", version)
		r.f.db.AddQueryHook(hook)
		peer := reopenConfigDB(t, r.f)
		peer.SetMaxOpenConns(1)
		deleteHook := newDeleteDiagnosticHook()
		defer deleteHook.report(t, "replay-commit-versus-delete-parent", version)
		defer barrier.releaseNow()
		peer.AddQueryHook(deleteHook)
		var deleteConnectionID int64
		if err := peer.NewRaw("SELECT CONNECTION_ID()").Scan(ctx, &deleteConnectionID); err != nil {
			t.Fatalf("read delete connection id failed (%s)", safeDiagnosticErrorType(err))
		}
		if deleteConnectionID == sourceConnID {
			t.Fatal("source and delete handles unexpectedly share a MariaDB connection")
		}
		observer := reopenConfigDB(t, r.f)
		mutationStarted := make(chan struct{}, 1)
		peer.AddQueryHook(monitorMutationQueryHook{before: func(_ context.Context, query string) {
			if isMonitorDeleteStartQuery(query) {
				select {
				case mutationStarted <- struct{}{}:
				default:
				}
			}
		}})
		replayDone := make(chan error, 1)
		deleteDone := make(chan error, 1)
		go func() {
			_, replayErr := r.store.IngestReplayBatch(ctx, r.session, r.batch(r.observation(4)), &services.AccessService{})
			replayDone <- replayErr
		}()
		select {
		case <-barrier.reached:
		case replayErr := <-replayDone:
			t.Fatalf("replay returned before its committed-cursor barrier (%s)", safeDiagnosticErrorType(replayErr))
		case <-ctx.Done():
			barrier.releaseNow()
			if !drainDiagnosticResults(replayDone) {
				t.Errorf("replay operation did not drain within bounded cleanup window")
			}
			t.Fatal("replay transaction did not reach its committed-cursor barrier")
		}
		go func() { deleteDone <- mariadb.NewMonitorRepo(peer).Delete(ctx, r.monitor) }()
		select {
		case <-mutationStarted:
		case deleteErr := <-deleteDone:
			barrier.releaseNow()
			if !drainDiagnosticResults(replayDone) {
				t.Errorf("replay operation did not drain within bounded cleanup window")
			}
			t.Fatalf("delete returned before its statement hook (%s)", safeDiagnosticErrorType(deleteErr))
		case <-ctx.Done():
			barrier.releaseNow()
			if !drainDiagnosticResults(replayDone, deleteDone) {
				t.Errorf("replay/delete operations did not drain within bounded cleanup window")
			}
			t.Fatal("production delete did not reach monitor statement")
		}
		observedWait, observedWaits, transactionStates, err := waitForDeleteBlockedBySource(ctx, observer, deleteConnectionID, sourceConnID)
		if err != nil {
			t.Logf("delete wait diagnostic while replay barrier held: replay_state=%s waits=[%s] transactions=[%s]",
				diagnosticOperationState(replayDone), summarizeDeleteWaits(observedWaits), summarizeDeleteTransactions(transactionStates, deleteConnectionID, sourceConnID))
			barrier.releaseNow()
			replayErr := receiveDiagnosticResult(ctx, replayDone)
			deleteErr := receiveDiagnosticResult(ctx, deleteDone)
			if replayErr == nil && deleteErr != nil {
				assertReplayAuthorityAndSentinel(t, ctx, r, sentinelID)
				assertReplayDeleteRowsUnchanged(t, ctx, r.f.db, r.monitor, baselineRows, 1)
				if diagnosticCount(t, ctx, r.f.db, "probe_streams", "probe_id = ? AND stream_id = ? AND committed_seq = 4", r.session.ProbeID, r.session.StreamID) != 1 ||
					diagnosticCount(t, ctx, r.f.db, "probe_telemetry_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 4 {
					t.Fatal("source transaction did not commit its observation while DELETE failed before lock observation")
				}
			} else if replayErr != nil && deleteErr != nil {
				assertReplayAuthorityAndSentinel(t, ctx, r, sentinelID)
				assertReplayDeleteRowsUnchanged(t, ctx, r.f.db, r.monitor, baselineRows, 0)
				if diagnosticCount(t, ctx, r.f.db, "probe_streams", "probe_id = ? AND stream_id = ? AND committed_seq = 3", r.session.ProbeID, r.session.StreamID) != 1 ||
					diagnosticCount(t, ctx, r.f.db, "probe_telemetry_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 3 {
					t.Fatal("both failed transactions did not preserve the replay seed state")
				}
			} else if replayErr != nil {
				assertReplayAuthorityAndSentinel(t, ctx, r, sentinelID)
				if diagnosticCount(t, ctx, r.f.db, "monitors", "id = ?", r.monitor) != 0 ||
					diagnosticCount(t, ctx, r.f.db, "probe_streams", "probe_id = ? AND stream_id = ? AND committed_seq = 3", r.session.ProbeID, r.session.StreamID) != 1 ||
					diagnosticCount(t, ctx, r.f.db, "probe_telemetry_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 3 {
					t.Fatal("failed replay was not rolled back before an independent successful delete")
				}
			} else {
				assertReplayAuthorityAndSentinel(t, ctx, r, sentinelID)
				assertReplayDeleteCascadeAbsent(t, ctx, r.f.db, r.monitor)
				if diagnosticCount(t, ctx, r.f.db, "probe_streams", "probe_id = ? AND stream_id = ? AND committed_seq = 4", r.session.ProbeID, r.session.StreamID) != 1 ||
					diagnosticCount(t, ctx, r.f.db, "probe_telemetry_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 4 {
					t.Fatal("successful transactions did not preserve source receipts/cursor")
				}
			}
			t.Fatalf("delete was not observed blocked by the held source transaction: %s; replay=%s delete=%s", err, safeDiagnosticErrorType(replayErr), safeDiagnosticErrorType(deleteErr))
		}
		t.Logf("delete lock wait correlated to source transaction: table=%s index=%s", observedWait.Table, observedWait.Index)
		barrier.releaseNow()
		replayErr := receiveDiagnosticResult(ctx, replayDone)
		deleteErr := receiveDiagnosticResult(ctx, deleteDone)
		if replayErr != nil || deleteErr != nil {
			assertReplayAuthorityAndSentinel(t, ctx, r, sentinelID)
			if replayErr != nil {
				if deleteErr == nil {
					if diagnosticCount(t, ctx, r.f.db, "monitors", "id = ?", r.monitor) != 0 {
						t.Fatal("successful delete did not remove the target monitor")
					}
				} else {
					assertReplayDeleteRowsUnchanged(t, ctx, r.f.db, r.monitor, baselineRows, 0)
				}
				if diagnosticCount(t, ctx, r.f.db, "probe_streams", "probe_id = ? AND stream_id = ? AND committed_seq = 3", r.session.ProbeID, r.session.StreamID) != 1 ||
					diagnosticCount(t, ctx, r.f.db, "probe_telemetry_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 3 {
					t.Fatal("failed replay did not roll back its cursor and telemetry receipt")
				}
			} else if deleteErr != nil {
				assertReplayDeleteRowsUnchanged(t, ctx, r.f.db, r.monitor, baselineRows, 1)
				if diagnosticCount(t, ctx, r.f.db, "probe_streams", "probe_id = ? AND stream_id = ? AND committed_seq = 4", r.session.ProbeID, r.session.StreamID) != 1 ||
					diagnosticCount(t, ctx, r.f.db, "probe_telemetry_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 4 {
					t.Fatal("replay commit was not retained when monitor deletion rolled back")
				}
			}
			t.Fatalf("production overlap returned errors: replay=%s delete=%s", safeDiagnosticErrorType(replayErr), safeDiagnosticErrorType(deleteErr))
		}
		if barrier.didTimeOut() {
			t.Fatal("replay barrier exceeded its short bounded hold")
		}
		assertReplayDeleteCascadeAbsent(t, ctx, r.f.db, r.monitor)
		assertReplayAuthorityAndSentinel(t, ctx, r, sentinelID)
		if diagnosticCount(t, ctx, r.f.db, "probe_telemetry_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 4 ||
			diagnosticCount(t, ctx, r.f.db, "probe_streams", "probe_id = ? AND stream_id = ? AND committed_seq = 4", r.session.ProbeID, r.session.StreamID) != 1 {
			t.Fatal("successful replay evidence/cursor was not retained after monitor cascade")
		}
	})

	t.Run("CurrentSnapshotCommitVersusDelete", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		r := newReplayFixture(t, "mariadb")
		var version string
		if err := r.f.db.NewRaw("SELECT VERSION()").Scan(ctx, &version); err != nil {
			t.Fatalf("read MariaDB version failed (%s)", safeDiagnosticErrorType(err))
		}
		sentinelID := localMonitor(t, r.f)
		activateReplayConfig(t, r)
		seedObservation := r.observation(1)
		seedObservation.Observation.Ping = 23
		seedObservation.Observation.Message = "source current"
		seeded, err := r.store.IngestReplayBatch(ctx, r.session, r.batch(seedObservation), &services.AccessService{})
		if err != nil || seeded.AcceptedCount != 1 || seeded.CommittedSeq != 1 {
			t.Fatalf("seed production observation failed: accepted=%d committed=%d err=%s", resultAccepted(seeded), resultCommitted(seeded), safeDiagnosticErrorType(err))
		}
		baselineRows := captureReplayDeleteRows(t, ctx, r.f.db, r.monitor)
		renewReplayDiagnosticLease(t, ctx, r)
		sourceConnID := readDiagnosticConnectionID(t, ctx, r.f.db)

		barrier := newReplayDeleteBarrier(currentReceiptBarrier)
		hook := newReplayDeleteDiagnosticHook(barrier)
		defer hook.report(t, "current-snapshot-commit-versus-delete", version)
		r.f.db.AddQueryHook(hook)
		peer := reopenConfigDB(t, r.f)
		peer.SetMaxOpenConns(1)
		deleteHook := newDeleteDiagnosticHook()
		defer deleteHook.report(t, "current-snapshot-commit-versus-delete-parent", version)
		defer barrier.releaseNow()
		peer.AddQueryHook(deleteHook)
		var deleteConnectionID int64
		if err := peer.NewRaw("SELECT CONNECTION_ID()").Scan(ctx, &deleteConnectionID); err != nil {
			t.Fatalf("read delete connection id failed (%s)", safeDiagnosticErrorType(err))
		}
		if deleteConnectionID == sourceConnID {
			t.Fatal("source and delete handles unexpectedly share a MariaDB connection")
		}
		observer := reopenConfigDB(t, r.f)
		mutationStarted := make(chan struct{}, 1)
		peer.AddQueryHook(monitorMutationQueryHook{before: func(_ context.Context, query string) {
			if isMonitorDeleteStartQuery(query) {
				select {
				case mutationStarted <- struct{}{}:
				default:
				}
			}
		}})
		snapshotDone := make(chan error, 1)
		deleteDone := make(chan error, 1)
		go func() {
			_, snapshotErr := r.store.ApplyCurrentSnapshot(ctx, r.session,
				currentSnapshot(r, 1, currentEntry(r, 1, domain.StatusUp)), &services.AccessService{})
			snapshotDone <- snapshotErr
		}()
		select {
		case <-barrier.reached:
		case snapshotErr := <-snapshotDone:
			t.Fatalf("current snapshot returned before its receipt barrier (%s)", safeDiagnosticErrorType(snapshotErr))
		case <-ctx.Done():
			barrier.releaseNow()
			if !drainDiagnosticResults(snapshotDone) {
				t.Errorf("current snapshot operation did not drain within bounded cleanup window")
			}
			t.Fatal("current snapshot did not reach its receipt barrier")
		}
		go func() { deleteDone <- mariadb.NewMonitorRepo(peer).Delete(ctx, r.monitor) }()
		select {
		case <-mutationStarted:
		case deleteErr := <-deleteDone:
			barrier.releaseNow()
			if !drainDiagnosticResults(snapshotDone) {
				t.Errorf("snapshot operation did not drain within bounded cleanup window")
			}
			t.Fatalf("delete returned before its statement hook (%s)", safeDiagnosticErrorType(deleteErr))
		case <-ctx.Done():
			barrier.releaseNow()
			if !drainDiagnosticResults(snapshotDone, deleteDone) {
				t.Errorf("snapshot/delete operations did not drain within bounded cleanup window")
			}
			t.Fatal("production delete did not reach monitor statement")
		}
		observedWait, observedWaits, transactionStates, err := waitForDeleteBlockedBySource(ctx, observer, deleteConnectionID, sourceConnID)
		if err != nil {
			t.Logf("delete wait diagnostic while snapshot barrier held: snapshot_state=%s waits=[%s] transactions=[%s]",
				diagnosticOperationState(snapshotDone), summarizeDeleteWaits(observedWaits), summarizeDeleteTransactions(transactionStates, deleteConnectionID, sourceConnID))
			barrier.releaseNow()
			snapshotErr := receiveDiagnosticResult(ctx, snapshotDone)
			deleteErr := receiveDiagnosticResult(ctx, deleteDone)
			if snapshotErr == nil && deleteErr != nil {
				assertReplayAuthorityAndSentinel(t, ctx, r, sentinelID)
				assertReplayDeleteRowsUnchanged(t, ctx, r.f.db, r.monitor, baselineRows, 0)
				if diagnosticCount(t, ctx, r.f.db, "probe_state_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 1 {
					t.Fatal("snapshot receipt did not commit while DELETE failed before lock observation")
				}
			} else if snapshotErr != nil && deleteErr != nil {
				assertReplayAuthorityAndSentinel(t, ctx, r, sentinelID)
				assertReplayDeleteRowsUnchanged(t, ctx, r.f.db, r.monitor, baselineRows, 0)
				if diagnosticCount(t, ctx, r.f.db, "probe_state_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 0 {
					t.Fatal("failed snapshot transaction unexpectedly persisted its receipt")
				}
			} else if snapshotErr != nil {
				assertReplayAuthorityAndSentinel(t, ctx, r, sentinelID)
				if diagnosticCount(t, ctx, r.f.db, "monitors", "id = ?", r.monitor) != 0 ||
					diagnosticCount(t, ctx, r.f.db, "probe_state_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 0 {
					t.Fatal("failed snapshot was not rolled back before an independent successful delete")
				}
			} else {
				assertReplayAuthorityAndSentinel(t, ctx, r, sentinelID)
				assertReplayDeleteCascadeAbsent(t, ctx, r.f.db, r.monitor)
				if diagnosticCount(t, ctx, r.f.db, "probe_state_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 1 {
					t.Fatal("successful current snapshot receipt was not retained after deletion")
				}
			}
			t.Fatalf("delete was not observed blocked by the held source transaction: %s; snapshot=%s delete=%s", err, safeDiagnosticErrorType(snapshotErr), safeDiagnosticErrorType(deleteErr))
		}
		t.Logf("delete lock wait correlated to source transaction: table=%s index=%s", observedWait.Table, observedWait.Index)
		barrier.releaseNow()
		snapshotErr := receiveDiagnosticResult(ctx, snapshotDone)
		deleteErr := receiveDiagnosticResult(ctx, deleteDone)
		if snapshotErr != nil || deleteErr != nil {
			assertReplayAuthorityAndSentinel(t, ctx, r, sentinelID)
			if deleteErr != nil {
				assertReplayDeleteRowsUnchanged(t, ctx, r.f.db, r.monitor, baselineRows, 0)
			} else if snapshotErr != nil && diagnosticCount(t, ctx, r.f.db, "monitors", "id = ?", r.monitor) != 0 {
				t.Fatal("successful delete did not remove the target monitor")
			}
			wantReceipts := 1
			if snapshotErr != nil {
				wantReceipts = 0
			}
			if diagnosticCount(t, ctx, r.f.db, "probe_state_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != wantReceipts {
				t.Fatalf("snapshot receipt rollback mismatch: want %d", wantReceipts)
			}
			t.Fatalf("production overlap returned errors: snapshot=%s delete=%s", safeDiagnosticErrorType(snapshotErr), safeDiagnosticErrorType(deleteErr))
		}
		if barrier.didTimeOut() {
			t.Fatal("current-snapshot barrier exceeded its short bounded hold")
		}
		assertReplayDeleteCascadeAbsent(t, ctx, r.f.db, r.monitor)
		assertReplayAuthorityAndSentinel(t, ctx, r, sentinelID)
		if diagnosticCount(t, ctx, r.f.db, "probe_state_receipts", "probe_id = ? AND stream_id = ?", r.session.ProbeID, r.session.StreamID) != 1 ||
			diagnosticCount(t, ctx, r.f.db, "probe_streams", "probe_id = ? AND stream_id = ? AND committed_seq = 1", r.session.ProbeID, r.session.StreamID) != 1 {
			t.Fatal("current-state receipt or replay cursor changed after monitor cascade")
		}
	})
}

func resultAccepted(result *domain.ProbeReplayResult) int64 {
	if result == nil {
		return -1
	}
	return result.AcceptedCount
}

func resultCommitted(result *domain.ProbeReplayResult) int64 {
	if result == nil {
		return -1
	}
	return result.CommittedSeq
}

func receiveDiagnosticResult(ctx context.Context, result <-chan error) error {
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func drainDiagnosticResults(channels ...<-chan error) bool {
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	pending := append([]<-chan error(nil), channels...)
	for len(pending) > 0 {
		remaining := pending[:0]
		for _, ch := range pending {
			select {
			case <-ch:
			default:
				remaining = append(remaining, ch)
			}
		}
		pending = remaining
		if len(pending) == 0 {
			return true
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline.C:
			return false
		}
	}
	return true
}

func TestClassifyReplayDeleteDiagnosticQueryUsesDirectTable(t *testing.T) {
	tests := []struct {
		query string
		want  deleteDiagnosticClass
	}{
		{"UPDATE probe_streams SET committed_seq = ? WHERE probe_id = ?", deleteDiagnosticClass{operation: "UPDATE", table: "probe_streams"}},
		{"INSERT INTO probe_state_receipts (probe_id, stream_id) VALUES (?, ?)", deleteDiagnosticClass{operation: "INSERT", table: "probe_state_receipts"}},
		{"INSERT INTO probe_delivery_events (delivery_id, source_alert_id) VALUES (?, ?)", deleteDiagnosticClass{operation: "INSERT", table: "probe_delivery_events"}},
		{"DELETE FROM monitors WHERE id IN (SELECT monitor_id FROM probe_streams)", deleteDiagnosticClass{operation: "MONITOR_DELETE", table: "monitors"}},
	}
	for _, tt := range tests {
		if got := classifyReplayDeleteQuery(tt.query); got != tt.want {
			t.Errorf("classification for %q = %+v, want %+v", tt.query, got, tt.want)
		}
	}
	for query, want := range map[string]bool{
		"SELECT `id` FROM `monitors` WHERE `id` = ? FOR UPDATE":                               true,
		"DELETE FROM `monitors` WHERE `id` = ?":                                               true,
		"SELECT `id` FROM `probe_streams` WHERE EXISTS (SELECT 1 FROM `monitors` FOR UPDATE)": false,
		"SELECT `id` FROM `monitors` WHERE `id` = ?":                                          false,
		"SELECT `id` FROM `monitors` WHERE EXISTS (SELECT 1 FROM `other` FOR UPDATE)":         false,
	} {
		if got := isMonitorDeleteStartQuery(query); got != want {
			t.Errorf("monitor mutation start classification for %q = %t, want %t", query, got, want)
		}
	}
}

func TestReplayDeleteWaitRequiresCascadeTableAndSafeIndex(t *testing.T) {
	for _, tt := range []struct {
		wait diagnosticInnoDBWait
		want bool
	}{
		{diagnosticInnoDBWait{Table: "monitors", Index: "PRIMARY"}, true},
		{diagnosticInnoDBWait{Table: "probe_observations", Index: "fk_observation_monitor"}, true},
		{diagnosticInnoDBWait{Table: "probes", Index: "PRIMARY"}, false},
		{diagnosticInnoDBWait{Table: "other_table", Index: "PRIMARY"}, false},
		{diagnosticInnoDBWait{Table: "probe_observations", Index: "PRIMARY;select"}, false},
		{diagnosticInnoDBWait{Table: "probe_observations", Index: ""}, false},
	} {
		if got := allowedReplayDeleteWait(tt.wait); got != tt.want {
			t.Errorf("allowedReplayDeleteWait(%+v) = %t, want %t", tt.wait, got, tt.want)
		}
	}
	if got := summarizeDeleteWaits([]diagnosticInnoDBWait{{Table: "probe_observations", Index: "PRIMARY", BlockingConnection: 17}}); got != "blocker=17 table=probe_observations index=PRIMARY" {
		t.Errorf("safe wait summary = %q", got)
	}
	if got := summarizeDeleteWaits([]diagnosticInnoDBWait{{Table: "secret.schema", Index: "idx\nprivate", BlockingConnection: 17}}); got != "blocker=17 table=unrecognized index=unrecognized" {
		t.Errorf("unsafe wait summary was not redacted: %q", got)
	}
	if got := summarizeDeleteTransactions([]diagnosticTransactionState{{ConnectionID: 17, State: "LOCK WAIT"}}, 17, 18); got != "delete={id:17 exists:true state:LOCK_WAIT} source={id:18 exists:false state:none}" {
		t.Errorf("transaction state summary = %q", got)
	}
}

var _ bun.QueryHook = (*replayDeleteDiagnosticHook)(nil)
