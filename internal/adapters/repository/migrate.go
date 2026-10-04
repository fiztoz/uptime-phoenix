package repository

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"sort"
	"strings"
	"time"
)

//go:embed mariadb/migrations/*.sql
var mariadbMigrations embed.FS

//go:embed sqlite/migrations/*.sql
var sqliteMigrations embed.FS

// RunMigrations executes all pending migrations for the specified engine.
func RunMigrations(db *sql.DB, engine string) error {
	ctx, cancel := context.WithTimeout(context.Background(), migrationStartupTimeout)
	defer cancel()
	return runMigrations(ctx, db, engine)
}

func runMigrations(ctx context.Context, db *sql.DB, engine string) error {
	var exec migrationExecutor = db
	if engine == "mariadb" {
		conn, err := db.Conn(ctx)
		if err != nil {
			return fmt.Errorf("acquire MariaDB migration connection: %w", err)
		}
		return runMariaDBMigrations(ctx, db, conn)
	}
	return runMigrationsWithExecutor(ctx, db, exec, engine)
}

func runMigrationsWithExecutor(ctx context.Context, db *sql.DB, exec migrationExecutor, engine string) error {
	// Create migrations tracking table if not exists.
	// Use engine-specific syntax for the auto-increment column.
	var createTableSQL string
	switch engine {
	case "mariadb":
		createTableSQL = `
			CREATE TABLE IF NOT EXISTS _migrations (
				id INT AUTO_INCREMENT PRIMARY KEY,
				filename VARCHAR(255) NOT NULL UNIQUE,
				applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
			)
		`
	case "sqlite":
		createTableSQL = `
			CREATE TABLE IF NOT EXISTS _migrations (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				filename TEXT NOT NULL UNIQUE,
				applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
			)
		`
	default:
		return fmt.Errorf("unknown engine: %s", engine)
	}
	if _, err := exec.ExecContext(ctx, createTableSQL); err != nil {
		return fmt.Errorf("create _migrations table: %w", err)
	}

	// Get list of already applied migrations.
	var applied []string
	rows, err := queryMigrationRows(ctx, db, exec)
	if err != nil {
		return fmt.Errorf("query applied migrations: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var fname string
		if scanErr := rows.Scan(&fname); scanErr != nil {
			return fmt.Errorf("scan filename: %w", scanErr)
		}
		applied = append(applied, fname)
	}
	if scanErr := rows.Err(); scanErr != nil {
		return fmt.Errorf("iterate applied migrations: %w", scanErr)
	}
	if closeErr := rows.Close(); closeErr != nil {
		return fmt.Errorf("close applied migrations rows: %w", closeErr)
	}

	// Determine which embedded migrations to use.
	var migrationsFS embed.FS
	var migrationsDir string
	switch engine {
	case "mariadb":
		migrationsFS = mariadbMigrations
		migrationsDir = "mariadb/migrations"
	case "sqlite":
		migrationsFS = sqliteMigrations
		migrationsDir = "sqlite/migrations"
	default:
		return fmt.Errorf("unknown engine: %s", engine)
	}

	// Read all migration files.
	entries, err := fs.ReadDir(migrationsFS, migrationsDir)
	if err != nil {
		return fmt.Errorf("read migrations directory: %w", err)
	}

	// Filter to .up.sql files and sort.
	var migrations []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".up.sql") {
			migrations = append(migrations, entry.Name())
		}
	}
	sort.Strings(migrations)

	// Apply pending migrations.
	for _, filename := range migrations {
		// Skip if already applied.
		if contains(applied, filename) {
			continue
		}

		// Read migration SQL.
		sqlPath := path.Join(migrationsDir, filename)
		sqlBytes, err := migrationsFS.ReadFile(sqlPath)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", filename, err)
		}

		// SQLite table rebuilds and their tracking record must commit together.
		// In particular, rebuilding a parent must not strand its copied children.
		slog.Info("Applying database migration", "migration", filename)
		if engine == "mariadb" && filename == "037_probe_heartbeat.up.sql" {
			if err := applyMariaDB037AndRecord(ctx, exec, filename, string(sqlBytes)); err != nil {
				return err
			}
			continue
		}
		if err := applyMigrationWithExecutor(ctx, db, exec, engine, filename, string(sqlBytes)); err != nil {
			return err
		}
	}

	return nil
}

func queryMigrationRows(ctx context.Context, db *sql.DB, exec migrationExecutor) (*sql.Rows, error) {
	if queryer, ok := exec.(interface {
		QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	}); ok {
		return queryer.QueryContext(ctx, "SELECT filename FROM _migrations ORDER BY filename")
	}
	return db.QueryContext(ctx, "SELECT filename FROM _migrations ORDER BY filename")
}

type migrationExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func applyMigration(ctx context.Context, db *sql.DB, engine, filename, source string) error {
	var exec migrationExecutor = db
	return applyMigrationWithExecutor(ctx, db, exec, engine, filename, source)
}

func applyMigrationWithExecutor(ctx context.Context, db *sql.DB, executor migrationExecutor, engine, filename, source string) error {
	exec := executor
	var tx *sql.Tx
	if engine == "sqlite" {
		var err error
		tx, err = db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", filename, err)
		}
		defer func() { _ = tx.Rollback() }()
		exec = tx
	}
	for i, stmt := range splitMigrationStatements(source) {
		if _, err := exec.ExecContext(ctx, stmt); err != nil {
			if isDuplicateColumnError(err) {
				continue
			}
			return fmt.Errorf("execute migration %s (statement %d): %w", filename, i+1, err)
		}
	}
	if err := recordMigration(ctx, exec, filename); err != nil {
		return fmt.Errorf("record migration %s: %w", filename, err)
	}
	if tx != nil {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", filename, err)
		}
	}
	return nil
}

func recordMigration(ctx context.Context, exec migrationExecutor, filename string) error {
	_, err := exec.ExecContext(ctx, "INSERT INTO _migrations (filename, applied_at) VALUES (?, ?)", filename, time.Now().UTC())
	return err
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// splitMigrationStatements splits a migration file into executable SQL statements.
// Handles semicolons inside CREATE TABLE ... PARTITION (...) blocks.
func splitMigrationStatements(sql string) []string {
	var out []string
	var b strings.Builder
	depth := 0
	inSingleQuote := false
	inDoubleQuote := false
	inLineComment := false
	inBlockComment := false

	for i := 0; i < len(sql); i++ {
		ch := sql[i]
		next := byte(0)
		if i+1 < len(sql) {
			next = sql[i+1]
		}

		if inLineComment {
			b.WriteByte(ch)
			if ch == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			b.WriteByte(ch)
			if ch == '*' && next == '/' {
				b.WriteByte(next)
				i++
				inBlockComment = false
			}
			continue
		}
		if !inSingleQuote && !inDoubleQuote {
			if ch == '-' && next == '-' {
				b.WriteByte(ch)
				b.WriteByte(next)
				i++
				inLineComment = true
				continue
			}
			if ch == '/' && next == '*' {
				b.WriteByte(ch)
				b.WriteByte(next)
				i++
				inBlockComment = true
				continue
			}
		}

		if ch == '\'' && !inDoubleQuote {
			inSingleQuote = !inSingleQuote
			b.WriteByte(ch)
			continue
		}
		if ch == '"' && !inSingleQuote {
			inDoubleQuote = !inDoubleQuote
			b.WriteByte(ch)
			continue
		}

		if !inSingleQuote && !inDoubleQuote {
			switch ch {
			case '(':
				depth++
			case ')':
				if depth > 0 {
					depth--
				}
			case ';':
				if depth == 0 {
					stmt := strings.TrimSpace(b.String())
					if stmt != "" {
						out = append(out, stmt)
					}
					b.Reset()
					continue
				}
			}
		}

		b.WriteByte(ch)
	}

	stmt := strings.TrimSpace(b.String())
	if stmt != "" {
		out = append(out, stmt)
	}
	return out
}

// isDuplicateColumnError returns true if the error indicates a column
// already exists. This makes ALTER TABLE ADD COLUMN migrations idempotent.
//
// MariaDB: Error 1060 (42S21) "Duplicate column name 'X'"
// SQLite:  "duplicate column name: X"
func isDuplicateColumnError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Duplicate column name") ||
		strings.Contains(msg, "duplicate column name")
}
