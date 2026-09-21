package services

import (
	"context"
	"testing"
	"time"
)

func TestSourceDeliveryQuiescenceBoundsInflightAndStopsClaims(t *testing.T) {
	for _, finish := range []bool{true, false} {
		name := "canceled uncertain attempt"
		if finish {
			name = "completed attempt"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			entered, release, quiesce, done := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			claims, committed := 0, false
			go func() {
				defer close(done)
				runSourceDeliveriesUntilQuiesced(ctx, quiesce, func(ctx context.Context) (bool, error) {
					claims++
					close(entered)
					select {
					case <-release:
						committed = true
						return true, nil
					case <-ctx.Done():
						return true, ctx.Err()
					}
				}, nil)
			}()
			<-entered
			close(quiesce)
			if finish {
				close(release)
			} else {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("attempt ignored shutdown bound")
			}
			if claims != 1 || committed != finish {
				t.Fatal("shutdown repeated a claim or invented a result", claims, committed)
			}
		})
	}
}
