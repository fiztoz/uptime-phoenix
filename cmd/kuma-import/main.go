// Command kuma-import converts an Uptime Kuma snapshot into a Phoenix
// BackupDocument JSON file for admin import.
//
// SQLite (default, Kuma v1 / v2 file snapshots):
//
//	go run ./cmd/kuma-import --input /path/to/kuma.db --output /secure/path/phoenix-backup.json
//
// MariaDB / MySQL (Kuma v2 external DB — same schema as SQLite):
//
//	go run ./cmd/kuma-import \
//	  --engine mariadb \
//	  --dsn 'kuma:pass@tcp(127.0.0.1:3306)/kuma?parseTime=true' \
//	  --output /secure/path/phoenix-backup.json
//
// The source database is opened READ-ONLY. The tool never writes either database
// and never calls a live Phoenix API. The resulting JSON contains secrets
// (notification tokens, proxy passwords); treat it as a credential bundle.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/importer/uptimekuma"
)

func main() {
	var (
		engine     string
		input      string
		dsn        string
		output     string
		reportPath string
		force      bool
		strict     bool
	)
	flag.StringVar(&engine, "engine", "", "source engine: sqlite (default) or mariadb (mysql is an alias)")
	flag.StringVar(&input, "input", "", "path to Uptime Kuma SQLite database (required for engine=sqlite, opened read-only)")
	flag.StringVar(&dsn, "dsn", "", "MariaDB/MySQL DSN for engine=mariadb (e.g. user:pass@tcp(host:3306)/kuma?parseTime=true)")
	flag.StringVar(&output, "output", "", "path for Phoenix backup JSON (required, mode 0600)")
	flag.StringVar(&reportPath, "report", "", "optional path for a safe summary JSON (counts/reasons, no secrets)")
	flag.BoolVar(&force, "force", false, "overwrite output if it already exists")
	flag.BoolVar(&strict, "strict", false, "exit nonzero if any supported-looking entity is skipped")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  kuma-import --input <kuma.db> --output <phoenix-backup.json> [options]\n")
		fmt.Fprintf(os.Stderr, "  kuma-import --engine mariadb --dsn 'user:pass@tcp(host:3306)/kuma' --output <phoenix-backup.json>\n\n")
		fmt.Fprintf(os.Stderr, "Converts a stopped Uptime Kuma snapshot (SQLite file or MariaDB) into a\n")
		fmt.Fprintf(os.Stderr, "Phoenix BackupDocument JSON file. Import the JSON via the Phoenix admin backup API.\n\n")
		fmt.Fprintf(os.Stderr, "WARNING: The output contains secrets (notification tokens, proxy passwords).\n")
		fmt.Fprintf(os.Stderr, "Store it with restricted permissions and delete it after import.\n")
		fmt.Fprintf(os.Stderr, "Never put database passwords on the command line in shared shells; prefer env vars.\n\n")
		flag.PrintDefaults()
	}
	flag.Parse()

	if output == "" {
		flag.Usage()
		os.Exit(2)
	}
	// Infer engine when omitted: DSN-only → mariadb; otherwise sqlite.
	if engine == "" {
		if dsn != "" && input == "" {
			engine = uptimekuma.EngineMariaDB
		} else {
			engine = uptimekuma.EngineSQLite
		}
	}
	if engine == uptimekuma.EngineSQLite || engine == "sqlite3" {
		if input == "" {
			flag.Usage()
			os.Exit(2)
		}
	} else {
		if dsn == "" {
			flag.Usage()
			os.Exit(2)
		}
	}

	result, err := uptimekuma.Convert(uptimekuma.Options{
		Engine:     engine,
		Input:      input,
		DSN:        dsn,
		Output:     output,
		ReportPath: reportPath,
		Force:      force,
		Strict:     strict,
	})
	if err != nil {
		// Strict mode still wrote the files; report counts if available.
		if result != nil && result.Report != nil {
			fmt.Fprintf(os.Stderr, "kuma-import: %v\n", err)
			fmt.Fprintf(os.Stderr, "converted: engine=%s monitors=%d groups=%d notifications=%d tags=%d status_pages=%d skipped=%d\n",
				result.Report.SourceEngine,
				result.Report.Monitors, result.Report.MonitorGroups, result.Report.Notifications,
				result.Report.Tags, result.Report.StatusPages, result.Report.SkipCount)
		} else {
			fmt.Fprintf(os.Stderr, "kuma-import: %v\n", err)
		}
		os.Exit(1)
	}

	_, _ = fmt.Fprintf(os.Stdout, "wrote %s (engine=%s monitors=%d groups=%d notifications=%d tags=%d status_pages=%d skipped=%d)\n",
		output,
		result.Report.SourceEngine,
		result.Report.Monitors, result.Report.MonitorGroups, result.Report.Notifications,
		result.Report.Tags, result.Report.StatusPages, result.Report.SkipCount,
	)
	if reportPath != "" {
		_, _ = fmt.Fprintf(os.Stdout, "report %s\n", reportPath)
	}
}
