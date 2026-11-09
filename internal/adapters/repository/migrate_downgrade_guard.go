package repository

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrSchemaNewerThanBinary means the database already holds a migration from a
// newer release. Running an older binary against it would read and write a
// schema it does not understand, so startup refuses.
var ErrSchemaNewerThanBinary = errors.New("database schema is newer than this binary")

// checkSchemaNotNewer refuses when any applied migration has a higher number
// than the newest migration embedded in this binary. Unknown names at or
// below that number are not a downgrade and are left alone.
func checkSchemaNotNewer(applied, embedded []string) error {
	latest, latestNum := "", -1
	for _, name := range embedded {
		if n, ok := migrationNumber(name); ok && n > latestNum {
			latest, latestNum = name, n
		}
	}
	if latestNum < 0 {
		return nil
	}
	newest, newestNum := "", latestNum
	for _, name := range applied {
		if n, ok := migrationNumber(name); ok && n > newestNum {
			newest, newestNum = name, n
		}
	}
	if newest == "" {
		return nil
	}
	return fmt.Errorf("%w: the database has migration %s applied, but this binary only knows migrations up to %s. Refusing to start. Run the release that applied it, or a newer one; to go back to this release, restore the pre-upgrade database backup (RUNBOOK section 3)",
		ErrSchemaNewerThanBinary, newest, latest)
}

// migrationNumber parses the leading NNN of "NNN_name.up.sql".
func migrationNumber(name string) (int, bool) {
	prefix, _, found := strings.Cut(name, "_")
	if !found || prefix == "" {
		return 0, false
	}
	n, err := strconv.Atoi(prefix)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}
