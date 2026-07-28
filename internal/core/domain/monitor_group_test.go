package domain

import "testing"

// TestMonitorGroup_Rollup table-tests Rollup across all four GroupConditions.
// Per the hard-won repo rule, these assert the actual derived status (and ok
// flag), never just "no panic" — a group that silently returns no status is
// exactly the kind of bug that must be caught here.
func TestMonitorGroup_Rollup(t *testing.T) {
	tests := []struct {
		name               string
		condition          GroupCondition
		threshold          int
		thresholdIsPercent bool
		children           []Status
		wantStatus         Status
		wantOK             bool
	}{
		{
			name:       "no children => ok=false",
			condition:  GroupConditionWorstOfChildren,
			children:   nil,
			wantStatus: StatusPending,
			wantOK:     false,
		},
		{
			name:       "ignore condition => ok=false even with children",
			condition:  GroupConditionIgnore,
			children:   []Status{StatusUp, StatusDown},
			wantStatus: StatusPending,
			wantOK:     false,
		},
		{
			name:       "worst_of_children: all up => up",
			condition:  GroupConditionWorstOfChildren,
			children:   []Status{StatusUp, StatusUp, StatusUp},
			wantStatus: StatusUp,
			wantOK:     true,
		},
		{
			name:       "worst_of_children: single down trips the group",
			condition:  GroupConditionWorstOfChildren,
			children:   []Status{StatusUp, StatusDown, StatusUp},
			wantStatus: StatusDown,
			wantOK:     true,
		},
		{
			name:       "maintenance children excluded from the tally",
			condition:  GroupConditionWorstOfChildren,
			children:   []Status{StatusMaintenance, StatusUp},
			wantStatus: StatusUp,
			wantOK:     true,
		},
		{
			name:       "all-maintenance => MAINTENANCE regardless of condition",
			condition:  GroupConditionAllDown,
			children:   []Status{StatusMaintenance, StatusMaintenance},
			wantStatus: StatusMaintenance,
			wantOK:     true,
		},
		{
			name:       "all_down: not every child down => up (redundant pool)",
			condition:  GroupConditionAllDown,
			children:   []Status{StatusDown, StatusUp},
			wantStatus: StatusUp,
			wantOK:     true,
		},
		{
			name:       "all_down: every child down => down",
			condition:  GroupConditionAllDown,
			children:   []Status{StatusDown, StatusDown},
			wantStatus: StatusDown,
			wantOK:     true,
		},
		{
			name:       "all_down: maintenance excluded, remaining all down => down",
			condition:  GroupConditionAllDown,
			children:   []Status{StatusMaintenance, StatusDown, StatusDown},
			wantStatus: StatusDown,
			wantOK:     true,
		},
		{
			// The floor: a threshold of 5 can never trip with only 3 children,
			// but if all 3 are DOWN the group must still report DOWN rather than
			// claiming UP with nothing actually up.
			name:               "threshold: all-children-down floor overrides an unreachable threshold",
			condition:          GroupConditionThreshold,
			threshold:          5,
			thresholdIsPercent: false,
			children:           []Status{StatusDown, StatusDown, StatusDown},
			wantStatus:         StatusDown,
			wantOK:             true,
		},
		{
			name:               "threshold by count: below threshold => up",
			condition:          GroupConditionThreshold,
			threshold:          2,
			thresholdIsPercent: false,
			children:           []Status{StatusDown, StatusUp, StatusUp},
			wantStatus:         StatusUp,
			wantOK:             true,
		},
		{
			name:               "threshold by count: at threshold => down",
			condition:          GroupConditionThreshold,
			threshold:          2,
			thresholdIsPercent: false,
			children:           []Status{StatusDown, StatusDown, StatusUp},
			wantStatus:         StatusDown,
			wantOK:             true,
		},
		{
			name:               "threshold by percent: at 50% of 4 => down",
			condition:          GroupConditionThreshold,
			threshold:          50,
			thresholdIsPercent: true,
			children:           []Status{StatusDown, StatusDown, StatusUp, StatusUp},
			wantStatus:         StatusDown,
			wantOK:             true,
		},
		{
			name:               "threshold by percent: below 50% of 4 => up",
			condition:          GroupConditionThreshold,
			threshold:          50,
			thresholdIsPercent: true,
			children:           []Status{StatusDown, StatusUp, StatusUp, StatusUp},
			wantStatus:         StatusUp,
			wantOK:             true,
		},
		{
			// Maintenance children shrink the active pool the percentage is
			// computed against, not just the down count.
			name:               "threshold by percent: maintenance shrinks the active denominator",
			condition:          GroupConditionThreshold,
			threshold:          50,
			thresholdIsPercent: true,
			children:           []Status{StatusMaintenance, StatusDown, StatusUp, StatusUp},
			wantStatus:         StatusUp,
			wantOK:             true,
		},
		{
			name:       "single pending child, no down => pending",
			condition:  GroupConditionWorstOfChildren,
			children:   []Status{StatusPending},
			wantStatus: StatusPending,
			wantOK:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := MonitorGroup{
				Condition:          tt.condition,
				Threshold:          tt.threshold,
				ThresholdIsPercent: tt.thresholdIsPercent,
			}
			gotStatus, gotOK := g.Rollup(tt.children)
			if gotOK != tt.wantOK {
				t.Fatalf("Rollup() ok = %v, want %v (status=%v)", gotOK, tt.wantOK, gotStatus)
			}
			if gotOK && gotStatus != tt.wantStatus {
				t.Fatalf("Rollup() status = %v, want %v", gotStatus, tt.wantStatus)
			}
		})
	}
}
