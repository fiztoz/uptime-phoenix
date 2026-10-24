package domain

import (
	"testing"
	"time"
)

func cleanupFixture(state, code, attempt string) ProbeCleanup {
	install := preflightIdentity()
	created := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	return ProbeCleanup{
		Identity: ProbeCleanupIdentity{CleanupID: "c1111111-1111-4111-8111-111111111111", Install: install, ClientKeyFingerprint: install.ClientKeyFingerprint, RequestedBy: 1},
		OwnerID:  "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", State: state, ErrorCode: code, AttemptID: attempt,
		CreatedAt: created, UpdatedAt: created.Add(time.Second), DeadlineAt: created.Add(ProbeCleanupLifetime),
	}
}

func TestProbeCleanupValidTable(t *testing.T) {
	const a = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	for _, tc := range []struct {
		state, code, attempt string
		expired, want        bool
	}{
		{ProbeCleanupQueued, "", "", false, true},
		{ProbeCleanupQueued, "", a, false, false},
		{ProbeCleanupRunning, "", a, false, true},
		{ProbeCleanupRunning, "", "", false, false},
		{ProbeCleanupCleaned, "", a, false, true},
		{ProbeCleanupCleaned, "not_started", a, false, false},
		{ProbeCleanupFailed, "not_started", a, false, true},
		{ProbeCleanupFailed, "resource_conflict", a, false, false},
		{ProbeCleanupInterrupted, "owner_interrupted", "", false, true},
		{ProbeCleanupInterrupted, "owner_interrupted", a, false, false},
		{ProbeCleanupInterrupted, "outcome_unknown", a, false, true},
		{ProbeCleanupInterrupted, "outcome_unknown", "", false, false},
		{ProbeCleanupInterrupted, "deadline_expired", "", true, true},
		{ProbeCleanupInterrupted, "deadline_expired", a, true, true},
		{ProbeCleanupInterrupted, "deadline_expired", a, false, false},
		{ProbeCleanupRunning, "", a, true, false},
		{"canceled", "canceled", a, false, false},
	} {
		c := cleanupFixture(tc.state, tc.code, tc.attempt)
		if tc.expired {
			c.UpdatedAt = c.DeadlineAt
		}
		if got := c.Valid(); got != tc.want {
			t.Fatalf("%s/%s attempt=%q expired=%v: Valid=%v, want %v", tc.state, tc.code, tc.attempt, tc.expired, got, tc.want)
		}
	}
	bad := cleanupFixture(ProbeCleanupQueued, "", "")
	bad.DeadlineAt = bad.DeadlineAt.Add(time.Second)
	if bad.Valid() {
		t.Fatal("a deadline other than created+lifetime was accepted")
	}
	docker := cleanupFixture(ProbeCleanupQueued, "", "")
	docker.Identity.Install.Mode = ProbeInstallDocker
	if docker.Valid() {
		t.Fatal("a non-systemd install was accepted")
	}
}

func TestProbeCleanupIntentAndEligibility(t *testing.T) {
	c := cleanupFixture(ProbeCleanupRunning, "", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	intent := c.Identity.Intent(c.OwnerID, c.AttemptID)
	spec, ok := intent.Resource()
	if !intent.Valid() || !ok || spec.Reference != "uptime-phoenix-probe-"+c.Identity.Install.OperationID || intent.MarkerSHA256() == "" {
		t.Fatalf("intent %+v spec %+v", intent, spec)
	}
	for state, want := range map[string]bool{
		ProbeInstallResourcesReady: true, ProbeInstallFailed: true, ProbeInstallCanceled: true, ProbeInstallInterrupted: true,
		ProbeInstallQueued: false, ProbeInstallRunning: false, ProbeInstallCancelRequested: false,
	} {
		if ProbeInstallCleanupEligible(state) != want {
			t.Fatalf("eligibility of %s", state)
		}
	}
	if ProbeCleanupResourceName(ProbeInstallIdentity{}) != "" {
		t.Fatal("named an invalid install")
	}
}
