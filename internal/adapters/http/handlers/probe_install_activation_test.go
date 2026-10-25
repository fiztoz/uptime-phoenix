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
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const (
	activationOpID      = "3a6f9d12-84b7-4c5e-8f21-6d0b9e7a1c34"
	activationOwnerID   = "d2c41b88-5f0a-4e63-9a17-0b8e6f3d5a29"
	activationAddress   = "activate.example.test:22"
	activationProbeID   = "9e1b7c3a-2d54-4a68-b0f9-7c3e5a1d8b46"
	activationStreamID  = "4f8a2c6e-1b93-4d07-a5e8-2c9b7d1f3a60"
	activationBinarySHA = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	activationBasePath  = "/api/probe-installs"
	// activationKeyCanary and activationTokenCanary must never appear in a
	// response or receipt.
	activationKeyCanary   = "test-activation-transient-key-canary"
	activationTokenCanary = "test-activation-enrollment-token-canary"
	// activationLeakMarker is a fake underlying diagnostic that must never
	// reach any response.
	activationLeakMarker = "activation-leaked-internal-detail-marker"
)

var (
	// Host, install client, activation client and certificate pins are all
	// distinct so a leak of any one is detectable.
	activationHostPin          = "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32))
	activationInstallClientPin = "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32))
	activationClientPin        = "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	activationCertPin          = strings.Repeat("c4", 32)
	activationCreated          = time.Date(2026, 10, 24, 9, 30, 0, 0, time.UTC)
)

// activationSecrets are the strings no response may ever contain. The probe
// ID is deliberately absent: it is the public identity the view exposes.
func activationSecrets() []string {
	return []string{
		activationKeyCanary, activationTokenCanary, "activate.example.test", activationHostPin, activationInstallClientPin,
		activationClientPin, activationCertPin, activationStreamID, activationOwnerID, activationBinarySHA,
		"a7b8c9d0-0000-4000-8000", "/var/lib/", "/opt/uptime-phoenix", "/etc/systemd", "wss://", "/ws/probe",
		activationLeakMarker, "private_key", "enrollment_token", "requested_by", "owner_id", "attempt_id",
		"stream_id", "certificate_fingerprint", "fingerprint", "reference", "endpoint",
	}
}

// activationFake captures the handler's service contract. It copies the
// borrowed private-key bytes during the call so the test can both verify the
// exact key handed over and observe the handler's best-effort clearing.
type activationFake struct {
	submission  services.ProbeActivationSubmission
	operationID string
	keyCopy     []byte
	act         *domain.ProbeActivation
	err         error
	submits     int
	observes    int
	cancels     int
}

func (f *activationFake) calls() int { return f.submits + f.observes + f.cancels }

func (f *activationFake) Submit(_ context.Context, submission services.ProbeActivationSubmission) (*domain.ProbeActivation, error) {
	f.submits++
	f.submission = submission
	f.keyCopy = append([]byte(nil), submission.PrivateKey...)
	if f.err != nil {
		return nil, f.err
	}
	return f.act, nil
}

func (f *activationFake) Observe(_ context.Context, operationID string) (*domain.ProbeActivation, error) {
	f.observes++
	f.operationID = operationID
	if f.err != nil {
		return nil, f.err
	}
	return f.act, nil
}

func (f *activationFake) Cancel(_ context.Context, operationID string) (*domain.ProbeActivation, error) {
	f.cancels++
	f.operationID = operationID
	if f.err != nil {
		return nil, f.err
	}
	return f.act, nil
}

func activationInstallIdentity() domain.ProbeInstallIdentity {
	return domain.ProbeInstallIdentity{
		OperationID:          activationOpID,
		Host:                 domain.ProbeSSHHost{Address: activationAddress, User: "root"},
		HostKeyFingerprint:   activationHostPin,
		ClientKeyFingerprint: activationInstallClientPin,
		RequestedBy:          7,
		LayoutVersion:        domain.ProbeInstallLayoutVersion,
		Mode:                 domain.ProbeInstallSystemd,
		Binary:               domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "arm64", SHA256: activationBinarySHA, Size: 1048576},
	}
}

func activationIdentity() domain.ProbeActivationIdentity {
	return domain.ProbeActivationIdentity{
		Install:              activationInstallIdentity(),
		ProbeKey:             "edge-1",
		Name:                 "Edge One",
		Location:             "Bangkok",
		ListenPort:           8443,
		ClientKeyFingerprint: activationClientPin,
		RequestedBy:          7,
	}
}

func activationFacts() domain.ProbeActivationFacts {
	return domain.ProbeActivationFacts{ProbeID: activationProbeID, StreamID: activationStreamID, CertificateFingerprint: activationCertPin}
}

func activationAt(seconds int) time.Time {
	return activationCreated.Add(time.Duration(seconds) * time.Second)
}

func activationAttempt(n int) string { return fmt.Sprintf("a7b8c9d0-0000-4000-8000-%012d", n) }

// activationReceipt builds a domain.Valid activation. stepStates lists the
// leading step states; the rest stay planned. An unknown step carries the
// operation code, as the journal requires; the probe_init facts are present
// exactly when probe_init is applied.
func activationReceipt(state, code string, stepStates ...string) *domain.ProbeActivation {
	identity := activationIdentity()
	activation := &domain.ProbeActivation{
		Identity:   identity,
		OwnerID:    activationOwnerID,
		State:      state,
		ErrorCode:  code,
		CreatedAt:  activationCreated,
		UpdatedAt:  activationCreated,
		DeadlineAt: activationCreated.Add(domain.ProbeActivationLifetime),
	}
	for i, spec := range identity.Steps() {
		step := domain.ProbeInstallResource{Spec: spec, State: domain.ProbeInstallResourcePlanned}
		if i < len(stepStates) && stepStates[i] != domain.ProbeInstallResourcePlanned {
			step.State = stepStates[i]
			step.AttemptID = activationAttempt(i + 1)
			step.StartedAt = activationAt(10*i + 1)
			activation.UpdatedAt = step.StartedAt
			if step.State != domain.ProbeInstallResourceInFlight {
				step.FinishedAt = activationAt(10*i + 2)
				activation.UpdatedAt = step.FinishedAt
			}
			switch step.State {
			case domain.ProbeInstallResourceNotStarted:
				step.ErrorCode = "not_started"
			case domain.ProbeInstallResourceUnknown:
				step.ErrorCode = code
			case domain.ProbeInstallResourceApplied:
				if i == 0 {
					activation.Facts = activationFacts()
				}
			}
		}
		activation.Steps = append(activation.Steps, step)
	}
	return activation
}

// activationLifecycle builds a domain.Valid receipt for a lifecycle state.
func activationLifecycle(state string) *domain.ProbeActivation {
	applied, inFlight := domain.ProbeInstallResourceApplied, domain.ProbeInstallResourceInFlight
	switch state {
	case domain.ProbeActivationQueued:
		return activationReceipt(state, "")
	case domain.ProbeActivationRunning, domain.ProbeActivationCancelRequested:
		return activationReceipt(state, "", applied, inFlight)
	case domain.ProbeActivationActive:
		return activationReceipt(state, "", applied, applied, applied, applied)
	case domain.ProbeActivationFailed:
		return activationReceipt(state, "execution_failed", applied, domain.ProbeInstallResourceNotStarted)
	case domain.ProbeActivationCanceled:
		return activationReceipt(state, "canceled", applied, domain.ProbeInstallResourceNotStarted)
	case domain.ProbeActivationInterrupted:
		return activationReceipt(state, "owner_interrupted", applied, domain.ProbeInstallResourceUnknown)
	}
	panic("unknown activation state " + state)
}

// activationAllReceipts enumerates one valid receipt per reachable lifecycle
// shape, including all three interruption causes and every probe_id phase.
func activationAllReceipts() map[string]*domain.ProbeActivation {
	applied, inFlight, unknown := domain.ProbeInstallResourceApplied, domain.ProbeInstallResourceInFlight, domain.ProbeInstallResourceUnknown
	expired := activationReceipt(domain.ProbeActivationInterrupted, "deadline_expired", applied, inFlight)
	expired.Steps[1].State = unknown
	expired.Steps[1].ErrorCode = "deadline_expired"
	expired.Steps[1].FinishedAt = expired.DeadlineAt
	expired.UpdatedAt = expired.DeadlineAt
	return map[string]*domain.ProbeActivation{
		"queued":                   activationLifecycle(domain.ProbeActivationQueued),
		"running":                  activationLifecycle(domain.ProbeActivationRunning),
		"running init in flight":   activationReceipt(domain.ProbeActivationRunning, "", inFlight),
		"cancel_requested":         activationLifecycle(domain.ProbeActivationCancelRequested),
		"cancel_requested init":    activationReceipt(domain.ProbeActivationCancelRequested, "", inFlight),
		"active":                   activationLifecycle(domain.ProbeActivationActive),
		"failed":                   activationLifecycle(domain.ProbeActivationFailed),
		"canceled":                 activationLifecycle(domain.ProbeActivationCanceled),
		"interrupted owner":        activationLifecycle(domain.ProbeActivationInterrupted),
		"interrupted outcome":      activationReceipt(domain.ProbeActivationInterrupted, "outcome_unknown", applied, unknown),
		"interrupted init unknown": activationReceipt(domain.ProbeActivationInterrupted, "outcome_unknown", unknown),
		"interrupted deadline":     expired,
		"canceled while queued":    activationReceipt(domain.ProbeActivationCanceled, "canceled"),
		"failed before any step":   activationReceipt(domain.ProbeActivationFailed, "unsupported_host"),
		"failed init not started":  activationReceipt(domain.ProbeActivationFailed, "execution_failed", domain.ProbeInstallResourceNotStarted),
	}
}

// activationMember is one ordered JSON member of the request body.
type activationMember struct{ key, raw string }

func activationQuote(s string) string {
	out, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(out)
}

// activationMembers is the five-member request in canonical order.
func activationMembers(privateKey string) []activationMember {
	return []activationMember{
		{"private_key", activationQuote(privateKey)},
		{"probe_key", activationQuote("edge-1")},
		{"name", activationQuote("Edge One")},
		{"location", activationQuote("Bangkok")},
		{"listen_port", "8443"},
	}
}

func activationJSON(members []activationMember) string {
	parts := make([]string, len(members))
	for i, m := range members {
		parts[i] = activationQuote(m.key) + ":" + m.raw
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func activationBody(privateKey string) string { return activationJSON(activationMembers(privateKey)) }

func activationSet(members []activationMember, key, raw string) []activationMember {
	out := append([]activationMember(nil), members...)
	for i := range out {
		if out[i].key == key {
			out[i].raw = raw
		}
	}
	return out
}

func activationDrop(members []activationMember, key string) []activationMember {
	var out []activationMember
	for _, m := range members {
		if m.key != key {
			out = append(out, m)
		}
	}
	return out
}

func activationRename(members []activationMember, from, to string) []activationMember {
	out := append([]activationMember(nil), members...)
	for i := range out {
		if out[i].key == from {
			out[i].key = to
		}
	}
	return out
}

func activationExtra(members []activationMember, key, raw string) []activationMember {
	return append(append([]activationMember(nil), members...), activationMember{key, raw})
}

// activationRoute is one handler route with its request wiring.
type activationRoute struct {
	name, method, path, id, body string
	call                         func(h *ProbeActivationHandlers) func(echo.Context) error
}

func activationRoutes() []activationRoute {
	return []activationRoute{
		{"submit", http.MethodPost, activationBasePath + "/" + activationOpID + "/activate", activationOpID, activationBody(activationKeyCanary),
			func(h *ProbeActivationHandlers) func(echo.Context) error { return h.Submit }},
		{"observe", http.MethodGet, activationBasePath + "/" + activationOpID + "/activation", activationOpID, "",
			func(h *ProbeActivationHandlers) func(echo.Context) error { return h.Observe }},
		{"cancel", http.MethodPost, activationBasePath + "/" + activationOpID + "/activation/cancel", activationOpID, "",
			func(h *ProbeActivationHandlers) func(echo.Context) error { return h.Cancel }},
	}
}

var (
	activationSubmitRoute  = activationRoutes()[0]
	activationObserveRoute = activationRoutes()[1]
	activationCancelRoute  = activationRoutes()[2]
)

// activationServeAs invokes one handler method with the given authenticated
// actor (0 means no actor in context), mirroring the router's param wiring
// without claiming middleware coverage. It also asserts the universal
// response invariants: no-store caching and no secret in any response body.
func activationServeAs(t *testing.T, actor int64, method, path, id, body string, call func(echo.Context) error) *httptest.ResponseRecorder {
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
	for _, secret := range activationSecrets() {
		if strings.Contains(response, secret) {
			t.Fatalf("response leaks %q: %s", secret, response)
		}
	}
	return rec
}

func activationServe(t *testing.T, r activationRoute, h *ProbeActivationHandlers) *httptest.ResponseRecorder {
	t.Helper()
	return activationServeAs(t, 1, r.method, r.path, r.id, r.body, r.call(h))
}

func activationServeBody(t *testing.T, r activationRoute, h *ProbeActivationHandlers, body string) *httptest.ResponseRecorder {
	t.Helper()
	return activationServeAs(t, 1, r.method, r.path, r.id, body, r.call(h))
}

func activationErrorBody(t *testing.T, rec *httptest.ResponseRecorder) regionalErrorView {
	t.Helper()
	var view regionalErrorView
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("error body is not the fixed error view: %v (%s)", err, rec.Body.String())
	}
	return view
}

func activationKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func activationSameKeys(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s keys = %v, want exactly %v", what, got, want)
	}
}

func activationStatusOf(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var view struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	return view.Status
}

// TestProbeActivationSubmitStrictBinding refuses every wire shape outside the
// five-member object and never reaches the service with rejected input.
func TestProbeActivationSubmitStrictBinding(t *testing.T) {
	base := activationMembers(activationKeyCanary)
	type bindCase struct {
		name, body string
		status     int
		code       string
	}
	fixed := []bindCase{
		{"empty", "", 400, "invalid_activation"},
		{"malformed", `{`, 400, "invalid_activation"},
		{"truncated member", `{"private_key":`, 400, "invalid_activation"},
		{"not an object", `[]`, 400, "invalid_activation"},
		{"null body", `null`, 400, "invalid_activation"},
		{"string body", `"x"`, 400, "invalid_activation"},
		{"empty object", `{}`, 400, "invalid_activation"},
		{"operation id in body", activationJSON(activationExtra(base, "operation_id", activationQuote(activationOpID))), 400, "invalid_activation"},
		{"actor smuggled in body", activationJSON(activationExtra(base, "requested_by", "1")), 400, "invalid_activation"},
		{"actor string smuggled in body", activationJSON(activationExtra(base, "requested_by", `"1"`)), 400, "invalid_activation"},
		{"ssh address smuggled in body", activationJSON(activationExtra(base, "ssh_address", `"evil.example.test:22"`)), 400, "invalid_activation"},
		{"host key smuggled in body", activationJSON(activationExtra(base, "host_key_fingerprint", `"SHA256:x"`)), 400, "invalid_activation"},
		{"enrollment token in body", activationJSON(activationExtra(base, "enrollment_token", `"tok"`)), 400, "invalid_activation"},
		{"endpoint in body", activationJSON(activationExtra(base, "endpoint", `"wss://evil.example.test/ws/probe/v1"`)), 400, "invalid_activation"},
		{"tls pin in body", activationJSON(activationExtra(base, "tls_pin", `"x"`)), 400, "invalid_activation"},
		{"trailing object", activationBody(activationKeyCanary) + ` {}`, 400, "invalid_activation"},
		{"trailing garbage", activationBody(activationKeyCanary) + `garbage`, 400, "invalid_activation"},
		{"trailing comma", strings.TrimSuffix(activationBody(activationKeyCanary), "}") + ",}", 400, "invalid_activation"},
		{"oversized body", strings.Repeat("a", 128<<10+1), 413, "activation_too_large"},
		{"oversized valid prefix", activationBody(activationKeyCanary) + strings.Repeat(" ", 128<<10), 413, "activation_too_large"},
		{"oversized member name", `{"` + strings.Repeat("k", 128<<10) + `":"x"}`, 413, "activation_too_large"},
		{"oversized private key", activationBody(strings.Repeat("k", domain.MaxProbeInstallPrivateKeySize+1)), 400, "invalid_activation"},
		{"empty private key", activationJSON(activationSet(base, "private_key", `""`)), 400, "invalid_activation"},
	}
	// Every loop below appends to the fixed table; size it up front.
	cases := make([]bindCase, 0, 256)
	cases = append(cases, fixed...)
	// probe_key must be the registry's canonical slug and never the reserved local key.
	for _, key := range []string{"", "Edge", "EDGE-1", "-edge", "edge-", "local", strings.Repeat("a", 64), "edge_1", "edge 1", "edge.1", "edge/1", "edgé", "edge\n", " edge", "edge "} {
		cases = append(cases, bindCase{"probe_key " + strconv.Quote(key), activationJSON(activationSet(base, "probe_key", activationQuote(key))), 400, "invalid_activation"})
	}
	// name: 1..200 runes, no control characters, no surrounding whitespace.
	for _, name := range []string{"", " edge", "edge ", "\tedge", "ed\nge", "ed\x00ge", "ed\x7fge", strings.Repeat("a", 201), strings.Repeat("é", 201), " "} {
		cases = append(cases, bindCase{"name " + strconv.Quote(name), activationJSON(activationSet(base, "name", activationQuote(name))), 400, "invalid_activation"})
	}
	// location: may be empty, but 0..255 runes, no control characters, no surrounding whitespace.
	for _, location := range []string{" x", "x ", "x\ny", "x\x00y", strings.Repeat("a", 256), strings.Repeat("é", 256)} {
		cases = append(cases, bindCase{"location " + strconv.Quote(location), activationJSON(activationSet(base, "location", activationQuote(location))), 400, "invalid_activation"})
	}
	// listen_port: 0 (default) or exactly the installed unit's 8443; the
	// probe listens nowhere else.
	for _, port := range []string{"1", "80", "1023", "1024", "8080", "9443", "65535", "65536", "70000", "99999", "4294967296"} {
		cases = append(cases, bindCase{"listen_port " + port, activationJSON(activationSet(base, "listen_port", port)), 400, "invalid_activation"})
	}
	// Every member: missing, null, duplicate, case variant, wrong JSON type.
	for _, m := range base {
		cases = append(cases,
			bindCase{"missing " + m.key, activationJSON(activationDrop(base, m.key)), 400, "invalid_activation"},
			bindCase{"null " + m.key, activationJSON(activationSet(base, m.key, "null")), 400, "invalid_activation"},
			bindCase{"duplicate " + m.key, activationJSON(append(append([]activationMember(nil), base...), m)), 400, "invalid_activation"},
			bindCase{"upper case variant " + m.key, activationJSON(activationRename(base, m.key, strings.ToUpper(m.key))), 400, "invalid_activation"},
			bindCase{"title case variant " + m.key, activationJSON(activationRename(base, m.key, strings.ToUpper(m.key[:1])+m.key[1:])), 400, "invalid_activation"},
			bindCase{"padded key " + m.key, activationJSON(activationRename(base, m.key, m.key+" ")), 400, "invalid_activation"},
		)
		wrong := []string{`42`, `true`, `[]`, `{}`, `["x"]`, `{"a":1}`}
		if m.key == "listen_port" {
			wrong = []string{`"8443"`, `true`, `[]`, `{}`, `[8443]`, `""`}
		}
		for _, raw := range wrong {
			cases = append(cases, bindCase{"wrong type " + m.key + "=" + raw, activationJSON(activationSet(base, m.key, raw)), 400, "invalid_activation"})
		}
	}
	// listen_port must be a plain non-negative base-10 integer literal.
	for _, literal := range []string{
		"-1", "-0", "+1", "1.5", "1.0", "8443.0", "8443.", ".5", "1e3", "1E3", "1e+3", "1e-3", "8.443e3", "8443e0",
		"01", "08443", "00", "0x20fb", "9223372036854775808", "99999999999999999999", "NaN", "Infinity",
	} {
		cases = append(cases, bindCase{"listen_port literal " + literal, activationJSON(activationSet(base, "listen_port", literal)), 400, "invalid_activation"})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &activationFake{act: activationLifecycle(domain.ProbeActivationQueued)}
			h := NewProbeActivationHandlers(fake, true)
			rec := activationServeBody(t, activationSubmitRoute, h, tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if view := activationErrorBody(t, rec); view.Code != tc.code || view.Error == "" {
				t.Fatalf("error view = %+v, want code %q with a fixed message", view, tc.code)
			}
			if fake.calls() != 0 {
				t.Fatalf("service called %d times for rejected input", fake.calls())
			}
		})
	}
}

// TestProbeActivationSubmitPathIdentity proves the install identity comes from
// the path only: a malformed path identity refuses before the body is read.
func TestProbeActivationSubmitPathIdentity(t *testing.T) {
	for _, id := range []string{"not-a-uuid", strings.ToUpper(activationOpID), "local", "00000000-0000-0000-0000-000000000000", activationOpID + "x", ""} {
		t.Run("malformed id "+id, func(t *testing.T) {
			fake := &activationFake{act: activationLifecycle(domain.ProbeActivationQueued)}
			h := NewProbeActivationHandlers(fake, true)
			rec := activationServeAs(t, 1, http.MethodPost, activationBasePath+"/"+id+"/activate", id, activationBody(activationKeyCanary), h.Submit)
			if rec.Code != http.StatusBadRequest || activationErrorBody(t, rec).Code != "invalid_operation_id" || fake.calls() != 0 {
				t.Fatalf("malformed id = %d, calls = %d (%s)", rec.Code, fake.calls(), rec.Body.String())
			}
		})
	}
	t.Run("oversized body is not read for a malformed id", func(t *testing.T) {
		fake := &activationFake{act: activationLifecycle(domain.ProbeActivationQueued)}
		h := NewProbeActivationHandlers(fake, true)
		rec := activationServeAs(t, 1, http.MethodPost, activationBasePath+"/bad/activate", "bad", strings.Repeat("a", 128<<10+1), h.Submit)
		if rec.Code != http.StatusBadRequest || activationErrorBody(t, rec).Code != "invalid_operation_id" {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
	})
	t.Run("path identity is forwarded", func(t *testing.T) {
		fake := &activationFake{act: activationLifecycle(domain.ProbeActivationQueued)}
		h := NewProbeActivationHandlers(fake, true)
		rec := activationServe(t, activationSubmitRoute, h)
		if rec.Code != http.StatusAccepted || fake.submission.OperationID != activationOpID {
			t.Fatalf("status = %d, operation id = %q (%s)", rec.Code, fake.submission.OperationID, rec.Body.String())
		}
	})
}

// TestProbeActivationSubmitAcceptedBoundaries accepts the largest legal key and
// text fields, the smallest ports and the documented whitespace and key-order
// freedoms of one JSON object.
func TestProbeActivationSubmitAcceptedBoundaries(t *testing.T) {
	base := activationMembers(activationKeyCanary)
	reversed := make([]activationMember, len(base))
	for i, m := range base {
		reversed[len(base)-1-i] = m
	}
	pem := "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjE=\n-----END OPENSSH PRIVATE KEY-----\n"
	for _, tc := range []struct {
		name, body string
		check      func(t *testing.T, got services.ProbeActivationSubmission)
	}{
		{"largest private key", activationBody(strings.Repeat("k", domain.MaxProbeInstallPrivateKeySize)), func(t *testing.T, got services.ProbeActivationSubmission) {
			if len(got.PrivateKey) != domain.MaxProbeInstallPrivateKeySize {
				t.Fatalf("key length = %d", len(got.PrivateKey))
			}
		}},
		{"smallest private key", activationBody("k"), func(t *testing.T, got services.ProbeActivationSubmission) {
			if string(got.PrivateKey) != "k" {
				t.Fatalf("key = %q", got.PrivateKey)
			}
		}},
		{"escaped multi-line key", activationBody(pem), func(t *testing.T, got services.ProbeActivationSubmission) {
			if string(got.PrivateKey) != pem {
				t.Fatalf("key = %q, want the decoded PEM", got.PrivateKey)
			}
		}},
		{"default port zero", activationJSON(activationSet(base, "listen_port", "0")), func(t *testing.T, got services.ProbeActivationSubmission) {
			if got.ListenPort != 0 {
				t.Fatalf("port = %d, want 0 (service applies the default)", got.ListenPort)
			}
		}},
		{"default port", activationJSON(activationSet(base, "listen_port", "0")), func(t *testing.T, got services.ProbeActivationSubmission) {
			if got.ListenPort != 0 {
				t.Fatalf("port = %d, want 0 passed through as the default", got.ListenPort)
			}
		}},
		{"unit port", activationJSON(activationSet(base, "listen_port", "8443")), func(t *testing.T, got services.ProbeActivationSubmission) {
			if got.ListenPort != 8443 {
				t.Fatalf("port = %d", got.ListenPort)
			}
		}},
		{"empty location", activationJSON(activationSet(base, "location", `""`)), func(t *testing.T, got services.ProbeActivationSubmission) {
			if got.Location != "" {
				t.Fatalf("location = %q", got.Location)
			}
		}},
		{"single character probe key", activationJSON(activationSet(base, "probe_key", `"a"`)), func(t *testing.T, got services.ProbeActivationSubmission) {
			if got.ProbeKey != "a" {
				t.Fatalf("probe key = %q", got.ProbeKey)
			}
		}},
		{"longest probe key", activationJSON(activationSet(base, "probe_key", activationQuote(strings.Repeat("a", 63)))), func(t *testing.T, got services.ProbeActivationSubmission) {
			if len(got.ProbeKey) != 63 {
				t.Fatalf("probe key length = %d", len(got.ProbeKey))
			}
		}},
		{"longest name counted in runes", activationJSON(activationSet(base, "name", activationQuote(strings.Repeat("é", 200)))), func(t *testing.T, got services.ProbeActivationSubmission) {
			if got.Name != strings.Repeat("é", 200) {
				t.Fatalf("name = %q", got.Name)
			}
		}},
		{"longest location counted in runes", activationJSON(activationSet(base, "location", activationQuote(strings.Repeat("é", 255)))), func(t *testing.T, got services.ProbeActivationSubmission) {
			if got.Location != strings.Repeat("é", 255) {
				t.Fatalf("location = %q", got.Location)
			}
		}},
		{"inner whitespace in name", activationJSON(activationSet(base, "name", `"Edge  One"`)), func(t *testing.T, got services.ProbeActivationSubmission) {
			if got.Name != "Edge  One" {
				t.Fatalf("name = %q", got.Name)
			}
		}},
		{"reordered members with whitespace", " \n" + strings.ReplaceAll(activationJSON(reversed), ",", " ,\n ") + " \n", nil},
		{"escaped key spelling", strings.Replace(activationBody(activationKeyCanary), `"listen_port"`, `"listen_port"`, 1), func(t *testing.T, got services.ProbeActivationSubmission) {
			if got.ListenPort != 8443 {
				t.Fatalf("port = %d", got.ListenPort)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &activationFake{act: activationLifecycle(domain.ProbeActivationQueued)}
			h := NewProbeActivationHandlers(fake, true)
			rec := activationServeBody(t, activationSubmitRoute, h, tc.body)
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

// TestProbeActivationSubmitAccepted verifies the 202-after-commit contract,
// the exact explicit view, actor-from-context, the full submission mapping and
// the best-effort clearing of the owned private-key buffer.
func TestProbeActivationSubmitAccepted(t *testing.T) {
	fake := &activationFake{act: activationLifecycle(domain.ProbeActivationQueued)}
	h := NewProbeActivationHandlers(fake, true)
	rec := activationServeAs(t, 42, http.MethodPost, activationSubmitRoute.path, activationOpID, activationBody(activationKeyCanary), h.Submit)
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
	got := fake.submission
	got.PrivateKey = nil
	want := services.ProbeActivationSubmission{OperationID: activationOpID, ProbeKey: "edge-1", Name: "Edge One", Location: "Bangkok", ListenPort: 8443, RequestedBy: 42}
	if got.OperationID != want.OperationID || got.ProbeKey != want.ProbeKey || got.Name != want.Name || got.Location != want.Location ||
		got.ListenPort != want.ListenPort || got.RequestedBy != want.RequestedBy {
		t.Fatalf("submission mapping = %+v, want %+v", got, want)
	}
	if string(fake.keyCopy) != activationKeyCanary {
		t.Fatalf("service key = %q, want the transient key content", fake.keyCopy)
	}
	for _, b := range fake.submission.PrivateKey {
		if b != 0 {
			t.Fatal("owned private-key buffer was not cleared after the call")
		}
	}
	var view map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	activationSameKeys(t, "view", activationKeys(view),
		"operation_id", "kind", "status", "created_at", "updated_at", "deadline_at", "probe_key", "name", "location", "listen_port", "probe_id", "steps", "failed_phase", "error")
	for key, wantRaw := range map[string]string{
		"operation_id": strconv.Quote(activationOpID), "kind": `"probe_activation"`, "status": `"queued"`,
		"created_at": `"2026-10-24T09:30:00Z"`, "updated_at": `"2026-10-24T09:30:00Z"`, "deadline_at": `"2026-10-24T09:45:00Z"`,
		"probe_key": `"edge-1"`, "name": `"Edge One"`, "location": `"Bangkok"`, "listen_port": "8443", "probe_id": "null", "error": "null",
	} {
		if string(view[key]) != wantRaw {
			t.Fatalf("view %s = %s, want %s", key, view[key], wantRaw)
		}
	}
}

// TestProbeActivationSubmitStatuses pins the 202/200 split: only a receipt
// that still has work to do answers 202; a settled receipt (exact retry,
// including a completed activation) is 200.
func TestProbeActivationSubmitStatuses(t *testing.T) {
	for _, tc := range []struct {
		state  string
		status int
	}{
		{domain.ProbeActivationQueued, 202},
		{domain.ProbeActivationRunning, 202},
		{domain.ProbeActivationCancelRequested, 202},
		{domain.ProbeActivationActive, 200},
		{domain.ProbeActivationFailed, 200},
		{domain.ProbeActivationCanceled, 200},
		{domain.ProbeActivationInterrupted, 200},
	} {
		t.Run(tc.state, func(t *testing.T) {
			fake := &activationFake{act: activationLifecycle(tc.state)}
			h := NewProbeActivationHandlers(fake, true)
			rec := activationServe(t, activationSubmitRoute, h)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
			}
			if got := activationStatusOf(t, rec); got != tc.state {
				t.Fatalf("status field = %q, want the stored state %q", got, tc.state)
			}
		})
	}
}

// TestProbeActivationViewShape proves every lifecycle shape renders exactly
// the documented keys (and no others) at every nesting level, with probe_id
// null until probe_init applied and the public identity afterwards.
func TestProbeActivationViewShape(t *testing.T) {
	wantKinds := []string{"probe_init", "registration", "service_start", "enrollment"}
	for name, activation := range activationAllReceipts() {
		t.Run(name, func(t *testing.T) {
			if !activation.Valid() {
				t.Fatal("fixture receipt is not domain.Valid")
			}
			fake := &activationFake{act: activation}
			h := NewProbeActivationHandlers(fake, true)
			rec := activationServe(t, activationObserveRoute, h)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
			}
			var view map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			activationSameKeys(t, "view", activationKeys(view),
				"operation_id", "kind", "status", "created_at", "updated_at", "deadline_at", "probe_key", "name", "location", "listen_port", "probe_id", "steps", "failed_phase", "error")
			if string(view["kind"]) != `"probe_activation"` || string(view["status"]) != strconv.Quote(activation.State) || string(view["operation_id"]) != strconv.Quote(activationOpID) {
				t.Fatalf("identity fields = %s %s %s", view["kind"], view["status"], view["operation_id"])
			}
			if string(view["probe_key"]) != `"edge-1"` || string(view["name"]) != `"Edge One"` || string(view["location"]) != `"Bangkok"` || string(view["listen_port"]) != "8443" {
				t.Fatalf("request echo = %s %s %s %s", view["probe_key"], view["name"], view["location"], view["listen_port"])
			}

			wantProbeID := "null"
			if activation.Steps[0].State == domain.ProbeInstallResourceApplied {
				wantProbeID = strconv.Quote(activationProbeID)
			}
			if string(view["probe_id"]) != wantProbeID {
				t.Fatalf("probe_id = %s, want %s", view["probe_id"], wantProbeID)
			}

			var steps []map[string]json.RawMessage
			if err := json.Unmarshal(view["steps"], &steps); err != nil {
				t.Fatal(err)
			}
			if len(steps) != len(wantKinds) {
				t.Fatalf("steps = %d, want %d", len(steps), len(wantKinds))
			}
			for i, step := range steps {
				activationSameKeys(t, "step", activationKeys(step), "kind", "state", "error_code", "started_at", "finished_at")
				probeWantStepTime(t, step["started_at"], activation.Steps[i].StartedAt)
				probeWantStepTime(t, step["finished_at"], activation.Steps[i].FinishedAt)
				if string(step["kind"]) != strconv.Quote(wantKinds[i]) {
					t.Fatalf("step %d kind = %s, want %s (order must follow the journal ordinals)", i, step["kind"], wantKinds[i])
				}
				if string(step["state"]) != strconv.Quote(activation.Steps[i].State) {
					t.Fatalf("step %d state = %s, want %s", i, step["state"], activation.Steps[i].State)
				}
				wantCode := "null"
				if code := activation.Steps[i].ErrorCode; code != "" {
					wantCode = strconv.Quote(code)
				}
				if string(step["error_code"]) != wantCode {
					t.Fatalf("step %d error_code = %s, want %s", i, step["error_code"], wantCode)
				}
			}

			probeWantFailedPhase(t, view["failed_phase"], activation.State, activation.Steps)
			if activation.ErrorCode == "" {
				if string(view["error"]) != "null" {
					t.Fatalf("error = %s, want null", view["error"])
				}
				return
			}
			var errView map[string]json.RawMessage
			if err := json.Unmarshal(view["error"], &errView); err != nil {
				t.Fatalf("error is not an object: %s", view["error"])
			}
			activationSameKeys(t, "error", activationKeys(errView), "code", "message")
			if string(errView["code"]) != strconv.Quote(activation.ErrorCode) || len(errView["message"]) <= 2 {
				t.Fatalf("error = %s", view["error"])
			}
		})
	}
}

// TestProbeActivationProbeIDPhases pins when the public probe identity appears:
// never before probe_init applied, always afterwards, on every route.
func TestProbeActivationProbeIDPhases(t *testing.T) {
	applied, inFlight, unknown := domain.ProbeInstallResourceApplied, domain.ProbeInstallResourceInFlight, domain.ProbeInstallResourceUnknown
	for _, tc := range []struct {
		name string
		act  *domain.ProbeActivation
		want string
	}{
		{"queued", activationReceipt(domain.ProbeActivationQueued, ""), "null"},
		{"init in flight", activationReceipt(domain.ProbeActivationRunning, "", inFlight), "null"},
		{"init outcome unknown", activationReceipt(domain.ProbeActivationInterrupted, "outcome_unknown", unknown), "null"},
		{"failed before init", activationReceipt(domain.ProbeActivationFailed, "unsupported_host"), "null"},
		{"init applied", activationReceipt(domain.ProbeActivationRunning, "", applied, inFlight), strconv.Quote(activationProbeID)},
		{"registration failed after init", activationReceipt(domain.ProbeActivationFailed, "execution_failed", applied, domain.ProbeInstallResourceNotStarted), strconv.Quote(activationProbeID)},
		{"active", activationLifecycle(domain.ProbeActivationActive), strconv.Quote(activationProbeID)},
	} {
		for _, route := range []activationRoute{activationObserveRoute, activationSubmitRoute} {
			t.Run(tc.name+" "+route.name, func(t *testing.T) {
				h := NewProbeActivationHandlers(&activationFake{act: tc.act}, true)
				rec := activationServe(t, route, h)
				var view map[string]json.RawMessage
				if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
					t.Fatal(err)
				}
				if string(view["probe_id"]) != tc.want {
					t.Fatalf("probe_id = %s, want %s", view["probe_id"], tc.want)
				}
			})
		}
	}
}

// TestProbeActivationErrorAllowlist pins the fixed operation error allowlist:
// it covers every code the journal can store, each with its own fixed message,
// and nothing else. It also pins the allowlist to the installer's code set, so
// a code added to one surface cannot silently drift from the other.
func TestProbeActivationErrorAllowlist(t *testing.T) {
	applied, notStarted := domain.ProbeInstallResourceApplied, domain.ProbeInstallResourceNotStarted
	failedCodes := []string{"unsupported_host", "artifact_rejected", "resource_conflict", "execution_failed", "permission_denied", "runtime_unverified", "not_started", "internal"}
	type codeCase struct {
		name, state, code string
		activation        *domain.ProbeActivation
	}
	cases := make([]codeCase, 0, len(failedCodes)+4)
	for _, code := range failedCodes {
		if !(domain.ProbeInstallOutcome{State: domain.ProbeInstallFailed, ErrorCode: code}).Valid() {
			t.Fatalf("journal no longer accepts failed code %q: update the allowlist", code)
		}
		cases = append(cases, codeCase{"failed " + code, domain.ProbeActivationFailed, code, activationReceipt(domain.ProbeActivationFailed, code, applied, notStarted)})
	}
	all := activationAllReceipts()
	cases = append(cases,
		codeCase{"canceled", domain.ProbeActivationCanceled, "canceled", activationLifecycle(domain.ProbeActivationCanceled)},
		codeCase{"owner_interrupted", domain.ProbeActivationInterrupted, "owner_interrupted", activationLifecycle(domain.ProbeActivationInterrupted)},
		codeCase{"outcome_unknown", domain.ProbeActivationInterrupted, "outcome_unknown", all["interrupted outcome"]},
		codeCase{"deadline_expired", domain.ProbeActivationInterrupted, "deadline_expired", all["interrupted deadline"]},
	)
	seen := map[string]bool{}
	messages := map[string]string{}
	for _, tc := range cases {
		seen[tc.code] = true
		t.Run(tc.name, func(t *testing.T) {
			if !tc.activation.Valid() {
				t.Fatal("fixture receipt is not domain.Valid")
			}
			h := NewProbeActivationHandlers(&activationFake{act: tc.activation}, true)
			rec := activationServe(t, activationObserveRoute, h)
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
			if view.Status != tc.state || view.Error.Code != tc.code || view.Error.Message != probeActivationStoredErrorMessages[tc.code] || view.Error.Message == "" {
				t.Fatalf("view = %+v, want state %q code %q with its fixed message", view, tc.state, tc.code)
			}
		})
	}
	// The allowlist is exactly the journal's code set: no stale or missing entry.
	if len(probeActivationStoredErrorMessages) != len(seen) {
		t.Fatalf("allowlist has %d codes, journal code set has %d", len(probeActivationStoredErrorMessages), len(seen))
	}
	if got, want := activationKeys(probeActivationStoredErrorMessages), activationKeys(probeInstallStoredErrorMessages); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("activation error codes %v drifted from the installer's %v", got, want)
	}
	for code, message := range probeActivationStoredErrorMessages {
		if !seen[code] {
			t.Fatalf("allowlist code %q is not a journal code", code)
		}
		if other, dup := messages[message]; dup {
			t.Fatalf("codes %q and %q share the message %q", code, other, message)
		}
		messages[message] = code
		for _, secret := range activationSecrets() {
			if strings.Contains(message, secret) {
				t.Fatalf("message for %q leaks %q", code, secret)
			}
		}
	}
	// Step codes are limited to the four the journal can store per step.
	if got := activationKeys(probeActivationStepErrorCodes); strings.Join(got, ",") != "deadline_expired,not_started,outcome_unknown,owner_interrupted" {
		t.Fatalf("step error code allowlist = %v", got)
	}
	if got := activationKeys(probeActivationStepKinds); strings.Join(got, ",") != "enrollment,probe_init,registration,service_start" {
		t.Fatalf("step kind allowlist = %v", got)
	}
}

// TestProbeActivationCorruptReceipts fails closed on any receipt the domain
// rejects, on a receipt for a different operation, and on anything outside the
// allowlists; nothing corrupt is ever serialized, on any route.
func TestProbeActivationCorruptReceipts(t *testing.T) {
	mutate := func(state string, f func(*domain.ProbeActivation)) *domain.ProbeActivation {
		activation := activationLifecycle(state)
		f(activation)
		return activation
	}
	const otherOperation = "5e4d3c2b-1a09-4f8e-b7d6-c5b4a3928170"
	for name, activation := range map[string]*domain.ProbeActivation{
		"nil receipt":        nil,
		"unknown error code": mutate(domain.ProbeActivationFailed, func(a *domain.ProbeActivation) { a.ErrorCode = "mystery-code" }),
		"free text code":     mutate(domain.ProbeActivationFailed, func(a *domain.ProbeActivation) { a.ErrorCode = activationLeakMarker }),
		"token as code":      mutate(domain.ProbeActivationFailed, func(a *domain.ProbeActivation) { a.ErrorCode = activationTokenCanary }),
		"nonterminal with error": mutate(domain.ProbeActivationRunning, func(a *domain.ProbeActivation) {
			a.ErrorCode = "internal"
		}),
		"failed without code": mutate(domain.ProbeActivationFailed, func(a *domain.ProbeActivation) { a.ErrorCode = "" }),
		"unknown state":       mutate(domain.ProbeActivationQueued, func(a *domain.ProbeActivation) { a.State = "enrolled" }),
		"bad owner":           mutate(domain.ProbeActivationQueued, func(a *domain.ProbeActivation) { a.OwnerID = "not-a-valid-owner-identity" }),
		"non-utc timestamps": mutate(domain.ProbeActivationQueued, func(a *domain.ProbeActivation) {
			zone := time.FixedZone("", 7*3600)
			a.CreatedAt, a.UpdatedAt, a.DeadlineAt = a.CreatedAt.In(zone), a.UpdatedAt.In(zone), a.DeadlineAt.In(zone)
		}),
		"step unknown code": mutate(domain.ProbeActivationInterrupted, func(a *domain.ProbeActivation) {
			a.Steps[1].ErrorCode = "mystery-code"
		}),
		"steps truncated": mutate(domain.ProbeActivationQueued, func(a *domain.ProbeActivation) { a.Steps = a.Steps[:2] }),
		"steps reordered": mutate(domain.ProbeActivationQueued, func(a *domain.ProbeActivation) {
			a.Steps[0], a.Steps[1] = a.Steps[1], a.Steps[0]
		}),
		"unknown step kind": mutate(domain.ProbeActivationQueued, func(a *domain.ProbeActivation) {
			a.Steps[1].Spec.Kind = "enroll"
		}),
		"active with unapplied step": mutate(domain.ProbeActivationActive, func(a *domain.ProbeActivation) {
			a.Steps[3].State = domain.ProbeInstallResourcePlanned
			a.Steps[3].AttemptID = ""
			a.Steps[3].StartedAt, a.Steps[3].FinishedAt = time.Time{}, time.Time{}
		}),
		"init applied without facts": mutate(domain.ProbeActivationRunning, func(a *domain.ProbeActivation) { a.Facts = domain.ProbeActivationFacts{} }),
		"facts before init applied": mutate(domain.ProbeActivationQueued, func(a *domain.ProbeActivation) {
			a.Facts = activationFacts()
		}),
		"facts with a malformed certificate pin": mutate(domain.ProbeActivationRunning, func(a *domain.ProbeActivation) {
			a.Facts.CertificateFingerprint = "not-a-hash"
		}),
		"facts with the local probe id": mutate(domain.ProbeActivationRunning, func(a *domain.ProbeActivation) {
			a.Facts.ProbeID = domain.LocalProbeID
		}),
		"port outside the unprivileged range": mutate(domain.ProbeActivationQueued, func(a *domain.ProbeActivation) {
			a.Identity.ListenPort = 80
		}),
		"non-root install host": mutate(domain.ProbeActivationQueued, func(a *domain.ProbeActivation) {
			a.Identity.Install.Host.User = "ubuntu"
		}),
		"docker install mode": mutate(domain.ProbeActivationQueued, func(a *domain.ProbeActivation) {
			a.Identity.Install.Mode = domain.ProbeInstallDocker
		}),
		"different operation": mutate(domain.ProbeActivationQueued, func(a *domain.ProbeActivation) {
			a.Identity.Install.OperationID = otherOperation
			for i, spec := range a.Identity.Steps() {
				a.Steps[i].Spec = spec
			}
		}),
	} {
		for _, route := range activationRoutes() {
			t.Run(name+" "+route.name, func(t *testing.T) {
				h := NewProbeActivationHandlers(&activationFake{act: activation}, true)
				rec := activationServe(t, route, h)
				if rec.Code != http.StatusServiceUnavailable || activationErrorBody(t, rec).Code != "activation_unavailable" {
					t.Fatalf("corrupt receipt = %d (%s)", rec.Code, rec.Body.String())
				}
				for _, leak := range []string{"mystery-code", "not-a-valid-owner-identity", otherOperation, "not-a-hash", "docker", "ubuntu", "enroll"} {
					if strings.Contains(rec.Body.String(), leak) {
						t.Fatalf("corrupt field %q leaked: %s", leak, rec.Body.String())
					}
				}
			})
		}
	}
}

// TestProbeActivationObserve covers the read lifecycle: 200 for every stored
// receipt, the exact operation id handed to the service, malformed and
// missing identities.
func TestProbeActivationObserve(t *testing.T) {
	for name, activation := range activationAllReceipts() {
		t.Run("observe "+name, func(t *testing.T) {
			fake := &activationFake{act: activation}
			h := NewProbeActivationHandlers(fake, true)
			rec := activationServe(t, activationObserveRoute, h)
			if rec.Code != http.StatusOK || fake.operationID != activationOpID || fake.observes != 1 || fake.calls() != 1 {
				t.Fatalf("observe = %d, op = %q, calls = %d (%s)", rec.Code, fake.operationID, fake.calls(), rec.Body.String())
			}
		})
	}
	for _, id := range []string{"not-a-uuid", strings.ToUpper(activationOpID), "local", "00000000-0000-0000-0000-000000000000", activationOpID + "x"} {
		t.Run("malformed id "+id, func(t *testing.T) {
			fake := &activationFake{act: activationLifecycle(domain.ProbeActivationQueued)}
			h := NewProbeActivationHandlers(fake, true)
			rec := activationServeAs(t, 1, http.MethodGet, activationBasePath+"/"+id+"/activation", id, "", h.Observe)
			if rec.Code != http.StatusBadRequest || activationErrorBody(t, rec).Code != "invalid_operation_id" {
				t.Fatalf("malformed id = %d (%s)", rec.Code, rec.Body.String())
			}
			if fake.calls() != 0 {
				t.Fatalf("service called %d times for a malformed identity", fake.calls())
			}
		})
	}
	t.Run("missing path parameter", func(t *testing.T) {
		fake := &activationFake{act: activationLifecycle(domain.ProbeActivationQueued)}
		h := NewProbeActivationHandlers(fake, true)
		rec := activationServeAs(t, 1, http.MethodGet, activationBasePath+"//activation", "", "", h.Observe)
		if rec.Code != http.StatusBadRequest || fake.calls() != 0 {
			t.Fatalf("missing id = %d, calls = %d", rec.Code, fake.calls())
		}
	})
	t.Run("receipt for another operation", func(t *testing.T) {
		other := "5e4d3c2b-1a09-4f8e-b7d6-c5b4a3928170"
		fake := &activationFake{act: activationLifecycle(domain.ProbeActivationRunning)}
		h := NewProbeActivationHandlers(fake, true)
		rec := activationServeAs(t, 1, http.MethodGet, activationBasePath+"/"+other+"/activation", other, "", h.Observe)
		if rec.Code != http.StatusServiceUnavailable || activationErrorBody(t, rec).Code != "activation_unavailable" {
			t.Fatalf("mismatched id = %d (%s)", rec.Code, rec.Body.String())
		}
	})
}

// TestProbeActivationCancel covers cancellation: 202 for recorded intent, 200
// for settled receipts (including active) returned unchanged, payload-free
// requests and the fail-closed contradiction paths.
func TestProbeActivationCancel(t *testing.T) {
	for _, tc := range []struct {
		name   string
		act    *domain.ProbeActivation
		status int
	}{
		{"intent recorded", activationLifecycle(domain.ProbeActivationCancelRequested), 202},
		{"queued cancels at once", activationReceipt(domain.ProbeActivationCanceled, "canceled"), 200},
		{"terminal canceled", activationLifecycle(domain.ProbeActivationCanceled), 200},
		{"terminal failed unchanged", activationLifecycle(domain.ProbeActivationFailed), 200},
		{"terminal interrupted unchanged", activationLifecycle(domain.ProbeActivationInterrupted), 200},
		{"active unchanged", activationLifecycle(domain.ProbeActivationActive), 200},
		{"queued receipt contradicts cancel", activationLifecycle(domain.ProbeActivationQueued), 503},
		{"running receipt contradicts cancel", activationLifecycle(domain.ProbeActivationRunning), 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &activationFake{act: tc.act}
			h := NewProbeActivationHandlers(fake, true)
			rec := activationServe(t, activationCancelRoute, h)
			if rec.Code != tc.status || fake.operationID != activationOpID || fake.cancels != 1 || fake.calls() != 1 {
				t.Fatalf("cancel = %d, op = %q, calls = %d (%s)", rec.Code, fake.operationID, fake.calls(), rec.Body.String())
			}
			if tc.status == 503 {
				if activationErrorBody(t, rec).Code != "activation_unavailable" {
					t.Fatalf("contradiction code = %s", rec.Body.String())
				}
				return
			}
			if got := activationStatusOf(t, rec); got != tc.act.State {
				t.Fatalf("cancel receipt status = %q, want unchanged %q", got, tc.act.State)
			}
		})
	}
	for _, tc := range []struct{ name, body string }{
		{"object payload", `{"reason":"x"}`},
		{"empty object payload", `{}`},
		{"null payload", `null`},
		{"text payload", `cancel`},
		{"payload after whitespace", "  \n\t" + `{}`},
		{"key payload", activationBody(activationKeyCanary)},
		{"payload past the limit", strings.Repeat(" ", probeActivationCancelMaxBodyBytes) + "x"},
		{"whitespace past the limit", strings.Repeat(" ", probeActivationCancelMaxBodyBytes+1)},
	} {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			fake := &activationFake{act: activationLifecycle(domain.ProbeActivationCancelRequested)}
			h := NewProbeActivationHandlers(fake, true)
			rec := activationServeBody(t, activationCancelRoute, h, tc.body)
			if rec.Code != http.StatusBadRequest || activationErrorBody(t, rec).Code != "invalid_request" {
				t.Fatalf("cancel payload = %d (%s)", rec.Code, rec.Body.String())
			}
			if fake.calls() != 0 {
				t.Fatalf("service called %d times for a payload-carrying cancel", fake.calls())
			}
		})
	}
	t.Run("accepts whitespace only", func(t *testing.T) {
		fake := &activationFake{act: activationLifecycle(domain.ProbeActivationCancelRequested)}
		h := NewProbeActivationHandlers(fake, true)
		rec := activationServeBody(t, activationCancelRoute, h, " \n\t")
		if rec.Code != http.StatusAccepted || fake.cancels != 1 {
			t.Fatalf("whitespace cancel = %d, calls = %d", rec.Code, fake.cancels)
		}
	})
	for _, id := range []string{"not-a-uuid", strings.ToUpper(activationOpID), "local"} {
		t.Run("malformed id "+id, func(t *testing.T) {
			fake := &activationFake{act: activationLifecycle(domain.ProbeActivationCancelRequested)}
			h := NewProbeActivationHandlers(fake, true)
			rec := activationServeAs(t, 1, http.MethodPost, activationBasePath+"/"+id+"/activation/cancel", id, "", h.Cancel)
			if rec.Code != http.StatusBadRequest || activationErrorBody(t, rec).Code != "invalid_operation_id" || fake.calls() != 0 {
				t.Fatalf("malformed id = %d, calls = %d (%s)", rec.Code, fake.calls(), rec.Body.String())
			}
		})
	}
}

// TestProbeActivationHTTPErrors verifies the error mapping on every route,
// including the bounded Retry-After on busy and the ban on echoing underlying
// diagnostics, key material or enrollment tokens.
func TestProbeActivationHTTPErrors(t *testing.T) {
	leaky := errors.New("storage backend failure " + activationLeakMarker + " " + activationKeyCanary + " " + activationTokenCanary + " /var/lib/secret")
	for _, tc := range []struct {
		name       string
		fakeErr    error
		status     int
		code       string
		retryAfter string
	}{
		{"validation", domain.ErrValidation, 400, "invalid_activation", ""},
		{"wrapped validation", fmt.Errorf("creating: %w", domain.ErrValidation), 400, "invalid_activation", ""},
		{"ssh input", domain.ErrProbeSSHInput, 400, "invalid_activation", ""},
		{"port conflict", ports.ErrConflict, 409, "activation_conflict", ""},
		{"wrapped port conflict", fmt.Errorf("install %s: %w", activationLeakMarker, ports.ErrConflict), 409, "activation_conflict", ""},
		{"domain conflict", fmt.Errorf("journal: %w", domain.ErrConflict), 409, "activation_conflict", ""},
		{"port not found", ports.ErrNotFound, 404, "activation_not_found", ""},
		{"domain not found", domain.ErrNotFound, 404, "activation_not_found", ""},
		{"busy", domain.ErrProbeInstallBusy, 503, "activations_busy", "5"},
		{"wrapped busy", fmt.Errorf("admission: %w", domain.ErrProbeInstallBusy), 503, "activations_busy", "5"},
		{"closed", domain.ErrProbeInstallClosed, 503, "activations_closed", ""},
		{"internal", domain.ErrInternal, 503, "activation_unavailable", ""},
		{"internal joined with validation", errors.Join(domain.ErrValidation, domain.ErrInternal), 503, "activation_unavailable", ""},
		{"context canceled", context.Canceled, 503, "activation_unavailable", ""},
		{"deadline exceeded", context.DeadlineExceeded, 503, "activation_unavailable", ""},
		{"underlying diagnostic", leaky, 503, "activation_unavailable", ""},
	} {
		for _, route := range activationRoutes() {
			t.Run(tc.name+" "+route.name, func(t *testing.T) {
				fake := &activationFake{err: tc.fakeErr}
				h := NewProbeActivationHandlers(fake, true)
				rec := activationServe(t, route, h)
				if rec.Code != tc.status {
					t.Fatalf("status = %d, want %d (%s)", rec.Code, tc.status, rec.Body.String())
				}
				if view := activationErrorBody(t, rec); view.Code != tc.code || view.Error == "" {
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

// TestProbeActivationSurfaceGates covers unauthenticated access, the disabled
// feature flag and the missing (including typed-nil) service on every route.
func TestProbeActivationSurfaceGates(t *testing.T) {
	var typedNil *activationFake
	for _, r := range activationRoutes() {
		for _, actor := range []int64{0, -1} {
			t.Run(fmt.Sprintf("unauthenticated %s actor %d", r.name, actor), func(t *testing.T) {
				fake := &activationFake{act: activationLifecycle(domain.ProbeActivationQueued)}
				h := NewProbeActivationHandlers(fake, true)
				// Actor 0 means no value in the context at all.
				rec := activationServeAs(t, actor, r.method, r.path, r.id, r.body, r.call(h))
				if rec.Code != http.StatusUnauthorized || activationErrorBody(t, rec).Code != "unauthenticated" {
					t.Fatalf("unauthenticated = %d (%s)", rec.Code, rec.Body.String())
				}
				if fake.calls() != 0 {
					t.Fatalf("service called %d times without an actor", fake.calls())
				}
			})
		}
		for _, tc := range []struct {
			name    string
			svc     probeActivationRunner
			enabled bool
			want    string
		}{
			{"disabled", &activationFake{act: activationLifecycle(domain.ProbeActivationQueued)}, false, "activations_disabled"},
			{"disabled without service", nil, false, "activations_disabled"},
			{"no service", nil, true, "activation_unavailable"},
			{"typed nil service", typedNil, true, "activation_unavailable"},
		} {
			t.Run(tc.name+" "+r.name, func(t *testing.T) {
				h := NewProbeActivationHandlers(tc.svc, tc.enabled)
				rec := activationServe(t, r, h)
				if rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("status = %d, want 503 (%s)", rec.Code, rec.Body.String())
				}
				if code := activationErrorBody(t, rec).Code; code != tc.want {
					t.Fatalf("code = %q, want %q", code, tc.want)
				}
				if fake, ok := tc.svc.(*activationFake); ok && fake != nil && fake.calls() != 0 {
					t.Fatalf("disabled service called %d times", fake.calls())
				}
			})
		}
	}
}

// TestProbeActivationDoesNotReadBodyWhenGated proves a gated request is
// refused before its body (which may hold a private key) is interpreted at all.
func TestProbeActivationDoesNotReadBodyWhenGated(t *testing.T) {
	fake := &activationFake{act: activationLifecycle(domain.ProbeActivationQueued)}
	h := NewProbeActivationHandlers(fake, false)
	rec := activationServeBody(t, activationSubmitRoute, h, strings.Repeat("a", 128<<10+1))
	if rec.Code != http.StatusServiceUnavailable || activationErrorBody(t, rec).Code != "activations_disabled" {
		t.Fatalf("gated submit = %d (%s)", rec.Code, rec.Body.String())
	}
}

// TestProbeActivationUTCTimestamps proves timestamps are rendered as UTC with
// the fixed 15 minute lifetime visible as deadline_at.
func TestProbeActivationUTCTimestamps(t *testing.T) {
	fake := &activationFake{act: activationLifecycle(domain.ProbeActivationRunning)}
	h := NewProbeActivationHandlers(fake, true)
	rec := activationServe(t, activationObserveRoute, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"created_at":"2026-10-24T09:30:00Z"`, `"updated_at":"2026-10-24T09:30:11Z"`, `"deadline_at":"2026-10-24T09:45:00Z"`} {
		if !bytes.Contains(rec.Body.Bytes(), []byte(want)) {
			t.Fatalf("missing %s in %s", want, rec.Body.String())
		}
	}
	t.Run("zoned receipt fails closed instead of converting", func(t *testing.T) {
		zoned := activationLifecycle(domain.ProbeActivationQueued)
		zone := time.FixedZone("", 7*3600)
		zoned.CreatedAt, zoned.UpdatedAt, zoned.DeadlineAt = zoned.CreatedAt.In(zone), zoned.UpdatedAt.In(zone), zoned.DeadlineAt.In(zone)
		rec := activationServe(t, activationObserveRoute, NewProbeActivationHandlers(&activationFake{act: zoned}, true))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
		}
	})
}

// TestProbeActivationResponseIsNotADomainType guards rule 5: the wire view must
// carry snake_case json tags only, never exported Go field names, and the
// secret-bearing receipt members must be absent by name as well as by value.
func TestProbeActivationResponseIsNotADomainType(t *testing.T) {
	for name, activation := range map[string]*domain.ProbeActivation{
		"running": activationLifecycle(domain.ProbeActivationRunning),
		"active":  activationLifecycle(domain.ProbeActivationActive),
		"failed":  activationLifecycle(domain.ProbeActivationFailed),
	} {
		t.Run(name, func(t *testing.T) {
			h := NewProbeActivationHandlers(&activationFake{act: activation}, true)
			rec := activationServe(t, activationObserveRoute, h)
			for _, goName := range []string{
				"OperationID", "Identity", "Install", "OwnerID", "Steps", "Facts", "ProbeID", "StreamID", "CertificateFingerprint",
				"ClientKeyFingerprint", "HostKeyFingerprint", "Spec", "AttemptID", "StartedAt", "FinishedAt", "ListenPort", "ProbeKey",
				"ErrorCode", "RequestedBy", "Host", "Reference", "Owned", "Ordinal",
			} {
				if strings.Contains(rec.Body.String(), `"`+goName+`"`) {
					t.Fatalf("response carries Go field name %q: %s", goName, rec.Body.String())
				}
			}
			if !strings.Contains(rec.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("Content-Type = %q", rec.Header().Get("Content-Type"))
			}
		})
	}
}

// TestProbeActivationReceiptSecretsNeverRender plants every secret the stored
// receipt carries and proves none reaches any route's response.
func TestProbeActivationReceiptSecretsNeverRender(t *testing.T) {
	for name, activation := range activationAllReceipts() {
		for _, route := range activationRoutes() {
			if route.name == "cancel" && !activation.Terminal() && activation.State != domain.ProbeActivationCancelRequested {
				// Cancel of an in-progress receipt is a fail-closed contradiction,
				// covered by TestProbeActivationCancel.
				continue
			}
			t.Run(name+" "+route.name, func(t *testing.T) {
				h := NewProbeActivationHandlers(&activationFake{act: activation}, true)
				// activationServe fails the test if any secret appears in the body.
				rec := activationServe(t, route, h)
				if rec.Code >= 500 {
					t.Fatalf("status = %d (%s)", rec.Code, rec.Body.String())
				}
			})
		}
	}
}

// TestProbeActivationRequestValidMatchesDomain keeps the handler's pre-check in
// lockstep with the domain: for every request-controlled field combination the
// handler accepts exactly what a full domain.ProbeActivationIdentity accepts.
func TestProbeActivationRequestValidMatchesDomain(t *testing.T) {
	// The placeholder's install half must stay domain.Valid, or every
	// submission would be refused; only the request-controlled fields are left
	// empty in the template itself.
	placeholder := probeActivationTemplate()
	placeholder.ProbeKey, placeholder.Name, placeholder.ListenPort = "edge-1", "Edge One", domain.ProbeActivationDefaultPort
	if !placeholder.Valid() || !placeholder.Install.Valid() {
		t.Fatal("placeholder identity is no longer domain.Valid; every submission would be refused")
	}
	if !probeActivationRequestValid(probeActivationSubmitRequest{ProbeKey: "edge-1", Name: "Edge One", Location: "", ListenPort: 0}, 1) {
		t.Fatal("baseline request refused")
	}
	long := func(r string, n int) string { return strings.Repeat(r, n) }
	keys := []string{"edge-1", "a", "0", long("a", 63), long("a", 64), "", "Edge", "-a", "a-", "local", "a_b", "a b", "é", "a\n"}
	names := []string{"Edge One", "x", long("é", 200), long("a", 201), "", " x", "x ", "a\tb", "a\x7fb", " ", "a b"}
	locations := []string{"", "Bangkok", long("é", 255), long("a", 256), " x", "x ", "a\nb"}
	ports := []int64{0, 1, 1023, 1024, 8443, 65535, 65536, 100000, -1}
	for _, key := range keys {
		for _, name := range names {
			for _, location := range locations {
				for _, port := range ports {
					for _, actor := range []int64{7, 0} {
						request := probeActivationSubmitRequest{PrivateKey: "k", ProbeKey: key, Name: name, Location: location, ListenPort: port}
						identity := activationIdentity()
						identity.ProbeKey, identity.Name, identity.Location, identity.RequestedBy = key, name, location, actor
						identity.ListenPort = int(port)
						if port == 0 {
							identity.ListenPort = domain.ProbeActivationDefaultPort
						}
						want := identity.Valid() && port >= 0
						if got := probeActivationRequestValid(request, actor); got != want {
							t.Fatalf("probeActivationRequestValid(key=%q name=%q location=%q port=%d actor=%d) = %v, domain says %v", key, name, location, port, actor, got, want)
						}
					}
				}
			}
		}
	}
}
