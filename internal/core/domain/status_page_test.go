package domain

import "testing"

func TestNormalizeDashboardStyle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "full", input: DashboardStyleFull, want: DashboardStyleFull},
		{name: "grid", input: DashboardStyleGrid, want: DashboardStyleGrid},
		{name: "pills", input: DashboardStylePills, want: DashboardStylePills},
		{name: "empty defaults to full", input: "", want: DashboardStyleFull},
		{name: "unknown defaults to full", input: "poster", want: DashboardStyleFull},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := NormalizeDashboardStyle(tt.input); got != tt.want {
				t.Fatalf("NormalizeDashboardStyle(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
