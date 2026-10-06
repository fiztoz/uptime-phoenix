package services_test

import (
	"context"
	"errors"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// Fixture values are disposable test strings, never real credentials.
const (
	pushFixtureA = "fixture-push-a"
	pushFixtureB = "fixture-push-b"
)

func pushTokenDoc(token string) *services.ConfigDocument {
	active := true
	cfg := map[string]any{}
	if token != "" {
		cfg["push_token"] = token
	}
	return &services.ConfigDocument{
		APIVersion: services.ConfigAPIVersion,
		Kind:       services.ConfigKind,
		Spec: services.ConfigSpec{
			Monitors: []services.ConfigMonitor{
				{
					Key: "push-check", Name: "Push", Type: "push", Active: &active,
					Interval: 60, Config: cfg,
				},
			},
		},
	}
}

// TestConfigApply_PushMonitor_PersistsLookupToken asserts a declaratively
// created push monitor resolves through the dedicated push_token lookup
// column, exactly like one created through the HTTP API (issue #66).
func TestConfigApply_PushMonitor_PersistsLookupToken(t *testing.T) {
	svc, keys, _, _, mons := newConfigSvc(t)
	ctx := context.Background()
	if _, err := svc.Apply(ctx, 1, pushTokenDoc(pushFixtureA), services.ConfigApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ck, err := keys.GetByKey(ctx, domain.ConfigResourceMonitor, "push-check")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	stored, err := mons.GetByID(ctx, ck.ResourceID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if stored.PushToken != pushFixtureA {
		t.Fatalf("stored PushToken = %q, want %q (the dedicated lookup column)", stored.PushToken, pushFixtureA)
	}
	if got := stored.Config["push_token"]; got != pushFixtureA {
		t.Fatalf("stored Config[push_token] = %v, want %q", got, pushFixtureA)
	}
	byToken, err := mons.GetByPushToken(ctx, pushFixtureA)
	if err != nil {
		t.Fatalf("GetByPushToken(%q) = %v, want the created monitor", pushFixtureA, err)
	}
	if byToken.ID != stored.ID {
		t.Fatalf("token resolved to monitor %d, want %d", byToken.ID, stored.ID)
	}
}

// TestConfigApply_PushMonitor_TokenRotationUpdatesLookup asserts an explicit
// value change moves the lookup: the new value resolves and the old one no
// longer does.
func TestConfigApply_PushMonitor_TokenRotationUpdatesLookup(t *testing.T) {
	svc, keys, _, _, mons := newConfigSvc(t)
	ctx := context.Background()
	if _, err := svc.Apply(ctx, 1, pushTokenDoc(pushFixtureA), services.ConfigApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := svc.Apply(ctx, 1, pushTokenDoc(pushFixtureB), services.ConfigApplyOptions{}); err != nil {
		t.Fatalf("apply update: %v", err)
	}
	ck, err := keys.GetByKey(ctx, domain.ConfigResourceMonitor, "push-check")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	stored, err := mons.GetByID(ctx, ck.ResourceID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if stored.PushToken != pushFixtureB {
		t.Fatalf("stored PushToken = %q, want %q", stored.PushToken, pushFixtureB)
	}
	byNew, err := mons.GetByPushToken(ctx, pushFixtureB)
	if err != nil || byNew.ID != stored.ID {
		t.Fatalf("new value lookup: %v id=%v", err, byNew)
	}
	if old, err := mons.GetByPushToken(ctx, pushFixtureA); err == nil {
		t.Fatalf("old value still resolves to monitor %d", old.ID)
	} else if !errors.Is(err, ports.ErrNotFound) {
		t.Fatalf("old value lookup error = %v, want not found", err)
	}
}

// TestConfigApply_PushMonitor_RedactedValuePreserved asserts the documented
// secret contract for the token: __REDACTED__ preserves the stored value in
// both the config and the lookup column, and export writes the sentinel.
func TestConfigApply_PushMonitor_RedactedValuePreserved(t *testing.T) {
	svc, keys, _, _, mons := newConfigSvc(t)
	ctx := context.Background()
	if _, err := svc.Apply(ctx, 1, pushTokenDoc(pushFixtureA), services.ConfigApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ck, err := keys.GetByKey(ctx, domain.ConfigResourceMonitor, "push-check")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	before, err := mons.GetByID(ctx, ck.ResourceID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}

	if _, err := svc.Apply(ctx, 1, pushTokenDoc(services.ConfigSecretRedacted), services.ConfigApplyOptions{}); err != nil {
		t.Fatalf("redacted apply: %v", err)
	}
	after, err := mons.GetByID(ctx, ck.ResourceID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if after.PushToken != before.PushToken || after.PushToken != pushFixtureA {
		t.Fatalf("redacted apply changed lookup value: before=%q after=%q", before.PushToken, after.PushToken)
	}
	if got := after.Config["push_token"]; got != pushFixtureA {
		t.Fatalf("redacted apply replaced config value: got %v", got)
	}
	if m, err := mons.GetByPushToken(ctx, pushFixtureA); err != nil || m.ID != after.ID {
		t.Fatalf("lookup after redacted apply: %v", err)
	}

	out, err := svc.Export(ctx, 1)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(out.Spec.Monitors) != 1 {
		t.Fatalf("exported monitors = %d, want 1", len(out.Spec.Monitors))
	}
	if got := out.Spec.Monitors[0].Config["push_token"]; got != services.ConfigSecretRedacted {
		t.Fatalf("export wrote a non-redacted value: %v", got)
	}
}

// TestConfigApply_PushMonitor_OmittedValuePreserved asserts an update that
// omits config.push_token entirely leaves the stored value and lookup intact.
func TestConfigApply_PushMonitor_OmittedValuePreserved(t *testing.T) {
	svc, keys, _, _, mons := newConfigSvc(t)
	ctx := context.Background()
	if _, err := svc.Apply(ctx, 1, pushTokenDoc(pushFixtureA), services.ConfigApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	doc := pushTokenDoc("")
	doc.Spec.Monitors[0].Config = nil
	if _, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{}); err != nil {
		t.Fatalf("omitted-value apply: %v", err)
	}
	ck, err := keys.GetByKey(ctx, domain.ConfigResourceMonitor, "push-check")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	after, err := mons.GetByID(ctx, ck.ResourceID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if after.PushToken != pushFixtureA {
		t.Fatalf("omitted-value apply dropped lookup value: %q", after.PushToken)
	}
	if got := after.Config["push_token"]; got != pushFixtureA {
		t.Fatalf("omitted-value apply replaced config value: %v", got)
	}
	if m, err := mons.GetByPushToken(ctx, pushFixtureA); err != nil || m.ID != after.ID {
		t.Fatalf("lookup after omitted-value apply: %v", err)
	}
}

// TestConfigApply_PushMonitor_OmittedCreateGeneratesToken asserts a create
// that supplies no token at all comes up with a generated lookup token, and
// that re-applying the same document stays the documented no-op: the generated
// token is invisible-by-contract and must neither read as drift nor rotate.
func TestConfigApply_PushMonitor_OmittedCreateGeneratesToken(t *testing.T) {
	svc, keys, _, _, mons := newConfigSvc(t)
	ctx := context.Background()
	doc := pushTokenDoc("")
	doc.Spec.Monitors[0].Config = nil
	res, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{})
	if err != nil || res.Creates != 1 {
		t.Fatalf("apply: res=%+v err=%v", res, err)
	}
	ck, err := keys.GetByKey(ctx, domain.ConfigResourceMonitor, "push-check")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	stored, err := mons.GetByID(ctx, ck.ResourceID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if stored.PushToken == "" || stored.PushToken == services.ConfigSecretRedacted {
		t.Fatalf("created PushToken = %q, want a generated token", stored.PushToken)
	}
	if got := stored.Config["push_token"]; got != stored.PushToken {
		t.Fatalf("stored Config[push_token] = %v, want %q", got, stored.PushToken)
	}
	if byToken, err := mons.GetByPushToken(ctx, stored.PushToken); err != nil || byToken.ID != stored.ID {
		t.Fatalf("lookup by generated token: %v id=%v", err, byToken)
	}

	res2, err := svc.Apply(ctx, 1, doc, services.ConfigApplyOptions{})
	if err != nil || res2.Updates != 0 || res2.Unchanged != 1 {
		t.Fatalf("second apply is not a no-op: res=%+v err=%v", res2, err)
	}
	after, err := mons.GetByID(ctx, ck.ResourceID)
	if err != nil {
		t.Fatalf("get monitor after re-apply: %v", err)
	}
	if after.PushToken != stored.PushToken {
		t.Fatalf("re-apply rotated the generated token: %q -> %q", stored.PushToken, after.PushToken)
	}
}

// TestConfigApply_PushMonitor_RedactedCreateGeneratesToken asserts a create
// that supplies only the redacted sentinel gets a freshly generated token
// (create must never ship a dead monitor the UI reports as "generated"), while
// the sentinel itself is never persisted and never resolves.
func TestConfigApply_PushMonitor_RedactedCreateGeneratesToken(t *testing.T) {
	svc, keys, _, _, mons := newConfigSvc(t)
	ctx := context.Background()
	if _, err := svc.Apply(ctx, 1, pushTokenDoc(services.ConfigSecretRedacted), services.ConfigApplyOptions{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	ck, err := keys.GetByKey(ctx, domain.ConfigResourceMonitor, "push-check")
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	stored, err := mons.GetByID(ctx, ck.ResourceID)
	if err != nil {
		t.Fatalf("get monitor: %v", err)
	}
	if stored.PushToken == "" || stored.PushToken == services.ConfigSecretRedacted {
		t.Fatalf("created PushToken = %q, want a generated token", stored.PushToken)
	}
	if got := stored.Config["push_token"]; got != stored.PushToken {
		t.Fatalf("redacted placeholder persisted in config: %v", got)
	}
	if byToken, err := mons.GetByPushToken(ctx, stored.PushToken); err != nil || byToken.ID != stored.ID {
		t.Fatalf("lookup by generated token: %v id=%v", err, byToken)
	}
	if m, err := mons.GetByPushToken(ctx, services.ConfigSecretRedacted); err == nil {
		t.Fatalf("redacted sentinel resolved to monitor %d", m.ID)
	}
}
