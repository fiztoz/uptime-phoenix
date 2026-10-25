package provisioning

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// These tests drive SSHProbeInstallExecutor through the real loopback SSH
// protocol fixture (ssh_binary_stage_send_test.go). One fixture serves both
// helper emulations and dispatches on the exec command: the authorized stage
// helper (ssh_binary_stage_authorized_send_test.go) for artifact_directory and
// the host-action helper (ssh_host_action_send_test.go) for the other three
// kinds. The remote side is protocol evidence only: no OpenSSH, MariaDB,
// installed helper, systemd or privileged service is consulted, and every key
// is generated per test. Outcomes are judged by adExecAwait, which requires a
// nil error and a report an executor is allowed to submit.

// Independent literals: a changed constant must not silently change what these
// tests accept.
const (
	instExecStageCommandLiteral = "/usr/local/bin/phoenix-probe-admin stage-binary-authorized"
	instExecHostCommandLiteral  = "/usr/local/bin/phoenix-probe-admin stage-host-action-authorized"
	instExecAttemptID           = "33333333-3333-4333-8333-333333333333"
	instExecOtherUUID           = "55555555-5555-4555-8555-555555555555"
)

var (
	instExecLayoutKinds = []string{"data_directory", "artifact_directory", "unit_file", "service"}
	instExecHostKinds   = []string{"data_directory", "unit_file", "service"}
)

func instExecEarlierAttempt(i int) string {
	return fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaa%02x", i)
}

type instExecHarness struct {
	f           *sendTFixture
	stageHelper *authSendHelper
	hostHelper  *hostActSendHelper
	stageAuth   *authSendAuthorizer
	hostAuth    *hostActSendAuthorizer
	binary      *sendTFakeBinary
	sender      *SSHBinaryStageSender
	exec        *SSHProbeInstallExecutor

	content  []byte
	identity domain.ProbeInstallIdentity
	owner    string
	reads    atomic.Int32
	closes   atomic.Int32
}

func newInstExecHarness(t *testing.T, tune func(h *instExecHarness)) *instExecHarness {
	t.Helper()
	h := &instExecHarness{content: []byte(hostActSendContent)}
	pin := sendTPin(h.content)
	h.binary = sendTBinary(pin, func(context.Context) (io.ReadCloser, error) {
		inner := sendTScripted(h.content, nil, io.EOF)
		return &sendTReader{
			readFn:  func(p []byte) (int, error) { h.reads.Add(1); return inner.Read(p) },
			closeFn: func() error { h.closes.Add(1); return nil },
		}, nil
	})
	authority := newAuthSendAuthority(t)
	h.stageHelper = &authSendHelper{authority: authority, nonce: authSendNonce(), pin: pin, content: h.content, verify: true}
	h.hostHelper = &hostActSendHelper{authority: authority, nonce: authSendNonce(), verify: true}
	h.stageAuth = newAuthSendAuthorizer(authority)
	h.hostAuth = newHostActSendAuthorizer(authority)
	if tune != nil {
		tune(h)
	}
	cfg := h.hostHelper.config()
	cfg.handle = func(f *sendTFixture, ch ssh.Channel, command string, dead <-chan struct{}) {
		switch command {
		case SSHProbeBinaryStageAuthorizedCommand:
			h.stageHelper.handle(f, ch, command, dead)
		case SSHProbeHostActionCommand:
			h.hostHelper.handle(f, ch, command, dead)
		default:
			authSendExit(ch, 127)
		}
	}
	h.f = serveStageSend(t, cfg)
	h.stageAuth.stdinLen = func() int { return len(h.f.stdinBytes()) }
	h.hostAuth.stdinLen = func() int { return len(h.f.stdinBytes()) }
	stage := h.f.intent(pin)
	h.identity, h.owner = stage.Identity, stage.OwnerID
	h.sender = h.f.sender(t)
	exec, err := NewSSHProbeInstallExecutor(h.sender, h.binary, h.stageAuth, h.hostAuth)
	if err != nil || exec == nil {
		t.Fatalf("constructor %v %v", exec, err)
	}
	h.exec = exec
	return h
}

// operation builds the receipt the journal hands the executor after
// acknowledging the reservation for kind: running, every earlier resource
// applied, kind in flight with the fixture's attempt, later ones planned.
func (h *instExecHarness) operation(t *testing.T, kind string) domain.ProbeInstallOperation {
	t.Helper()
	specs := h.identity.Resources()
	if len(specs) != 4 || !slices.Equal(kindsOf(specs), instExecLayoutKinds) {
		t.Fatalf("fixture identity resource layout %+v", specs)
	}
	index := slices.Index(instExecLayoutKinds, kind)
	if index < 0 {
		t.Fatalf("kind %q is not in the layout", kind)
	}
	resources := make([]domain.ProbeInstallResource, len(specs))
	for i, spec := range specs {
		resources[i] = domain.ProbeInstallResource{Spec: spec, State: domain.ProbeInstallResourcePlanned}
		if i < index {
			resources[i].State = domain.ProbeInstallResourceApplied
			resources[i].AttemptID = instExecEarlierAttempt(i)
		}
	}
	resources[index].State = domain.ProbeInstallResourceInFlight
	resources[index].AttemptID = instExecAttemptID
	return domain.ProbeInstallOperation{Identity: h.identity, OwnerID: h.owner, State: domain.ProbeInstallRunning, Resources: resources}
}

func kindsOf(specs []domain.ProbeInstallResourceSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Kind
	}
	return out
}

func (h *instExecHarness) hostIntent(kind string) domain.ProbeHostActionIntent {
	return domain.ProbeHostActionIntent{Identity: h.identity, OwnerID: h.owner, AttemptID: instExecAttemptID, Kind: kind}
}

func (h *instExecHarness) apply(t *testing.T, op domain.ProbeInstallOperation, kind string) domain.ProbeInstallResourceOutcome {
	t.Helper()
	return adExecApply(t, t.Context(), h.exec, op, kind)
}

func (h *instExecHarness) waitIdle(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		execs := h.f.execCalls.Load()
		started := h.stageHelper.started.Load() + h.hostHelper.started.Load()
		finished := h.stageHelper.finished.Load() + h.hostHelper.finished.Load()
		if execs > 0 && started >= execs && finished >= started {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("helper emulations did not finish: execs %d started %d finished %d", execs, started, finished)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// requireNoIO asserts a refusal happened before anything observable: no
// connection, SSH authentication, exec, stdin, authorization or payload open.
func (h *instExecHarness) requireNoIO(t *testing.T) {
	t.Helper()
	if conn, auth, exec := h.f.connCalls.Load(), h.f.authCalls.Load(), h.f.execCalls.Load(); conn != 0 || auth != 0 || exec != 0 {
		t.Fatalf("refusal performed I/O: conn %d auth %d exec %d", conn, auth, exec)
	}
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("server received %d stdin bytes, want 0", len(got))
	}
	if h.stageAuth.calls.Load() != 0 || h.hostAuth.calls.Load() != 0 {
		t.Fatalf("authorizers reached: stage %d host %d", h.stageAuth.calls.Load(), h.hostAuth.calls.Load())
	}
	if h.binary.opens.Load() != 0 || h.reads.Load() != 0 || h.closes.Load() != 0 || h.binary.snapshotCloses.Load() != 0 {
		t.Fatalf("payload source touched: opens %d reads %d closes %d snapshot closes %d",
			h.binary.opens.Load(), h.reads.Load(), h.closes.Load(), h.binary.snapshotCloses.Load())
	}
}

// requireNoStageInput is the evidence for a failure after exec but before the
// first stdin byte: nothing was delivered.
func (h *instExecHarness) requireNoStageInput(t *testing.T) {
	t.Helper()
	h.waitIdle(t)
	if got := h.f.stdinBytes(); len(got) != 0 {
		t.Fatalf("server received %d stdin bytes, want 0", len(got))
	}
	if h.f.connCalls.Load() != 1 || h.f.execCalls.Load() != 1 {
		t.Fatalf("connections %d execs %d, want 1 and 1 (no retry)", h.f.connCalls.Load(), h.f.execCalls.Load())
	}
}

func TestSSHProbeInstallExecutorConstructorRefusesNilCollaborators(t *testing.T) {
	h := newInstExecHarness(t, nil)
	var nilSender *SSHBinaryStageSender
	var typedNilBinary *sendTFakeBinary
	var typedNilStage *authSendAuthorizer
	var typedNilStageFunc authSendFuncAuthorizer
	var typedNilActions *hostActSendAuthorizer
	var typedNilActionsFunc hostActSendFuncAuthorizer
	cases := []struct {
		name    string
		sender  *SSHBinaryStageSender
		binary  ports.PreparedProbeBinary
		stage   ProbeBinaryStageAuthorizer
		actions ProbeHostActionAuthorizer
	}{
		{"nil sender", nilSender, h.binary, h.stageAuth, h.hostAuth},
		{"untyped nil binary", h.sender, nil, h.stageAuth, h.hostAuth},
		{"typed nil binary", h.sender, typedNilBinary, h.stageAuth, h.hostAuth},
		{"untyped nil stage authorizer", h.sender, h.binary, nil, h.hostAuth},
		{"typed nil pointer stage authorizer", h.sender, h.binary, typedNilStage, h.hostAuth},
		{"typed nil func stage authorizer", h.sender, h.binary, typedNilStageFunc, h.hostAuth},
		{"untyped nil action authorizer", h.sender, h.binary, h.stageAuth, nil},
		{"typed nil pointer action authorizer", h.sender, h.binary, h.stageAuth, typedNilActions},
		{"typed nil func action authorizer", h.sender, h.binary, h.stageAuth, typedNilActionsFunc},
		{"everything nil", nil, nil, nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := NewSSHProbeInstallExecutor(tc.sender, tc.binary, tc.stage, tc.actions)
			if err != ErrSSHBinaryStageInput || e != nil {
				t.Fatalf("%v %v, want nil executor and ErrSSHBinaryStageInput", e, err)
			}
		})
	}
	// The executor must satisfy the port (checked at compile time).
	var _ ports.ProbeInstallResourceExecutor = h.exec
	// Construction performs no I/O of any kind.
	h.requireNoIO(t)
}

func TestSSHProbeInstallExecutorDispatchesEveryKind(t *testing.T) {
	for _, kind := range instExecLayoutKinds {
		t.Run(kind, func(t *testing.T) {
			h := newInstExecHarness(t, nil)
			op := h.operation(t, kind)
			before := slices.Clone(op.Resources)

			got := h.apply(t, op, kind)
			adExecRequireOutcome(t, got, adExecApplied)
			if got.ErrorCode != "" {
				t.Fatalf("applied carries error code %q", got.ErrorCode)
			}
			h.waitIdle(t)

			if h.f.connCalls.Load() != 1 {
				t.Fatalf("connections = %d, want 1", h.f.connCalls.Load())
			}
			if got := h.f.requestTypes(); !slices.Equal(got, []string{"exec"}) {
				t.Fatalf("session requests %v", got)
			}
			if h.binary.snapshotCloses.Load() != 0 {
				t.Fatalf("borrowed snapshot closed %d times", h.binary.snapshotCloses.Load())
			}
			stdin := h.f.stdinBytes()

			if kind == "artifact_directory" {
				if got := h.f.execCommands(); !slices.Equal(got, []string{instExecStageCommandLiteral}) {
					t.Fatalf("exec commands %v", got)
				}
				if h.stageAuth.calls.Load() != 1 || h.hostAuth.calls.Load() != 0 {
					t.Fatalf("authorizers: stage %d host %d, want 1 and 0", h.stageAuth.calls.Load(), h.hostAuth.calls.Load())
				}
				if !bytes.HasPrefix(stdin, []byte(hostActFrameStageMagic)) || bytes.HasPrefix(stdin, []byte(hostActFrameMagicLiteral)) {
					t.Fatal("artifact_directory did not send a stage header")
				}
				if !bytes.HasSuffix(stdin, h.content) {
					t.Fatal("artifact_directory did not deliver the pinned payload")
				}
				if h.stageHelper.verified.Load() != 1 {
					t.Fatalf("stage helper verdict %d, want accepted grant", h.stageHelper.verified.Load())
				}
				if h.binary.opens.Load() != 1 || h.closes.Load() != 1 {
					t.Fatalf("payload opens %d closes %d", h.binary.opens.Load(), h.closes.Load())
				}
			} else {
				intent := h.hostIntent(kind)
				frame, err := EncodeProbeHostActionFrame(intent)
				if err != nil {
					t.Fatal(err)
				}
				if got := h.f.execCommands(); !slices.Equal(got, []string{instExecHostCommandLiteral}) {
					t.Fatalf("exec commands %v", got)
				}
				if h.stageAuth.calls.Load() != 0 || h.hostAuth.calls.Load() != 1 {
					t.Fatalf("authorizers: stage %d host %d, want 0 and 1", h.stageAuth.calls.Load(), h.hostAuth.calls.Load())
				}
				// The intent is rebuilt from the receipt: the operation's identity
				// and owner, the in-flight attempt and the requested kind.
				requests := h.hostAuth.seenRequests()
				if len(requests) != 1 || requests[0].Intent != intent || requests[0].Challenge.MarkerSHA256 != intent.MarkerSHA256() ||
					requests[0].Challenge.AuthorityKeyID != h.hostHelper.authority.keyID || requests[0].Challenge.Nonce != h.hostHelper.nonce {
					t.Fatalf("host authorizer requests %+v", requests)
				}
				if len(stdin) != len(frame)+ProbeBinaryStageGrantFrameSize || !bytes.HasPrefix(stdin, frame) {
					t.Fatalf("stdin = %d bytes, want exactly the action frame (%d) and a grant frame", len(stdin), len(frame))
				}
				if h.hostHelper.verified.Load() != 1 {
					t.Fatalf("host helper verdict %d, want accepted grant", h.hostHelper.verified.Load())
				}
				if parsed := h.hostHelper.parsedIntents(); len(parsed) != 1 || parsed[0] != intent {
					t.Fatalf("host helper decoded %+v", parsed)
				}
				// A host action never reads or opens the pinned binary.
				if h.binary.opens.Load() != 0 || h.reads.Load() != 0 || h.closes.Load() != 0 {
					t.Fatalf("host action touched the payload: opens %d reads %d closes %d", h.binary.opens.Load(), h.reads.Load(), h.closes.Load())
				}
			}
			if !slices.Equal(op.Resources, before) {
				t.Fatalf("operation receipt was mutated: %+v, was %+v", op.Resources, before)
			}
		})
	}
}

// The four resources applied in layout order through one executor and one
// sender: the commands, authorizers and frames follow the dispatch, and the
// borrowed collaborators stay usable across calls.
func TestSSHProbeInstallExecutorAppliesLayoutInOrder(t *testing.T) {
	h := newInstExecHarness(t, func(h *instExecHarness) {
		// The stage helper verifies against the start of the fixture's stdin
		// buffer, which accumulates across execs; its grant check is exercised
		// in the single-exec dispatch test above.
		h.stageHelper.verify = false
	})
	for _, kind := range instExecLayoutKinds {
		adExecRequireOutcome(t, h.apply(t, h.operation(t, kind), kind), adExecApplied)
	}
	h.waitIdle(t)
	wantCommands := []string{instExecHostCommandLiteral, instExecStageCommandLiteral, instExecHostCommandLiteral, instExecHostCommandLiteral}
	if got := h.f.execCommands(); !slices.Equal(got, wantCommands) {
		t.Fatalf("exec commands %v, want %v", got, wantCommands)
	}
	if h.f.connCalls.Load() != 4 {
		t.Fatalf("connections = %d, want one per resource", h.f.connCalls.Load())
	}
	if h.stageAuth.calls.Load() != 1 || h.hostAuth.calls.Load() != 3 {
		t.Fatalf("authorizers: stage %d host %d, want 1 and 3", h.stageAuth.calls.Load(), h.hostAuth.calls.Load())
	}
	seen := h.hostAuth.seenRequests()
	kinds := make([]string, 0, len(seen))
	digests := map[string]bool{}
	for _, r := range seen {
		kinds = append(kinds, r.Intent.Kind)
		if digests[r.Challenge.MarkerSHA256] {
			t.Fatalf("two host actions share the marker digest %s", r.Challenge.MarkerSHA256)
		}
		digests[r.Challenge.MarkerSHA256] = true
	}
	if !slices.Equal(kinds, instExecHostKinds) {
		t.Fatalf("host actions authorized in order %v, want %v", kinds, instExecHostKinds)
	}
	// Each frame crossed the wire once, in order, after its own exec.
	stdin := h.f.stdinBytes()
	last := -1
	for _, kind := range instExecHostKinds {
		frame, err := EncodeProbeHostActionFrame(h.hostIntent(kind))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Count(stdin, frame) != 1 {
			t.Fatalf("%s action frame appears %d times in the stdin transcript", kind, bytes.Count(stdin, frame))
		}
		if at := bytes.Index(stdin, frame); at <= last {
			t.Fatalf("%s action frame is out of order in the transcript", kind)
		} else {
			last = at
		}
	}
	if h.binary.snapshotCloses.Load() != 0 {
		t.Fatalf("borrowed snapshot closed %d times", h.binary.snapshotCloses.Load())
	}
	if h.binary.opens.Load() != 1 || h.closes.Load() != 1 {
		t.Fatalf("payload opens %d closes %d, want exactly the one artifact stage", h.binary.opens.Load(), h.closes.Load())
	}

	// A resource that already applied cannot be applied again: not in flight.
	replay := h.operation(t, "service")
	replay.Resources[3].State = domain.ProbeInstallResourceApplied
	adExecRequireOutcome(t, h.apply(t, replay, "service"), adExecNotStarted)
	if h.f.execCalls.Load() != 4 {
		t.Fatalf("a replayed resource reached the host: %d execs", h.f.execCalls.Load())
	}
}

func TestHostActionIntentFromReceiptRebuildsExactIntent(t *testing.T) {
	h := newInstExecHarness(t, nil)
	for _, kind := range instExecHostKinds {
		op := h.operation(t, kind)
		// A different reserved attempt and owner are what must be read back.
		op.OwnerID = instExecOtherUUID
		index := slices.Index(instExecLayoutKinds, kind)
		op.Resources[index].AttemptID = "66666666-6666-4666-8666-666666666666"
		before := slices.Clone(op.Resources)

		intent, ok := hostActionIntentFromReceipt(op, kind)
		want := domain.ProbeHostActionIntent{Identity: op.Identity, OwnerID: instExecOtherUUID, AttemptID: "66666666-6666-4666-8666-666666666666", Kind: kind}
		if !ok || intent != want || !intent.Valid() {
			t.Fatalf("%s: %+v ok=%v, want %+v", kind, intent, ok, want)
		}
		if spec, found := intent.Resource(); !found || spec != op.Resources[index].Spec {
			t.Fatalf("%s: intent resource %+v is not the receipt's spec %+v", kind, spec, op.Resources[index].Spec)
		}
		if !slices.Equal(op.Resources, before) {
			t.Fatalf("%s: operation was mutated", kind)
		}
	}

	t.Run("later resources are not consulted", func(t *testing.T) {
		// The journal's reservation of this resource and the applied
		// predecessors decide admission; the artifact executor behaves the same.
		for _, kind := range []string{"data_directory", "unit_file"} {
			op := h.operation(t, kind)
			index := slices.Index(instExecLayoutKinds, kind)
			for i := index + 1; i < len(op.Resources); i++ {
				op.Resources[i].State = domain.ProbeInstallResourceUnknown
			}
			if _, ok := hostActionIntentFromReceipt(op, kind); !ok {
				t.Fatalf("%s refused over the state of a later resource", kind)
			}
		}
	})

	t.Run("never builds a host action for the artifact directory or a foreign kind", func(t *testing.T) {
		for _, kind := range []string{"artifact_directory", "", "image_cache", "container", "Service", "service ", "unit-file", "systemd"} {
			op := h.operation(t, "unit_file")
			if kind == "artifact_directory" {
				op = h.operation(t, "artifact_directory")
			}
			if intent, ok := hostActionIntentFromReceipt(op, kind); ok || intent != (domain.ProbeHostActionIntent{}) {
				t.Fatalf("kind %q built %+v", kind, intent)
			}
		}
	})

	t.Run("zero operation", func(t *testing.T) {
		for _, kind := range instExecHostKinds {
			if intent, ok := hostActionIntentFromReceipt(domain.ProbeInstallOperation{}, kind); ok || intent != (domain.ProbeHostActionIntent{}) {
				t.Fatalf("zero operation built %+v", intent)
			}
		}
	})
}

// instExecReceiptMutations is every way a receipt can fail to be the exact
// acknowledged reservation for kind. Each returns a mutated copy.
func instExecReceiptMutations(otherFP, kind string) map[string]func(op *domain.ProbeInstallOperation) {
	index := slices.Index(instExecLayoutKinds, kind)
	m := map[string]func(op *domain.ProbeInstallOperation){
		"docker mode":        func(op *domain.ProbeInstallOperation) { op.Identity.Mode = domain.ProbeInstallDocker },
		"empty mode":         func(op *domain.ProbeInstallOperation) { op.Identity.Mode = "" },
		"unknown mode":       func(op *domain.ProbeInstallOperation) { op.Identity.Mode = "podman" },
		"non-root ssh user":  func(op *domain.ProbeInstallOperation) { op.Identity.Host.User = "deploy" },
		"invalid layout":     func(op *domain.ProbeInstallOperation) { op.Identity.LayoutVersion = 0 },
		"newer layout":       func(op *domain.ProbeInstallOperation) { op.Identity.LayoutVersion = 2 },
		"zero actor":         func(op *domain.ProbeInstallOperation) { op.Identity.RequestedBy = 0 },
		"invalid binary pin": func(op *domain.ProbeInstallOperation) { op.Identity.Binary.Size = 0 },
		"empty owner":        func(op *domain.ProbeInstallOperation) { op.OwnerID = "" },
		"malformed owner":    func(op *domain.ProbeInstallOperation) { op.OwnerID = "not-an-id" },
		"empty attempt":      func(op *domain.ProbeInstallOperation) { op.Resources[index].AttemptID = "" },
		"malformed attempt":  func(op *domain.ProbeInstallOperation) { op.Resources[index].AttemptID = "not-an-id" },
		"uppercase attempt": func(op *domain.ProbeInstallOperation) {
			op.Resources[index].AttemptID = "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA"
		},
		"no resources":         func(op *domain.ProbeInstallOperation) { op.Resources = nil },
		"empty resources":      func(op *domain.ProbeInstallOperation) { op.Resources = []domain.ProbeInstallResource{} },
		"extra resource":       func(op *domain.ProbeInstallOperation) { op.Resources = append(op.Resources, op.Resources[3]) },
		"one resource missing": func(op *domain.ProbeInstallOperation) { op.Resources = op.Resources[:3] },
		"operation id no longer matches the references": func(op *domain.ProbeInstallOperation) {
			op.Identity.OperationID = instExecOtherUUID
		},
		"host key pin of another host": func(op *domain.ProbeInstallOperation) {
			op.Identity.HostKeyFingerprint = otherFP
		},
		"client key pin of another client": func(op *domain.ProbeInstallOperation) {
			op.Identity.ClientKeyFingerprint = otherFP
		},
		"this resource and a neighbor swapped": func(op *domain.ProbeInstallOperation) {
			j := (index + 1) % len(op.Resources)
			op.Resources[index], op.Resources[j] = op.Resources[j], op.Resources[index]
		},
	}
	for i := range instExecLayoutKinds {
		name := instExecLayoutKinds[i]
		m["spec kind altered for "+name] = func(op *domain.ProbeInstallOperation) { op.Resources[i].Spec.Kind = "image_cache" }
		m["spec reference altered for "+name] = func(op *domain.ProbeInstallOperation) { op.Resources[i].Spec.Reference += "/../x" }
		m["spec reference emptied for "+name] = func(op *domain.ProbeInstallOperation) { op.Resources[i].Spec.Reference = "" }
		m["spec ordinal altered for "+name] = func(op *domain.ProbeInstallOperation) { op.Resources[i].Spec.Ordinal = 9 }
		m["spec not owned for "+name] = func(op *domain.ProbeInstallOperation) { op.Resources[i].Spec.Owned = false }
	}
	m["reference of another operation"] = func(op *domain.ProbeInstallOperation) {
		other := op.Identity
		other.OperationID = instExecOtherUUID
		op.Resources[index].Spec.Reference = other.Resources()[index].Reference
	}
	m["reference outside the layout"] = func(op *domain.ProbeInstallOperation) { op.Resources[index].Spec.Reference = "/opt/other" }
	for _, s := range []string{domain.ProbeInstallResourcePlanned, domain.ProbeInstallResourceApplied, domain.ProbeInstallResourceNotStarted, domain.ProbeInstallResourceUnknown, "", "bogus"} {
		m["resource state "+fmt.Sprintf("%q", s)+" instead of in_flight"] = func(op *domain.ProbeInstallOperation) { op.Resources[index].State = s }
		for j := range index {
			if s == domain.ProbeInstallResourceApplied {
				continue
			}
			m[fmt.Sprintf("predecessor %s %q instead of applied", instExecLayoutKinds[j], s)] = func(op *domain.ProbeInstallOperation) { op.Resources[j].State = s }
		}
	}
	m["predecessor in flight instead of applied"] = func(op *domain.ProbeInstallOperation) {
		if index > 0 {
			op.Resources[index-1].State = domain.ProbeInstallResourceInFlight
		} else {
			op.Resources[index].State = domain.ProbeInstallResourceInFlight
			op.State = domain.ProbeInstallQueued
		}
	}
	for _, s := range []string{
		domain.ProbeInstallQueued, domain.ProbeInstallResourcesReady, domain.ProbeInstallCancelRequested,
		domain.ProbeInstallFailed, domain.ProbeInstallCanceled, domain.ProbeInstallInterrupted, "", "bogus",
	} {
		m["operation state "+fmt.Sprintf("%q", s)] = func(op *domain.ProbeInstallOperation) { op.State = s }
	}
	return m
}

func TestSSHProbeInstallExecutorRefusesMismatchedReceiptsWithNoIO(t *testing.T) {
	h := newInstExecHarness(t, nil)
	_, otherSigner := deploymentKey(t)
	otherFP := ssh.FingerprintSHA256(otherSigner.PublicKey())
	for _, kind := range instExecHostKinds {
		for name, mutate := range instExecReceiptMutations(otherFP, kind) {
			t.Run(kind+"/"+name, func(t *testing.T) {
				op := h.operation(t, kind)
				mutate(&op)
				got := h.apply(t, op, kind)
				if got != adExecNotStarted {
					t.Fatalf("outcome %+v, want not_started", got)
				}
				h.requireNoIO(t)
			})
		}
	}

	// The unmutated receipt proceeds: the refusals above are the receipt's
	// doing, not an inert fixture.
	for _, kind := range instExecHostKinds {
		t.Run("control "+kind, func(t *testing.T) {
			control := newInstExecHarness(t, nil)
			adExecRequireOutcome(t, control.apply(t, control.operation(t, kind), kind), adExecApplied)
		})
	}
}

// Exhaustive admission matrix over the states the executor reads. Only
// running + every predecessor applied + the kind in flight may proceed.
func TestSSHProbeInstallExecutorStateMatrixWithNoIO(t *testing.T) {
	h := newInstExecHarness(t, nil)
	operationStates := []string{
		domain.ProbeInstallQueued, domain.ProbeInstallRunning, domain.ProbeInstallResourcesReady,
		domain.ProbeInstallCancelRequested, domain.ProbeInstallFailed, domain.ProbeInstallCanceled,
		domain.ProbeInstallInterrupted, "", "bogus",
	}
	resourceStates := []string{
		domain.ProbeInstallResourcePlanned, domain.ProbeInstallResourceInFlight, domain.ProbeInstallResourceApplied,
		domain.ProbeInstallResourceNotStarted, domain.ProbeInstallResourceUnknown, "", "bogus",
	}
	for _, kind := range instExecHostKinds {
		index := slices.Index(instExecLayoutKinds, kind)
		checked := 0
		for _, opState := range operationStates {
			for _, own := range resourceStates {
				for _, pred := range resourceStates {
					if index == 0 && pred != resourceStates[0] {
						continue // data_directory has no predecessors; run each own/op state once
					}
					admitted := opState == domain.ProbeInstallRunning && own == domain.ProbeInstallResourceInFlight &&
						(index == 0 || pred == domain.ProbeInstallResourceApplied)
					if admitted {
						continue // the one admitted combination; see the dispatch test
					}
					op := h.operation(t, kind)
					op.State = opState
					for j := range index {
						op.Resources[j].State = pred
					}
					op.Resources[index].State = own
					if got := h.apply(t, op, kind); got != adExecNotStarted {
						t.Fatalf("%s: operation %q own %q predecessors %q: outcome %+v, want not_started", kind, opState, own, pred, got)
					}
					checked++
				}
			}
		}
		if checked == 0 {
			t.Fatalf("%s: nothing checked", kind)
		}
	}
	h.requireNoIO(t)
}

// An operation whose in-flight resource is another kind never lets a host
// action for the requested kind through: not in flight, or a predecessor that
// is not applied.
func TestSSHProbeInstallExecutorRefusesKindThatIsNotTheInFlightResource(t *testing.T) {
	h := newInstExecHarness(t, nil)
	for _, requested := range instExecLayoutKinds {
		for _, inFlight := range instExecLayoutKinds {
			if requested == inFlight {
				continue
			}
			op := h.operation(t, inFlight)
			if got := h.apply(t, op, requested); got != adExecNotStarted {
				t.Fatalf("requested %s while %s is in flight: %+v, want not_started", requested, inFlight, got)
			}
		}
	}
	h.requireNoIO(t)
}

// A genuine docker operation graph (image_cache, data_directory, container)
// shares the data_directory kind name with the systemd layout. No kind of a
// docker operation may reach the systemd host actions.
func TestSSHProbeInstallExecutorRefusesGenuineDockerOperationWithNoIO(t *testing.T) {
	h := newInstExecHarness(t, nil)
	docker := h.identity
	docker.Binary = domain.ProbeBinaryPin{}
	docker.Image = domain.ProbeImageSelection{
		Pin:            domain.ProbeImagePin{Repository: "ghcr.io/example/probe", Version: "1.2.3", Architecture: "amd64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 100},
		ManifestSHA256: strings.Repeat("b", 64),
		ConfigSHA256:   strings.Repeat("c", 64),
		LayerCount:     1,
	}
	docker.Mode = domain.ProbeInstallDocker
	specs := docker.Resources()
	if len(specs) != 3 || !slices.Equal(kindsOf(specs), []string{"image_cache", "data_directory", "container"}) {
		t.Fatalf("fixture docker layout %+v", specs)
	}
	for index, kind := range kindsOf(specs) {
		resources := make([]domain.ProbeInstallResource, len(specs))
		for i, spec := range specs {
			resources[i] = domain.ProbeInstallResource{Spec: spec, State: domain.ProbeInstallResourcePlanned}
			if i < index {
				resources[i].State = domain.ProbeInstallResourceApplied
				resources[i].AttemptID = instExecEarlierAttempt(i)
			}
		}
		resources[index].State = domain.ProbeInstallResourceInFlight
		resources[index].AttemptID = instExecAttemptID
		op := domain.ProbeInstallOperation{Identity: docker, OwnerID: h.owner, State: domain.ProbeInstallRunning, Resources: resources}
		for _, requested := range append([]string{"unit_file", "service", "artifact_directory"}, kindsOf(specs)...) {
			if got := h.apply(t, op, requested); got != adExecNotStarted {
				t.Fatalf("docker operation with %s in flight, requested %s: %+v, want not_started", kind, requested, got)
			}
		}
	}
	h.requireNoIO(t)
}

func TestSSHProbeInstallExecutorRefusesUnknownKindsWithNoIO(t *testing.T) {
	h := newInstExecHarness(t, nil)
	op := h.operation(t, "unit_file")
	for _, kind := range []string{
		"", "image_cache", "container", "systemd", "Artifact_Directory", "ARTIFACT_DIRECTORY", " artifact_directory", "artifact_directory\n",
		"Service", "SERVICE", " service", "service ", "service\n", "service\x00", "unit-file", "unit_file/", "../service", "Data_Directory", "data_directory ",
	} {
		if got := h.apply(t, op, kind); got != adExecNotStarted {
			t.Fatalf("kind %q: outcome %+v, want not_started", kind, got)
		}
	}
	h.requireNoIO(t)
}

func TestSSHProbeInstallExecutorNilAndZeroValuesAreNotStarted(t *testing.T) {
	h := newInstExecHarness(t, nil)
	var nilExec *SSHProbeInstallExecutor
	cases := map[string]ports.ProbeInstallResourceExecutor{
		"nil executor":  nilExec,
		"zero executor": &SSHProbeInstallExecutor{},
		"no action authorizer": &SSHProbeInstallExecutor{
			artifact: h.exec.artifact, sender: h.sender,
		},
		"typed nil action authorizer": &SSHProbeInstallExecutor{
			artifact: h.exec.artifact, sender: h.sender, actions: (*hostActSendAuthorizer)(nil),
		},
		"no sender":   &SSHProbeInstallExecutor{artifact: h.exec.artifact, actions: h.hostAuth},
		"no artifact": &SSHProbeInstallExecutor{sender: h.sender, actions: h.hostAuth},
	}
	for name, e := range cases {
		for _, kind := range instExecLayoutKinds {
			got := adExecApply(t, t.Context(), e, h.operation(t, kind), kind)
			if got != adExecNotStarted {
				t.Fatalf("%s/%s: outcome %+v, want not_started", name, kind, got)
			}
		}
	}
	h.requireNoIO(t)
}

func TestSSHProbeInstallExecutorCancelBeforeCallIsNotStartedWithNoIO(t *testing.T) {
	h := newInstExecHarness(t, nil)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	expired, stop := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer stop()
	var nilCtx *sendTNilContext
	for _, kind := range instExecHostKinds {
		for name, ctx := range map[string]context.Context{"canceled": canceled, "expired": expired, "typed nil": nilCtx} {
			if got := adExecApply(t, ctx, h.exec, h.operation(t, kind), kind); got != adExecNotStarted {
				t.Fatalf("%s/%s: outcome %+v, want not_started", kind, name, got)
			}
		}
	}
	h.requireNoIO(t)
}

func TestSSHProbeInstallExecutorHostActionAuthorizerRefusalIsNotStarted(t *testing.T) {
	cases := []struct {
		name string
		hook func(ctx context.Context, a *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error)
	}{
		{"plain refusal", func(context.Context, *hostActSendAuthorizer, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
			return domain.ProbeHostActionGrant{}, errors.New("journal attempt already consumed")
		}},
		{"context canceled sentinel", func(context.Context, *hostActSendAuthorizer, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
			return domain.ProbeHostActionGrant{}, context.Canceled
		}},
		{"domain sentinel", func(context.Context, *hostActSendAuthorizer, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
			return domain.ProbeHostActionGrant{}, domain.ErrProbeArtifactStorage
		}},
		{"grant for a different request", func(_ context.Context, a *hostActSendAuthorizer, r domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
			r.Challenge.Nonce[0] ^= 0xff
			return hostActSendSign(a.authority, r), nil
		}},
		{"zero grant without an error", func(context.Context, *hostActSendAuthorizer, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
			return domain.ProbeHostActionGrant{}, nil
		}},
	}
	for _, kind := range instExecHostKinds {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				h := newInstExecHarness(t, func(h *instExecHarness) { h.hostAuth.hook = tc.hook })
				got := h.apply(t, h.operation(t, kind), kind)
				adExecRequireOutcome(t, got, adExecNotStarted)
				h.requireNoStageInput(t)
				if h.hostAuth.calls.Load() != 1 || h.stageAuth.calls.Load() != 0 {
					t.Fatalf("authorizers: host %d stage %d, want exactly 1 and 0 (no retry)", h.hostAuth.calls.Load(), h.stageAuth.calls.Load())
				}
			})
		}
	}
}

func TestSSHProbeInstallExecutorHelperRefusalBeforeStdinIsNotStarted(t *testing.T) {
	cases := []struct {
		name string
		tune func(helper *hostActSendHelper)
	}{
		{"helper exits 1 without a challenge", func(helper *hostActSendHelper) {
			helper.noChallenge = true
			helper.noChallengeStatus = 1
		}},
		{"helper exits 0 without a challenge", func(helper *hostActSendHelper) { helper.noChallenge = true }},
		{"malformed challenge line", func(helper *hostActSendHelper) {
			helper.challengeFn = func(*sendTFixture) []byte { return []byte("this is not a challenge line\n") }
		}},
	}
	for _, kind := range instExecHostKinds {
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				h := newInstExecHarness(t, func(h *instExecHarness) { tc.tune(h.hostHelper) })
				adExecRequireOutcome(t, h.apply(t, h.operation(t, kind), kind), adExecNotStarted)
				h.requireNoStageInput(t)
				if h.hostAuth.calls.Load() != 0 || h.stageAuth.calls.Load() != 0 {
					t.Fatalf("authorizers reached: host %d stage %d", h.hostAuth.calls.Load(), h.stageAuth.calls.Load())
				}
			})
		}
	}
}

// Anything after the first stdin byte is outcome_unknown: the helper may have
// applied the action.
func TestSSHProbeInstallExecutorHelperFailureAfterGrantIsUnknown(t *testing.T) {
	type step = func(h *hostActSendHelper)
	otherKind := map[string]string{"data_directory": "service", "unit_file": "data_directory", "service": "unit_file"}
	for _, kind := range instExecHostKinds {
		cases := []struct {
			name string
			tune step
		}{
			{"nonzero exit with a valid receipt", func(h *hostActSendHelper) { h.exit = sendTExit() }},
			{"nonzero exit with no receipt", func(h *hostActSendHelper) {
				h.exit = sendTExit()
				h.tail = func(*sendTFixture, domain.ProbeHostActionIntent, []byte) string { return "" }
			}},
			{"signal exit", func(h *hostActSendHelper) { h.signal = "TERM" }},
			{"missing exit status", func(h *hostActSendHelper) { h.noExit = true }},
			{"no receipt", func(h *hostActSendHelper) {
				h.tail = func(*sendTFixture, domain.ProbeHostActionIntent, []byte) string { return "" }
			}},
			{"receipt for another kind", func(h *hostActSendHelper) {
				h.tail = func(f *sendTFixture, _ domain.ProbeHostActionIntent, _ []byte) string {
					return string(EncodeProbeHostActionReceipt(hostActSendIntentFor(f, otherKind[kind])))
				}
			}},
			{"receipt for another operation", func(h *hostActSendHelper) {
				h.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
					intent.Identity.OperationID = instExecOtherUUID
					return string(EncodeProbeHostActionReceipt(intent))
				}
			}},
			{"truncated receipt", func(h *hostActSendHelper) {
				h.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
					return string(EncodeProbeHostActionReceipt(intent))[:20]
				}
			}},
			{"receipt followed by an extra byte", func(h *hostActSendHelper) {
				h.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
					return string(EncodeProbeHostActionReceipt(intent)) + "x"
				}
			}},
		}
		for _, tc := range cases {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				h := newInstExecHarness(t, func(h *instExecHarness) { tc.tune(h.hostHelper) })
				got := h.apply(t, h.operation(t, kind), kind)
				adExecRequireOutcome(t, got, adExecUnknown)
				h.waitIdle(t)

				// The whole input crossed the boundary exactly once.
				frame, err := EncodeProbeHostActionFrame(h.hostIntent(kind))
				if err != nil {
					t.Fatal(err)
				}
				if stdin := h.f.stdinBytes(); len(stdin) != len(frame)+ProbeBinaryStageGrantFrameSize || !bytes.HasPrefix(stdin, frame) {
					t.Fatalf("server stdin = %d bytes", len(stdin))
				}
				if h.hostAuth.calls.Load() != 1 || h.stageAuth.calls.Load() != 0 {
					t.Fatalf("authorizers: host %d stage %d", h.hostAuth.calls.Load(), h.stageAuth.calls.Load())
				}
				if h.f.connCalls.Load() != 1 || h.f.execCalls.Load() != 1 {
					t.Fatalf("connections %d execs %d, want 1 and 1 (no retry)", h.f.connCalls.Load(), h.f.execCalls.Load())
				}
			})
		}
	}
}

func TestSSHProbeInstallExecutorHelperRejectingGrantIsUnknown(t *testing.T) {
	forger := newAuthSendAuthority(t)
	for _, kind := range instExecHostKinds {
		t.Run(kind, func(t *testing.T) {
			h := newInstExecHarness(t, func(h *instExecHarness) {
				h.hostAuth.hook = func(_ context.Context, _ *hostActSendAuthorizer, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
					return hostActSendSign(forger, request), nil
				}
			})
			adExecRequireOutcome(t, h.apply(t, h.operation(t, kind), kind), adExecUnknown)
			h.waitIdle(t)
			if h.hostHelper.verified.Load() != 2 {
				t.Fatalf("helper verdict %d, want the forged grant rejected", h.hostHelper.verified.Load())
			}
		})
	}
}

// Once the first stdin byte has been delivered, cancellation can no longer
// prove the host untouched: it must report unknown, never not_started.
func TestSSHProbeInstallExecutorCancelAfterStdinIsUnknown(t *testing.T) {
	for _, kind := range instExecHostKinds {
		t.Run(kind, func(t *testing.T) {
			release := make(chan struct{})
			var releaseOnce sync.Once
			releaseHelper := func() { releaseOnce.Do(func() { close(release) }) }
			h := newInstExecHarness(t, func(h *instExecHarness) {
				h.hostHelper.tail = func(_ *sendTFixture, intent domain.ProbeHostActionIntent, _ []byte) string {
					<-release
					return string(EncodeProbeHostActionReceipt(intent))
				}
			})
			// Registered after the fixture's own cleanup, so it runs first and
			// lets the held helper emulation finish before teardown.
			t.Cleanup(releaseHelper)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := adExecApplyAsync(ctx, h.exec, h.operation(t, kind), kind)
			adExecWaitStdinEOF(t, h.f)
			adExecExpectBlocked(t, done, "apply while the helper holds its receipt")
			cancel()
			adExecRequireOutcome(t, adExecAwait(t, done), adExecUnknown)
			releaseHelper()
			h.waitIdle(t)
		})
	}
}

// Cancellation while the authorizer blocks is pre-stdin: not_started.
func TestSSHProbeInstallExecutorCancelDuringAuthorizationIsNotStarted(t *testing.T) {
	h := newInstExecHarness(t, func(h *instExecHarness) { h.hostAuth.hook = hostActSendBlockUntilDone })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := adExecApplyAsync(ctx, h.exec, h.operation(t, "unit_file"), "unit_file")
	select {
	case <-h.hostAuth.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("authorizer not reached")
	}
	adExecExpectBlocked(t, done, "apply while the authorizer blocks")
	cancel()
	adExecRequireOutcome(t, adExecAwait(t, done), adExecNotStarted)
	h.requireNoStageInput(t)
}

// The artifact_directory path of the combined executor keeps the artifact
// executor's receipt rules and never reaches the host-action helper or its
// authorizer.
func TestSSHProbeInstallExecutorArtifactDirectoryPathIsUnchanged(t *testing.T) {
	h := newInstExecHarness(t, nil)
	for name, mutate := range map[string]func(op *domain.ProbeInstallOperation){
		"data_directory not applied": func(op *domain.ProbeInstallOperation) { op.Resources[0].State = domain.ProbeInstallResourcePlanned },
		"artifact not in flight":     func(op *domain.ProbeInstallOperation) { op.Resources[1].State = domain.ProbeInstallResourcePlanned },
		"empty attempt":              func(op *domain.ProbeInstallOperation) { op.Resources[1].AttemptID = "" },
		"docker mode":                func(op *domain.ProbeInstallOperation) { op.Identity.Mode = domain.ProbeInstallDocker },
		"operation not running":      func(op *domain.ProbeInstallOperation) { op.State = domain.ProbeInstallQueued },
		"layout altered": func(op *domain.ProbeInstallOperation) {
			op.Resources[2].Spec.Reference = "/etc/systemd/system/evil.service"
		},
		"another binary pin": func(op *domain.ProbeInstallOperation) {
			op.Identity.Binary = sendTPin([]byte("another pinned payload"))
		},
		"service asked via artifact": func(op *domain.ProbeInstallOperation) { op.Resources[1].Spec.Kind = "service" },
	} {
		op := h.operation(t, "artifact_directory")
		mutate(&op)
		if got := h.apply(t, op, "artifact_directory"); got != adExecNotStarted {
			t.Fatalf("%s: outcome %+v, want not_started", name, got)
		}
	}
	h.requireNoIO(t)

	// And the control proceeds through the stage path only.
	control := newInstExecHarness(t, nil)
	adExecRequireOutcome(t, control.apply(t, control.operation(t, "artifact_directory"), "artifact_directory"), adExecApplied)
	control.waitIdle(t)
	if control.hostAuth.calls.Load() != 0 || control.hostHelper.started.Load() != 0 {
		t.Fatalf("artifact_directory reached the host-action helper: auth %d started %d", control.hostAuth.calls.Load(), control.hostHelper.started.Load())
	}
	if strings.Contains(strings.Join(control.f.execCommands(), "\n"), "host-action") {
		t.Fatalf("exec commands %v", control.f.execCommands())
	}
}
