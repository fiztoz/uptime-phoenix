package domain

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestM8PreflightIdentityAndOutcomes(t *testing.T) {
	pin := "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	for _, invalid := range []string{"", strings.ToLower(pin), pin + "=", pin + "x", "SHA256:short"} {
		if ValidSSHKeyFingerprint(invalid) {
			t.Fatal("noncanonical fingerprint accepted")
		}
	}
	now := time.Now().UTC()
	op := ProbePreflightOperation{Identity: ProbePreflightIdentity{OperationID: "11111111-1111-4111-8111-111111111111", Host: ProbeSSHHost{Address: "edge.example:22", User: "deploy"}, HostKeyFingerprint: pin, ClientKeyFingerprint: pin, RequestedBy: 7}, OwnerID: "22222222-2222-4222-8222-222222222222", State: ProbePreflightQueued, CreatedAt: now, UpdatedAt: now, DeadlineAt: now.Add(ProbePreflightLifetime)}
	if !op.Valid() {
		t.Fatal("valid queued receipt refused")
	}
	changed := op
	changed.DeadlineAt = changed.DeadlineAt.Add(time.Microsecond)
	if changed.Valid() {
		t.Fatal("corrupt/renewed deadline accepted")
	}
	changed = op
	changed.State = ProbePreflightPassed
	if changed.Valid() {
		t.Fatal("success without compatible facts accepted")
	}
	changed.Result = &ProbeHostPreflight{OS: "linux", Architecture: "amd64", Systemd: true, Privilege: "root"}
	if !changed.Valid() {
		t.Fatal("compatible preflight receipt refused")
	}
	changed.Result.Architecture = "other"
	if changed.Valid() {
		t.Fatal("unsupported result accepted")
	}
	for _, outcome := range []ProbePreflightOutcome{{State: ProbePreflightFailed, ErrorCode: "raw diagnostics"}, {State: ProbePreflightCanceled}, {State: ProbePreflightInterrupted, ErrorCode: "canceled"}, {State: ProbePreflightRunning}} {
		if outcome.Valid() {
			t.Fatal("invalid terminal outcome accepted")
		}
	}
}
