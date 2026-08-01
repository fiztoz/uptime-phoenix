package domain

import "testing"

func TestMonitorEffectiveOwner_OwnWhenNotInheriting(t *testing.T) {
	m := &Monitor{Owner: "Alice", InheritGroupOwner: false}
	gid := int64(1)
	m.GroupID = &gid
	groups := map[int64]*MonitorGroup{1: {ID: 1, Owner: "Group Team"}}
	if got := m.EffectiveOwner(groups); got != "Alice" {
		t.Fatalf("EffectiveOwner = %q; want Alice", got)
	}
}

func TestMonitorEffectiveOwner_InheritsNearestGroup(t *testing.T) {
	parentID := int64(1)
	childID := int64(2)
	m := &Monitor{Owner: "fallback", InheritGroupOwner: true, GroupID: &childID}
	groups := map[int64]*MonitorGroup{
		1: {ID: 1, Owner: "Parent Team"},
		2: {ID: 2, ParentID: &parentID, Owner: ""},
	}
	if got := m.EffectiveOwner(groups); got != "Parent Team" {
		t.Fatalf("EffectiveOwner = %q; want Parent Team (ancestor)", got)
	}
	groups[2].Owner = "Child Team"
	if got := m.EffectiveOwner(groups); got != "Child Team" {
		t.Fatalf("EffectiveOwner = %q; want Child Team (nearest)", got)
	}
}

func TestMonitorEffectiveOwner_FallsBackToOwnWhenChainEmpty(t *testing.T) {
	gid := int64(9)
	m := &Monitor{Owner: "solo", InheritGroupOwner: true, GroupID: &gid}
	groups := map[int64]*MonitorGroup{9: {ID: 9, Owner: ""}}
	if got := m.EffectiveOwner(groups); got != "solo" {
		t.Fatalf("EffectiveOwner = %q; want solo", got)
	}
}
