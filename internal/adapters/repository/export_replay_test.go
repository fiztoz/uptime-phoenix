package repository

// SetReplayPerEventForTest makes s apply observation batches one event at a
// time, the path mixed batches take, so tests can compare it with the batched
// path. It only skips the deferred current-state and dirty-bucket writes: the
// bulk assignment lock and multi-row markDirtyTx still apply, so it is not the
// pre-change baseline. Measure that on the base commit instead.
func SetReplayPerEventForTest(s *ProbeReplayStore, perEvent bool) { s.perEventReplay = perEvent }
