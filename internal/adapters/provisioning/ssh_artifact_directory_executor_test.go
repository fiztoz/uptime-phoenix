package provisioning

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// These tests drive SSHArtifactDirectoryExecutor through the real loopback SSH
// protocol fixture (ssh_binary_stage_send_test.go) and the authorized-helper
// emulation plus fake authorizer harness (ssh_binary_stage_authorized_send_test.go).
// The remote side is protocol evidence only: no OpenSSH, MariaDB, installed
// helper or privileged service is consulted, and every key is generated per
// test. They do not establish InnoDB lease or fencing behavior, which is the
// authorizer's concern and is exercised elsewhere.

// Independent literals: a changed constant must not silently change what these
// tests accept.
const (
	adExecKindLiteral    = "artifact_directory"
	adExecDataAttemptID  = "66666666-6666-4666-8666-666666666666"
	adExecOtherAttemptID = "55555555-5555-4555-8555-555555555555"
	adExecOtherOwnerID   = "77777777-7777-4777-8777-777777777777"
	adExecOtherOperation = "44444444-4444-4444-8444-444444444444"
)

var (
	adExecApplied    = domain.ProbeInstallResourceOutcome{State: "applied", ErrorCode: ""}
	adExecNotStarted = domain.ProbeInstallResourceOutcome{State: "not_started", ErrorCode: "not_started"}
	adExecUnknown    = domain.ProbeInstallResourceOutcome{State: "unknown", ErrorCode: "outcome_unknown"}
)

// adExecOperation builds the operation receipt the journal hands the executor
// after acknowledging the artifact_directory reservation: running, the data
// directory applied, the artifact directory in flight with the intent's
// attempt, and every later resource still planned.
func adExecOperation(t *testing.T, intent domain.ProbeBinaryStageIntent) domain.ProbeInstallOperation {
	t.Helper()
	specs := intent.Identity.Resources()
	if len(specs) != 4 || specs[0].Kind != "data_directory" || specs[1].Kind != adExecKindLiteral {
		t.Fatalf("fixture identity resource layout %+v", specs)
	}
	resources := make([]domain.ProbeInstallResource, len(specs))
	for i, spec := range specs {
		resources[i] = domain.ProbeInstallResource{Spec: spec, State: domain.ProbeInstallResourcePlanned}
	}
	resources[0].State = domain.ProbeInstallResourceApplied
	resources[0].AttemptID = adExecDataAttemptID
	resources[1].State = domain.ProbeInstallResourceInFlight
	resources[1].AttemptID = intent.AttemptID
	return domain.ProbeInstallOperation{
		Identity:  intent.Identity,
		OwnerID:   intent.OwnerID,
		State:     domain.ProbeInstallRunning,
		Resources: resources,
	}
}

func adExecNew(t *testing.T, h *authSendHarness) *SSHArtifactDirectoryExecutor {
	t.Helper()
	e, err := NewSSHArtifactDirectoryExecutor(h.sender, h.binary, h.auth)
	if err != nil || e == nil {
		t.Fatalf("constructor %v %v", e, err)
	}
	return e
}

type adExecResult struct {
	out domain.ProbeInstallResourceOutcome
	err error
}

func adExecApplyAsync(ctx context.Context, e ports.ProbeInstallResourceExecutor, op domain.ProbeInstallOperation, kind string) <-chan adExecResult {
	done := make(chan adExecResult, 1)
	go func() {
		out, err := e.ApplyResource(ctx, op, kind)
		done <- adExecResult{out: out, err: err}
	}()
	return done
}

// adExecAwait is the single place every outcome is judged for the port
// contract: a nil error and a report an executor is allowed to submit.
func adExecAwait(t *testing.T, done <-chan adExecResult) domain.ProbeInstallResourceOutcome {
	t.Helper()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("ApplyResource returned error %v; it must never return one", res.err)
		}
		if !res.out.Valid() {
			t.Fatalf("outcome %+v is not a valid executor report", res.out)
		}
		return res.out
	case <-time.After(30 * time.Second):
		t.Fatal("ApplyResource did not return")
		return domain.ProbeInstallResourceOutcome{}
	}
}

func adExecApply(t *testing.T, ctx context.Context, e ports.ProbeInstallResourceExecutor, op domain.ProbeInstallOperation, kind string) domain.ProbeInstallResourceOutcome {
	t.Helper()
	return adExecAwait(t, adExecApplyAsync(ctx, e, op, kind))
}

func adExecExpectBlocked(t *testing.T, done <-chan adExecResult, what string) {
	t.Helper()
	select {
	case res := <-done:
		t.Fatalf("%s returned early: %+v %v", what, res.out, res.err)
	case <-time.After(250 * time.Millisecond):
	}
}

func adExecRequireOutcome(t *testing.T, got, want domain.ProbeInstallResourceOutcome) {
	t.Helper()
	if got != want {
		t.Fatalf("outcome %+v, want %+v", got, want)
	}
}

// adExecRequireNoIO asserts the refusal happened before anything observable:
// no connection, no SSH authentication, no exec, no stdin, no authorization,
// and the payload source was never even opened.
func adExecRequireNoIO(t *testing.T, h *authSendHarness) {
	t.Helper()
	if conn, auth, exec := h.f.connCalls.Load(), h.f.authCalls.Load(), h.f.execCalls.Load(); conn != 0 || auth != 0 || exec != 0 {
		t.Fatalf("refusal performed I/O: conn %d auth %d exec %d", conn, auth, exec)
	}
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("server received %d stdin bytes, want 0", len(got))
	}
	if h.auth.calls.Load() != 0 {
		t.Fatalf("authorizer reached %d times, want 0", h.auth.calls.Load())
	}
	if h.binary.opens.Load() != 0 || h.reads.Load() != 0 || h.closes.Load() != 0 {
		t.Fatalf("payload source touched: opens %d reads %d closes %d", h.binary.opens.Load(), h.reads.Load(), h.closes.Load())
	}
	if h.binary.snapshotCloses.Load() != 0 {
		t.Fatalf("borrowed snapshot closed %d times", h.binary.snapshotCloses.Load())
	}
}

// adExecRequireNoStageInput is the weaker pre-stdin evidence for failures that
// legitimately open the source or the TCP connection first (trust, connect):
// no exec, no stdin, no authorization, no payload read, owned readers closed.
func adExecRequireNoStageInput(t *testing.T, h *authSendHarness) {
	t.Helper()
	if exec := h.f.execCalls.Load(); exec != 0 {
		t.Fatalf("exec started: %d", exec)
	}
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("server received %d stdin bytes, want 0", len(got))
	}
	if h.auth.calls.Load() != 0 {
		t.Fatalf("authorizer reached %d times, want 0", h.auth.calls.Load())
	}
	if h.reads.Load() != 0 {
		t.Fatalf("payload source read %d times", h.reads.Load())
	}
	if h.closes.Load() != h.binary.opens.Load() {
		t.Fatalf("owned reader closes %d, opens %d", h.closes.Load(), h.binary.opens.Load())
	}
	if h.binary.snapshotCloses.Load() != 0 {
		t.Fatalf("borrowed snapshot closed %d times", h.binary.snapshotCloses.Load())
	}
}

func adExecWaitStdinEOF(t *testing.T, f *sendTFixture) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !f.stdinEOFSeen() {
		if time.Now().After(deadline) {
			t.Fatal("helper never saw the end of the stage input")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSSHArtifactDirectoryExecutorConstructorRefusesNilCollaborators(t *testing.T) {
	h := newAuthSendHarness(t, nil)
	var nilSender *SSHBinaryStageSender
	var typedNilBinary *sendTFakeBinary
	var typedNilAuthorizer *authSendAuthorizer
	var typedNilFuncAuthorizer authSendFuncAuthorizer
	cases := []struct {
		name       string
		sender     *SSHBinaryStageSender
		binary     ports.PreparedProbeBinary
		authorizer ProbeBinaryStageAuthorizer
	}{
		{name: "nil sender", sender: nilSender, binary: h.binary, authorizer: h.auth},
		{name: "untyped nil binary", sender: h.sender, binary: nil, authorizer: h.auth},
		{name: "typed nil binary", sender: h.sender, binary: typedNilBinary, authorizer: h.auth},
		{name: "untyped nil authorizer", sender: h.sender, binary: h.binary, authorizer: nil},
		{name: "typed nil pointer authorizer", sender: h.sender, binary: h.binary, authorizer: typedNilAuthorizer},
		{name: "typed nil func authorizer", sender: h.sender, binary: h.binary, authorizer: typedNilFuncAuthorizer},
		{name: "everything nil", sender: nil, binary: nil, authorizer: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := NewSSHArtifactDirectoryExecutor(tc.sender, tc.binary, tc.authorizer)
			if err != ErrSSHBinaryStageInput || e != nil {
				t.Fatalf("%s: %v %v, want nil executor and ErrSSHBinaryStageInput", tc.name, e, err)
			}
		})
	}

	e, err := NewSSHArtifactDirectoryExecutor(h.sender, h.binary, h.auth)
	if err != nil || e == nil {
		t.Fatalf("valid collaborators refused: %v %v", e, err)
	}
	var port ports.ProbeInstallResourceExecutor = e
	if port == nil {
		t.Fatal("executor does not satisfy the port")
	}
	// Construction performs no I/O of any kind.
	adExecRequireNoIO(t, h)
}

func TestSSHArtifactDirectoryExecutorAppliesAuthorizedStage(t *testing.T) {
	h := newAuthSendHarness(t, nil)
	e := adExecNew(t, h)
	var port ports.ProbeInstallResourceExecutor = e
	op := adExecOperation(t, h.intent)
	before := slices.Clone(op.Resources)

	got := adExecAwait(t, adExecApplyAsync(t.Context(), port, op, "artifact_directory"))
	adExecRequireOutcome(t, got, adExecApplied)
	if got.ErrorCode != "" {
		t.Fatalf("applied carries error code %q", got.ErrorCode)
	}
	h.helper.waitIdle(t, h.f)

	if got := h.f.requestTypes(); !slices.Equal(got, []string{"exec"}) {
		t.Fatalf("session requests %v", got)
	}
	if got := h.f.execCommands(); !slices.Equal(got, []string{authSendCommandLiteral}) {
		t.Fatalf("exec commands %v", got)
	}
	if h.f.connCalls.Load() != 1 {
		t.Fatalf("connections = %d, want 1", h.f.connCalls.Load())
	}

	// Exactly one authorization, for the request rebuilt from the receipt and
	// the helper's own challenge, before any stdin byte existed.
	if h.auth.calls.Load() != 1 {
		t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
	}
	if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0] != h.wantRequest() {
		t.Fatalf("authorizer request %+v, want %+v", requests, h.wantRequest())
	}
	h.auth.mu.Lock()
	stdinAtCall := slices.Clone(h.auth.stdinAtCall)
	h.auth.mu.Unlock()
	if !slices.Equal(stdinAtCall, []int{0}) {
		t.Fatalf("stdin bytes visible when authorizer ran: %v, want [0]", stdinAtCall)
	}

	// Server stdin is the header, the 72-byte grant frame, then the exact binary.
	want := h.wantStdin(t, h.genuineSignature(), h.content)
	if gotStdin := h.f.stdinBytes(); !bytes.Equal(gotStdin, want) {
		t.Fatalf("server stdin = %d bytes, want %d", len(gotStdin), len(want))
	}
	if !h.f.stdinEOFSeen() {
		t.Fatal("missing stdin EOF")
	}
	if h.helper.verified.Load() != 1 {
		t.Fatalf("helper emulation verdict %d, want accepted grant", h.helper.verified.Load())
	}

	// The collaborators are borrowed: the snapshot is opened once, its owned
	// reader closed once, and the snapshot itself is never closed.
	if h.binary.opens.Load() != 1 || h.closes.Load() != 1 || h.reads.Load() < 1 {
		t.Fatalf("source opens %d closes %d reads %d", h.binary.opens.Load(), h.closes.Load(), h.reads.Load())
	}
	if h.binary.snapshotCloses.Load() != 0 {
		t.Fatalf("borrowed snapshot closed %d times", h.binary.snapshotCloses.Load())
	}
	if !slices.Equal(op.Resources, before) {
		t.Fatalf("operation receipt was mutated: %+v, was %+v", op.Resources, before)
	}
}

// The intent is rebuilt from the operation receipt, not from anything the
// harness knows: a different reserved attempt or owner is what reaches the
// authorizer and the wire. The helper emulation expects the fixture's own
// attempt and owner, so it rejects the stage, which is Unknown after stdin.
func TestSSHArtifactDirectoryExecutorRebuildsIntentFromReceipt(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(intent *domain.ProbeBinaryStageIntent)
	}{
		{name: "reserved attempt id", mutate: func(i *domain.ProbeBinaryStageIntent) { i.AttemptID = adExecOtherAttemptID }},
		{name: "owner id", mutate: func(i *domain.ProbeBinaryStageIntent) { i.OwnerID = adExecOtherOwnerID }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthSendHarness(t, nil)
			receiptIntent := h.intent
			tc.mutate(&receiptIntent)
			if receiptIntent == h.intent {
				t.Fatal("mutation did not change the intent")
			}
			e := adExecNew(t, h)
			got := adExecApply(t, t.Context(), e, adExecOperation(t, receiptIntent), "artifact_directory")
			adExecRequireOutcome(t, got, adExecUnknown)
			h.helper.waitIdle(t, h.f)

			wantRequest := h.wantRequest()
			wantRequest.Intent = receiptIntent
			wantRequest.Challenge.MarkerSHA256 = receiptIntent.MarkerSHA256()
			if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0] != wantRequest {
				t.Fatalf("authorizer request %+v, want %+v", requests, wantRequest)
			}
			header, err := EncodeProbeBinaryStageHeader(receiptIntent)
			if err != nil {
				t.Fatal(err)
			}
			stdin := h.f.stdinBytes()
			if !bytes.HasPrefix(stdin, header) {
				t.Fatalf("stage header is not the receipt's: %d stdin bytes", len(stdin))
			}
			if h.helper.verified.Load() != 2 {
				t.Fatalf("helper emulation verdict %d, want rejected", h.helper.verified.Load())
			}
		})
	}
}

func TestSSHArtifactDirectoryExecutorRefusesOtherKinds(t *testing.T) {
	h := newAuthSendHarness(t, nil)
	e := adExecNew(t, h)
	op := adExecOperation(t, h.intent)
	kinds := []string{
		"data_directory", "unit_file", "service", "", "image_cache", "container",
		"Artifact_Directory", "ARTIFACT_DIRECTORY", " artifact_directory", "artifact_directory ", "artifact_directory\n", "artifact_directory/",
	}
	for _, kind := range kinds {
		got := adExecApply(t, t.Context(), e, op, kind)
		if got != adExecNotStarted {
			t.Fatalf("kind %q: outcome %+v, want not_started", kind, got)
		}
		adExecRequireNoIO(t, h)
	}
}

func TestSSHArtifactDirectoryExecutorRefusesMismatchedReceipts(t *testing.T) {
	h := newAuthSendHarness(t, nil)
	e := adExecNew(t, h)
	otherDir := func() string {
		other := h.intent
		other.Identity.OperationID = adExecOtherOperation
		return other.Directory()
	}()
	if otherDir == "" || otherDir == h.intent.Directory() {
		t.Fatalf("fixture directories %q %q", otherDir, h.intent.Directory())
	}

	cases := []struct {
		name   string
		mutate func(op *domain.ProbeInstallOperation)
	}{
		{name: "docker mode", mutate: func(op *domain.ProbeInstallOperation) { op.Identity.Mode = domain.ProbeInstallDocker }},
		// The whole receipt layout must equal the identity-derived layout.
		{name: "data_directory spec kind altered", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[0].Spec.Kind = "image_cache" }},
		{name: "data_directory reference altered", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[0].Spec.Reference = "/var/lib/other" }},
		{name: "artifact ordinal altered", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[1].Spec.Ordinal = 7 }},
		{name: "artifact not owned", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[1].Spec.Owned = false }},
		{name: "unit_file reference altered", mutate: func(op *domain.ProbeInstallOperation) {
			op.Resources[2].Spec.Reference = "/etc/systemd/system/evil.service"
		}},
		{name: "extra resource", mutate: func(op *domain.ProbeInstallOperation) {
			op.Resources = append(op.Resources, op.Resources[3])
		}},
		{name: "unknown mode", mutate: func(op *domain.ProbeInstallOperation) { op.Identity.Mode = "" }},
		{name: "non-root ssh user", mutate: func(op *domain.ProbeInstallOperation) { op.Identity.Host.User = "deploy" }},
		{name: "invalid layout version", mutate: func(op *domain.ProbeInstallOperation) { op.Identity.LayoutVersion = 0 }},
		{name: "host key pin of another host", mutate: func(op *domain.ProbeInstallOperation) {
			_, other := deploymentKey(t)
			op.Identity.HostKeyFingerprint = ssh.FingerprintSHA256(other.PublicKey())
		}},
		{name: "client key pin of another client", mutate: func(op *domain.ProbeInstallOperation) {
			_, other := deploymentKey(t)
			op.Identity.ClientKeyFingerprint = ssh.FingerprintSHA256(other.PublicKey())
		}},
		{name: "identity binary pin differs from the prepared binary", mutate: func(op *domain.ProbeInstallOperation) {
			op.Identity.Binary = sendTPin([]byte("another pinned payload"))
		}},
		{name: "invalid identity binary pin", mutate: func(op *domain.ProbeInstallOperation) { op.Identity.Binary.Size = 0 }},
		{name: "operation id no longer matches the reserved directory", mutate: func(op *domain.ProbeInstallOperation) {
			op.Identity.OperationID = adExecOtherOperation
		}},
		{name: "empty owner id", mutate: func(op *domain.ProbeInstallOperation) { op.OwnerID = "" }},
		{name: "malformed owner id", mutate: func(op *domain.ProbeInstallOperation) { op.OwnerID = "not-an-id" }},
		{name: "empty attempt id", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[1].AttemptID = "" }},
		{name: "malformed attempt id", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[1].AttemptID = "not-an-id" }},
		{name: "no resources", mutate: func(op *domain.ProbeInstallOperation) { op.Resources = nil }},
		{name: "empty resources", mutate: func(op *domain.ProbeInstallOperation) { op.Resources = []domain.ProbeInstallResource{} }},
		{name: "one resource only", mutate: func(op *domain.ProbeInstallOperation) { op.Resources = op.Resources[:1] }},
		{name: "resource spec kind is the unit file", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[1].Spec.Kind = "unit_file" }},
		{name: "resource spec kind is the data directory", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[1].Spec.Kind = "data_directory" }},
		{name: "empty resource spec kind", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[1].Spec.Kind = "" }},
		{name: "reference of another operation", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[1].Spec.Reference = otherDir }},
		{name: "empty reference", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[1].Spec.Reference = "" }},
		{name: "reference with a trailing slash", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[1].Spec.Reference += "/" }},
		{name: "reference with a traversal suffix", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[1].Spec.Reference += "/../x" }},
		{name: "reference is the data directory", mutate: func(op *domain.ProbeInstallOperation) {
			op.Resources[1].Spec.Reference = op.Resources[0].Spec.Reference
		}},
		{name: "reference outside the layout", mutate: func(op *domain.ProbeInstallOperation) { op.Resources[1].Spec.Reference = "/opt/other" }},
		{name: "artifact and data directory swapped", mutate: func(op *domain.ProbeInstallOperation) {
			op.Resources[0], op.Resources[1] = op.Resources[1], op.Resources[0]
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			op := adExecOperation(t, h.intent)
			tc.mutate(&op)
			got := adExecApply(t, t.Context(), e, op, "artifact_directory")
			if got != adExecNotStarted {
				t.Fatalf("outcome %+v, want not_started", got)
			}
			adExecRequireNoIO(t, h)
		})
	}

	// The unmutated receipt proceeds: the refusals above are the receipt's
	// doing, not an inert fixture.
	t.Run("control receipt proceeds", func(t *testing.T) {
		control := newAuthSendHarness(t, nil)
		got := adExecApply(t, t.Context(), adExecNew(t, control), adExecOperation(t, control.intent), "artifact_directory")
		adExecRequireOutcome(t, got, adExecApplied)
	})

	t.Run("later resources are not consulted for admission", func(t *testing.T) {
		// Resources beyond the artifact directory carry no predecessor meaning;
		// the executor refusing on them would deadlock a legitimate reservation.
		// Only the Resources[1] reservation and Resources[0] predecessor decide.
		control := newAuthSendHarness(t, nil)
		op := adExecOperation(t, control.intent)
		op.Resources[2].State = domain.ProbeInstallResourcePlanned
		op.Resources[3].State = domain.ProbeInstallResourcePlanned
		got := adExecApply(t, t.Context(), adExecNew(t, control), op, "artifact_directory")
		adExecRequireOutcome(t, got, adExecApplied)
	})
}

func TestSSHArtifactDirectoryExecutorRefusesMismatchedPreparedBinary(t *testing.T) {
	h := newAuthSendHarness(t, nil)
	op := adExecOperation(t, h.intent)
	other := sendTBytesBinary([]byte("another pinned payload"))
	brokenPin := sendTBytesBinary(h.content)
	brokenPin.pin.Size = 0
	wrongVersion := sendTBytesBinary(h.content)
	wrongVersion.pin.Version = "9.9.9"
	cases := []struct {
		name   string
		binary *sendTFakeBinary
	}{
		{name: "different content pin", binary: other},
		{name: "invalid pin", binary: brokenPin},
		{name: "same digest, different version", binary: wrongVersion},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := NewSSHArtifactDirectoryExecutor(h.sender, tc.binary, h.auth)
			if err != nil || e == nil {
				t.Fatalf("constructor %v %v", e, err)
			}
			got := adExecApply(t, t.Context(), e, op, "artifact_directory")
			adExecRequireOutcome(t, got, adExecNotStarted)
			adExecRequireNoIO(t, h)
			if tc.binary.opens.Load() != 0 || tc.binary.snapshotCloses.Load() != 0 {
				t.Fatalf("mismatched binary touched: opens %d closes %d", tc.binary.opens.Load(), tc.binary.snapshotCloses.Load())
			}
		})
	}
}

// Exhaustive admission matrix over the three states the executor reads. Only
// running + data directory applied + artifact directory in flight may proceed;
// every other combination is a refusal with no I/O.
func TestSSHArtifactDirectoryExecutorStateMatrix(t *testing.T) {
	h := newAuthSendHarness(t, nil)
	e := adExecNew(t, h)
	operationStates := []string{
		domain.ProbeInstallQueued, domain.ProbeInstallRunning, domain.ProbeInstallResourcesReady,
		domain.ProbeInstallCancelRequested, domain.ProbeInstallFailed, domain.ProbeInstallCanceled,
		domain.ProbeInstallInterrupted, "", "bogus",
	}
	resourceStates := []string{
		domain.ProbeInstallResourcePlanned, domain.ProbeInstallResourceInFlight, domain.ProbeInstallResourceApplied,
		domain.ProbeInstallResourceNotStarted, domain.ProbeInstallResourceUnknown, "", "bogus",
	}
	checked := 0
	for _, opState := range operationStates {
		for _, r0 := range resourceStates {
			for _, r1 := range resourceStates {
				if opState == "running" && r0 == "applied" && r1 == "in_flight" {
					continue // the one admitted combination; see the happy path
				}
				op := adExecOperation(t, h.intent)
				op.State, op.Resources[0].State, op.Resources[1].State = opState, r0, r1
				got := adExecApply(t, t.Context(), e, op, "artifact_directory")
				if got != adExecNotStarted {
					t.Fatalf("operation %q data %q artifact %q: outcome %+v, want not_started", opState, r0, r1, got)
				}
				checked++
			}
		}
	}
	if want := len(operationStates)*len(resourceStates)*len(resourceStates) - 1; checked != want {
		t.Fatalf("checked %d combinations, want %d", checked, want)
	}
	adExecRequireNoIO(t, h)
}

func TestSSHArtifactDirectoryExecutorAuthorizerRefusalIsNotStarted(t *testing.T) {
	cases := []struct {
		name string
		hook func(ctx context.Context, a *authSendAuthorizer, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error)
	}{
		{name: "plain refusal", hook: func(context.Context, *authSendAuthorizer, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
			return domain.ProbeBinaryStageGrant{}, errors.New("journal attempt already consumed")
		}},
		{name: "context canceled sentinel", hook: func(context.Context, *authSendAuthorizer, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
			return domain.ProbeBinaryStageGrant{}, context.Canceled
		}},
		{name: "deadline exceeded sentinel", hook: func(context.Context, *authSendAuthorizer, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
			return domain.ProbeBinaryStageGrant{}, context.DeadlineExceeded
		}},
		{name: "domain sentinel", hook: func(context.Context, *authSendAuthorizer, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
			return domain.ProbeBinaryStageGrant{}, domain.ErrProbeArtifactStorage
		}},
		{name: "grant for a different request", hook: func(_ context.Context, a *authSendAuthorizer, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
			request.Challenge.Nonce[0] ^= 0xff
			return a.authority.sign(request), nil
		}},
		{name: "zero grant without an error", hook: func(context.Context, *authSendAuthorizer, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
			return domain.ProbeBinaryStageGrant{}, nil
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthSendHarness(t, func(_ *authSendHelper, auth *authSendAuthorizer) { auth.hook = tc.hook })
			e := adExecNew(t, h)
			got := adExecApply(t, t.Context(), e, adExecOperation(t, h.intent), "artifact_directory")
			adExecRequireOutcome(t, got, adExecNotStarted)
			h.requireNoStageInput(t, 1)
			if h.auth.calls.Load() != 1 {
				t.Fatalf("authorizer calls = %d, want exactly 1 (no retry)", h.auth.calls.Load())
			}
			if requests := h.auth.seenRequests(); len(requests) != 1 || requests[0] != h.wantRequest() {
				t.Fatalf("authorizer request %+v", requests)
			}
		})
	}
}

// Failures after exec but before any stdin byte never reach the authorizer and
// are not_started: the helper was started but was given nothing to stage.
func TestSSHArtifactDirectoryExecutorHelperRefusalBeforeStdinIsNotStarted(t *testing.T) {
	cases := []struct {
		name string
		tune func(helper *authSendHelper)
	}{
		{name: "helper exits 1 without a challenge", tune: func(helper *authSendHelper) {
			helper.noChallenge = true
			helper.noChallengeStatus = 1
		}},
		{name: "helper exits 0 without a challenge", tune: func(helper *authSendHelper) { helper.noChallenge = true }},
		{name: "malformed challenge line", tune: func(helper *authSendHelper) {
			helper.challengeFn = func(*sendTFixture) []byte { return []byte("this is not a challenge line\n") }
		}},
		{name: "helper exits right after the challenge", tune: func(helper *authSendHelper) { helper.exitAfterChallenge = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthSendHarness(t, func(helper *authSendHelper, _ *authSendAuthorizer) { tc.tune(helper) })
			e := adExecNew(t, h)
			got := adExecApply(t, t.Context(), e, adExecOperation(t, h.intent), "artifact_directory")
			adExecRequireOutcome(t, got, adExecNotStarted)
			h.requireNoStageInput(t, 1)
		})
	}
}

func TestSSHArtifactDirectoryExecutorHelperFailureAfterGrantIsUnknown(t *testing.T) {
	receipt := func(f *sendTFixture, helper *authSendHelper) string { return sendTReceipt(f.intent(helper.pin)) }
	cases := []struct {
		name string
		tune func(helper *authSendHelper)
	}{
		{name: "nonzero exit with a valid receipt", tune: func(helper *authSendHelper) { helper.exit = sendTExit() }},
		{name: "nonzero exit with no receipt", tune: func(helper *authSendHelper) {
			helper.exit = sendTExit()
			helper.tail = func(*sendTFixture, []byte) string { return "" }
		}},
		{name: "signal exit with a valid receipt", tune: func(helper *authSendHelper) { helper.signal = "TERM" }},
		{name: "missing exit status", tune: func(helper *authSendHelper) { helper.noExit = true }},
		{name: "no receipt", tune: func(helper *authSendHelper) { helper.tail = func(*sendTFixture, []byte) string { return "" } }},
		{name: "receipt for another operation", tune: func(helper *authSendHelper) {
			helper.tail = func(f *sendTFixture, _ []byte) string { return sendTOtherOpReceipt(f, helper.pin) }
		}},
		{name: "truncated receipt", tune: func(helper *authSendHelper) {
			helper.tail = func(f *sendTFixture, _ []byte) string { return receipt(f, helper)[:20] }
		}},
		{name: "receipt followed by an extra byte", tune: func(helper *authSendHelper) {
			helper.tail = func(f *sendTFixture, _ []byte) string { return receipt(f, helper) + "x" }
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newAuthSendHarness(t, func(helper *authSendHelper, _ *authSendAuthorizer) { tc.tune(helper) })
			e := adExecNew(t, h)
			got := adExecApply(t, t.Context(), e, adExecOperation(t, h.intent), "artifact_directory")
			adExecRequireOutcome(t, got, adExecUnknown)
			h.helper.waitIdle(t, h.f)

			// The whole stage input crossed the boundary exactly once.
			if want := h.wantStdin(t, h.genuineSignature(), h.content); !bytes.Equal(h.f.stdinBytes(), want) {
				t.Fatalf("server stdin = %d bytes, want %d", len(h.f.stdinBytes()), len(want))
			}
			if h.auth.calls.Load() != 1 {
				t.Fatalf("authorizer calls = %d, want exactly 1", h.auth.calls.Load())
			}
			if h.f.connCalls.Load() != 1 {
				t.Fatalf("connections = %d, want 1 (no retry)", h.f.connCalls.Load())
			}
			if got := h.f.execCommands(); !slices.Equal(got, []string{authSendCommandLiteral}) {
				t.Fatalf("exec commands %v", got)
			}
		})
	}
}

// A grant the sender accepts structurally but the helper's trusted key rejects
// is discoverable only by the helper, after the stage input was sent.
func TestSSHArtifactDirectoryExecutorHelperRejectingGrantIsUnknown(t *testing.T) {
	forger := newAuthSendAuthority(t)
	h := newAuthSendHarness(t, func(_ *authSendHelper, auth *authSendAuthorizer) {
		auth.hook = func(_ context.Context, _ *authSendAuthorizer, request domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
			return forger.sign(request), nil
		}
	})
	e := adExecNew(t, h)
	got := adExecApply(t, t.Context(), e, adExecOperation(t, h.intent), "artifact_directory")
	adExecRequireOutcome(t, got, adExecUnknown)
	h.helper.waitIdle(t, h.f)
	forged := h.wantStdin(t, adExecForgedSignature(h, forger), h.content)
	if !bytes.Equal(h.f.stdinBytes(), forged) {
		t.Fatalf("server stdin = %d bytes, want %d", len(h.f.stdinBytes()), len(forged))
	}
	if h.helper.verified.Load() != 2 {
		t.Fatalf("helper emulation verdict %d, want rejected", h.helper.verified.Load())
	}
}

// adExecForgedSignature is the signature the forger's authority produces over
// the request the sender builds from the helper's challenge.
func adExecForgedSignature(h *authSendHarness, forger *authSendAuthority) []byte {
	grant := forger.sign(h.wantRequest())
	return grant.Signature[:]
}

// Once the first stdin byte has been delivered, cancellation can no longer
// prove the host is untouched: it must report unknown, never not_started.
func TestSSHArtifactDirectoryExecutorCancelAfterStdinIsUnknown(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseHelper := func() { releaseOnce.Do(func() { close(release) }) }
	h := newAuthSendHarness(t, func(helper *authSendHelper, _ *authSendAuthorizer) {
		helper.tail = func(f *sendTFixture, _ []byte) string {
			<-release
			return sendTReceipt(f.intent(helper.pin))
		}
	})
	// Registered after the fixture's own cleanup, so it runs first and lets the
	// held helper emulation finish before the fixture is torn down.
	t.Cleanup(releaseHelper)

	e := adExecNew(t, h)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := adExecApplyAsync(ctx, e, adExecOperation(t, h.intent), "artifact_directory")
	adExecWaitStdinEOF(t, h.f)
	adExecExpectBlocked(t, done, "apply while the helper holds its receipt")
	cancel()
	got := adExecAwait(t, done)
	adExecRequireOutcome(t, got, adExecUnknown)

	releaseHelper()
	h.helper.waitIdle(t, h.f)
	if want := h.wantStdin(t, h.genuineSignature(), h.content); !bytes.Equal(h.f.stdinBytes(), want) {
		t.Fatalf("server stdin = %d bytes, want %d", len(h.f.stdinBytes()), len(want))
	}
	if h.helper.verified.Load() != 1 {
		t.Fatalf("helper emulation verdict %d, want accepted grant", h.helper.verified.Load())
	}
	if h.auth.calls.Load() != 1 || h.f.connCalls.Load() != 1 {
		t.Fatalf("authorizer calls %d connections %d, want 1 and 1", h.auth.calls.Load(), h.f.connCalls.Load())
	}
}

func TestSSHArtifactDirectoryExecutorHostTrustFailureIsNotStarted(t *testing.T) {
	_, otherSigner := deploymentKey(t)
	wrongFP := ssh.FingerprintSHA256(otherSigner.PublicKey())

	t.Run("server presents a key other than the pinned fingerprint", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		sender, err := NewSSHBinaryStageSender(t.Context(), h.f.clientKey, wrongFP, h.f.clientFP(), h.f.policy())
		if err != nil {
			t.Fatal(err)
		}
		intent := h.intent
		intent.Identity.HostKeyFingerprint = wrongFP
		e, err := NewSSHArtifactDirectoryExecutor(sender, h.binary, h.auth)
		if err != nil {
			t.Fatal(err)
		}
		got := adExecApply(t, t.Context(), e, adExecOperation(t, intent), "artifact_directory")
		adExecRequireOutcome(t, got, adExecNotStarted)
		// The trust path really ran: the TCP connection was made and rejected
		// before the client authenticated, so no helper was ever started.
		if h.f.connCalls.Load() != 1 {
			t.Fatalf("connections = %d, want 1 (trust rejected after dial)", h.f.connCalls.Load())
		}
		if h.f.authCalls.Load() != 0 {
			t.Fatalf("client authenticated before host trust: %d", h.f.authCalls.Load())
		}
		adExecRequireNoStageInput(t, h)
	})

	t.Run("sender pinned to another host than the operation", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		sender, err := NewSSHBinaryStageSender(t.Context(), h.f.clientKey, wrongFP, h.f.clientFP(), h.f.policy())
		if err != nil {
			t.Fatal(err)
		}
		e, err := NewSSHArtifactDirectoryExecutor(sender, h.binary, h.auth)
		if err != nil {
			t.Fatal(err)
		}
		got := adExecApply(t, t.Context(), e, adExecOperation(t, h.intent), "artifact_directory")
		adExecRequireOutcome(t, got, adExecNotStarted)
		adExecRequireNoIO(t, h)
	})

	t.Run("operation pins another host than the sender", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		intent := h.intent
		intent.Identity.HostKeyFingerprint = wrongFP
		got := adExecApply(t, t.Context(), adExecNew(t, h), adExecOperation(t, intent), "artifact_directory")
		adExecRequireOutcome(t, got, adExecNotStarted)
		adExecRequireNoIO(t, h)
	})

	t.Run("destination rejected by the endpoint policy", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		sender, err := NewSSHBinaryStageSender(t.Context(), h.f.clientKey, h.f.hostFP(), h.f.clientFP(), probe.EndpointPolicy{})
		if err != nil {
			t.Fatal(err)
		}
		e, err := NewSSHArtifactDirectoryExecutor(sender, h.binary, h.auth)
		if err != nil {
			t.Fatal(err)
		}
		got := adExecApply(t, t.Context(), e, adExecOperation(t, h.intent), "artifact_directory")
		adExecRequireOutcome(t, got, adExecNotStarted)
		if h.f.connCalls.Load() != 0 {
			t.Fatalf("dialed a rejected destination: %d", h.f.connCalls.Load())
		}
		adExecRequireNoStageInput(t, h)
	})

	t.Run("connect failure", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		intent := h.intent
		intent.Identity.Host.Address = authSendDeadAddress(t)
		got := adExecApply(t, t.Context(), adExecNew(t, h), adExecOperation(t, intent), "artifact_directory")
		adExecRequireOutcome(t, got, adExecNotStarted)
		if h.f.connCalls.Load() != 0 {
			t.Fatalf("fixture contacted: %d", h.f.connCalls.Load())
		}
		adExecRequireNoStageInput(t, h)
	})
}

func TestSSHArtifactDirectoryExecutorContextRefusals(t *testing.T) {
	t.Run("canceled context performs no I/O", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		e := adExecNew(t, h)
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		got := adExecApply(t, canceled, e, adExecOperation(t, h.intent), "artifact_directory")
		adExecRequireOutcome(t, got, adExecNotStarted)
		adExecRequireNoIO(t, h)
	})

	t.Run("expired deadline performs no I/O", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		e := adExecNew(t, h)
		expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		got := adExecApply(t, expired, e, adExecOperation(t, h.intent), "artifact_directory")
		adExecRequireOutcome(t, got, adExecNotStarted)
		adExecRequireNoIO(t, h)
	})

	t.Run("typed nil context performs no I/O", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		e := adExecNew(t, h)
		var nilCtx *sendTNilContext
		got := adExecApply(t, nilCtx, e, adExecOperation(t, h.intent), "artifact_directory")
		adExecRequireOutcome(t, got, adExecNotStarted)
		adExecRequireNoIO(t, h)
	})

	t.Run("cancel while the authorizer blocks is not_started", func(t *testing.T) {
		h := newAuthSendHarness(t, func(_ *authSendHelper, auth *authSendAuthorizer) { auth.hook = authSendBlockUntilDone })
		e := adExecNew(t, h)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := adExecApplyAsync(ctx, e, adExecOperation(t, h.intent), "artifact_directory")
		sendTWaitSignal(t, h.auth.entered, "authorizer call")
		adExecExpectBlocked(t, done, "apply while the authorizer blocks")
		cancel()
		got := adExecAwait(t, done)
		adExecRequireOutcome(t, got, adExecNotStarted)
		h.requireNoStageInput(t, 1)
		if h.auth.calls.Load() != 1 {
			t.Fatalf("authorizer calls = %d, want 1", h.auth.calls.Load())
		}
	})

	t.Run("caller deadline below the authorization budget never consumes an admission", func(t *testing.T) {
		h := newAuthSendHarness(t, nil)
		e := adExecNew(t, h)
		ctx, cancel := context.WithTimeout(t.Context(), stageSendAuthorizeMinBudget/2)
		defer cancel()
		got := adExecApply(t, ctx, e, adExecOperation(t, h.intent), "artifact_directory")
		adExecRequireOutcome(t, got, adExecNotStarted)
		if h.auth.calls.Load() != 0 {
			t.Fatalf("authorizer calls = %d, want 0", h.auth.calls.Load())
		}
		h.requireNoStageInput(t, 1)
	})
}

// A receiver or collaborator that is not fully built degrades to a refusal,
// never to a panic or a returned error.
func TestSSHArtifactDirectoryExecutorDegenerateReceiversAreRefusals(t *testing.T) {
	h := newAuthSendHarness(t, nil)
	op := adExecOperation(t, h.intent)
	var typedNilExecutor *SSHArtifactDirectoryExecutor
	cases := []struct {
		name string
		e    ports.ProbeInstallResourceExecutor
	}{
		{name: "nil executor", e: typedNilExecutor},
		{name: "zero value", e: &SSHArtifactDirectoryExecutor{}},
		{name: "no sender", e: &SSHArtifactDirectoryExecutor{binary: h.binary, authorizer: h.auth}},
		{name: "zero sender", e: &SSHArtifactDirectoryExecutor{sender: &SSHBinaryStageSender{}, binary: h.binary, authorizer: h.auth}},
		{name: "no binary", e: &SSHArtifactDirectoryExecutor{sender: h.sender, authorizer: h.auth}},
		{name: "typed nil binary", e: &SSHArtifactDirectoryExecutor{sender: h.sender, binary: (*sendTFakeBinary)(nil), authorizer: h.auth}},
		{name: "no authorizer", e: &SSHArtifactDirectoryExecutor{sender: h.sender, binary: h.binary}},
		{name: "typed nil authorizer", e: &SSHArtifactDirectoryExecutor{sender: h.sender, binary: h.binary, authorizer: (*authSendAuthorizer)(nil)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := adExecApply(t, t.Context(), tc.e, op, "artifact_directory")
			adExecRequireOutcome(t, got, adExecNotStarted)
			adExecRequireNoIO(t, h)
		})
	}
}

// Every path an executor can take reports a valid, nil-error outcome: applied
// carries no error code, the other two carry exactly the fixed codes.
func TestSSHArtifactDirectoryExecutorOutcomesAreAlwaysValid(t *testing.T) {
	if !adExecApplied.Valid() || !adExecNotStarted.Valid() || !adExecUnknown.Valid() {
		t.Fatal("expected outcome literals are not valid reports")
	}
	for _, bad := range []domain.ProbeInstallResourceOutcome{
		{State: "applied", ErrorCode: "x"},
		{State: "not_started", ErrorCode: ""},
		{State: "unknown", ErrorCode: ""},
		{State: "in_flight"},
		{},
	} {
		if bad.Valid() {
			t.Fatalf("%+v unexpectedly valid", bad)
		}
	}
	// One end-to-end sample per class; adExecAwait asserts err == nil and
	// Valid() on every one.
	h := newAuthSendHarness(t, nil)
	e := adExecNew(t, h)
	refused := adExecApply(t, t.Context(), e, adExecOperation(t, h.intent), "unit_file")
	adExecRequireOutcome(t, refused, adExecNotStarted)
	applied := adExecApply(t, t.Context(), e, adExecOperation(t, h.intent), "artifact_directory")
	adExecRequireOutcome(t, applied, adExecApplied)

	hu := newAuthSendHarness(t, func(helper *authSendHelper, _ *authSendAuthorizer) { helper.exit = sendTExit() })
	unknown := adExecApply(t, t.Context(), adExecNew(t, hu), adExecOperation(t, hu.intent), "artifact_directory")
	adExecRequireOutcome(t, unknown, adExecUnknown)
}
