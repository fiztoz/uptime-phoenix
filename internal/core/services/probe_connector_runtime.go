package services

import (
	"context"
	"errors"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// withRuntime keeps stable ownership across all reconnect attempts and backoff.
// It cancels and joins the action before releasing authority. Watchdog state will
// belong to this scope, not to connectOnce or a connection-generation callback.
func (s *ProbeConnectorService) withRuntime(ctx context.Context, probeID string, action func(context.Context, domain.ProbeRuntimeLease) error) error {
	acquireCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	lease, err := s.runtimes.AcquireRuntime(acquireCtx, probeID, s.ownerID)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.runtimes.ReleaseRuntime(releaseCtx, lease)
	}()
	ownedCtx, stop := context.WithCancel(ctx)
	defer stop()
	renewed := make(chan struct{})
	var renewErr error // Read only after joining the renewal goroutine.
	go func() {
		defer close(renewed)
		ticker := time.NewTicker(s.renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ownedCtx.Done():
				return
			case <-ticker.C:
			}
			checkCtx, cancel := context.WithTimeout(ownedCtx, 5*time.Second)
			_, err := s.runtimes.RenewRuntime(checkCtx, lease)
			cancel()
			if err != nil {
				// Normal action completion cancels in-flight renewal too. That
				// cleanup must not turn successful enrollment into a failure.
				if errors.Is(err, context.Canceled) && ownedCtx.Err() != nil {
					return
				}
				renewErr = err
				stop()
				return
			}
		}
	}()
	err = action(ownedCtx, lease)
	stop()
	<-renewed
	if renewErr != nil {
		return renewErr
	}
	return err
}
