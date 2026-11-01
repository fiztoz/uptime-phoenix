package scheduler

import (
	"sync"
	"time"
)

// pushArming remembers when this scheduler started evaluating each push
// monitor's deadline (#161 review). It lives only in memory, so a process
// start, a resume, a new assignment, or a newly acquired lease re-arms the
// monitor: its push deadline then counts from the arming time instead of from
// a push that predates the downtime, and the scheduler does not turn its own
// downtime or a pause into a DOWN.
type pushArming struct {
	armed sync.Map // monitorID int64 -> time.Time
}

// observe arms the monitors of this tick's checks that are not armed yet,
// stamps every push check with its arming time, and disarms monitors that are
// no longer scheduled here (paused, deleted, unassigned, or lease lost).
func (a *pushArming) observe(checks []scheduledCheck, now time.Time) {
	present := make(map[int64]struct{}, len(checks))
	for i := range checks {
		id := checks[i].Monitor.ID
		present[id] = struct{}{}
		if checks[i].Monitor.Type != "push" {
			continue
		}
		v, _ := a.armed.LoadOrStore(id, now)
		checks[i].PushArmedAt = v.(time.Time)
	}
	a.armed.Range(func(key, _ any) bool {
		if _, ok := present[key.(int64)]; !ok {
			a.armed.Delete(key)
		}
		return true
	})
}
