package services

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type validatingSender struct{ typ string }

func (v validatingSender) Type() string { return v.typ }
func (v validatingSender) Validate(cfg map[string]any) error {
	if s, _ := cfg["url"].(string); s == "" {
		return errors.New("url is required")
	}
	return nil
}
func (v validatingSender) Send(context.Context, map[string]any, domain.AlertContext) error {
	return nil
}

// TestNotificationConfigIsValidatedBeforeSave pins the notifier part of #118:
// Create/Update persisted any type string and any config, so a typo'd type or
// a webhook with no URL was saved and only failed at alert time.
func TestNotificationConfigIsValidatedBeforeSave(t *testing.T) {
	newSvc := func() (*NotificationService, *fakeNotifRepo) {
		repo := newFakeNotifRepo()
		svc := NewNotificationService(repo, newFakeMonitorNotifLinkRepo())
		svc.RegisterSender(validatingSender{typ: "webhook"})
		return svc, repo
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		n    domain.Notification
	}{
		{"unregistered type", domain.Notification{UserID: 1, Name: "x", Type: "webhok", Config: map[string]any{"url": "https://example.test"}}},
		{"provider rejects config", domain.Notification{UserID: 1, Name: "x", Type: "webhook", Config: map[string]any{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, repo := newSvc()
			n := tc.n
			if err := svc.Create(ctx, &n); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Create = %v; want ErrValidation", err)
			}
			n = tc.n
			n.ID = 1
			if err := svc.Update(ctx, &n); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("Update = %v; want ErrValidation", err)
			}
			if saved, err := repo.ListAll(ctx); err != nil || len(saved) != 0 {
				t.Fatalf("invalid config persisted: count=%d error=%v", len(saved), err)
			}
		})
	}
	t.Run("empty registry fails closed", func(t *testing.T) {
		for _, op := range []string{"create", "update"} {
			repo := newFakeNotifRepo()
			svc := NewNotificationService(repo, newFakeMonitorNotifLinkRepo())
			n := domain.Notification{ID: 1, UserID: 1, Name: "x", Type: "webhook", Config: map[string]any{"url": "https://example.test"}}
			var err error
			if op == "create" {
				err = svc.Create(ctx, &n)
			} else {
				err = svc.Update(ctx, &n)
			}
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("%s with no registered sender = %v; want ErrValidation", op, err)
			}
			if saved, err := repo.ListAll(ctx); err != nil || len(saved) != 0 {
				t.Fatalf("unconfigured service persisted a notification: count=%d error=%v", len(saved), err)
			}
		}
	})
	t.Run("valid config saves", func(t *testing.T) {
		svc, repo := newSvc()
		n := domain.Notification{UserID: 1, Name: "ok", Type: "webhook", Config: map[string]any{"url": "https://example.test"}}
		if err := svc.Create(ctx, &n); err != nil {
			t.Fatalf("Create valid = %v", err)
		}
		if saved, err := repo.GetByID(ctx, n.ID); err != nil || saved == nil || saved.Name != n.Name {
			t.Fatalf("valid config not persisted: error=%v", err)
		}
	})
}
