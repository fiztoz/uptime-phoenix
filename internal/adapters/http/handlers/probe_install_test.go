package handlers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	installOpID    = "7b0d6c4e-2a53-4f7b-9d3e-5c1a8e4f6b20"
	installOwnerID = "0c9a1f3e-6d27-4b85-a4e1-3f5b7c9d2e18"
	installAddress = "edge.example.test:22"
	// installKeyCanary must never appear in a response or receipt.
	installKeyCanary = "test-install-transient-key-canary"
	// installLeakMarker is a fake underlying diagnostic that must never reach
	// any response.
	installLeakMarker = "install-leaked-internal-detail-marker"
	installSHA        = "abababababababababababababababababababababababababababababababab"
	installSize       = 1048576
	installPath       = "/api/probe-installs"
)

var (
	// The host and client fingerprints are distinct so a leak of either is
	// detectable. Both are canonical OpenSSH SHA256 fingerprints.
	installHostPin   = "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	installClientPin = "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	installCreated   = time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
)

// installSecrets are the strings no response may ever contain.
func installSecrets() []string {
	return []string{
		installKeyCanary, "edge.example.test", installHostPin, installClientPin, installOwnerID,
		installSHA, "a1b2c3d4-0000-4000-8000", "/var/lib/", "/opt/uptime-phoenix", "/etc/systemd",
		installLeakMarker, "requested_by", "owner_id", "attempt_id", "reference",
	}
}

// installFake captures the handler's service contract. It copies the borrowed
// private-key bytes during the call so the test can both verify the exact key
// handed over and observe the handler's best-effort buffer clearing.
type installFake struct {
	submission  domain.ProbeInstallSubmission
	operationID string
	keyCopy     []byte
	op          *domain.ProbeInstallOperation
	err         error
	submits     int
	observes    int
	cancels     int
}

func (f *installFake) calls() int { return f.submits + f.observes + f.cancels }

func (f *installFake) Submit(_ context.Context, submission domain.ProbeInstallSubmission) (*domain.ProbeInstallOperation, error) {
	f.submits++
	f.submission = submission
	f.keyCopy = append([]byte(nil), submission.PrivateKey...)
	if f.err != nil {
		return nil, f.err
	}
	return f.op, nil
}

func (f *installFake) Observe(_ context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	f.observes++
	f.operationID = operationID
	if f.err != nil {
		return nil, f.err
	}
	return f.op, nil
}

func (f *installFake) Cancel(_ context.Context, operationID string) (*domain.ProbeInstallOperation, error) {
	f.cancels++
	f.operationID = operationID
	if f.err != nil {
		return nil, f.err
	}
	return f.op, nil
}

// installPin is the operator pin carried by every receipt and request.
func installPin() domain.ProbeBinaryPin {
	return domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "arm64", SHA256: installSHA, Size: installSize}
}

func installIdentity() domain.ProbeInstallIdentity {
	return domain.ProbeInstallIdentity{
		OperationID:          installOpID,
		Host:                 domain.ProbeSSHHost{Address: installAddress, User: "root"},
		HostKeyFingerprint:   installHostPin,
		ClientKeyFingerprint: installClientPin,
		RequestedBy:          7,
		LayoutVersion:        domain.ProbeInstallLayoutVersion,
		Mode:                 domain.ProbeInstallSystemd,
		Binary:               installPin(),
	}
}

func installAt(seconds int) time.Time {
	return installCreated.Add(time.Duration(seconds) * time.Second)
}

func installAttempt(n int) string { return fmt.Sprintf("a1b2c3d4-0000-4000-8000-%012d", n) }

// installOperation builds a domain.Valid systemd receipt. resourceStates lists
// the leading resource states; the rest stay planned. An unknown resource
// carries the operation code, as the journal requires.
func installOperation(state, code string, resourceStates ...string) *domain.ProbeInstallOperation {
	identity := installIdentity()
	operation := &domain.ProbeInstallOperation{
		Identity:   identity,
		OwnerID:    installOwnerID,
		State:      state,
		ErrorCode:  code,
		CreatedAt:  installCreated,
		UpdatedAt:  installCreated,
		DeadlineAt: installCreated.Add(domain.ProbeInstallLifetime),
	}
	for i, spec := range identity.Resources() {
		resource := domain.ProbeInstallResource{Spec: spec, State: domain.ProbeInstallResourcePlanned}
		if i < len(resourceStates) && resourceStates[i] != domain.ProbeInstallResourcePlanned {
			resource.State = resourceStates[i]
			resource.AttemptID = installAttempt(i + 1)
			resource.StartedAt = installAt(10*i + 1)
			operation.UpdatedAt = resource.StartedAt
			if resource.State != domain.ProbeInstallResourceInFlight {
				resource.FinishedAt = installAt(10*i + 2)
				operation.UpdatedAt = resource.FinishedAt
			}
			switch resource.State {
			case domain.ProbeInstallResourceNotStarted:
				resource.ErrorCode = "not_started"
			case domain.ProbeInstallResourceUnknown:
				resource.ErrorCode = code
			}
		}
		operation.Resources = append(operation.Resources, resource)
	}
	return operation
}

// installReceipt builds a domain.Valid receipt for a lifecycle state.
func installReceipt(state string) *domain.ProbeInstallOperation {
	applied, inFlight := domain.ProbeInstallResourceApplied, domain.ProbeInstallResourceInFlight
	switch state {
	case domain.ProbeInstallQueued:
		return installOperation(state, "")
	case domain.ProbeInstallRunning, domain.ProbeInstallCancelRequested:
		return installOperation(state, "", applied, inFlight)
	case domain.ProbeInstallResourcesReady:
		return installOperation(state, "", applied, applied, applied, applied)
	case domain.ProbeInstallFailed:
		return installOperation(state, "execution_failed", applied, domain.ProbeInstallResourceNotStarted)
	case domain.ProbeInstallCanceled:
		return installOperation(state, "canceled", applied, domain.ProbeInstallResourceNotStarted)
	case domain.ProbeInstallInterrupted:
		return installOperation(state, "owner_interrupted", applied, domain.ProbeInstallResourceUnknown)
	}
	panic("unknown install state " + state)
}

// installMember is one ordered JSON member of the request body.
type installMember struct{ key, raw string }

// installMembers is the frozen nine-member request in canonical order.
func installMembers(privateKey string) []installMember {
	return []installMember{
		{"operation_id", strconv.Quote(installOpID)},
		{"ssh_address", strconv.Quote(installAddress)},
		{"ssh_user", strconv.Quote("root")},
		{"host_key_fingerprint", strconv.Quote(installHostPin)},
		{"private_key", strconv.Quote(privateKey)},
		{"binary_version", strconv.Quote("1.2.3")},
		{"binary_architecture", strconv.Quote("arm64")},
		{"binary_sha256", strconv.Quote(installSHA)},
		{"binary_size", strconv.Itoa(installSize)},
	}
}

func installJSON(members []installMember) string {
	parts := make([]string, len(members))
	for i, m := range members {
		parts[i] = strconv.Quote(m.key) + ":" + m.raw
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func installBody(privateKey string) string { return installJSON(installMembers(privateKey)) }

func installSet(members []installMember, key, raw string) []installMember {
	out := append([]installMember(nil), members...)
	for i := range out {
		if out[i].key == key {
			out[i].raw = raw
		}
	}
	return out
}

func installDrop(members []installMember, key string) []installMember {
	var out []installMember
	for _, m := range members {
		if m.key != key {
			out = append(out, m)
		}
	}
	return out
}

func installRename(members []installMember, from, to string) []installMember {
	out := append([]installMember(nil), members...)
	for i := range out {
		if out[i].key == from {
			out[i].key = to
		}
	}
	return out
}

// installServeAs invokes one handler method with the given authenticated
// actor (0 means no actor in context), mirroring the router's param wiring
// without claiming middleware coverage. It also asserts the universal
// response invariants: no-store caching and no secret in any response body.
func installServeAs(t *testing.T, actor int64, method, path, id, body string, call func(echo.Context) error) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c := e.NewContext(req, rec)
	if id != "" {
		c.SetParamNames("operation_id")
		c.SetParamValues(id)
	}
	if actor != 0 {
		c.Set(ContextUserIDKey, actor)
	}
	if err := call(c); err != nil {
		e.HTTPErrorHandler(err, c)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (status %d)", got, rec.Code)
	}
	response := rec.Body.String()
	for _, secret := range installSecrets() {
		if strings.Contains(response, secret) {
			t.Fatalf("response leaks %q: %s", secret, response)
		}
	}
	return rec
}

func installServe(t *testing.T, method, path, id, body string, call func(echo.Context) error) *httptest.ResponseRecorder {
	t.Helper()
	return installServeAs(t, 1, method, path, id, body, call)
}

func installErrorBody(t *testing.T, rec *httptest.ResponseRecorder) regionalErrorView {
	t.Helper()
	var view regionalErrorView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("error body is not the fixed error view: %v (%s)", err, rec.Body.String())
	}
	return view
}

func installKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func installSameKeys(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s keys = %v, want exactly %v", what, got, want)
	}
}

// TestProbeInstallSubmitStrictBinding refuses every wire shape outside the
// frozen nine-member object and never reaches the service with rejected input.
func TestProbeInstallSubmitStrictBinding(t *testing.T) {
	base := installMembers(installKeyCanary)
	type bindCase struct {
		name, body string
		status     int
		code       string
	}
	cases := []bindCase{
		{"empty", "", 400, "invalid_install"},
		{"malformed", `{`, 400, "invalid_install"},
		{"truncated member", `{"operation_id":`, 400, "invalid_install"},
		{"not an object", `[]`, 400, "invalid_install"},
		{"null body", `null`, 400, "invalid_install"},
		{"empty object", `{}`, 400, "invalid_install"},
		{"unknown key", installJSON(append(append([]installMember(nil), base...), installMember{"requested_by", "1"})), 400, "invalid_install"},
		{"actor smuggled in body", installJSON(append(append([]installMember(nil), base...), installMember{"requested_by", `"1"`})), 400, "invalid_install"},
		{"server-local artifact path", installJSON(append(append([]installMember(nil), base...), installMember{"binary_path", `"/tmp/probe"`})), 400, "invalid_install"},
		{"trailing object", installBody(installKeyCanary) + ` {}`, 400, "invalid_install"},
		{"trailing garbage", installBody(installKeyCanary) + `garbage`, 400, "invalid_install"},
		{"trailing comma", strings.TrimSuffix(installBody(installKeyCanary), "}") + ",}", 400, "invalid_install"},
		{"oversized body", strings.Repeat("a", 128<<10+1), 413, "install_too_large"},
		{"oversized valid prefix", installBody(installKeyCanary) + strings.Repeat(" ", 128<<10), 413, "install_too_large"},
		{"oversized member name", `{"` + strings.Repeat("k", 128<<10) + `":"x"}`, 413, "install_too_large"},
		{"oversized private key", installBody(strings.Repeat("k", domain.MaxProbeInstallPrivateKeySize+1)), 400, "invalid_install"},
		{"empty private key", installJSON(installSet(base, "private_key", `""`)), 400, "invalid_install"},
		{"invalid operation id", installJSON(installSet(base, "operation_id", `"not-a-uuid"`)), 400, "invalid_install"},
		{"uppercase operation id", installJSON(installSet(base, "operation_id", strconv.Quote(strings.ToUpper(installOpID)))), 400, "invalid_install"},
		{"local probe operation id", installJSON(installSet(base, "operation_id", `"local"`)), 400, "invalid_install"},
		{"address without port", installJSON(installSet(base, "ssh_address", `"edge.example.test"`)), 400, "invalid_install"},
		{"address port zero", installJSON(installSet(base, "ssh_address", `"edge.example.test:0"`)), 400, "invalid_install"},
		{"empty address", installJSON(installSet(base, "ssh_address", `""`)), 400, "invalid_install"},
		{"non-canonical fingerprint", installJSON(installSet(base, "host_key_fingerprint", `"not-a-fingerprint"`)), 400, "invalid_install"},
		{"md5 style fingerprint", installJSON(installSet(base, "host_key_fingerprint", `"MD5:aa:bb"`)), 400, "invalid_install"},
		{"empty fingerprint", installJSON(installSet(base, "host_key_fingerprint", `""`)), 400, "invalid_install"},
		{"pin empty version", installJSON(installSet(base, "binary_version", `""`)), 400, "invalid_install"},
		{"pin v-prefixed version", installJSON(installSet(base, "binary_version", `"v1.2.3"`)), 400, "invalid_install"},
		{"pin path-like version", installJSON(installSet(base, "binary_version", `"../1.2.3"`)), 400, "invalid_install"},
		{"pin overlong version", installJSON(installSet(base, "binary_version", strconv.Quote("1.2.3-"+strings.Repeat("a", 64)))), 400, "invalid_install"},
		{"pin unsupported architecture", installJSON(installSet(base, "binary_architecture", `"x86_64"`)), 400, "invalid_install"},
		{"pin uppercase architecture", installJSON(installSet(base, "binary_architecture", `"ARM64"`)), 400, "invalid_install"},
		{"pin short digest", installJSON(installSet(base, "binary_sha256", strconv.Quote(installSHA[:63]))), 400, "invalid_install"},
		{"pin uppercase digest", installJSON(installSet(base, "binary_sha256", strconv.Quote(strings.ToUpper(installSHA)))), 400, "invalid_install"},
		{"pin non-hex digest", installJSON(installSet(base, "binary_sha256", strconv.Quote(strings.Repeat("z", 64)))), 400, "invalid_install"},
		{"pin zero size", installJSON(installSet(base, "binary_size", "0")), 400, "invalid_install"},
		{"pin oversized binary", installJSON(installSet(base, "binary_size", strconv.FormatInt(domain.MaxProbeBinarySize+1, 10))), 400, "invalid_install"},
	}
	// Non-root users: the installer is root-only.
	for _, user := range []string{"ubuntu", "Root", "ROOT", "root ", "administrator", "", "root\n", "root:x"} {
		cases = append(cases, bindCase{"non-root user " + strconv.Quote(user), installJSON(installSet(base, "ssh_user", strconv.Quote(user))), 400, "invalid_install"})
	}
	// Every member: missing, null, duplicate, case variant, wrong JSON type.
	for _, m := range base {
		cases = append(cases,
			bindCase{"missing " + m.key, installJSON(installDrop(base, m.key)), 400, "invalid_install"},
			bindCase{"null " + m.key, installJSON(installSet(base, m.key, "null")), 400, "invalid_install"},
			bindCase{"duplicate " + m.key, installJSON(append(append([]installMember(nil), base...), m)), 400, "invalid_install"},
			bindCase{"upper case variant " + m.key, installJSON(installRename(base, m.key, strings.ToUpper(m.key))), 400, "invalid_install"},
			bindCase{"title case variant " + m.key, installJSON(installRename(base, m.key, strings.ToUpper(m.key[:1])+m.key[1:])), 400, "invalid_install"},
			bindCase{"padded key " + m.key, installJSON(installRename(base, m.key, m.key+" ")), 400, "invalid_install"},
		)
		wrong := []string{`42`, `true`, `[]`, `{}`, `["x"]`, `{"a":1}`}
		if m.key == "binary_size" {
			wrong = []string{`"1048576"`, `true`, `[]`, `{}`, `[1048576]`, `""`}
		}
		for _, raw := range wrong {
			cases = append(cases, bindCase{"wrong type " + m.key + "=" + raw, installJSON(installSet(base, m.key, raw)), 400, "invalid_install"})
		}
	}
	// binary_size must be a plain non-negative base-10 integer literal.
	for _, literal := range []string{
		"-1", "-0", "+1", "1.5", "1.0", "1048576.0", "1048576.", ".5", "1e3", "1E3", "1e+3", "1e-3", "1.048576e6",
		"01", "0x10", "9223372036854775808", "99999999999999999999", "NaN", "Infinity",
	} {
		cases = append(cases, bindCase{"binary_size literal " + literal, installJSON(installSet(base, "binary_size", literal)), 400, "invalid_install"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &installFake{op: installReceipt(domain.ProbeInstallQueued)}
			h := NewProbeInstallHandlers(fake, true)
			rec := installServe(t, http.MethodPost, installPath, "", tc.body, h.Submit)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if view := installErrorBody(t, rec); view.Code != tc.code {
				t.Fatalf("code = %q, want %q", view.Code, tc.code)
			}
			if fake.calls() != 0 {
				t.Fatalf("service called %d times for rejected input", fake.calls())
			}
		})
	}
}

// TestProbeInstallSubmitAcceptedBoundaries accepts the largest legal key and
// pin and the documented whitespace and key-order freedoms of one JSON object.
func TestProbeInstallSubmitAcceptedBoundaries(t *testing.T) {
	base := installMembers(installKeyCanary)
	reversed := make([]installMember, len(base))
	for i, m := range base {
		reversed[len(base)-1-i] = m
	}
	for _, tc := range []struct {
		name, body string
		check      func(t *testing.T, got domain.ProbeInstallSubmission)
	}{
		{"largest private key", installBody(strings.Repeat("k", domain.MaxProbeInstallPrivateKeySize)), func(t *testing.T, got domain.ProbeInstallSubmission) {
			if len(got.PrivateKey) != domain.MaxProbeInstallPrivateKeySize {
				t.Fatalf("key length = %d", len(got.PrivateKey))
			}
		}},
		{"largest binary", installJSON(installSet(base, "binary_size", strconv.FormatInt(domain.MaxProbeBinarySize, 10))), func(t *testing.T, got domain.ProbeInstallSubmission) {
			if got.Binary.Size != domain.MaxProbeBinarySize {
				t.Fatalf("size = %d", got.Binary.Size)
			}
		}},
		{"smallest binary", installJSON(installSet(base, "binary_size", "1")), func(t *testing.T, got domain.ProbeInstallSubmission) {
			if got.Binary.Size != 1 {
				t.Fatalf("size = %d", got.Binary.Size)
			}
		}},
		{"reordered members with whitespace", " \n" + strings.ReplaceAll(installJSON(reversed), ",", " ,\n ") + " \n", nil},
		{"escaped key spelling", strings.Replace(installBody(installKeyCanary), `"operation_id"`, `"operation_id"`, 1), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &installFake{op: installReceipt(domain.ProbeInstallQueued)}
			h := NewProbeInstallHandlers(fake, true)
			rec := installServe(t, http.MethodPost, installPath, "", tc.body, h.Submit)
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body.String())
			}
			if fake.submits != 1 {
				t.Fatalf("submits = %d, want 1", fake.submits)
			}
			if tc.check != nil {
				got := fake.submission
				got.PrivateKey = fake.keyCopy
				tc.check(t, got)
			}
		})
	}
}

// TestProbeInstallSubmitAccepted verifies the 202-after-commit contract, the
// exact explicit view, actor-from-context, the full submission mapping and the
// best-effort clearing of the owned private-key buffer.
func TestProbeInstallSubmitAccepted(t *testing.T) {
	fake := &installFake{op: installReceipt(domain.ProbeInstallQueued)}
	h := NewProbeInstallHandlers(fake, true)
	rec := installServeAs(t, 42, http.MethodPost, installPath, "", installBody(installKeyCanary), h.Submit)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if fake.submits != 1 || fake.calls() != 1 {
		t.Fatalf("service calls = %d (submits %d), want exactly one submit", fake.calls(), fake.submits)
	}
	// The actor comes only from the authenticated context.
	if fake.submission.RequestedBy != 42 {
		t.Fatalf("RequestedBy = %d, want context actor 42", fake.submission.RequestedBy)
	}
	want := domain.ProbeInstallSubmission{
		OperationID:        installOpID,
		Host:               domain.ProbeSSHHost{Address: installAddress, User: "root"},
		HostKeyFingerprint: installHostPin,
		Binary:             installPin(),
		RequestedBy:        42,
	}
	got := fake.submission
	got.PrivateKey = nil
	if got.OperationID != want.OperationID || got.Host != want.Host || got.HostKeyFingerprint != want.HostKeyFingerprint ||
		got.Binary != want.Binary || got.RequestedBy != want.RequestedBy {
		t.Fatalf("submission mapping = %+v, want %+v", got, want)
	}
	if string(fake.keyCopy) != installKeyCanary {
		t.Fatalf("service key = %q, want the transient key content", fake.keyCopy)
	}
	for _, b := range fake.submission.PrivateKey {
		if b != 0 {
			t.Fatal("owned private-key buffer was not cleared after the call")
		}
	}
	var view struct {
		OperationID string `json:"operation_id"`
		Kind        string `json:"kind"`
		Mode        string `json:"mode"`
		Status      string `json:"status"`
		CreatedAt   string `json:"created_at"`
		UpdatedAt   string `json:"updated_at"`
		DeadlineAt  string `json:"deadline_at"`
		Error       any    `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.OperationID != installOpID || view.Kind != "ssh_probe_install" || view.Mode != "systemd" || view.Status != domain.ProbeInstallQueued {
		t.Fatalf("view identity fields = %+v", view)
	}
	if view.CreatedAt != "2026-10-16T10:00:00Z" || view.UpdatedAt != "2026-10-16T10:00:00Z" || view.DeadlineAt != "2026-10-16T10:15:00Z" {
		t.Fatalf("timestamps not rendered as UTC: %+v", view)
	}
	if view.Error != nil {
		t.Fatalf("in-progress receipt must render null error: %s", rec.Body.String())
	}
}

// TestProbeInstallSubmitStatuses pins the 202/200 split: only a receipt that
// still has work to do answers 202; a settled receipt (exact retry) is 200.
func TestProbeInstallSubmitStatuses(t *testing.T) {
	for _, tc := range []struct {
		state  string
		status int
	}{
		{domain.ProbeInstallQueued, 202},
		{domain.ProbeInstallRunning, 202},
		{domain.ProbeInstallCancelRequested, 202},
		{domain.ProbeInstallResourcesReady, 200},
		{domain.ProbeInstallFailed, 200},
		{domain.ProbeInstallCanceled, 200},
		{domain.ProbeInstallInterrupted, 200},
	} {
		t.Run(tc.state, func(t *testing.T) {
			fake := &installFake{op: installReceipt(tc.state)}
			h := NewProbeInstallHandlers(fake, true)
			rec := installServe(t, http.MethodPost, installPath, "", installBody(installKeyCanary), h.Submit)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			var view struct {
				Status string `json:"status"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.Status != tc.state {
				t.Fatalf("status field = %q, want the stored state %q", view.Status, tc.state)
			}
		})
	}
}

// installAllReceipts enumerates one valid receipt per reachable lifecycle
// shape, including all three interruption causes.
func installAllReceipts() map[string]*domain.ProbeInstallOperation {
	applied, unknown := domain.ProbeInstallResourceApplied, domain.ProbeInstallResourceUnknown
	expired := installOperation(domain.ProbeInstallInterrupted, "deadline_expired", applied, domain.ProbeInstallResourceInFlight)
	expired.Resources[1].State = unknown
	expired.Resources[1].ErrorCode = "deadline_expired"
	expired.Resources[1].FinishedAt = expired.DeadlineAt
	expired.UpdatedAt = expired.DeadlineAt
	return map[string]*domain.ProbeInstallOperation{
		"queued":                  installReceipt(domain.ProbeInstallQueued),
		"running":                 installReceipt(domain.ProbeInstallRunning),
		"cancel_requested":        installReceipt(domain.ProbeInstallCancelRequested),
		"resources_ready":         installReceipt(domain.ProbeInstallResourcesReady),
		"failed":                  installReceipt(domain.ProbeInstallFailed),
		"canceled":                installReceipt(domain.ProbeInstallCanceled),
		"interrupted owner":       installReceipt(domain.ProbeInstallInterrupted),
		"interrupted outcome":     installOperation(domain.ProbeInstallInterrupted, "outcome_unknown", applied, unknown),
		"interrupted deadline":    expired,
		"canceled while queued":   installOperation(domain.ProbeInstallCanceled, "canceled"),
		"failed before any write": installOperation(domain.ProbeInstallFailed, "unsupported_host"),
	}
}

// TestProbeInstallViewShape proves every lifecycle shape renders exactly the
// documented keys (and no others) at every nesting level.
func TestProbeInstallViewShape(t *testing.T) {
	for name, operation := range installAllReceipts() {
		t.Run(name, func(t *testing.T) {
			if !operation.Valid() {
				t.Fatal("fixture receipt is not domain.Valid")
			}
			fake := &installFake{op: operation}
			h := NewProbeInstallHandlers(fake, true)
			rec := installServe(t, http.MethodGet, installPath+"/"+installOpID, installOpID, "", h.Observe)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			var view map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			installSameKeys(t, "view", installKeys(view),
				"operation_id", "kind", "status", "mode", "created_at", "updated_at", "deadline_at", "binary", "resources", "failed_phase", "error")

			var binary map[string]json.RawMessage
			if err := json.Unmarshal(view["binary"], &binary); err != nil {
				t.Fatal(err)
			}
			installSameKeys(t, "binary", installKeys(binary), "version", "architecture")
			if string(binary["version"]) != `"1.2.3"` || string(binary["architecture"]) != `"arm64"` {
				t.Fatalf("binary = %s", view["binary"])
			}

			var resources []map[string]json.RawMessage
			if err := json.Unmarshal(view["resources"], &resources); err != nil {
				t.Fatal(err)
			}
			wantKinds := []string{"data_directory", "artifact_directory", "unit_file", "service"}
			if len(resources) != len(wantKinds) {
				t.Fatalf("resources = %d, want %d", len(resources), len(wantKinds))
			}
			for i, resource := range resources {
				installSameKeys(t, "resource", installKeys(resource), "kind", "state", "error_code", "started_at", "finished_at")
				probeWantStepTime(t, resource["started_at"], operation.Resources[i].StartedAt)
				probeWantStepTime(t, resource["finished_at"], operation.Resources[i].FinishedAt)
				if string(resource["kind"]) != strconv.Quote(wantKinds[i]) {
					t.Fatalf("resource %d kind = %s, want %s (order must follow the journal ordinals)", i, resource["kind"], wantKinds[i])
				}
				if string(resource["state"]) != strconv.Quote(operation.Resources[i].State) {
					t.Fatalf("resource %d state = %s, want %s", i, resource["state"], operation.Resources[i].State)
				}
				wantCode := "null"
				if code := operation.Resources[i].ErrorCode; code != "" {
					wantCode = strconv.Quote(code)
				}
				if string(resource["error_code"]) != wantCode {
					t.Fatalf("resource %d error_code = %s, want %s", i, resource["error_code"], wantCode)
				}
			}

			probeWantFailedPhase(t, view["failed_phase"], operation.State, operation.Resources)
			if operation.ErrorCode == "" {
				if string(view["error"]) != "null" {
					t.Fatalf("error = %s, want null", view["error"])
				}
				return
			}
			var errView map[string]json.RawMessage
			if err := json.Unmarshal(view["error"], &errView); err != nil {
				t.Fatalf("error is not an object: %s", view["error"])
			}
			installSameKeys(t, "error", installKeys(errView), "code", "message")
			if string(errView["code"]) != strconv.Quote(operation.ErrorCode) || len(errView["message"]) <= 2 {
				t.Fatalf("error = %s", view["error"])
			}
		})
	}
}

// TestProbeInstallErrorAllowlist pins the fixed operation error allowlist: it
// covers every code the journal can store, each with its own fixed message,
// and nothing else.
func TestProbeInstallErrorAllowlist(t *testing.T) {
	applied, notStarted := domain.ProbeInstallResourceApplied, domain.ProbeInstallResourceNotStarted
	failedCodes := []string{"unsupported_host", "artifact_rejected", "resource_conflict", "execution_failed", "permission_denied", "runtime_unverified", "not_started", "internal"}
	type codeCase struct {
		name, state, code string
		operation         *domain.ProbeInstallOperation
	}
	var cases []codeCase
	for _, code := range failedCodes {
		if !(domain.ProbeInstallOutcome{State: domain.ProbeInstallFailed, ErrorCode: code}).Valid() {
			t.Fatalf("journal no longer accepts failed code %q: update the allowlist", code)
		}
		cases = append(cases, codeCase{"failed " + code, domain.ProbeInstallFailed, code, installOperation(domain.ProbeInstallFailed, code, applied, notStarted)})
	}
	cases = append(cases,
		codeCase{"canceled", domain.ProbeInstallCanceled, "canceled", installReceipt(domain.ProbeInstallCanceled)},
		codeCase{"owner_interrupted", domain.ProbeInstallInterrupted, "owner_interrupted", installReceipt(domain.ProbeInstallInterrupted)},
		codeCase{"outcome_unknown", domain.ProbeInstallInterrupted, "outcome_unknown", installAllReceipts()["interrupted outcome"]},
		codeCase{"deadline_expired", domain.ProbeInstallInterrupted, "deadline_expired", installAllReceipts()["interrupted deadline"]},
	)
	seen := map[string]bool{}
	for _, tc := range cases {
		seen[tc.code] = true
		t.Run(tc.name, func(t *testing.T) {
			if !tc.operation.Valid() {
				t.Fatal("fixture receipt is not domain.Valid")
			}
			h := NewProbeInstallHandlers(&installFake{op: tc.operation}, true)
			rec := installServe(t, http.MethodGet, installPath+"/"+installOpID, installOpID, "", h.Observe)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			var view struct {
				Status string `json:"status"`
				Error  struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if view.Status != tc.state || view.Error.Code != tc.code || view.Error.Message != probeInstallStoredErrorMessages[tc.code] || view.Error.Message == "" {
				t.Fatalf("view = %+v, want state %q code %q with its fixed message", view, tc.state, tc.code)
			}
		})
	}
	// The allowlist is exactly the journal's code set: no stale or missing entry.
	if len(probeInstallStoredErrorMessages) != len(seen) {
		t.Fatalf("allowlist has %d codes, journal code set has %d", len(probeInstallStoredErrorMessages), len(seen))
	}
	for code, message := range probeInstallStoredErrorMessages {
		if !seen[code] {
			t.Fatalf("allowlist code %q is not a journal code", code)
		}
		for _, secret := range installSecrets() {
			if strings.Contains(message, secret) {
				t.Fatalf("message for %q leaks %q", code, secret)
			}
		}
	}
	// Resource codes are limited to the four the journal can store per resource.
	if got := installKeys(probeInstallResourceErrorCodes); strings.Join(got, ",") != "deadline_expired,not_started,outcome_unknown,owner_interrupted" {
		t.Fatalf("resource error code allowlist = %v", got)
	}
}

// TestProbeInstallCorruptReceipts fails closed on any receipt the domain
// rejects, on a receipt for a different operation, and on a receipt outside
// this surface; nothing corrupt is ever serialized.
func TestProbeInstallCorruptReceipts(t *testing.T) {
	mutate := func(state string, f func(*domain.ProbeInstallOperation)) *domain.ProbeInstallOperation {
		op := installReceipt(state)
		f(op)
		return op
	}
	// A fully valid Docker receipt: the journal accepts it, but this surface
	// only describes the systemd path and must not render a binary-less view.
	dockerMode := mutate(domain.ProbeInstallQueued, func(op *domain.ProbeInstallOperation) {
		op.Identity.Mode = domain.ProbeInstallDocker
		op.Identity.Binary = domain.ProbeBinaryPin{}
		op.Identity.Image = domain.ProbeImageSelection{
			Pin:            domain.ProbeImagePin{Repository: "ghcr.io/example/probe", Version: "1.2.3", Architecture: "arm64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 100},
			ManifestSHA256: strings.Repeat("b", 64),
			ConfigSHA256:   strings.Repeat("c", 64),
			LayerCount:     1,
		}
		op.Resources = nil
		for _, spec := range op.Identity.Resources() {
			op.Resources = append(op.Resources, domain.ProbeInstallResource{Spec: spec, State: domain.ProbeInstallResourcePlanned})
		}
	})
	if !dockerMode.Valid() {
		t.Fatal("docker fixture receipt is not domain.Valid")
	}
	for name, op := range map[string]*domain.ProbeInstallOperation{
		"nil receipt":        nil,
		"unknown error code": mutate(domain.ProbeInstallFailed, func(op *domain.ProbeInstallOperation) { op.ErrorCode = "mystery-code" }),
		"free text code":     mutate(domain.ProbeInstallFailed, func(op *domain.ProbeInstallOperation) { op.ErrorCode = installLeakMarker }),
		"nonterminal with error": mutate(domain.ProbeInstallRunning, func(op *domain.ProbeInstallOperation) {
			op.ErrorCode = "internal"
		}),
		"failed without code": mutate(domain.ProbeInstallFailed, func(op *domain.ProbeInstallOperation) { op.ErrorCode = "" }),
		"unknown state":       mutate(domain.ProbeInstallQueued, func(op *domain.ProbeInstallOperation) { op.State = "installed" }),
		"bad owner":           mutate(domain.ProbeInstallQueued, func(op *domain.ProbeInstallOperation) { op.OwnerID = "not-a-valid-owner-identity" }),
		"non-utc timestamps": mutate(domain.ProbeInstallQueued, func(op *domain.ProbeInstallOperation) {
			zone := time.FixedZone("", 7*3600)
			op.CreatedAt, op.UpdatedAt, op.DeadlineAt = op.CreatedAt.In(zone), op.UpdatedAt.In(zone), op.DeadlineAt.In(zone)
		}),
		"resource unknown code": mutate(domain.ProbeInstallInterrupted, func(op *domain.ProbeInstallOperation) {
			op.Resources[1].ErrorCode = "mystery-code"
		}),
		"resources truncated": mutate(domain.ProbeInstallQueued, func(op *domain.ProbeInstallOperation) { op.Resources = op.Resources[:2] }),
		"resources reordered": mutate(domain.ProbeInstallQueued, func(op *domain.ProbeInstallOperation) {
			op.Resources[0], op.Resources[1] = op.Resources[1], op.Resources[0]
		}),
		"ready with unapplied resource": mutate(domain.ProbeInstallResourcesReady, func(op *domain.ProbeInstallOperation) {
			op.Resources[3].State = domain.ProbeInstallResourcePlanned
			op.Resources[3].AttemptID = ""
			op.Resources[3].StartedAt, op.Resources[3].FinishedAt = time.Time{}, time.Time{}
		}),
		"different operation": mutate(domain.ProbeInstallQueued, func(op *domain.ProbeInstallOperation) {
			op.Identity.OperationID = "5e4d3c2b-1a09-4f8e-b7d6-c5b4a3928170"
			for i := range op.Resources {
				op.Resources[i].Spec = op.Identity.Resources()[i]
			}
		}),
		"docker mode": dockerMode,
	} {
		for _, route := range []struct {
			name   string
			method string
			path   string
			id     string
			body   string
			call   func(h *ProbeInstallHandlers) func(echo.Context) error
		}{
			{"observe", http.MethodGet, installPath + "/" + installOpID, installOpID, "", func(h *ProbeInstallHandlers) func(echo.Context) error { return h.Observe }},
			{"cancel", http.MethodPost, installPath + "/" + installOpID + "/cancel", installOpID, "", func(h *ProbeInstallHandlers) func(echo.Context) error { return h.Cancel }},
			{"submit", http.MethodPost, installPath, "", installBody(installKeyCanary), func(h *ProbeInstallHandlers) func(echo.Context) error { return h.Submit }},
		} {
			t.Run(name+" "+route.name, func(t *testing.T) {
				h := NewProbeInstallHandlers(&installFake{op: op}, true)
				rec := installServe(t, route.method, route.path, route.id, route.body, route.call(h))
				if rec.Code != http.StatusServiceUnavailable || installErrorBody(t, rec).Code != "install_unavailable" {
					t.Fatalf("corrupt receipt = %d (%s)", rec.Code, rec.Body.String())
				}
				for _, leak := range []string{"mystery-code", "not-a-valid-owner-identity", "5e4d3c2b-1a09-4f8e-b7d6-c5b4a3928170", "docker"} {
					if strings.Contains(rec.Body.String(), leak) {
						t.Fatalf("corrupt field %q leaked: %s", leak, rec.Body.String())
					}
				}
			})
		}
	}
}

// TestProbeInstallObserve covers the read lifecycle: 200 for every stored
// receipt, the exact operation id handed to the service, malformed and
// missing identities.
func TestProbeInstallObserve(t *testing.T) {
	for name, operation := range installAllReceipts() {
		t.Run("observe "+name, func(t *testing.T) {
			fake := &installFake{op: operation}
			h := NewProbeInstallHandlers(fake, true)
			rec := installServe(t, http.MethodGet, installPath+"/"+installOpID, installOpID, "", h.Observe)
			if rec.Code != http.StatusOK || fake.operationID != installOpID || fake.observes != 1 || fake.calls() != 1 {
				t.Fatalf("observe = %d, op = %q, calls = %d (%s)", rec.Code, fake.operationID, fake.calls(), rec.Body.String())
			}
		})
	}
	for _, id := range []string{"not-a-uuid", strings.ToUpper(installOpID), "local", "00000000-0000-0000-0000-000000000000", installOpID + "x"} {
		t.Run("malformed id "+id, func(t *testing.T) {
			fake := &installFake{op: installReceipt(domain.ProbeInstallQueued)}
			h := NewProbeInstallHandlers(fake, true)
			rec := installServe(t, http.MethodGet, installPath+"/"+id, id, "", h.Observe)
			if rec.Code != http.StatusBadRequest || installErrorBody(t, rec).Code != "invalid_operation_id" {
				t.Fatalf("malformed id = %d (%s)", rec.Code, rec.Body.String())
			}
			if fake.calls() != 0 {
				t.Fatalf("service called %d times for a malformed identity", fake.calls())
			}
		})
	}
	t.Run("missing path parameter", func(t *testing.T) {
		fake := &installFake{op: installReceipt(domain.ProbeInstallQueued)}
		h := NewProbeInstallHandlers(fake, true)
		rec := installServe(t, http.MethodGet, installPath+"/", "", "", h.Observe)
		if rec.Code != http.StatusBadRequest || fake.calls() != 0 {
			t.Fatalf("missing id = %d, calls = %d", rec.Code, fake.calls())
		}
	})
}

// TestProbeInstallCancel covers cancellation: 202 for recorded intent, 200 for
// settled receipts returned unchanged, payload-free requests and the
// fail-closed contradiction paths.
func TestProbeInstallCancel(t *testing.T) {
	cancelPath := installPath + "/" + installOpID + "/cancel"
	statusOf := func(t *testing.T, rec *httptest.ResponseRecorder) string {
		t.Helper()
		var view struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		return view.Status
	}
	for _, tc := range []struct {
		name   string
		op     *domain.ProbeInstallOperation
		status int
	}{
		{"intent recorded", installReceipt(domain.ProbeInstallCancelRequested), 202},
		{"queued cancels at once", installOperation(domain.ProbeInstallCanceled, "canceled"), 200},
		{"terminal canceled", installReceipt(domain.ProbeInstallCanceled), 200},
		{"terminal failed unchanged", installReceipt(domain.ProbeInstallFailed), 200},
		{"terminal interrupted unchanged", installReceipt(domain.ProbeInstallInterrupted), 200},
		{"resources_ready unchanged", installReceipt(domain.ProbeInstallResourcesReady), 200},
		{"queued receipt contradicts cancel", installReceipt(domain.ProbeInstallQueued), 503},
		{"running receipt contradicts cancel", installReceipt(domain.ProbeInstallRunning), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &installFake{op: tc.op}
			h := NewProbeInstallHandlers(fake, true)
			rec := installServe(t, http.MethodPost, cancelPath, installOpID, "", h.Cancel)
			if rec.Code != tc.status || fake.operationID != installOpID || fake.cancels != 1 || fake.calls() != 1 {
				t.Fatalf("cancel = %d, op = %q, calls = %d (%s)", rec.Code, fake.operationID, fake.calls(), rec.Body.String())
			}
			if tc.status == 503 {
				if installErrorBody(t, rec).Code != "install_unavailable" {
					t.Fatalf("contradiction code = %s", rec.Body.String())
				}
				return
			}
			if got := statusOf(t, rec); got != tc.op.State {
				t.Fatalf("cancel receipt status = %q, want unchanged %q", got, tc.op.State)
			}
		})
	}
	for _, tc := range []struct{ name, body string }{
		{"object payload", `{"reason":"x"}`},
		{"empty object payload", `{}`},
		{"null payload", `null`},
		{"text payload", `cancel`},
		{"payload after whitespace", "  \n\t" + `{}`},
		{"payload past the limit", strings.Repeat(" ", probeInstallCancelMaxBodyBytes) + "x"},
		{"whitespace past the limit", strings.Repeat(" ", probeInstallCancelMaxBodyBytes+1)},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			fake := &installFake{op: installReceipt(domain.ProbeInstallCancelRequested)}
			h := NewProbeInstallHandlers(fake, true)
			rec := installServe(t, http.MethodPost, cancelPath, installOpID, tc.body, h.Cancel)
			if rec.Code != http.StatusBadRequest || installErrorBody(t, rec).Code != "invalid_request" {
				t.Fatalf("cancel payload = %d (%s)", rec.Code, rec.Body.String())
			}
			if fake.calls() != 0 {
				t.Fatalf("service called %d times for a payload-carrying cancel", fake.calls())
			}
		})
	}
	t.Run("accepts whitespace only", func(t *testing.T) {
		fake := &installFake{op: installReceipt(domain.ProbeInstallCancelRequested)}
		h := NewProbeInstallHandlers(fake, true)
		rec := installServe(t, http.MethodPost, cancelPath, installOpID, " \n\t", h.Cancel)
		if rec.Code != http.StatusAccepted || fake.cancels != 1 {
			t.Fatalf("whitespace cancel = %d, calls = %d", rec.Code, fake.cancels)
		}
	})
	for _, id := range []string{"not-a-uuid", strings.ToUpper(installOpID), "local"} {
		t.Run("malformed id "+id, func(t *testing.T) {
			fake := &installFake{op: installReceipt(domain.ProbeInstallCancelRequested)}
			h := NewProbeInstallHandlers(fake, true)
			rec := installServe(t, http.MethodPost, installPath+"/"+id+"/cancel", id, "", h.Cancel)
			if rec.Code != http.StatusBadRequest || installErrorBody(t, rec).Code != "invalid_operation_id" || fake.calls() != 0 {
				t.Fatalf("malformed id = %d, calls = %d (%s)", rec.Code, fake.calls(), rec.Body.String())
			}
		})
	}
}

// TestProbeInstallHTTPErrors verifies the frozen error mapping on every route,
// including the bounded Retry-After on busy and the ban on echoing underlying
// diagnostics.
func TestProbeInstallHTTPErrors(t *testing.T) {
	leaky := errors.New("storage backend failure " + installLeakMarker + " /var/lib/secret")
	for _, tc := range []struct {
		name       string
		fakeErr    error
		status     int
		code       string
		retryAfter string
	}{
		{"validation", domain.ErrValidation, 400, "invalid_install", ""},
		{"wrapped validation", fmt.Errorf("creating: %w", domain.ErrValidation), 400, "invalid_install", ""},
		{"ssh input", domain.ErrProbeSSHInput, 400, "invalid_install", ""},
		{"artifact rejected", domain.ErrProbeInstallArtifact, 422, "artifact_rejected", ""},
		{"wrapped artifact rejected", fmt.Errorf("preparing %s: %w", installLeakMarker, domain.ErrProbeInstallArtifact), 422, "artifact_rejected", ""},
		{"preflight architecture", &domain.ProbeInstallPreflightError{Code: domain.ProbeInstallPreflightArchitecture}, 422, "host_architecture_mismatch", ""},
		{"preflight existing install", fmt.Errorf("gate %s: %w", installLeakMarker, &domain.ProbeInstallPreflightError{Code: domain.ProbeInstallPreflightExisting}), 422, "host_existing_install", ""},
		{"preflight bad host key", &domain.ProbeInstallPreflightError{Code: domain.ProbeInstallPreflightTrust}, 422, "host_trust_failed", ""},
		{"preflight unknown code", &domain.ProbeInstallPreflightError{Code: "host_" + installLeakMarker}, 503, "install_unavailable", ""},
		{"bare preflight sentinel", domain.ErrProbeInstallPreflight, 503, "install_unavailable", ""},
		{"port conflict", ports.ErrConflict, 409, "install_conflict", ""},
		{"domain conflict", fmt.Errorf("journal: %w", domain.ErrConflict), 409, "install_conflict", ""},
		{"port not found", ports.ErrNotFound, 404, "install_not_found", ""},
		{"domain not found", domain.ErrNotFound, 404, "install_not_found", ""},
		{"busy", domain.ErrProbeInstallBusy, 503, "installs_busy", "5"},
		{"wrapped busy", fmt.Errorf("admission: %w", domain.ErrProbeInstallBusy), 503, "installs_busy", "5"},
		{"closed", domain.ErrProbeInstallClosed, 503, "installs_closed", ""},
		{"internal", domain.ErrInternal, 503, "install_unavailable", ""},
		{"internal joined with validation", errors.Join(domain.ErrValidation, domain.ErrInternal), 503, "install_unavailable", ""},
		{"context canceled", context.Canceled, 503, "install_unavailable", ""},
		{"underlying diagnostic", leaky, 503, "install_unavailable", ""},
	} {
		for _, route := range []struct {
			name   string
			method string
			path   string
			id     string
			body   string
			call   func(h *ProbeInstallHandlers) func(echo.Context) error
		}{
			{"submit", http.MethodPost, installPath, "", installBody(installKeyCanary), func(h *ProbeInstallHandlers) func(echo.Context) error { return h.Submit }},
			{"observe", http.MethodGet, installPath + "/" + installOpID, installOpID, "", func(h *ProbeInstallHandlers) func(echo.Context) error { return h.Observe }},
			{"cancel", http.MethodPost, installPath + "/" + installOpID + "/cancel", installOpID, "", func(h *ProbeInstallHandlers) func(echo.Context) error { return h.Cancel }},
		} {
			t.Run(tc.name+" "+route.name, func(t *testing.T) {
				fake := &installFake{err: tc.fakeErr}
				h := NewProbeInstallHandlers(fake, true)
				rec := installServe(t, route.method, route.path, route.id, route.body, route.call(h))
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
				}
				if view := installErrorBody(t, rec); view.Code != tc.code || view.Error == "" {
					t.Fatalf("error view = %+v, want code %q with a fixed message", view, tc.code)
				}
				if got := rec.Header().Get("Retry-After"); got != tc.retryAfter {
					t.Fatalf("Retry-After = %q, want %q", got, tc.retryAfter)
				}
				if fake.calls() != 1 {
					t.Fatalf("service calls = %d, want 1", fake.calls())
				}
			})
		}
	}
}

// TestProbeInstallSurfaceGates covers unauthenticated access, the disabled
// feature flag and the missing (including typed-nil) service on every route.
func TestProbeInstallSurfaceGates(t *testing.T) {
	var typedNil *installFake
	type route struct {
		name   string
		method string
		path   string
		id     string
		body   string
		call   func(h *ProbeInstallHandlers) func(echo.Context) error
	}
	routes := []route{
		{"submit", http.MethodPost, installPath, "", installBody(installKeyCanary), func(h *ProbeInstallHandlers) func(echo.Context) error { return h.Submit }},
		{"observe", http.MethodGet, installPath + "/" + installOpID, installOpID, "", func(h *ProbeInstallHandlers) func(echo.Context) error { return h.Observe }},
		{"cancel", http.MethodPost, installPath + "/" + installOpID + "/cancel", installOpID, "", func(h *ProbeInstallHandlers) func(echo.Context) error { return h.Cancel }},
	}
	for _, r := range routes {
		for _, actor := range []int64{0, -1} {
			t.Run(fmt.Sprintf("unauthenticated %s actor %d", r.name, actor), func(t *testing.T) {
				fake := &installFake{op: installReceipt(domain.ProbeInstallQueued)}
				h := NewProbeInstallHandlers(fake, true)
				// Actor 0 means no value in the context at all.
				rec := installServeAs(t, actor, r.method, r.path, r.id, r.body, r.call(h))
				if rec.Code != http.StatusUnauthorized || installErrorBody(t, rec).Code != "unauthenticated" {
					t.Fatalf("unauthenticated = %d (%s)", rec.Code, rec.Body.String())
				}
				if fake.calls() != 0 {
					t.Fatalf("service called %d times without an actor", fake.calls())
				}
			})
		}
		for _, tc := range []struct {
			name    string
			svc     probeInstallRunner
			enabled bool
			want    string
		}{
			{"disabled", &installFake{op: installReceipt(domain.ProbeInstallQueued)}, false, "installs_disabled"},
			{"disabled without service", nil, false, "installs_disabled"},
			{"no service", nil, true, "install_unavailable"},
			{"typed nil service", typedNil, true, "install_unavailable"},
		} {
			t.Run(tc.name+" "+r.name, func(t *testing.T) {
				h := NewProbeInstallHandlers(tc.svc, tc.enabled)
				rec := installServe(t, r.method, r.path, r.id, r.body, r.call(h))
				if rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
				}
				if code := installErrorBody(t, rec).Code; code != tc.want {
					t.Fatalf("code = %q, want %q", code, tc.want)
				}
				if fake, ok := tc.svc.(*installFake); ok && fake != nil && fake.calls() != 0 {
					t.Fatalf("disabled service called %d times", fake.calls())
				}
			})
		}
	}
}

// TestProbeInstallDoesNotReadBodyWhenGated proves a gated request is refused
// before its body (which may hold a private key) is interpreted at all.
func TestProbeInstallDoesNotReadBodyWhenGated(t *testing.T) {
	fake := &installFake{op: installReceipt(domain.ProbeInstallQueued)}
	h := NewProbeInstallHandlers(fake, false)
	rec := installServe(t, http.MethodPost, installPath, "", strings.Repeat("a", 128<<10+1), h.Submit)
	if rec.Code != http.StatusServiceUnavailable || installErrorBody(t, rec).Code != "installs_disabled" {
		t.Fatalf("gated submit = %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestProbeInstallUTCTimestamps proves timestamps are rendered as UTC with the
// fixed 15 minute lifetime visible as deadline_at.
func TestProbeInstallUTCTimestamps(t *testing.T) {
	fake := &installFake{op: installReceipt(domain.ProbeInstallRunning)}
	h := NewProbeInstallHandlers(fake, true)
	rec := installServe(t, http.MethodGet, installPath+"/"+installOpID, installOpID, "", h.Observe)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"created_at":"2026-10-16T10:00:00Z"`, `"updated_at":"2026-10-16T10:00:11Z"`, `"deadline_at":"2026-10-16T10:15:00Z"`} {
		if !bytes.Contains(rec.Body.Bytes(), []byte(want)) {
			t.Fatalf("missing %s in %s", want, rec.Body.String())
		}
	}
}

// TestProbeInstallResponseIsNotADomainType guards rule 5: the wire view must
// carry snake_case json tags only, never exported Go field names.
func TestProbeInstallResponseIsNotADomainType(t *testing.T) {
	fake := &installFake{op: installReceipt(domain.ProbeInstallRunning)}
	h := NewProbeInstallHandlers(fake, true)
	rec := installServe(t, http.MethodGet, installPath+"/"+installOpID, installOpID, "", h.Observe)
	for _, goName := range []string{"OperationID", "Identity", "OwnerID", "Resources", "HostKeyFingerprint", "ClientKeyFingerprint", "Spec", "AttemptID", "StartedAt", "FinishedAt", "SHA256", "Size"} {
		if strings.Contains(rec.Body.String(), `"`+goName+`"`) {
			t.Fatalf("response carries Go field name %q: %s", goName, rec.Body.String())
		}
	}
	if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
	}
}

// TestProbeInstallPreflightMessagesCoverEveryCode keeps the HTTP allowlist
// and the domain's fixed refusal codes identical.
func TestProbeInstallPreflightMessagesCoverEveryCode(t *testing.T) {
	codes := []string{
		domain.ProbeInstallPreflightTrust, domain.ProbeInstallPreflightDestination, domain.ProbeInstallPreflightAuth,
		domain.ProbeInstallPreflightUnreachable, domain.ProbeInstallPreflightHelper, domain.ProbeInstallPreflightOS,
		domain.ProbeInstallPreflightArchitecture, domain.ProbeInstallPreflightRoot, domain.ProbeInstallPreflightSystemd,
		domain.ProbeInstallPreflightAuthority, domain.ProbeInstallPreflightExisting,
	}
	if len(probeInstallPreflightMessages) != len(codes) {
		t.Fatalf("messages=%d codes=%d", len(probeInstallPreflightMessages), len(codes))
	}
	for _, code := range codes {
		if !domain.ValidProbeInstallPreflightCode(code) || probeInstallPreflightMessages[code] == "" {
			t.Fatalf("code %q lacks a domain or HTTP entry", code)
		}
	}
}

// probeWantStepTime asserts a nullable step time renders exactly the stored
// DB-clock instant, or null when unset.
func probeWantStepTime(t *testing.T, raw json.RawMessage, want time.Time) {
	t.Helper()
	if want.IsZero() {
		if string(raw) != "null" {
			t.Fatalf("unset step time rendered %s", raw)
		}
		return
	}
	var got time.Time
	if err := json.Unmarshal(raw, &got); err != nil || !got.Equal(want) {
		t.Fatalf("step time %s, want %s (%v)", raw, want, err)
	}
}

// probeWantFailedPhase asserts failed_phase names the first step that did not
// apply, only for failed, canceled and interrupted receipts.
func probeWantFailedPhase(t *testing.T, raw json.RawMessage, state string, steps []domain.ProbeInstallResource) {
	t.Helper()
	want := "null"
	if state == "failed" || state == "canceled" || state == "interrupted" {
		for _, step := range steps {
			if step.State == domain.ProbeInstallResourceNotStarted || step.State == domain.ProbeInstallResourceUnknown {
				want = strconv.Quote(step.Spec.Kind)
				break
			}
		}
	}
	if string(raw) != want {
		t.Fatalf("failed_phase = %s, want %s (state %s)", raw, want, state)
	}
}
