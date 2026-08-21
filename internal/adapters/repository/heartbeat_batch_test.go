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
	if !strings.Contains(one, "ORDER BY time DESC, id DESC LIMIT 1") {
		t.Fatalf("n=1 missing GetLatest order: %q", one)
	}
	if strings.Count(one, "?") != 1 {
		t.Fatalf("n=1 placeholders = %d, want 1", strings.Count(one, "?"))
	}
	if strings.HasPrefix(one, "(") {
		t.Fatalf("n=1 must not wrap the compound term in parens (SQLite syntax error): %q", one)
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
