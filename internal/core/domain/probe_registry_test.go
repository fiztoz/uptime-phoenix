package domain

import "testing"

func TestLocalWorkerMayRun(t *testing.T) {
	if !LocalWorkerMayRun(nil) {
		t.Fatal("legacy monitors without an assignment set must remain locally runnable")
	}
	local := &MonitorProbeAssignments{Assignments: []ProbeAssignment{{ProbeID: LocalProbeID}}}
	if !LocalWorkerMayRun(local) {
		t.Fatal("local assignment must be runnable")
	}
	both := &MonitorProbeAssignments{Assignments: []ProbeAssignment{
		{ProbeID: LocalProbeID}, {ProbeID: "e645246b-b176-4422-8ae5-b79629ee6a29"},
	}}
	if !LocalWorkerMayRun(both) {
		t.Fatal("mixed assignment including local must be runnable")
	}
	remote := &MonitorProbeAssignments{Assignments: []ProbeAssignment{
		{ProbeID: "e645246b-b176-4422-8ae5-b79629ee6a29"},
	}}
	if LocalWorkerMayRun(remote) {
		t.Fatal("remote-only assignment must not be runnable by the hub worker")
	}
}
