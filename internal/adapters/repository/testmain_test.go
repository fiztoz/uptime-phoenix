package repository_test

import (
	"fmt"
	"os"
	"testing"
)

// TestMain fails a misspelled MariaDB matrix configuration instead of allowing
// an apparently green package whose real-engine contracts were all skipped.
func TestMain(m *testing.M) {
	if os.Getenv("MARIADB_TEST_DSN") != "" {
		_, _ = fmt.Fprintln(os.Stderr, "MARIADB_TEST_DSN is unsupported; set TEST_MARIADB_DSN to run the MariaDB contracts")
		os.Exit(2)
	}
	os.Exit(m.Run())
}
