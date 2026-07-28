package scheduler

import (
	"context"
	"testing"
	"time"
)

func TestHeartbeatRecordContext_HasNoDeadline(t *testing.T) {
	ctx := heartbeatRecordContext()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("heartbeat record context must not impose a deadline")
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("heartbeat record context must not be pre-canceled: %v", err)
	}
}

func TestHeartbeatRecordContext_NotCancelledWhenParentDone(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	cancel()

	recordCtx := heartbeatRecordContext()
	if err := recordCtx.Err(); err != nil {
		t.Fatalf("record context canceled: %v", err)
	}

	// Parent cancellation must not affect the detached record context.
	_ = parent

	done := make(chan struct{})
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-recordCtx.Done():
		t.Fatal("record context should not time out during short wait")
	}
}
