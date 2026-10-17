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
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const preflightOpID = "d1123604-32c5-40aa-89f9-b62f93dceac2"
const preflightOwnerID = "e645246b-b176-4422-8ae5-b79629ee6a29"

// preflightPin is a canonical OpenSSH SHA256 fingerprint (32 zero bytes).
var preflightPin = "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))

// preflightKeyCanary must never appear in a response or receipt.
var preflightKeyCanary = "test-preflight-transient-key-canary"

// leakedDiagnosticMarker is the substring of the fake underlying storage
// error that must never reach any response.
var leakedDiagnosticMarker = "leaked-internal-detail-marker"

// preflightFake captures the handler's service contract. It copies the
// borrowed private-key bytes during the call so the test can both verify the
// exact key handed over and observe the handler's best-effort buffer clearing.
type preflightFake struct {
	submission  domain.ProbePreflightSubmission
	operationID string
	keyCopy     []byte
	op          *domain.ProbePreflightOperation
	err         error
	calls       int
}

func (f *preflightFake) Submit(_ context.Context, submission domain.ProbePreflightSubmission) (*domain.ProbePreflightOperation, error) {
	f.calls++
	f.submission = submission
	f.keyCopy = append([]byte(nil), submission.PrivateKey...)
	if f.err != nil {
		return nil, f.err
	}
	return f.op, nil
}

func (f *preflightFake) Observe(_ context.Context, operationID string) (*domain.ProbePreflightOperation, error) {
	f.calls++
	f.operationID = operationID
	if f.err != nil {
		return nil, f.err
	}
	return f.op, nil
}

func (f *preflightFake) Cancel(_ context.Context, operationID string) (*domain.ProbePreflightOperation, error) {
	f.calls++
	f.operationID = operationID
	if f.err != nil {
		return nil, f.err
	}
	return f.op, nil
}

// preflightReceipt builds a domain.Valid receipt for the given lifecycle state.
func preflightReceipt(state string) *domain.ProbePreflightOperation {
	created := time.Date(2026, 10, 16, 10, 0, 0, 0, time.UTC)
	operation := &domain.ProbePreflightOperation{
		Identity: domain.ProbePreflightIdentity{
			OperationID:          preflightOpID,
			Host:                 domain.ProbeSSHHost{Address: "edge.example.test:22", User: "root"},
			HostKeyFingerprint:   preflightPin,
			ClientKeyFingerprint: preflightPin,
			RequestedBy:          7,
		},
		OwnerID:    preflightOwnerID,
		State:      state,
		CreatedAt:  created,
		UpdatedAt:  created,
		DeadlineAt: created.Add(domain.ProbePreflightLifetime),
	}
	switch state {
	case domain.ProbePreflightPassed:
		operation.Result = &domain.ProbeHostPreflight{OS: "linux", Architecture: "amd64", DockerCLI: true, Privilege: "sudo"}
	case domain.ProbePreflightFailed:
		operation.ErrorCode = "ssh_connect"
	case domain.ProbePreflightCanceled:
		operation.ErrorCode = "canceled"
	case domain.ProbePreflightInterrupted:
		operation.ErrorCode = "interrupted"
	}
	return operation
}

// preflightBody renders the frozen five-string request shape.
func preflightBody(key string) string {
	return fmt.Sprintf(`{"operation_id":%q,"ssh_address":%q,"ssh_user":"root","host_key_fingerprint":%q,"private_key":%q}`,
		preflightOpID, "edge.example.test:22", preflightPin, key)
}

// JSON member fragments are assembled from a single quote rune so each
// mutated body is composed from one canonical spelling.
var (
	quote             = string(rune('"'))
	opIDMember        = quote + "operation_id" + quote + ":"
	requestedByMember = quote + "requested_by" + quote + ":" + quote + "1" + quote + ","
	caseVariantMember = quote + "operation_ID" + quote + ":"
	sshUserMember     = quote + "ssh_user" + quote + ":" + quote + "root" + quote
	sshUserNumber     = quote + "ssh_user" + quote + ":42"
	nullKeyMember     = quote + "private_key" + quote + ":null," + quote + "pad" + quote + ":"
	privateKeyMember  = quote + "private_key" + quote + ":"
)

// preflightBodyWithout renders the frozen body minus the trailing required
// member, leaving a complete object with one key absent.
func preflightBodyWithout(drop string) string {
	body := preflightBody(preflightKeyCanary)
	start := strings.Index(body, quote+drop+quote+":")
	if start < 0 {
		return body
	}
	trimmed := strings.TrimSuffix(body[:start], ",")
	return trimmed + "}"
}

// preflightServe invokes one handler method with an authenticated context,
// mirroring the router's param wiring without claiming middleware coverage.
func preflightServe(h *ProbePreflightHandlers, method, path, id, body string, call func(echo.Context) error) *httptest.ResponseRecorder {
	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c := e.NewContext(req, rec)
	if id != "" {
		c.SetParamNames("operation_id")
		c.SetParamValues(id)
	}
	c.Set(ContextUserIDKey, int64(1))
	if err := call(c); err != nil {
		e.HTTPErrorHandler(err, c)
	}
	return rec
}

func preflightErrorBody(t *testing.T, rec *httptest.ResponseRecorder) regionalErrorView {
	t.Helper()
	var view regionalErrorView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("error body is not the fixed error view: %v (%s)", err, rec.Body.String())
	}
	return view
}

// TestProbePreflightSubmitStrictBinding refuses every wire shape outside the
// frozen five-string object and never reaches the service with rejected input.
func TestProbePreflightSubmitStrictBinding(t *testing.T) {
	validBody := preflightBody(preflightKeyCanary)
	oversizedKey := strings.Repeat("k", 64<<10+1)
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"empty", "", 400, "invalid_preflight"},
		{"malformed", `{`, 400, "invalid_preflight"},
		{"not an object", `[]`, 400, "invalid_preflight"},
		{"empty object", `{}`, 400, "invalid_preflight"},
		{"unknown key", strings.Replace(validBody, opIDMember, requestedByMember+opIDMember, 1), 400, "invalid_preflight"},
		{"case variant key", strings.Replace(validBody, opIDMember, caseVariantMember, 1), 400, "invalid_preflight"},
		{"duplicate key", strings.Replace(validBody, opIDMember, opIDMember+opIDMember, 1), 400, "invalid_preflight"},
		{"missing key", preflightBodyWithout("private_key"), 400, "invalid_preflight"},
		{"non-string value", strings.Replace(validBody, sshUserMember, sshUserNumber, 1), 400, "invalid_preflight"},
		{"null value", strings.Replace(validBody, privateKeyMember, nullKeyMember, 1), 400, "invalid_preflight"},
		{"trailing object", validBody + ` {}`, 400, "invalid_preflight"},
		{"trailing garbage", validBody + `garbage`, 400, "invalid_preflight"},
		{"invalid operation id", strings.Replace(validBody, preflightOpID, "not-a-uuid", 1), 400, "invalid_preflight"},
		{"address without port", strings.Replace(validBody, "edge.example.test:22", "edge.example.test", 1), 400, "invalid_preflight"},
		{"non-canonical fingerprint", strings.Replace(validBody, preflightPin, "not-a-fingerprint", 1), 400, "invalid_preflight"},
		{"empty private key", strings.Replace(validBody, preflightKeyCanary, "", 1), 400, "invalid_preflight"},
		{"oversized private key", preflightBody(oversizedKey), 400, "invalid_preflight"},
		{"oversized body", strings.Repeat("a", 128<<10+1), 413, "preflight_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &preflightFake{op: preflightReceipt(domain.ProbePreflightQueued)}
			h := NewProbePreflightHandlers(fake, true)
			rec := preflightServe(h, http.MethodPost, "/api/probe-host-preflights", "", tc.body, h.Submit)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if view := preflightErrorBody(t, rec); view.Code != tc.code {
				t.Fatalf("code = %q, want %q", view.Code, tc.code)
			}
			if fake.calls != 0 {
				t.Fatalf("service called %d times for rejected input", fake.calls)
			}
			if strings.Contains(rec.Body.String(), preflightKeyCanary) {
				t.Fatal("rejected input echoed in the error response")
			}
		})
	}
}

// TestProbePreflightSubmitAccepted verifies the 202-after-commit contract, the
// exact explicit view, actor-from-context, secret canary exclusion and the
// best-effort clearing of the owned private-key buffer.
func TestProbePreflightSubmitAccepted(t *testing.T) {
	fake := &preflightFake{op: preflightReceipt(domain.ProbePreflightQueued)}
	h := NewProbePreflightHandlers(fake, true)
	rec := preflightServe(h, http.MethodPost, "/api/probe-host-preflights", "", preflightBody(preflightKeyCanary), h.Submit)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (%s)", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	if fake.calls != 1 {
		t.Fatalf("service calls = %d, want 1", fake.calls)
	}
	// Actor only from context, never from the body.
	if fake.submission.RequestedBy != 1 {
		t.Fatalf("RequestedBy = %d, want context actor 1", fake.submission.RequestedBy)
	}
	if fake.submission.OperationID != preflightOpID ||
		fake.submission.Host.Address != "edge.example.test:22" || fake.submission.Host.User != "root" ||
		fake.submission.HostKeyFingerprint != preflightPin {
		t.Fatalf("submission mapping = %+v", fake.submission)
	}
	if string(fake.keyCopy) != preflightKeyCanary {
		t.Fatalf("service key = %q, want the transient PEM content", fake.keyCopy)
	}
	for _, b := range fake.submission.PrivateKey {
		if b != 0 {
			t.Fatal("owned private-key buffer was not cleared after the call")
		}
	}
	// The explicit view carries exactly the frozen fields and nothing secret.
	var view map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if len(view) != 9 {
		t.Fatalf("view has %d fields, want exactly 9: %s", len(view), rec.Body.String())
	}
	for _, key := range []string{"operation_id", "kind", "status", "phase", "created_at", "updated_at", "deadline_at", "error", "preflight"} {
		if _, ok := view[key]; !ok {
			t.Fatalf("view missing %q: %s", key, rec.Body.String())
		}
	}
	var receipt struct {
		OperationID string `json:"operation_id"`
		Kind        string `json:"kind"`
		Status      string `json:"status"`
		Phase       string `json:"phase"`
		CreatedAt   string `json:"created_at"`
		DeadlineAt  string `json:"deadline_at"`
		Error       any    `json:"error"`
		Preflight   any    `json:"preflight"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.OperationID != preflightOpID || receipt.Kind != "ssh_host_preflight" ||
		receipt.Status != domain.ProbePreflightQueued || receipt.Phase != "host_preflight" {
		t.Fatalf("receipt identity fields = %+v", receipt)
	}
	if receipt.CreatedAt != "2026-10-16T10:00:00Z" || receipt.DeadlineAt != "2026-10-16T10:01:30Z" {
		t.Fatalf("timestamps not rendered as UTC: created=%s deadline=%s", receipt.CreatedAt, receipt.DeadlineAt)
	}
	if receipt.Error != nil || receipt.Preflight != nil {
		t.Fatalf("in-progress receipt must render null error and preflight: %s", rec.Body.String())
	}
	response := rec.Body.String()
	for _, secret := range []string{preflightKeyCanary, "edge.example.test", preflightPin, preflightOwnerID} {
		if strings.Contains(response, secret) {
			t.Fatalf("response leaks %q: %s", secret, response)
		}
	}
}

// TestProbePreflightSubmitTerminalRetry verifies an exact terminal retry
// answers 200 with the original immutable outcome and its fixed error view.
func TestProbePreflightSubmitTerminalRetry(t *testing.T) {
	fake := &preflightFake{op: preflightReceipt(domain.ProbePreflightFailed)}
	h := NewProbePreflightHandlers(fake, true)
	rec := preflightServe(h, http.MethodPost, "/api/probe-host-preflights", "", preflightBody(preflightKeyCanary), h.Submit)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var receipt struct {
		Status string `json:"status"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Preflight any `json:"preflight"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != domain.ProbePreflightFailed || receipt.Error.Code != "ssh_connect" ||
		receipt.Error.Message != "SSH connection or authentication failed" {
		t.Fatalf("failed receipt = %+v", receipt)
	}
	if receipt.Preflight != nil {
		t.Fatalf("failed receipt must not carry preflight facts: %s", rec.Body.String())
	}

	// A passed terminal retry renders the explicit compatibility facts.
	fake.op = preflightReceipt(domain.ProbePreflightPassed)
	rec = preflightServe(h, http.MethodPost, "/api/probe-host-preflights", "", preflightBody(preflightKeyCanary), h.Submit)
	if rec.Code != http.StatusOK {
		t.Fatalf("passed retry status = %d, want 200", rec.Code)
	}
	var passed struct {
		Status    string `json:"status"`
		Error     any    `json:"error"`
		Preflight struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			DockerCLI    bool   `json:"docker_cli"`
			Systemd      bool   `json:"systemd"`
			Privilege    string `json:"privilege"`
		} `json:"preflight"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &passed); err != nil {
		t.Fatal(err)
	}
	if passed.Status != domain.ProbePreflightPassed || passed.Error != nil ||
		passed.Preflight.OS != "linux" || passed.Preflight.Architecture != "amd64" ||
		!passed.Preflight.DockerCLI || passed.Preflight.Systemd || passed.Preflight.Privilege != "sudo" {
		t.Fatalf("passed receipt = %+v", passed)
	}
}

// TestProbePreflightObserveAndCancel covers the read and cancellation
// lifecycle: 200 reads, 202 intent, terminal receipts returned unchanged,
// payload-free cancel and malformed or missing identities.
func TestProbePreflightObserveAndCancel(t *testing.T) {
	t.Run("observe queued", func(t *testing.T) {
		fake := &preflightFake{op: preflightReceipt(domain.ProbePreflightQueued)}
		h := NewProbePreflightHandlers(fake, true)
		rec := preflightServe(h, http.MethodGet, "/api/probe-host-preflights/"+preflightOpID, preflightOpID, "", h.Observe)
		if rec.Code != http.StatusOK || fake.operationID != preflightOpID {
			t.Fatalf("observe = %d, op = %q (%s)", rec.Code, fake.operationID, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
		}
	})
	t.Run("cancel requests intent", func(t *testing.T) {
		fake := &preflightFake{op: preflightReceipt(domain.ProbePreflightCancelRequested)}
		h := NewProbePreflightHandlers(fake, true)
		rec := preflightServe(h, http.MethodPost, "/api/probe-host-preflights/"+preflightOpID+"/cancel", preflightOpID, "", h.Cancel)
		if rec.Code != http.StatusAccepted || fake.operationID != preflightOpID {
			t.Fatalf("cancel = %d, op = %q (%s)", rec.Code, fake.operationID, rec.Body.String())
		}
		var receipt struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Status != domain.ProbePreflightCancelRequested {
			t.Fatalf("cancel receipt status = %q, want %q", receipt.Status, domain.ProbePreflightCancelRequested)
		}
	})
	t.Run("cancel on terminal is unchanged", func(t *testing.T) {
		fake := &preflightFake{op: preflightReceipt(domain.ProbePreflightPassed)}
		h := NewProbePreflightHandlers(fake, true)
		rec := preflightServe(h, http.MethodPost, "/api/probe-host-preflights/"+preflightOpID+"/cancel", preflightOpID, "", h.Cancel)
		if rec.Code != http.StatusOK {
			t.Fatalf("terminal cancel = %d, want 200 (%s)", rec.Code, rec.Body.String())
		}
		var receipt struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Status != domain.ProbePreflightPassed {
			t.Fatalf("completed preflight relabeled %q, want unchanged %q", receipt.Status, domain.ProbePreflightPassed)
		}
	})
	t.Run("cancel rejects a payload", func(t *testing.T) {
		fake := &preflightFake{op: preflightReceipt(domain.ProbePreflightCancelRequested)}
		h := NewProbePreflightHandlers(fake, true)
		rec := preflightServe(h, http.MethodPost, "/api/probe-host-preflights/"+preflightOpID+"/cancel", preflightOpID, `{"reason":"x"}`, h.Cancel)
		if rec.Code != http.StatusBadRequest || preflightErrorBody(t, rec).Code != "invalid_request" {
			t.Fatalf("cancel payload = %d (%s)", rec.Code, rec.Body.String())
		}
		if fake.calls != 0 {
			t.Fatalf("service called %d times for a payload-carrying cancel", fake.calls)
		}
	})
	t.Run("nonterminal receipt that is not cancel_requested fails closed", func(t *testing.T) {
		fake := &preflightFake{op: preflightReceipt(domain.ProbePreflightQueued)}
		h := NewProbePreflightHandlers(fake, true)
		rec := preflightServe(h, http.MethodPost, "/api/probe-host-preflights/"+preflightOpID+"/cancel", preflightOpID, "", h.Cancel)
		if rec.Code != http.StatusServiceUnavailable || preflightErrorBody(t, rec).Code != "preflight_unavailable" {
			t.Fatalf("unexpected cancel receipt = %d (%s)", rec.Code, rec.Body.String())
		}
	})
	for _, tc := range []struct {
		name, method, path, id string
	}{
		{"observe malformed id", http.MethodGet, "/api/probe-host-preflights/not-a-uuid", "not-a-uuid"},
		{"cancel malformed id", http.MethodPost, "/api/probe-host-preflights/not-a-uuid/cancel", "not-a-uuid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &preflightFake{op: preflightReceipt(domain.ProbePreflightQueued)}
			h := NewProbePreflightHandlers(fake, true)
			call := h.Observe
			if tc.method == http.MethodPost {
				call = h.Cancel
			}
			rec := preflightServe(h, tc.method, tc.path, tc.id, "", call)
			if rec.Code != http.StatusBadRequest || preflightErrorBody(t, rec).Code != "invalid_operation_id" {
				t.Fatalf("malformed id = %d (%s)", rec.Code, rec.Body.String())
			}
			if fake.calls != 0 {
				t.Fatalf("service called %d times for a malformed identity", fake.calls)
			}
		})
	}
	t.Run("missing operation", func(t *testing.T) {
		fake := &preflightFake{err: ports.ErrNotFound}
		h := NewProbePreflightHandlers(fake, true)
		rec := preflightServe(h, http.MethodGet, "/api/probe-host-preflights/"+preflightOpID, preflightOpID, "", h.Observe)
		if rec.Code != http.StatusNotFound || preflightErrorBody(t, rec).Code != "preflight_not_found" {
			t.Fatalf("missing operation = %d (%s)", rec.Code, rec.Body.String())
		}
	})
}

// TestProbePreflightHTTPErrors verifies the frozen error mapping, including
// the bounded Retry-After on busy and the ban on underlying diagnostics.
func TestProbePreflightHTTPErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		fakeErr    error
		status     int
		code       string
		retryAfter string
	}{
		{"validation", domain.ErrValidation, 400, "invalid_preflight", ""},
		{"ssh input", domain.ErrProbeSSHInput, 400, "invalid_preflight", ""},
		{"changed identity", ports.ErrConflict, 409, "preflight_conflict", ""},
		{"busy", domain.ErrProbePreflightBusy, 503, "preflights_busy", "5"},
		{"closed", domain.ErrProbePreflightClosed, 503, "preflights_closed", ""},
		{"underlying diagnostic", errors.New("storage backend failure " + leakedDiagnosticMarker), 503, "preflight_unavailable", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &preflightFake{err: tc.fakeErr}
			h := NewProbePreflightHandlers(fake, true)
			rec := preflightServe(h, http.MethodPost, "/api/probe-host-preflights", "", preflightBody(preflightKeyCanary), h.Submit)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if view := preflightErrorBody(t, rec); view.Code != tc.code {
				t.Fatalf("code = %q, want %q", view.Code, tc.code)
			}
			if got := rec.Header().Get("Retry-After"); got != tc.retryAfter {
				t.Fatalf("Retry-After = %q, want %q", got, tc.retryAfter)
			}
			if strings.Contains(rec.Body.String(), leakedDiagnosticMarker) {
				t.Fatalf("underlying diagnostic leaked: %s", rec.Body.String())
			}
		})
	}
}

// TestProbePreflightSurfaceGates covers unauthenticated access, the disabled
// feature flag and the missing-service surface.
func TestProbePreflightSurfaceGates(t *testing.T) {
	t.Run("unauthenticated", func(t *testing.T) {
		fake := &preflightFake{op: preflightReceipt(domain.ProbePreflightQueued)}
		h := NewProbePreflightHandlers(fake, true)
		e := echo.New()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/probe-host-preflights", strings.NewReader(preflightBody(preflightKeyCanary)))
		c := e.NewContext(req, rec)
		if err := h.Submit(c); err != nil {
			e.HTTPErrorHandler(err, c)
		}
		if rec.Code != http.StatusUnauthorized || preflightErrorBody(t, rec).Code != "unauthenticated" {
			t.Fatalf("unauthenticated = %d (%s)", rec.Code, rec.Body.String())
		}
		if fake.calls != 0 {
			t.Fatalf("service called %d times without an actor", fake.calls)
		}
	})
	for _, tc := range []struct {
		name    string
		svc     probePreflightRunner
		enabled bool
		want    string
	}{
		{"disabled", &preflightFake{op: preflightReceipt(domain.ProbePreflightQueued)}, false, "preflights_disabled"},
		{"no service", nil, true, "preflight_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := NewProbePreflightHandlers(tc.svc, tc.enabled)
			rec := preflightServe(h, http.MethodGet, "/api/probe-host-preflights/"+preflightOpID, preflightOpID, "", h.Observe)
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
			}
			if code := preflightErrorBody(t, rec).Code; code != tc.want {
				t.Fatalf("code = %q, want %q", code, tc.want)
			}
		})
	}
}

// TestProbePreflightCorruptReceipts fails closed on any receipt the domain
// rejects, and never serializes corrupt fields or invented outcomes.
func TestProbePreflightCorruptReceipts(t *testing.T) {
	unknownCode := preflightReceipt(domain.ProbePreflightQueued)
	unknownCode.State = "failed"
	unknownCode.ErrorCode = "mystery-code"
	for _, tc := range []struct {
		name string
		op   *domain.ProbePreflightOperation
	}{
		{"nil receipt", nil},
		{"unknown error code", unknownCode},
		{"nonterminal with error", func() *domain.ProbePreflightOperation {
			op := preflightReceipt(domain.ProbePreflightQueued)
			op.ErrorCode = "internal"
			return op
		}()},
		{"passed without facts", func() *domain.ProbePreflightOperation {
			op := preflightReceipt(domain.ProbePreflightPassed)
			op.Result = nil
			return op
		}()},
		{"bad owner", func() *domain.ProbePreflightOperation {
			op := preflightReceipt(domain.ProbePreflightQueued)
			op.OwnerID = "not-a-valid-owner-identity"
			return op
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &preflightFake{op: tc.op}
			h := NewProbePreflightHandlers(fake, true)
			rec := preflightServe(h, http.MethodGet, "/api/probe-host-preflights/"+preflightOpID, preflightOpID, "", h.Observe)
			if rec.Code != http.StatusServiceUnavailable || preflightErrorBody(t, rec).Code != "preflight_unavailable" {
				t.Fatalf("corrupt receipt = %d (%s)", rec.Code, rec.Body.String())
			}
			body := rec.Body.String()
			for _, leak := range []string{"mystery-code", "not-a-valid-owner-identity"} {
				if strings.Contains(body, leak) {
					t.Fatalf("corrupt field %q leaked: %s", leak, body)
				}
			}
		})
	}
}

// TestProbePreflightUTCTimestamps proves non-UTC stored timestamps are
// rendered as UTC on the wire.
func TestProbePreflightUTCTimestamps(t *testing.T) {
	zone := time.FixedZone("", 7*3600)
	op := preflightReceipt(domain.ProbePreflightQueued)
	op.CreatedAt = time.Date(2026, 10, 16, 17, 0, 0, 0, zone)
	op.UpdatedAt = op.CreatedAt
	op.DeadlineAt = op.CreatedAt.Add(domain.ProbePreflightLifetime)
	fake := &preflightFake{op: op}
	h := NewProbePreflightHandlers(fake, true)
	rec := preflightServe(h, http.MethodGet, "/api/probe-host-preflights/"+preflightOpID, preflightOpID, "", h.Observe)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"created_at":"2026-10-16T10:00:00Z"`)) {
		t.Fatalf("created_at not normalized to UTC: %s", rec.Body.String())
	}
}
