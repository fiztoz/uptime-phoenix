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

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const requireDeleteDiagnosticEnv = "REQUIRE_MARIADB_DELETE_DIAGNOSTIC"

type deleteDiagnosticClass struct {
	operation string
	table     string
}

type deleteDiagnosticFailure struct {
	class deleteDiagnosticClass
	errno uint16
	state string
}

type deleteDiagnosticHook struct {
	mu       sync.Mutex
	counts   map[deleteDiagnosticClass]int
	failures []deleteDiagnosticFailure
}

func newDeleteDiagnosticHook() *deleteDiagnosticHook {
	return &deleteDiagnosticHook{counts: make(map[deleteDiagnosticClass]int)}
}

func (h *deleteDiagnosticHook) BeforeQuery(ctx context.Context, event *bun.QueryEvent) context.Context {
	class := classifyDeleteDiagnosticQuery(event.Operation(), event.Query)
	if event.Stash == nil {
		event.Stash = make(map[any]any)
	}
	event.Stash[h] = class
	h.mu.Lock()
	h.counts[class]++
	h.mu.Unlock()
	return ctx
}

func (h *deleteDiagnosticHook) AfterQuery(_ context.Context, event *bun.QueryEvent) {
	if event.Err == nil || errors.Is(event.Err, sql.ErrNoRows) {
		return
	}
	class, ok := event.Stash[h].(deleteDiagnosticClass)
	if !ok {
		class = deleteDiagnosticClass{operation: "UNKNOWN", table: "other"}
	}
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

func classifyDeleteDiagnosticQuery(operation, query string) deleteDiagnosticClass {
	q := strings.ToLower(query)
	op := strings.ToUpper(strings.TrimSpace(operation))
	// Bun RawQuery.Operation reports SELECT for raw statements, so prefer the
	// literal leading SQL verb when it is one of the expected statement forms.
	fields := strings.Fields(strings.TrimLeft(q, " \t\r\n"))
	if len(fields) > 0 {
		switch strings.ToUpper(fields[0]) {
		case "SELECT", "UPDATE", "DELETE", "INSERT", "REPLACE":
			op = strings.ToUpper(fields[0])
		}
	}
	table := deleteDiagnosticTopLevelTable(op, q)
	if op == "SELECT" && table == "monitors" && strings.Contains(q, " for update") {
		return deleteDiagnosticClass{operation: "LEASE_CLAIM", table: table}
	}
	if strings.Contains(q, "monitor_probe_assignment_sets") && strings.Contains(q, "not exists") {
		return deleteDiagnosticClass{operation: "SOURCE_GRAPH_READ", table: "monitor_probe_assignment_sets"}
	}
	if op == "DELETE" && table == "monitors" {
		return deleteDiagnosticClass{operation: "MONITOR_DELETE", table: table}
	}
	if op == "UPDATE" && table == "probes" {
		return deleteDiagnosticClass{operation: "REGISTRATION_LOCK", table: table}
	}
	if op == "UPDATE" && table == "monitors" {
		setClause := q
		if setAt := strings.Index(q, " set "); setAt >= 0 {
			setClause = q[setAt+len(" set "):]
			if whereAt := strings.Index(setClause, " where "); whereAt >= 0 {
				setClause = setClause[:whereAt]
			}
		}
		switch {
		case strings.Contains(setClause, "worker_id = null"):
			return deleteDiagnosticClass{operation: "LEASE_RELEASE", table: table}
		case strings.Contains(setClause, "worker_id ="):
			return deleteDiagnosticClass{operation: "LEASE_CLAIM", table: table}
		case strings.Contains(setClause, "leased_at ="):
			return deleteDiagnosticClass{operation: "LEASE_REFRESH", table: table}
		}
	}
	if table != "other" && op != "" {
		return deleteDiagnosticClass{operation: op, table: table}
	}
	return deleteDiagnosticClass{operation: "OTHER", table: table}
}

func deleteDiagnosticTopLevelTable(operation, query string) string {
	var table string
	switch operation {
	case "UPDATE", "REPLACE":
		fields := strings.Fields(query)
		if len(fields) >= 2 {
			table = fields[1]
		}
	case "DELETE":
		fields := strings.Fields(query)
		if len(fields) >= 3 && strings.EqualFold(fields[1], "from") {
			table = fields[2]
		}
	case "INSERT":
		fields := strings.Fields(query)
		for i := 1; i+1 < len(fields); i++ {
			if strings.EqualFold(fields[i], "into") {
				table = fields[i+1]
				break
			}
		}
	case "SELECT":
		from := strings.Index(query, " from ")
		if from >= 0 {
			fields := strings.Fields(query[from+len(" from "):])
			if len(fields) > 0 {
				table = fields[0]
			}
		}
	}
	table = strings.Trim(table, "`\";,()")
	switch table {
	case "monitor_probe_assignment_history", "monitor_probe_assignment_sets",
		"monitor_probe_assignments", "probe_observations", "monitor_probe_state",
		"probe_incidents", "probe_delivery_events", "probes", "monitors":
		return table
	default:
		return "other"
	}
}

func TestClassifyDeleteDiagnosticQueryUsesRawVerbAndTopLevelTable(t *testing.T) {
	tests := []struct {
		name, operation, query string
		want                   deleteDiagnosticClass
	}{
		{"raw refresh", "SELECT", "UPDATE monitors SET leased_at = ? WHERE worker_id = ?", deleteDiagnosticClass{operation: "LEASE_REFRESH", table: "monitors"}},
		{"raw release", "SELECT", "UPDATE monitors SET worker_id = NULL, leased_at = NULL WHERE worker_id = ?", deleteDiagnosticClass{operation: "LEASE_RELEASE", table: "monitors"}},
		{"claim with assignment predicate", "SELECT", "SELECT * FROM monitors WHERE active = TRUE AND (worker_id IS NULL OR leased_at < ? OR worker_id = ?) AND (NOT EXISTS (SELECT 1 FROM monitor_probe_assignment_sets s WHERE s.monitor_id = monitors.id) OR EXISTS (SELECT 1 FROM monitor_probe_assignments a WHERE a.monitor_id = monitors.id)) ORDER BY id LIMIT ? FOR UPDATE", deleteDiagnosticClass{operation: "LEASE_CLAIM", table: "monitors"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyDeleteDiagnosticQuery(tt.operation, tt.query); got != tt.want {
				t.Fatalf("classification = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func (h *deleteDiagnosticHook) report(t *testing.T, subcase, version string) {
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
	for _, failure := range failures {
		errorParts = append(errorParts, fmt.Sprintf("%s/%s errno=%d state=%s",
			failure.class.operation, failure.class.table, failure.errno, failure.state))
	}
	reproduced := false
	for _, failure := range failures {
		if failure.errno == 1020 && failure.state == "HY000" {
			reproduced = true
		}
	}
	t.Logf("delete diagnostic subcase=%s mariadb_version=%q query_classes=[%s] correlated_errors=[%s] 1020_reproduced=%t",
		subcase, version, strings.Join(parts, ","), strings.Join(errorParts, ";"), reproduced)
}

// TestMonitorDeleteConcurrencyDiagnostic records bounded, statement-class-only
// MariaDB evidence for the RC3 synthetic monitor delete failure.
func TestMonitorDeleteConcurrencyDiagnostic(t *testing.T) {
	dsn := os.Getenv("TEST_MARIADB_DSN")
	if dsn == "" {
		if os.Getenv(requireDeleteDiagnosticEnv) == "1" {
			t.Fatal("required MariaDB delete diagnostic cannot skip without TEST_MARIADB_DSN")
		}
		t.Skip("TEST_MARIADB_DSN is unset; skipping MariaDB delete diagnostic")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()

	t.Run("AppliedSourceReaderVersusDelete", func(t *testing.T) {
		f := newProbeRegistryFixture(t, "mariadb")
		f.db.SetMaxOpenConns(8)
		hook := newDeleteDiagnosticHook()
		f.db.AddQueryHook(hook)
		var version string
		defer func() { hook.report(t, "applied-source-reader-versus-delete", version) }()
		if err := f.db.NewRaw("SELECT VERSION()").Scan(ctx, &version); err != nil {
			t.Fatal("read MariaDB version")
		}
		seedInstallation(t, f)
		id := localMonitor(t, f)
		sentinel := localMonitor(t, f)
		ownerID := f.user(t)
		if _, err := f.db.NewUpdate().Table("monitors").Set("user_id = ?", ownerID).Where("id IN (?)", bun.List([]int64{id, sentinel})).Exec(ctx); err != nil {
			t.Fatal("assign valid synthetic monitor owners")
		}
		builder, _ := sourceConfigBuilder(t, f, f.db)
		at := time.Now().UTC().Truncate(time.Microsecond)
		meta, err := builder.Prepare(ctx, "11111111-2222-4333-8444-555555555555", 0, at, at)
		if err != nil {
			t.Fatal("prepare source snapshot")
		}
		reader := repository.NewProbeActivationStore(f.db, probe.LocalConfigEncoder{})
		if _, err := reader.ActivateLocal(ctx, ports.LocalActivationParams{
			Target: meta.ProbeConfigTarget, Revision: meta.Revision, SHA256: meta.SHA256,
			AppliedAt: at, AssignmentCount: -1,
		}); err != nil {
			t.Fatal("activate source snapshot")
		}

		peer := reopenConfigDB(t, f)
		peer.SetMaxOpenConns(1)
		observer := reopenConfigDB(t, f)
		peerHook := newDeleteDiagnosticHook()
		peer.AddQueryHook(peerHook)
		defer func() { peerHook.report(t, "applied-source-reader-versus-delete-writer", version) }()
		var writerConnection int64
		if err := peer.NewRaw("SELECT CONNECTION_ID()").Scan(ctx, &writerConnection); err != nil {
			t.Fatal("read diagnostic connection id")
		}
		graph := make(chan struct{})
		release := make(chan struct{})
		var graphOnce, releaseOnce sync.Once
		unblock := func() { releaseOnce.Do(func() { close(release) }) }
		defer unblock()
		f.db.AddQueryHook(monitorMutationQueryHook{before: func(ctx context.Context, query string) {
			if strings.Contains(query, "NOT EXISTS (SELECT 1 FROM monitor_probe_assignment_sets") {
				graphOnce.Do(func() {
					close(graph)
					select {
					case <-release:
					case <-ctx.Done():
					}
				})
			}
		}})
		readDone := make(chan error, 1)
		go func() { _, readErr := reader.ReadAppliedLocal(ctx); readDone <- readErr }()
		select {
		case <-graph:
		case <-ctx.Done():
			t.Fatal("applied-source reader did not reach assignment graph")
		}
		registrationAttempted := make(chan struct{}, 1)
		monitorAttempted := make(chan struct{}, 1)
		peer.AddQueryHook(monitorMutationQueryHook{before: func(_ context.Context, query string) {
			switch {
			case strings.HasPrefix(query, "UPDATE `probes`"):
				select {
				case registrationAttempted <- struct{}{}:
				default:
				}
			case strings.HasPrefix(query, "DELETE FROM `monitors`"):
				select {
				case monitorAttempted <- struct{}{}:
				default:
				}
			}
		}})
		deleteDone := make(chan error, 1)
		go func() { deleteDone <- mariadb.NewMonitorRepo(peer).Delete(ctx, id) }()
		select {
		case <-registrationAttempted:
		case <-ctx.Done():
			t.Fatal("delete did not attempt its registration-first lock")
		}
		var waiting int
		poll := time.NewTicker(10 * time.Millisecond)
		defer poll.Stop()
		for waiting == 0 {
			if err := observer.NewRaw(`SELECT COUNT(*) FROM information_schema.PROCESSLIST
				WHERE ID = ? AND COMMAND = 'Query' AND INFO LIKE 'UPDATE %probes%'`, writerConnection).Scan(ctx, &waiting); err != nil {
				t.Fatal("observe registration-lock wait")
			}
			if waiting != 0 {
				break
			}
			select {
			case <-poll.C:
			case <-ctx.Done():
				t.Fatal("delete did not wait on the source registration lock")
			}
		}
		select {
		case <-monitorAttempted:
			t.Fatal("delete reached monitor SQL while the applied-source graph lock was held")
		default:
		}
		unblock()
		if err := <-readDone; err != nil {
			t.Fatalf("applied-source read failed: %v", err)
		}
		deleteErr := <-deleteDone
		if deleteErr != nil {
			monitorCount, monitorErr := f.db.NewSelect().Table("monitors").Where("id = ?", id).Count(ctx)
			setCount, setErr := f.db.NewSelect().Table("monitor_probe_assignment_sets").Where("monitor_id = ?", id).Count(ctx)
			assignmentCount, assignmentErr := f.db.NewSelect().Table("monitor_probe_assignments").Where("monitor_id = ?", id).Count(ctx)
			if monitorErr != nil || setErr != nil || assignmentErr != nil || monitorCount != 1 || setCount == 0 || assignmentCount == 0 {
				t.Fatalf("failed delete did not roll back parent/cascade: monitor=%d monitor_err=%v sets=%d set_err=%v assignments=%d assignment_err=%v",
					monitorCount, monitorErr, setCount, setErr, assignmentCount, assignmentErr)
			}
			t.Fatalf("production monitor delete failed after rollback was verified: %v", deleteErr)
		}
		for _, table := range []struct{ name, column string }{
			{"monitors", "id"}, {"monitor_probe_assignment_sets", "monitor_id"},
			{"monitor_probe_assignments", "monitor_id"}, {"monitor_probe_assignment_history", "monitor_id"},
		} {
			count, err := f.db.NewSelect().Table(table.name).Where(table.column+" = ?", id).Count(ctx)
			if err != nil || count != 0 {
				t.Fatalf("delete cascade %s: rows=%d err=%v", table.name, count, err)
			}
		}
		if _, err := mariadb.NewMonitorRepo(f.db).GetByID(ctx, sentinel); err != nil {
			t.Fatalf("unrelated monitor was not preserved: %v", err)
		}
		localCount, err := f.db.NewSelect().Table("probes").Where("id = ?", domain.LocalProbeID).Count(ctx)
		if err != nil || localCount != 1 {
			t.Fatalf("local registration changed: count=%d err=%v", localCount, err)
		}
		if _, err := reader.ReadAppliedLocal(ctx); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("deleted source remained executable: %v", err)
		}
	})

	t.Run("DeleteVersusWorkerLeaseLifecycle", func(t *testing.T) {
		f := newProbeRegistryFixture(t, "mariadb")
		f.db.SetMaxOpenConns(8)
		hook := newDeleteDiagnosticHook()
		f.db.AddQueryHook(hook)
		var version string
		defer func() { hook.report(t, "delete-versus-worker-lease-lifecycle", version) }()
		if err := f.db.NewRaw("SELECT VERSION()").Scan(ctx, &version); err != nil {
			t.Fatal("read MariaDB version")
		}
		target := localMonitor(t, f)
		sentinel := localMonitor(t, f)
		ownerID := f.user(t)
		if _, err := f.db.NewUpdate().Table("monitors").Set("user_id = ?", ownerID).Where("id IN (?)", bun.List([]int64{target, sentinel})).Exec(ctx); err != nil {
			t.Fatal("assign valid synthetic monitor owners")
		}
		repo := mariadb.NewMonitorRepo(f.db)
		claimed, err := repo.ClaimBatch(ctx, "delete-diagnostic-worker", 1, time.Minute)
		if err != nil || len(claimed) != 1 || claimed[0].ID != target {
			t.Fatalf("claim target monitor lease: claimed=%d err=%v", len(claimed), err)
		}
		otherClaim, err := repo.ClaimBatch(ctx, "unrelated-diagnostic-worker", 1, time.Minute)
		if err != nil || len(otherClaim) != 1 || otherClaim[0].ID != sentinel {
			t.Fatalf("claim sentinel monitor lease: claimed=%d err=%v", len(otherClaim), err)
		}
		peer := reopenConfigDB(t, f)
		peerHook := newDeleteDiagnosticHook()
		peer.AddQueryHook(peerHook)
		defer func() { peerHook.report(t, "delete-versus-worker-lease-lifecycle-peer", version) }()
		deleteRepo := mariadb.NewMonitorRepo(peer)
		start := make(chan struct{})
		type operationResult struct {
			name string
			err  error
		}
		results := make(chan operationResult, 3)
		go func() { <-start; results <- operationResult{name: "delete", err: deleteRepo.Delete(ctx, target)} }()
		go func() {
			<-start
			_, refreshErr := repo.RefreshLease(ctx, "delete-diagnostic-worker", time.Minute)
			results <- operationResult{name: "refresh", err: refreshErr}
		}()
		go func() {
			<-start
			_, releaseErr := repo.ReleaseLeases(ctx, "delete-diagnostic-worker")
			results <- operationResult{name: "release", err: releaseErr}
		}()
		close(start)
		var deleteErr error
		var operationErrors []string
		for i := 0; i < 3; i++ {
			select {
			case result := <-results:
				if result.name == "delete" {
					deleteErr = result.err
				}
				if result.err != nil {
					operationErrors = append(operationErrors, result.name)
				}
			case <-ctx.Done():
				t.Fatal("concurrent delete/lease operation exceeded diagnostic bound")
			}
		}
		count, err := f.db.NewSelect().Table("monitors").Where("id = ?", target).Count(ctx)
		if err != nil || (deleteErr == nil && count != 0) || (deleteErr != nil && count != 1) {
			t.Fatalf("target delete/rollback state: delete_err=%v count=%d query_err=%v", deleteErr, count, err)
		}
		if deleteErr != nil {
			assignmentSets, setErr := f.db.NewSelect().Table("monitor_probe_assignment_sets").Where("monitor_id = ?", target).Count(ctx)
			assignments, assignmentErr := f.db.NewSelect().Table("monitor_probe_assignments").Where("monitor_id = ?", target).Count(ctx)
			if setErr != nil || assignmentErr != nil || assignmentSets == 0 || assignments == 0 {
				t.Fatalf("failed delete did not roll back target cascade: sets=%d set_err=%v assignments=%d assignment_err=%v", assignmentSets, setErr, assignments, assignmentErr)
			}
		} else {
			for _, table := range []struct{ name, column string }{
				{"monitor_probe_assignment_sets", "monitor_id"},
				{"monitor_probe_assignments", "monitor_id"},
				{"monitor_probe_assignment_history", "monitor_id"},
			} {
				rows, queryErr := f.db.NewSelect().Table(table.name).Where(table.column+" = ?", target).Count(ctx)
				if queryErr != nil || rows != 0 {
					t.Fatalf("successful target delete left %s rows=%d err=%v", table.name, rows, queryErr)
				}
			}
		}
		var sentinelWorker *string
		if err := f.db.NewSelect().Table("monitors").Column("worker_id").Where("id = ?", sentinel).Scan(ctx, &sentinelWorker); err != nil {
			t.Fatalf("read unrelated sentinel lease: %v", err)
		}
		if sentinelWorker == nil || *sentinelWorker != "unrelated-diagnostic-worker" {
			t.Fatalf("unrelated worker lease changed: present=%t", sentinelWorker != nil)
		}
		if len(operationErrors) != 0 {
			t.Fatalf("concurrent delete/lease operations failed: %s", strings.Join(operationErrors, ","))
		}
	})
}
