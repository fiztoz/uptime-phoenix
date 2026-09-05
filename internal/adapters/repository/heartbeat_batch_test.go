package repository

import (
	"strings"
	"testing"
)

func TestLatestHeartbeatsUnionSQL(t *testing.T) {
	t.Parallel()

	if got := latestHeartbeatsUnionSQL(0); got != "" {
		t.Fatalf("n=0: got %q, want empty", got)
	}

	one := latestHeartbeatsUnionSQL(1)
	if strings.Contains(one, "UNION ALL") {
		t.Fatalf("n=1 should be a single SELECT, got %q", one)
	}
	if strings.HasPrefix(strings.TrimSpace(one), "(") {
		t.Fatalf("n=1 must not wrap the UNION arm in parens (SQLite syntax error): %q", one)
	}
	if !strings.Contains(one, "ORDER BY time DESC, id DESC LIMIT 1") {
		t.Fatalf("n=1 missing GetLatest order: %q", one)
	}
	if strings.Count(one, "?") != 1 {
		t.Fatalf("n=1 placeholders = %d, want 1", strings.Count(one, "?"))
	}
	if !strings.Contains(one, "AS latest_hb") {
		t.Fatalf("n=1 missing derived-table alias (MariaDB requires it): %q", one)
	}

	two := latestHeartbeatsUnionSQL(2)
	if strings.Count(two, "UNION ALL") != 1 {
		t.Fatalf("n=2 UNION ALL count = %d, want 1", strings.Count(two, "UNION ALL"))
	}
	if strings.Count(two, "?") != 2 {
		t.Fatalf("n=2 placeholders = %d, want 2", strings.Count(two, "?"))
	}

	many := latestHeartbeatsUnionSQL(500)
	if strings.Count(many, "UNION ALL") != 499 {
		t.Fatalf("n=500 UNION ALL count = %d, want 499", strings.Count(many, "UNION ALL"))
	}
	if strings.Contains(many, "ROW_NUMBER()") {
		t.Fatal("batch SQL must not use ROW_NUMBER; that scan held MariaDB sessions for minutes")
	}
}

func TestLatestRecentHeartbeatsUnionSQL(t *testing.T) {
	t.Parallel()

	if got := latestRecentHeartbeatsUnionSQL(0); got != "" {
		t.Fatalf("n=0: got %q, want empty", got)
	}

	one := latestRecentHeartbeatsUnionSQL(1)
	if !strings.Contains(one, "time >= ?") {
		t.Fatalf("recent arm missing the partition-pruning time bound: %q", one)
	}
	if !strings.Contains(one, "ORDER BY time DESC, id DESC LIMIT 1") {
		t.Fatalf("recent arm must keep GetLatest's tie-breaking order: %q", one)
	}
	if strings.Count(one, "?") != 2 {
		t.Fatalf("n=1 placeholders = %d, want 2 (monitor_id, bound)", strings.Count(one, "?"))
	}
	if !strings.Contains(one, "AS latest_hb") {
		t.Fatalf("n=1 missing derived-table alias (MariaDB requires it): %q", one)
	}

	two := latestRecentHeartbeatsUnionSQL(2)
	if strings.Count(two, "UNION ALL") != 1 {
		t.Fatalf("n=2 UNION ALL count = %d, want 1", strings.Count(two, "UNION ALL"))
	}
	if strings.Count(two, "?") != 4 {
		t.Fatalf("n=2 placeholders = %d, want 4", strings.Count(two, "?"))
	}
}

func TestLatestImportantBeforeUnionSQL(t *testing.T) {
	t.Parallel()

	if got := latestImportantBeforeUnionSQL(0); got != "" {
		t.Fatalf("n=0: got %q, want empty", got)
	}

	one := latestImportantBeforeUnionSQL(1)
	if strings.Contains(one, "UNION ALL") {
		t.Fatalf("n=1 should be a single SELECT, got %q", one)
	}
	if strings.HasPrefix(strings.TrimSpace(one), "(") {
		t.Fatalf("n=1 must not wrap the UNION arm in parens (SQLite syntax error): %q", one)
	}
	if !strings.Contains(one, "important = TRUE") {
		t.Fatalf("n=1 missing important filter: %q", one)
	}
	if !strings.Contains(one, "ORDER BY time DESC, id DESC LIMIT 1") {
		t.Fatalf("n=1 missing GetLatest-shaped order: %q", one)
	}
	if strings.Count(one, "?") != 2 {
		t.Fatalf("n=1 placeholders = %d, want 2 (monitor_id, before)", strings.Count(one, "?"))
	}
	if !strings.Contains(one, "AS latest_imp") {
		t.Fatalf("n=1 missing derived-table alias (MariaDB requires it): %q", one)
	}

	two := latestImportantBeforeUnionSQL(2)
	if strings.Count(two, "UNION ALL") != 1 {
		t.Fatalf("n=2 UNION ALL count = %d, want 1", strings.Count(two, "UNION ALL"))
	}
	if strings.Count(two, "?") != 4 {
		t.Fatalf("n=2 placeholders = %d, want 4", strings.Count(two, "?"))
	}

	many := latestImportantBeforeUnionSQL(500)
	if strings.Count(many, "UNION ALL") != 499 {
		t.Fatalf("n=500 UNION ALL count = %d, want 499", strings.Count(many, "UNION ALL"))
	}
	if strings.Contains(many, "ROW_NUMBER()") {
		t.Fatal("batch SQL must not use ROW_NUMBER; that scan held MariaDB sessions for minutes")
	}
}
