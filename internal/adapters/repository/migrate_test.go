package repository

import (
	"strings"
	"testing"
)

func TestSplitMigrationStatements_partitions(t *testing.T) {
	sql := `
CREATE TABLE a (id INT);
CREATE TABLE heartbeats (
    id BIGINT,
    time TIMESTAMP,
    PRIMARY KEY (id, time)
) PARTITION BY RANGE (UNIX_TIMESTAMP(time)) (
    PARTITION p1 VALUES LESS THAN (100),
    PARTITION pmax VALUES LESS THAN MAXVALUE
);
CREATE TABLE b (id INT);
`
	stmts := splitMigrationStatements(sql)
	if len(stmts) != 3 {
		t.Fatalf("got %d statements, want 3: %v", len(stmts), stmts)
	}
	if !strings.HasPrefix(stmts[0], "CREATE TABLE a") {
		t.Errorf("stmt0: %q", stmts[0])
	}
	if !strings.Contains(stmts[1], "PARTITION pmax") {
		t.Errorf("stmt1 missing partition: %q", stmts[1])
	}
	if !strings.HasPrefix(stmts[2], "CREATE TABLE b") {
		t.Errorf("stmt2: %q", stmts[2])
	}
}
