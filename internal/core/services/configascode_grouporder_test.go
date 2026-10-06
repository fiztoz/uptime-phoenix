package services

import (
	"strings"
	"testing"
)

func groupIndexOf(out []ConfigMonitorGroup, key string) int {
	for i, g := range out {
		if g.Key == key {
			return i
		}
	}
	return -1
}

// TestOrderConfigGroups_ParentsFirstAndRoots asserts the ordering emits every
// parent before its children for out-of-order declarations, tolerates multiple
// roots, and silently skips parent keys absent from the document.
func TestOrderConfigGroups_ParentsFirstAndRoots(t *testing.T) {
	groups := []ConfigMonitorGroup{
		{Key: "leaf", Name: "Leaf", Parent: "mid"},
		{Key: "other-root", Name: "Other Root"},
		{Key: "orphan-child", Name: "Orphan", Parent: "unknown-parent"},
		{Key: "mid", Name: "Mid", Parent: "root"},
		{Key: "root", Name: "Root"},
	}
	out, err := orderConfigGroups(groups)
	if err != nil {
		t.Fatalf("valid graph rejected: %v", err)
	}
	if len(out) != len(groups) {
		t.Fatalf("ordered %d groups, want %d", len(out), len(groups))
	}
	for _, pair := range [][2]string{{"root", "mid"}, {"mid", "leaf"}} {
		pi, ci := groupIndexOf(out, pair[0]), groupIndexOf(out, pair[1])
		if pi < 0 || ci < 0 {
			t.Fatalf("missing %v in %v", pair, out)
		}
		if pi >= ci {
			t.Fatalf("parent %q (idx %d) must precede child %q (idx %d)", pair[0], pi, pair[1], ci)
		}
	}
}

// TestOrderConfigGroups_RejectsCycles asserts the defensive ordering returns an
// error naming the offending chain instead of recursing forever (issue #64 —
// the pre-fix traversal was process-fatal).
func TestOrderConfigGroups_RejectsCycles(t *testing.T) {
	cases := map[string][]ConfigMonitorGroup{
		"self":  {{Key: "self", Name: "Self", Parent: "self"}},
		"two":   {{Key: "a", Name: "A", Parent: "b"}, {Key: "b", Name: "B", Parent: "a"}},
		"three": {{Key: "a", Name: "A", Parent: "b"}, {Key: "b", Name: "B", Parent: "c"}, {Key: "c", Name: "C", Parent: "a"}},
	}
	for name, groups := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := orderConfigGroups(groups)
			if err == nil {
				t.Fatalf("cyclic graph ordered instead of rejected: %v", out)
			}
			if !strings.Contains(err.Error(), "cycle") {
				t.Fatalf("error lacks cycle detail: %v", err)
			}
			for _, g := range groups {
				if !strings.Contains(err.Error(), g.Key) {
					t.Fatalf("error does not name offending group %q: %v", g.Key, err)
				}
			}
		})
	}
}
