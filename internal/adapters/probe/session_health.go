package probe

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// HealthReceipt retains validated session identity and the local monotonic read
// time. ReceivedAt must not be converted to UTC or replaced with the peer clock
// before a watchdog measures elapsed time from its own process baseline.
type HealthReceipt struct {
	Health     Health
	Generation Decimal
	ReceivedAt time.Time
}

// RunWithHealth isolates ordered health callbacks from ordered non-health work.
// Both callbacks must honor cancellation. They run concurrently with each other,
// but each lane has one worker. Overload closes the session rather than silently
// dropping or coalescing health samples. All callbacks are joined before return.
func (s *Session) RunWithHealth(ctx context.Context, handle func(context.Context, Envelope) error, health func(context.Context, HealthReceipt) error) error {
	if handle == nil || health == nil {
		return errors.New("frame and health handlers are required")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Each frame is bounded by MaxFrameBytes. Health contains only validated
	// bounded fields. A peer exceeding either backlog must reconnect/replay.
	frames := make(chan Envelope, 16)
	samples := make(chan HealthReceipt, 4)
	errorsCh := make(chan error, 1)
	var workers sync.WaitGroup
	start := func(next func(context.Context) error) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for runCtx.Err() == nil {
				if err := next(runCtx); err != nil {
					if runCtx.Err() != nil {
						return
					}
					select {
					case errorsCh <- err:
					default:
					}
					cancel()
					_ = s.Close()
					return
				}
			}
		}()
	}
	start(func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame := <-frames:
			return s.callIncoming(ctx, func(ctx context.Context) error { return handle(ctx, frame) })
		}
	})
	start(func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case sample := <-samples:
			return s.callIncoming(ctx, func(ctx context.Context) error { return health(ctx, sample) })
		}
	})
	err := s.run(runCtx, func(ctx context.Context, e Envelope, receivedAt time.Time, validated *Health) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.Type == "health" {
			// Retain the exact validated fields. Re-decoding raw JSON into a
			// struct would let unknown case-insensitive aliases replace them.
			if validated == nil {
				return errors.New("invalid health payload")
			}
			select {
			case samples <- HealthReceipt{Health: *validated, Generation: e.ConnectionGeneration, ReceivedAt: receivedAt}:
				return nil
			default:
				return errors.New("incoming health queue full")
			}
		}
		select {
		case frames <- e:
			return nil
		default:
			return errors.New("incoming frame queue full")
		}
	})
	cancel()
	workers.Wait()
	select {
	case workerErr := <-errorsCh:
		return fmt.Errorf("incoming handler failed: %w", workerErr)
	default:
		return err
	}
}

func (s *Session) callIncoming(ctx context.Context, call func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timeout := s.handlerTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	opCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := call(opCtx); err != nil {
		return err
	}
	return opCtx.Err()
}
