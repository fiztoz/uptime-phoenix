package repository

// SetReplayPerEventForTest makes s apply observation batches one event at a
// time, the path mixed batches take, so tests can compare it with the batched
// path.
func SetReplayPerEventForTest(s *ProbeReplayStore, perEvent bool) { s.perEventReplay = perEvent }
