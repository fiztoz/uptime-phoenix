package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type localDeliveryReaderFake struct {
	consumerTestActivationRepo
	err error
}

var _ ports.LocalDeliveryConfigReader = (*localDeliveryReaderFake)(nil)

func (r *localDeliveryReaderFake) ReadAppliedLocalDelivery(context.Context, int64, int64) (*domain.LocalProbeConfigDefinition, error) {
	return nil, r.err
}

func TestDeliveryConfigReadFailurePreservesQueuedWork(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		superseded bool
	}{
		{"source drift", ports.ErrConflict, false},
		{"unavailable storage", errors.New("unavailable"), false},
		{"rotated channel", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now().UTC()
			until := now.Add(5 * time.Minute)
			item := domain.QueuedDelivery{
				DeliveryIntent: domain.DeliveryIntent{DeliveryID: "queued", ProbeID: domain.LocalProbeID, NotificationID: 7, NotificationVersion: 1},
				Status:         domain.DeliveryStatusLeased, Attempt: 1, LeaseToken: "claim", LeaseUntil: &until,
			}
			outbox := newFakeDeliveryOutboxRepo()
			outbox.intents[item.DeliveryID] = &item
			reader := &localDeliveryReaderFake{consumerTestActivationRepo: consumerTestActivationRepo{
				active: map[string]*domain.ProbeActiveConfig{domain.LocalProbeID: {Revision: 2}},
			}, err: tc.err}
			consumer := NewDeliveryOutboxConsumer(outbox, nil, DefaultDeliveryConsumerConfig())
			consumer.now = func() time.Time { return now }
			consumer.SetActivationRepository(reader)
			err := consumer.ProcessOne(t.Context(), item)
			if tc.err != nil && !errors.Is(err, tc.err) {
				t.Fatalf("read failure was not preserved: %v", err)
			}
			if tc.superseded {
				if err != nil || len(outbox.finishLogs) != 1 || outbox.finishLogs[0].Status != domain.DeliveryStatusSuperseded {
					t.Fatal("explicit channel rotation did not supersede work")
				}
			} else if len(outbox.finishLogs) != 0 || item.Status != domain.DeliveryStatusLeased {
				t.Fatal("transient read failure permanently discarded queued work")
			}
		})
	}
}
