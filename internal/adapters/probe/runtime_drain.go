package probe

import (
	"context"
	"errors"
	"time"
)

// EdgeDrainResult describes the durable prefix, never a socket-send result.
type EdgeDrainResult struct {
	StreamID     string
	TargetSeq    int64
	CommittedSeq int64
	Flushed      bool
}

func (r *EdgeRuntime) admitMutation() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.draining {
		return false
	}
	r.mutations.Add(1)
	return true
}

// Quiesce rejects new sessions and management mutations, then joins callbacks
// already admitted under their bounded session-handler contexts. Replay and
// current-state receipts keep running until Close. Quiescence is permanent.
func (r *EdgeRuntime) Quiesce() {
	r.mu.Lock()
	r.draining = true
	r.mu.Unlock()
	r.mutations.Wait()
}

// WaitForReplay observes the existing pump's durable ACK progress until ctx ends.
// Call after Quiesce and after joining every source producer. This method never
// sends a batch, advances a cursor, prunes evidence, or extends the caller deadline.
func (r *EdgeRuntime) WaitForReplay(ctx context.Context) (EdgeDrainResult, error) {
	r.mu.Lock()
	draining := r.draining
	r.mu.Unlock()
	if !draining {
		return EdgeDrainResult{}, errors.New("edge drain requires quiescence")
	}
	i, _, err := r.state(ctx)
	if err != nil {
		return EdgeDrainResult{}, errors.New("edge drain storage unavailable")
	}
	result := EdgeDrainResult{StreamID: i.StreamID, TargetSeq: i.LastCreatedSeq, CommittedSeq: i.CommittedSeq}
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if i.CommittedSeq >= result.TargetSeq {
			result.Flushed = true
			return result, nil
		}
		select {
		case <-ctx.Done():
			return result, ctx.Err()
		case <-ticker.C:
		}
		next, _, err := r.state(ctx)
		if err != nil {
			return result, errors.New("edge drain storage unavailable")
		}
		if next.HubID != i.HubID || next.ProbeID != i.ProbeID || next.StreamID != i.StreamID || next.LastCreatedSeq != result.TargetSeq || next.CommittedSeq < i.CommittedSeq {
			return result, errors.New("edge drain authority or producer changed")
		}
		i = next
		result.CommittedSeq = i.CommittedSeq
	}
}
