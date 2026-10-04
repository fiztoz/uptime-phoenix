package repository

import (
	"testing"
	"time"
)

func TestAssignmentChangeTimeOrdersClockRollbackAtDatabasePrecision(t *testing.T) {
	previous := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	zone := time.FixedZone("UTC+7", 7*3600)
	first := assignmentChangeTime(previous.In(zone))
	second := assignmentChangeTime(first)
	if first.Location() != time.UTC || second.Location() != time.UTC ||
		!first.Equal(previous.Add(time.Microsecond)) || !second.Equal(first.Add(time.Microsecond)) {
		t.Fatalf("clock rollback produced overlapping intervals: %s, %s after %s", first, second, previous)
	}
}
