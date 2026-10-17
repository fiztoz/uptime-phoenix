package domain

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"
)

func installTestFingerprint() string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
}

// installTestAttempt returns one canonical attempt UUID per ordinal.
func installTestAttempt(n int) string {
	return fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa%02x", n)
}

func installTestIdentity(mode string) ProbeInstallIdentity {
	identity := ProbeInstallIdentity{
		OperationID:          "11111111-1111-4111-8111-111111111111",
		Host:                 ProbeSSHHost{Address: "edge.example:22", User: "root"},
		HostKeyFingerprint:   installTestFingerprint(),
		ClientKeyFingerprint: installTestFingerprint(),
		RequestedBy:          7,
		LayoutVersion:        ProbeInstallLayoutVersion,
		Mode:                 mode,
	}
	if mode == ProbeInstallSystemd {
		identity.Binary = ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: strings.Repeat("a", 64), Size: 100}
	} else {
		identity.Image = ProbeImageSelection{
			Pin:            ProbeImagePin{Repository: "ghcr.io/example/probe", Version: "1.2.3", Architecture: "amd64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 100},
			ManifestSHA256: strings.Repeat("b", 64),
			ConfigSHA256:   strings.Repeat("c", 64),
			LayerCount:     1,
		}
	}
	return identity
}

// installTestTime returns microsecond-precision UTC test stamps.
func installTestTime(offset time.Duration) time.Time {
	return time.Date(2026, 3, 4, 5, 6, 7, 123456000, time.UTC).Add(offset)
}

func installTestOperation(mode string) ProbeInstallOperation {
	identity := installTestIdentity(mode)
	created := installTestTime(0)
	operation := ProbeInstallOperation{
		Identity:   identity,
		OwnerID:    "22222222-2222-4222-8222-222222222222",
		State:      ProbeInstallQueued,
		CreatedAt:  created,
		UpdatedAt:  created,
		DeadlineAt: created.Add(ProbeInstallLifetime),
	}
	specs := identity.Resources()
	operation.Resources = make([]ProbeInstallResource, len(specs))
	for i, spec := range specs {
		operation.Resources[i] = ProbeInstallResource{Spec: spec, State: ProbeInstallResourcePlanned}
	}
	return operation
}

func TestProbeInstallIdentityAndLayouts(t *testing.T) {
	for _, mode := range []string{ProbeInstallSystemd, ProbeInstallDocker} {
		identity := installTestIdentity(mode)
		if !identity.Valid() {
			t.Fatalf("%s identity refused", mode)
		}
		specs := identity.Resources()
		if len(specs) == 0 {
			t.Fatalf("%s layout missing", mode)
		}
		for i, spec := range specs {
			if spec.Ordinal != i+1 || spec.Kind == "" || spec.Reference == "" {
				t.Fatalf("%s layout entry %d malformed: %+v", mode, i, spec)
			}
		}
		if mode == ProbeInstallSystemd {
			base := "uptime-phoenix-probe-" + identity.OperationID
			want := []ProbeInstallResourceSpec{
				{Ordinal: 1, Kind: "data_directory", Reference: "/var/lib/" + base, Owned: true},
				{Ordinal: 2, Kind: "artifact_directory", Reference: "/opt/" + base, Owned: true},
				{Ordinal: 3, Kind: "unit_file", Reference: "/etc/systemd/system/" + base + ".service", Owned: true},
				{Ordinal: 4, Kind: "service", Reference: base + ".service", Owned: true},
			}
			if len(specs) != len(want) {
				t.Fatalf("systemd layout size %d", len(specs))
			}
			for i := range want {
				if specs[i] != want[i] {
					t.Fatalf("systemd layout %d = %+v, want %+v", i, specs[i], want[i])
				}
			}
		} else {
			base := "uptime-phoenix-probe-" + identity.OperationID
			want := []ProbeInstallResourceSpec{
				{Ordinal: 1, Kind: "image_cache", Reference: identity.Image.Reference(), Owned: false},
				{Ordinal: 2, Kind: "data_directory", Reference: "/var/lib/" + base, Owned: true},
				{Ordinal: 3, Kind: "container", Reference: base, Owned: true},
			}
			if len(specs) != len(want) {
				t.Fatalf("docker layout size %d", len(specs))
			}
			for i := range want {
				if specs[i] != want[i] {
					t.Fatalf("docker layout %d = %+v, want %+v", i, specs[i], want[i])
				}
			}
			if want[0].Reference == "" || !strings.Contains(want[0].Reference, "@sha256:"+identity.Image.ManifestSHA256) {
				t.Fatalf("docker image cache is not the exact manifest reference: %q", want[0].Reference)
			}
		}
	}
	// One complete artifact alternative only: the unused one is exactly zero.
	systemd := installTestIdentity(ProbeInstallSystemd)
	systemd.Image = installTestIdentity(ProbeInstallDocker).Image
	if systemd.Valid() || systemd.Resources() != nil {
		t.Fatal("systemd identity accepted a hidden image selection")
	}
	docker := installTestIdentity(ProbeInstallDocker)
	docker.Binary = installTestIdentity(ProbeInstallSystemd).Binary
	if docker.Valid() || docker.Resources() != nil {
		t.Fatal("docker identity accepted a hidden binary pin")
	}
	for _, mutate := range []func(*ProbeInstallIdentity){
		func(r *ProbeInstallIdentity) { r.Mode = "" },
		func(r *ProbeInstallIdentity) { r.Mode = "podman" },
		func(r *ProbeInstallIdentity) { r.LayoutVersion = ProbeInstallLayoutVersion + 1 },
		func(r *ProbeInstallIdentity) { r.RequestedBy = 0 },
		func(r *ProbeInstallIdentity) { r.OperationID = "00000000-0000-0000-0000-000000000000" },
		func(r *ProbeInstallIdentity) { r.HostKeyFingerprint = "SHA256:short" },
		func(r *ProbeInstallIdentity) { r.Host = ProbeSSHHost{} },
	} {
		identity := installTestIdentity(ProbeInstallSystemd)
		mutate(&identity)
		if identity.Valid() || identity.Resources() != nil {
			t.Fatalf("mutated identity accepted: %+v", identity)
		}
	}
	invalid := installTestIdentity(ProbeInstallSystemd)
	invalid.Mode = "bogus"
	if invalid.Resources() != nil {
		t.Fatal("invalid identity derived a resource graph")
	}
}

func TestProbeInstallGraphTimestampsAndCausality(t *testing.T) {
	valid := installTestOperation(ProbeInstallSystemd)
	if !valid.Valid() {
		t.Fatal("valid queued graph refused")
	}
	for _, mutate := range []func(*ProbeInstallOperation){
		func(o *ProbeInstallOperation) { o.CreatedAt = time.Time{} },
		func(o *ProbeInstallOperation) { o.CreatedAt = installTestTime(0).In(time.Local) },
		func(o *ProbeInstallOperation) { o.CreatedAt = installTestTime(0).Add(time.Nanosecond) },
		func(o *ProbeInstallOperation) { o.UpdatedAt = installTestTime(0).Add(-time.Microsecond) },
		func(o *ProbeInstallOperation) {
			o.DeadlineAt = installTestTime(ProbeInstallLifetime - time.Microsecond)
		},
	} {
		operation := installTestOperation(ProbeInstallSystemd)
		mutate(&operation)
		if operation.Valid() {
			t.Fatalf("timestamp corruption accepted: %+v", operation)
		}
	}
	// One resource in flight: causality of the single active attempt.
	running := installTestOperation(ProbeInstallSystemd)
	running.State = ProbeInstallRunning
	running.UpdatedAt = installTestTime(time.Minute)
	running.Resources[0].State = ProbeInstallResourceInFlight
	running.Resources[0].AttemptID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	running.Resources[0].StartedAt = installTestTime(time.Second)
	if !running.Valid() {
		t.Fatal("valid running graph refused")
	}
	for _, mutate := range []func(*ProbeInstallOperation){
		func(o *ProbeInstallOperation) { o.Resources[0].StartedAt = installTestTime(0).Add(-time.Microsecond) },
		func(o *ProbeInstallOperation) { o.Resources[0].StartedAt = o.DeadlineAt },
		func(o *ProbeInstallOperation) { o.Resources[0].StartedAt = o.UpdatedAt.Add(time.Microsecond) },
		func(o *ProbeInstallOperation) {
			o.Resources[0].StartedAt = o.Resources[0].StartedAt.Add(time.Nanosecond)
		},
		func(o *ProbeInstallOperation) { o.Resources[0].FinishedAt = installTestTime(time.Second) },
		func(o *ProbeInstallOperation) { o.Resources[0].ErrorCode = "outcome_unknown" },
	} {
		operation := running
		operation.Resources = append([]ProbeInstallResource(nil), running.Resources...)
		mutate(&operation)
		if operation.Valid() {
			t.Fatalf("attempt causality corruption accepted: %+v", operation.Resources[0])
		}
	}
	// Finished attempts: applied must finish before the deadline and inside
	// the updated window; successors may not start before their predecessor
	// finished.
	applied := installTestOperation(ProbeInstallSystemd)
	applied.State = ProbeInstallResourcesReady
	applied.UpdatedAt = installTestTime(10 * time.Second)
	for i := range applied.Resources {
		applied.Resources[i] = ProbeInstallResource{
			Spec:       applied.Resources[i].Spec,
			State:      ProbeInstallResourceApplied,
			AttemptID:  installTestAttempt(i),
			StartedAt:  installTestTime(time.Duration(2*i+1) * time.Second),
			FinishedAt: installTestTime(time.Duration(2*i+2) * time.Second),
		}
	}
	if !applied.Valid() {
		t.Fatal("valid resources_ready graph refused")
	}
	for _, mutate := range []func(*ProbeInstallOperation){
		func(o *ProbeInstallOperation) {
			o.Resources[1].StartedAt = o.Resources[0].FinishedAt.Add(-time.Microsecond)
		},
		func(o *ProbeInstallOperation) {
			o.Resources[0].FinishedAt = o.Resources[0].StartedAt.Add(-time.Microsecond)
		},
		func(o *ProbeInstallOperation) { o.Resources[0].FinishedAt = o.UpdatedAt.Add(time.Microsecond) },
		func(o *ProbeInstallOperation) { o.Resources[0].FinishedAt = o.DeadlineAt },
		func(o *ProbeInstallOperation) { o.Resources[1].AttemptID = o.Resources[0].AttemptID },
		func(o *ProbeInstallOperation) { o.Resources[0].ErrorCode = "internal" },
	} {
		operation := applied
		operation.Resources = append([]ProbeInstallResource(nil), applied.Resources...)
		mutate(&operation)
		if operation.Valid() {
			t.Fatalf("finished attempt corruption accepted: %+v", operation.Resources[0])
		}
	}
}

func TestProbeInstallGraphShapeAndOrdering(t *testing.T) {
	base := installTestOperation(ProbeInstallSystemd)
	truncated := base
	truncated.Resources = base.Resources[:len(base.Resources)-1]
	if truncated.Valid() {
		t.Fatal("truncated graph accepted")
	}
	reordered := base
	reordered.Resources = append([]ProbeInstallResource(nil), base.Resources...)
	reordered.Resources[0], reordered.Resources[1] = reordered.Resources[1], reordered.Resources[0]
	if reordered.Valid() {
		t.Fatal("reordered graph accepted")
	}
	wrongSpec := base
	wrongSpec.Resources = append([]ProbeInstallResource(nil), base.Resources...)
	wrongSpec.Resources[0].Spec.Ordinal = 2
	if wrongSpec.Valid() {
		t.Fatal("wrong spec ordinal accepted")
	}
	residue := base
	residue.Resources = append([]ProbeInstallResource(nil), base.Resources...)
	residue.Resources[0].AttemptID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	if residue.Valid() {
		t.Fatal("planned resource with attempt residue accepted")
	}
	residue2 := base
	residue2.Resources = append([]ProbeInstallResource(nil), base.Resources...)
	residue2.Resources[0].StartedAt = installTestTime(time.Second)
	if residue2.Valid() {
		t.Fatal("planned resource with start residue accepted")
	}
	// The graph admits only one frontier: applied prefix, at most one active
	// or finished-non-applied attempt, then planned tail.
	frontier := installTestOperation(ProbeInstallSystemd)
	frontier.State = ProbeInstallRunning
	frontier.UpdatedAt = installTestTime(time.Minute)
	frontier.Resources[0].State = ProbeInstallResourceInFlight
	frontier.Resources[0].AttemptID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	frontier.Resources[0].StartedAt = installTestTime(time.Second)
	frontier.Resources[1].State = ProbeInstallResourceApplied
	frontier.Resources[1].AttemptID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	frontier.Resources[1].StartedAt = installTestTime(2 * time.Second)
	frontier.Resources[1].FinishedAt = installTestTime(3 * time.Second)
	if frontier.Valid() {
		t.Fatal("applied resource behind an open frontier accepted")
	}
	twoOpen := installTestOperation(ProbeInstallSystemd)
	twoOpen.State = ProbeInstallRunning
	twoOpen.UpdatedAt = installTestTime(time.Minute)
	for i, attempt := range []string{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"} {
		twoOpen.Resources[i].State = ProbeInstallResourceInFlight
		twoOpen.Resources[i].AttemptID = attempt
		twoOpen.Resources[i].StartedAt = installTestTime(time.Duration(i+1) * time.Second)
	}
	if twoOpen.Valid() {
		t.Fatal("two open attempts accepted")
	}
	unknownThenOpen := installTestOperation(ProbeInstallSystemd)
	unknownThenOpen.State = ProbeInstallInterrupted
	unknownThenOpen.ErrorCode = "owner_interrupted"
	unknownThenOpen.UpdatedAt = installTestTime(time.Minute)
	unknownThenOpen.Resources[0].State = ProbeInstallResourceUnknown
	unknownThenOpen.Resources[0].AttemptID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	unknownThenOpen.Resources[0].ErrorCode = "owner_interrupted"
	unknownThenOpen.Resources[0].StartedAt = installTestTime(time.Second)
	unknownThenOpen.Resources[0].FinishedAt = installTestTime(2 * time.Second)
	unknownThenOpen.Resources[1].State = ProbeInstallResourceInFlight
	unknownThenOpen.Resources[1].AttemptID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	unknownThenOpen.Resources[1].StartedAt = installTestTime(3 * time.Second)
	if unknownThenOpen.Valid() {
		t.Fatal("open attempt after an uncertain attempt accepted")
	}
	// Not started means the mutation was genuinely never invoked: the rest of
	// the graph stays planned and the attempt reports its fixed code.
	notStarted := installTestOperation(ProbeInstallSystemd)
	notStarted.State = ProbeInstallFailed
	notStarted.ErrorCode = "not_started"
	notStarted.UpdatedAt = installTestTime(time.Minute)
	notStarted.Resources[0].State = ProbeInstallResourceNotStarted
	notStarted.Resources[0].AttemptID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	notStarted.Resources[0].ErrorCode = "not_started"
	notStarted.Resources[0].StartedAt = installTestTime(time.Second)
	notStarted.Resources[0].FinishedAt = installTestTime(2 * time.Second)
	if !notStarted.Valid() {
		t.Fatal("valid not_started failure refused")
	}
	badCode := notStarted
	badCode.Resources = append([]ProbeInstallResource(nil), notStarted.Resources...)
	badCode.Resources[0].ErrorCode = "execution_failed"
	if badCode.Valid() {
		t.Fatal("not_started attempt with a foreign code accepted")
	}
}

func TestProbeInstallStatesOutcomesAndNoInstalledSuccess(t *testing.T) {
	// Outcomes: only unsuccessful terminal shapes, never installed success.
	for _, outcome := range []ProbeInstallOutcome{
		{State: ProbeInstallFailed, ErrorCode: "unsupported_host"},
		{State: ProbeInstallFailed, ErrorCode: "artifact_rejected"},
		{State: ProbeInstallFailed, ErrorCode: "resource_conflict"},
		{State: ProbeInstallFailed, ErrorCode: "execution_failed"},
		{State: ProbeInstallFailed, ErrorCode: "permission_denied"},
		{State: ProbeInstallFailed, ErrorCode: "runtime_unverified"},
		{State: ProbeInstallFailed, ErrorCode: "not_started"},
		{State: ProbeInstallFailed, ErrorCode: "internal"},
		{State: ProbeInstallCanceled, ErrorCode: "canceled"},
		{State: ProbeInstallInterrupted, ErrorCode: "owner_interrupted"},
	} {
		if !outcome.Valid() {
			t.Fatalf("public outcome refused: %+v", outcome)
		}
	}
	for _, outcome := range []ProbeInstallOutcome{
		{State: ProbeInstallQueued},
		{State: ProbeInstallRunning},
		{State: ProbeInstallResourcesReady},
		{State: ProbeInstallCancelRequested},
		{State: "installed"},
		{State: "succeeded", ErrorCode: "ok"},
		{State: ProbeInstallFailed, ErrorCode: "installed"},
		{State: ProbeInstallFailed, ErrorCode: ""},
		{State: ProbeInstallFailed, ErrorCode: "remote: Permission denied (publickey)"},
		{State: ProbeInstallCanceled, ErrorCode: ""},
		{State: ProbeInstallInterrupted, ErrorCode: "deadline_expired"},
		{State: ProbeInstallInterrupted, ErrorCode: "outcome_unknown"},
		{State: ProbeInstallInterrupted, ErrorCode: ""},
	} {
		if outcome.Valid() {
			t.Fatalf("success or malformed outcome accepted: %+v", outcome)
		}
	}
	// Resource outcomes: three joined report shapes only.
	for _, outcome := range []ProbeInstallResourceOutcome{
		{State: ProbeInstallResourceApplied, ErrorCode: ""},
		{State: ProbeInstallResourceNotStarted, ErrorCode: "not_started"},
		{State: ProbeInstallResourceUnknown, ErrorCode: "outcome_unknown"},
	} {
		if !outcome.Valid() {
			t.Fatalf("joined resource report refused: %+v", outcome)
		}
	}
	for _, outcome := range []ProbeInstallResourceOutcome{
		{State: ProbeInstallResourcePlanned},
		{State: ProbeInstallResourceInFlight},
		{State: ProbeInstallResourceApplied, ErrorCode: "internal"},
		{State: ProbeInstallResourceNotStarted, ErrorCode: ""},
		{State: ProbeInstallResourceUnknown, ErrorCode: "deadline_expired"},
		{State: ProbeInstallResourceUnknown, ErrorCode: "raw remote stderr"},
	} {
		if outcome.Valid() {
			t.Fatalf("malformed resource report accepted: %+v", outcome)
		}
	}
	// State machine: preparation progress is never terminal success.
	for _, state := range []string{ProbeInstallQueued, ProbeInstallRunning, ProbeInstallResourcesReady, ProbeInstallCancelRequested} {
		operation := installTestOperation(ProbeInstallSystemd)
		operation.State = state
		switch state {
		case ProbeInstallQueued:
		case ProbeInstallRunning:
			operation.UpdatedAt = installTestTime(time.Minute)
			operation.Resources[0].State = ProbeInstallResourceInFlight
			operation.Resources[0].AttemptID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
			operation.Resources[0].StartedAt = installTestTime(time.Second)
		case ProbeInstallResourcesReady:
			operation.UpdatedAt = installTestTime(10 * time.Second)
			for i := range operation.Resources {
				operation.Resources[i] = ProbeInstallResource{
					Spec:       operation.Resources[i].Spec,
					State:      ProbeInstallResourceApplied,
					AttemptID:  installTestAttempt(i),
					StartedAt:  installTestTime(time.Duration(2*i+1) * time.Second),
					FinishedAt: installTestTime(time.Duration(2*i+2) * time.Second),
				}
			}
		case ProbeInstallCancelRequested:
			operation.UpdatedAt = installTestTime(time.Minute)
		}
		if !operation.Valid() {
			t.Fatalf("valid %s graph refused", state)
		}
		if operation.Terminal() {
			t.Fatalf("%s reported terminal (installed-success leak)", state)
		}
	}
	terminal := installTestOperation(ProbeInstallSystemd)
	terminal.State = ProbeInstallCanceled
	terminal.ErrorCode = "canceled"
	terminal.UpdatedAt = installTestTime(time.Minute)
	if !terminal.Valid() || !terminal.Terminal() {
		t.Fatal("valid canceled operation refused")
	}
	for _, mutate := range []func(*ProbeInstallOperation){
		func(o *ProbeInstallOperation) { o.ErrorCode = "" },
		func(o *ProbeInstallOperation) { o.State = ProbeInstallFailed },
	} {
		operation := terminal
		mutate(&operation)
		if operation.Valid() {
			t.Fatalf("broken terminal accepted: %+v", operation)
		}
	}
	// Unknown outcomes interrupt and must echo the storage transition code.
	unknown := installTestOperation(ProbeInstallDocker)
	unknown.State = ProbeInstallInterrupted
	unknown.ErrorCode = "outcome_unknown"
	unknown.UpdatedAt = installTestTime(time.Minute)
	unknown.Resources[0].State = ProbeInstallResourceUnknown
	unknown.Resources[0].AttemptID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	unknown.Resources[0].ErrorCode = "outcome_unknown"
	unknown.Resources[0].StartedAt = installTestTime(time.Second)
	unknown.Resources[0].FinishedAt = installTestTime(2 * time.Second)
	if !unknown.Valid() || !unknown.Terminal() {
		t.Fatal("valid outcome_unknown interruption refused")
	}
	mismatch := unknown
	mismatch.Resources = append([]ProbeInstallResource(nil), unknown.Resources...)
	mismatch.Resources[0].ErrorCode = "owner_interrupted"
	if mismatch.Valid() {
		t.Fatal("unknown attempt code disagreeing with the operation accepted")
	}
	runningUnknown := unknown
	runningUnknown.State = ProbeInstallRunning
	runningUnknown.ErrorCode = ""
	if runningUnknown.Valid() {
		t.Fatal("running operation with an uncertain attempt accepted")
	}
	// Cold expiry records deadline_expired after the deadline window.
	expired := installTestOperation(ProbeInstallSystemd)
	expired.State = ProbeInstallInterrupted
	expired.ErrorCode = "deadline_expired"
	expired.UpdatedAt = expired.DeadlineAt
	expired.Resources[0].State = ProbeInstallResourceUnknown
	expired.Resources[0].AttemptID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	expired.Resources[0].ErrorCode = "deadline_expired"
	expired.Resources[0].StartedAt = installTestTime(time.Second)
	expired.Resources[0].FinishedAt = expired.DeadlineAt
	if !expired.Valid() {
		t.Fatal("valid deadline_expired interruption refused")
	}
	forged := expired
	forged.UpdatedAt = installTestTime(time.Minute)
	forged.Resources = append([]ProbeInstallResource(nil), expired.Resources...)
	forged.Resources[0].FinishedAt = installTestTime(2 * time.Second)
	if forged.Valid() {
		t.Fatal("deadline_expired recorded inside the live window accepted")
	}
}
