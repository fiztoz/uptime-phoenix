package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type mariaDBMigrationCatalog interface {
	migrationExecutor
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type migrationColumn struct {
	name, typ, nullable, defaultValue, after, charset, collation string
}

// applyMariaDB037 resumes only known, structurally valid prefixes of migration 037.
func applyMariaDB037(ctx context.Context, raw migrationExecutor, _ string) error {
	catalog, ok := raw.(mariaDBMigrationCatalog)
	if !ok {
		return fmt.Errorf("migration 037 requires a queryable MariaDB connection")
	}
	columns := []migrationColumn{
		{"probe_id", "varchar(36)", "NO", "local", "", "ascii", "ascii_bin"},
		{"stream_id", "varchar(36)", "YES", "", "", "ascii", "ascii_bin"},
		{"source_seq", "bigint(20)", "YES", "", "", "", ""},
		{"assignment_generation", "bigint(20)", "NO", "1", "", "", ""},
		{"received_at", "datetime(6)", "YES", "", "", "", ""},
		{"config_revision", "bigint(20)", "NO", "0", "", "", ""},
	}
	if err := ensureColumns(ctx, catalog, "heartbeats", columns, `ALTER TABLE heartbeats
    ADD COLUMN probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'local',
    ADD COLUMN stream_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NULL,
    ADD COLUMN source_seq BIGINT NULL,
    ADD COLUMN assignment_generation BIGINT NOT NULL DEFAULT 1,
    ADD COLUMN received_at DATETIME(6) NULL,
    ADD COLUMN config_revision BIGINT NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := ensureIndex(ctx, catalog, "heartbeats", "idx_hb_monitor_probe_time", []string{"monitor_id", "probe_id", "time", "id"}, false, []string{"A", "A", "D", "D"}, "CREATE INDEX idx_hb_monitor_probe_time ON heartbeats (monitor_id, probe_id, time DESC, id DESC)"); err != nil {
		return err
	}
	for _, table := range []struct {
		name, prefix string
	}{{"heartbeat_1m", "1m"}, {"heartbeat_1h", "1h"}, {"heartbeat_1d", "1d"}} {
		if err := ensureColumns(ctx, catalog, table.name, []migrationColumn{
			{"probe_id", "varchar(36)", "NO", "local", "monitor_id", "ascii", "ascii_bin"},
			{"unknown_count", "int(11)", "NO", "0", "maint_count", "", ""},
		}, "ALTER TABLE "+table.name+" ADD COLUMN probe_id VARCHAR(36) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'local' AFTER monitor_id, ADD COLUMN unknown_count INT NOT NULL DEFAULT 0 AFTER maint_count"); err != nil {
			return err
		}
		if err := ensureIndex(ctx, catalog, table.name, "uq_monitor_probe_bucket", []string{"monitor_id", "probe_id", "bucket"}, true, []string{"A", "A", "A"}, "ALTER TABLE "+table.name+" ADD UNIQUE KEY uq_monitor_probe_bucket (monitor_id, probe_id, bucket)"); err != nil {
			return err
		}
		if err := dropLegacyUnique(ctx, catalog, table.name); err != nil {
			return err
		}
		if err := ensureIndex(ctx, catalog, table.name, "idx_"+table.prefix+"_monitor_probe_bucket", []string{"monitor_id", "probe_id", "bucket"}, false, []string{"A", "A", "D"}, "ALTER TABLE "+table.name+" ADD INDEX idx_"+table.prefix+"_monitor_probe_bucket (monitor_id, probe_id, bucket DESC)"); err != nil {
			return err
		}
	}
	return nil
}

func applyMariaDB037AndRecord(ctx context.Context, exec migrationExecutor, filename, source string) error {
	if err := applyMariaDB037(ctx, exec, source); err != nil {
		return err
	}
	if err := recordMigration(ctx, exec, filename); err != nil {
		return fmt.Errorf("record migration %s: %w", filename, err)
	}
	return nil
}

func ensureColumns(ctx context.Context, db mariaDBMigrationCatalog, table string, expected []migrationColumn, alterSQL string) error {
	existing := make(map[string]migrationColumn)
	rows, err := db.QueryContext(ctx, `SELECT COLUMN_NAME, LOWER(COLUMN_TYPE), IS_NULLABLE, COLUMN_DEFAULT, COALESCE(ORDINAL_POSITION, 0), COALESCE(CHARACTER_SET_NAME, ''), COALESCE(COLLATION_NAME, '')
FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?`, table)
	if err != nil {
		return fmt.Errorf("inspect migration 037 columns on %s: %w", table, err)
	}
	positions := make(map[string]int)
	for rows.Next() {
		var c migrationColumn
		var defaultValue sql.NullString
		var ordinal int
		if err := rows.Scan(&c.name, &c.typ, &c.nullable, &defaultValue, &ordinal, &c.charset, &c.collation); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan migration 037 columns on %s: %w", table, err)
		}
		if defaultValue.Valid {
			c.defaultValue = defaultValue.String
		}
		existing[c.name] = c
		positions[c.name] = ordinal
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read migration 037 columns on %s: %w", table, err)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	missing := 0
	for _, want := range expected {
		got, ok := existing[want.name]
		if !ok {
			missing++
			continue
		}
		gotDefault := strings.Trim(got.defaultValue, "'")
		badDefault := (want.defaultValue != "" && got.defaultValue != want.defaultValue && gotDefault != want.defaultValue) || (want.defaultValue == "" && want.nullable == "YES" && got.defaultValue != "" && !strings.EqualFold(got.defaultValue, "NULL"))
		if got.typ != want.typ || got.nullable != want.nullable || badDefault || (want.charset != "" && (got.charset != want.charset || got.collation != want.collation)) {
			return fmt.Errorf("migration 037 refuses unexpected %s.%s shape (type=%s nullable=%s default=%q)", table, want.name, got.typ, got.nullable, got.defaultValue)
		}
		if want.after != "" {
			_, ok := existing[want.after]
			if !ok {
				return fmt.Errorf("migration 037 cannot validate %s.%s position without %s", table, want.name, want.after)
			}
			if positions[want.name] != positions[want.after]+1 {
				return fmt.Errorf("migration 037 refuses unexpected %s.%s position", table, want.name)
			}
		}
	}
	if missing == 0 {
		return nil
	}
	if missing != len(expected) {
		return fmt.Errorf("migration 037 found a partial column group on %s; refusing ambiguous recovery", table)
	}
	if _, err := db.ExecContext(ctx, alterSQL); err != nil {
		return fmt.Errorf("add migration 037 columns on %s: %w", table, err)
	}
	return nil
}

func ensureIndex(ctx context.Context, db mariaDBMigrationCatalog, table, index string, columns []string, unique bool, directions []string, createSQL string) error {
	rows, err := db.QueryContext(ctx, `SELECT COLUMN_NAME, NON_UNIQUE, COALESCE(COLLATION, 'A'), SUB_PART, INDEX_TYPE FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = ? ORDER BY SEQ_IN_INDEX`, table, index)
	if err != nil {
		return fmt.Errorf("inspect migration 037 index %s.%s: %w", table, index, err)
	}
	var gotColumns, gotDirections []string
	var badShape bool
	var nonUnique int
	for rows.Next() {
		var column, direction, indexType string
		var subPart sql.NullInt64
		if err := rows.Scan(&column, &nonUnique, &direction, &subPart, &indexType); err != nil {
			_ = rows.Close()
			return err
		}
		gotColumns, gotDirections = append(gotColumns, strings.ToLower(column)), append(gotDirections, direction)
		badShape = badShape || subPart.Valid || !strings.EqualFold(indexType, "BTREE")
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(gotColumns) > 0 {
		wantUnique := 0
		if !unique {
			wantUnique = 1
		}
		if !equalStrings(gotColumns, columns) || !equalStrings(gotDirections, directions) || nonUnique != wantUnique || badShape {
			return fmt.Errorf("migration 037 refuses unexpected index %s.%s definition: columns=%v directions=%v non_unique=%d", table, index, gotColumns, gotDirections, nonUnique)
		}
		return nil
	}
	if _, err := db.ExecContext(ctx, createSQL); err != nil {
		return fmt.Errorf("create migration 037 index %s.%s: %w", table, index, err)
	}
	return nil
}

func dropLegacyUnique(ctx context.Context, db mariaDBMigrationCatalog, table string) error {
	rows, err := db.QueryContext(ctx, `SELECT COLUMN_NAME, NON_UNIQUE, SUB_PART, INDEX_TYPE FROM information_schema.STATISTICS
WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND INDEX_NAME = 'uq_monitor_bucket' ORDER BY SEQ_IN_INDEX`, table)
	if err != nil {
		return fmt.Errorf("inspect legacy unique index on %s: %w", table, err)
	}
	var columns []string
	var nonUnique int
	var badShape bool
	for rows.Next() {
		var column, indexType string
		var subPart sql.NullInt64
		if err := rows.Scan(&column, &nonUnique, &subPart, &indexType); err != nil {
			_ = rows.Close()
			return err
		}
		columns = append(columns, strings.ToLower(column))
		badShape = badShape || subPart.Valid || !strings.EqualFold(indexType, "BTREE")
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(columns) == 0 {
		return nil
	}
	if !equalStrings(columns, []string{"monitor_id", "bucket"}) || nonUnique != 0 || badShape {
		return fmt.Errorf("migration 037 refuses unexpected legacy index on %s: columns=%v non_unique=%d", table, columns, nonUnique)
	}
	if _, err := db.ExecContext(ctx, "ALTER TABLE "+table+" DROP INDEX uq_monitor_bucket"); err != nil {
		return fmt.Errorf("drop legacy unique index on %s: %w", table, err)
	}
	return nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
