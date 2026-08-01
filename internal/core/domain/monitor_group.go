package domain

import "time"

// GroupCondition determines how a group derives its status from its children.
type GroupCondition string

const (
	// GroupConditionWorstOfChildren takes the worst status among the children:
	// a single DOWN child makes the whole group DOWN. This is the default.
	GroupConditionWorstOfChildren GroupCondition = "worst_of_children"

	// GroupConditionAllDown makes the group DOWN only when every child is DOWN.
	// Intended for redundant pools, where one surviving member still means the
	// service as a whole is up.
	GroupConditionAllDown GroupCondition = "all_down"

	// GroupConditionThreshold makes the group DOWN once the number of DOWN
	// children reaches Threshold — an absolute count, or a percentage of the
	// children when ThresholdIsPercent is set.
	GroupConditionThreshold GroupCondition = "threshold"

	// GroupConditionIgnore makes the group purely organizational: it derives no
	// status at all and renders as a plain folder.
	GroupConditionIgnore GroupCondition = "ignore"
)

// ValidGroupConditions lists every condition accepted by the API, in the order
// the UI should offer them.
var ValidGroupConditions = []GroupCondition{
	GroupConditionWorstOfChildren,
	GroupConditionAllDown,
	GroupConditionThreshold,
	GroupConditionIgnore,
}

// Valid reports whether c is a condition this build understands.
func (c GroupCondition) Valid() bool {
	for _, v := range ValidGroupConditions {
		if c == v {
			return true
		}
	}
	return false
}

// MonitorGroup is a folder that monitors (and other groups) can be filed under.
// Unlike a monitor it is never checked and has no interval, URL, or heartbeat —
// its status, when it has one, is derived from its children via Condition.
type MonitorGroup struct {
	ID          int64
	UserID      int64
	Name        string
	Description string
	// Owner is free-text contact for the team responsible for monitors in this
	// folder. Display-only; no authorization meaning. Monitors may inherit it
	// via Monitor.InheritGroupOwner (and ancestor walk).
	Owner string

	// ParentID nests this group inside another group. nil means top-level.
	// Nesting is arbitrarily deep; the service rejects cycles.
	ParentID *int64

	Condition GroupCondition

	// Threshold is the number of DOWN children that trips the group DOWN when
	// Condition is GroupConditionThreshold. When ThresholdIsPercent it is a
	// percentage (1-100) of the group's children instead of an absolute count.
	Threshold          int
	ThresholdIsPercent bool

	Weight    int  // display order among siblings
	Collapsed bool // whether the UI renders this group collapsed by default

	// LastStatus is the last derived status this group was OBSERVED at by the
	// alerting path — the persisted half of group transition detection.
	//
	// It is not a cache of Rollup: it exists so two workers processing heartbeats
	// for two monitors in the same group at the same instant cannot both decide
	// "the group just went DOWN" and both alert. The transition is claimed with a
	// compare-and-set UPDATE against this column (see
	// ports.MonitorGroupRepository.ClaimStatusTransition), so exactly one worker
	// wins. An in-memory map would double-send under Redis fan-out.
	//
	// nil means the group has never been evaluated. It is written ONLY by the CAS;
	// the repositories deliberately exclude it from a normal Update so a stale
	// group object round-tripping through the admin API cannot clobber it.
	LastStatus *Status

	CreatedAt time.Time
	UpdatedAt time.Time
}

// Rollup derives the group's status from the statuses of its direct children —
// child monitors plus subgroups whose own status has already been resolved.
//
// ok is false when the group has no status to display: an "ignore" group, or a
// group with no children at all. Callers render those as a plain folder.
//
// Children in MAINTENANCE are excluded from the tally rather than counted as
// DOWN: maintenance is deliberate downtime and must not trip a group. A group
// whose children are *all* in maintenance is itself MAINTENANCE.
func (g MonitorGroup) Rollup(children []Status) (Status, bool) {
	if g.Condition == GroupConditionIgnore || len(children) == 0 {
		return StatusPending, false
	}

	t := tallyStatuses(children)

	// Every child is in maintenance, so the group is too.
	if t.active == 0 {
		return StatusMaintenance, true
	}

	switch {
	case g.trips(t):
		return StatusDown, true
	case t.up > 0:
		return StatusUp, true
	default:
		return StatusPending, true
	}
}

// statusTally counts children by status. Children in MAINTENANCE are counted in
// neither active nor any bucket, so they cannot trip a group.
type statusTally struct {
	down, up, active int
}

func tallyStatuses(children []Status) statusTally {
	var t statusTally
	for _, s := range children {
		if s == StatusMaintenance {
			continue
		}
		t.active++
		switch s {
		case StatusDown:
			t.down++
		case StatusUp:
			t.up++
		}
	}
	return t
}

// trips reports whether the tally is bad enough to force the group DOWN.
// Callers must have ruled out an all-maintenance group (t.active == 0) first.
func (g MonitorGroup) trips(t statusTally) bool {
	// If every child is DOWN the group is DOWN under any condition. Without this
	// floor a misconfigured threshold (5 DOWN required, but only 3 children)
	// could never trip, and the group would claim UP with nothing running.
	if t.down == t.active {
		return true
	}

	switch g.Condition {
	case GroupConditionAllDown:
		return false // the all-down case is the floor above
	case GroupConditionThreshold:
		// A threshold below 1 would trip on zero DOWN children, pinning the group
		// DOWN forever. Treat it as 1 — same as worst-of-children.
		limit := max(g.Threshold, 1)
		if g.ThresholdIsPercent {
			// Cross-multiply rather than divide, to avoid float rounding.
			return t.down*100 >= limit*t.active
		}
		return t.down >= limit
	default: // GroupConditionWorstOfChildren
		return t.down > 0
	}
}
