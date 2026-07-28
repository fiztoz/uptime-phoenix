package repository

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
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
	ctx := context.Background()

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
	if _, err := db.ExecContext(ctx, createTableSQL); err != nil {
		return fmt.Errorf("create _migrations table: %w", err)
	}

	// Get list of already applied migrations.
	var applied []string
	rows, err := db.QueryContext(ctx, "SELECT filename FROM _migrations ORDER BY filename")
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

		// Execute migration (one statement at a time for clearer errors; DDL may auto-commit).
		fmt.Printf("Applying migration: %s\n", filename)
		for i, stmt := range splitMigrationStatements(string(sqlBytes)) {
			if _, err := db.ExecContext(ctx, stmt); err != nil {
				// Ignore "duplicate column" errors — this makes ALTER TABLE
				// migrations idempotent when the column already exists (e.g.
				// added in an earlier migration that was edited after the DB
				// was first created).
				if isDuplicateColumnError(err) {
					fmt.Printf("  statement %d: column already exists, skipping\n", i+1)
					continue
				}
				return fmt.Errorf("execute migration %s (statement %d): %w", filename, i+1, err)
			}
		}

		// Record migration.
		if _, err := db.ExecContext(ctx, "INSERT INTO _migrations (filename, applied_at) VALUES (?, ?)",
			filename, time.Now().UTC()); err != nil {
			return fmt.Errorf("record migration %s: %w", filename, err)
		}
	}

	return nil
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
