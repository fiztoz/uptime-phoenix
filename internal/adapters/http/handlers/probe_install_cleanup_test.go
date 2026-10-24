package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const cleanupHTTPID = "c1111111-1111-4111-8111-111111111111"

type cleanupFake struct {
	mu          sync.Mutex
	receipt     *domain.ProbeCleanup
	err         error
	submissions []services.ProbeCleanupSubmission
	latest      []string
}

func (f *cleanupFake) Submit(_ context.Context, s services.ProbeCleanupSubmission) (*domain.ProbeCleanup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s.PrivateKey = append([]byte(nil), s.PrivateKey...)
	f.submissions = append(f.submissions, s)
	return f.receipt, f.err
}

func (f *cleanupFake) Latest(_ context.Context, id string) (*domain.ProbeCleanup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.latest = append(f.latest, id)
	return f.receipt, f.err
}

func (f *cleanupFake) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.submissions) + len(f.latest)
}

func cleanupHTTPReceipt(t *testing.T, state, code string) *domain.ProbeCleanup {
	t.Helper()
	identity := probeActivationTemplate().Install
	identity.OperationID = installOpID
	created := time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)
	r := &domain.ProbeCleanup{
		Identity: domain.ProbeCleanupIdentity{CleanupID: cleanupHTTPID, Install: identity, ClientKeyFingerprint: identity.ClientKeyFingerprint, RequestedBy: 1},
		OwnerID:  "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", State: state, ErrorCode: code,
		CreatedAt: created, UpdatedAt: created.Add(time.Second), DeadlineAt: created.Add(domain.ProbeCleanupLifetime),
	}
	if state != domain.ProbeCleanupQueued && code != "owner_interrupted" {
		r.AttemptID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	}
	if code == "deadline_expired" {
		r.UpdatedAt = r.DeadlineAt
	}
	if !r.Valid() {
		t.Fatalf("fixture %s/%s invalid", state, code)
	}
	return r
}

func cleanupHTTPBody(members map[string]any) string {
	raw, _ := json.Marshal(members)
	return string(raw)
}

func cleanupHTTPMembers() map[string]any {
	return map[string]any{"cleanup_id": cleanupHTTPID, "private_key": installKeyCanary}
}

func TestProbeCleanupHTTPSubmitAndLatest(t *testing.T) {
	path := installPath + "/" + installOpID + "/cleanup"
	for _, tc := range []struct {
		state, code string
		status      int
	}{
		{domain.ProbeCleanupQueued, "", http.StatusAccepted},
		{domain.ProbeCleanupRunning, "", http.StatusAccepted},
		{domain.ProbeCleanupCleaned, "", http.StatusOK},
		{domain.ProbeCleanupFailed, "not_started", http.StatusOK},
		{domain.ProbeCleanupInterrupted, "owner_interrupted", http.StatusOK},
		{domain.ProbeCleanupInterrupted, "deadline_expired", http.StatusOK},
		{domain.ProbeCleanupInterrupted, "outcome_unknown", http.StatusOK},
	} {
		t.Run(tc.state+"/"+tc.code, func(t *testing.T) {
			fake := &cleanupFake{receipt: cleanupHTTPReceipt(t, tc.state, tc.code)}
			h := NewProbeCleanupHandlers(fake, true)
			rec := installServeAs(t, 7, http.MethodPost, path, installOpID, cleanupHTTPBody(cleanupHTTPMembers()), h.Submit)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			var view map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(view))
			for k := range view {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			if strings.Join(keys, ",") != "cleanup_id,created_at,deadline_at,error,kind,operation_id,status,updated_at" {
				t.Fatalf("view keys %v", keys)
			}
			if string(view["status"]) != `"`+tc.state+`"` || string(view["kind"]) != `"probe_install_cleanup"` {
				t.Fatalf("view %s", rec.Body.String())
			}
			if (tc.code == "") != (string(view["error"]) == "null") || (tc.code != "" && !strings.Contains(string(view["error"]), tc.code)) {
				t.Fatalf("error member %s for code %q", view["error"], tc.code)
			}
			sub := fake.submissions[0]
			if sub.CleanupID != cleanupHTTPID || sub.OperationID != installOpID || sub.RequestedBy != 7 || string(sub.PrivateKey) != installKeyCanary {
				t.Fatalf("submission %+v", sub)
			}
			got := installServe(t, http.MethodGet, path, installOpID, "", h.Latest)
			if got.Code != http.StatusOK || len(fake.latest) != 1 || fake.latest[0] != installOpID {
				t.Fatalf("latest %d %v", got.Code, fake.latest)
			}
		})
	}
}

func TestProbeCleanupHTTPStrictDecoding(t *testing.T) {
	path := installPath + "/" + installOpID + "/cleanup"
	with := func(edit func(m map[string]any)) string {
		m := cleanupHTTPMembers()
		edit(m)
		return cleanupHTTPBody(m)
	}
	for name, body := range map[string]string{
		"empty":            "",
		"array":            "[]",
		"missing key":      with(func(m map[string]any) { delete(m, "private_key") }),
		"missing id":       with(func(m map[string]any) { delete(m, "cleanup_id") }),
		"unknown member":   with(func(m map[string]any) { m["operation_id"] = installOpID }),
		"case variant":     with(func(m map[string]any) { delete(m, "cleanup_id"); m["Cleanup_ID"] = cleanupHTTPID }),
		"null key":         with(func(m map[string]any) { m["private_key"] = nil }),
		"numeric id":       with(func(m map[string]any) { m["cleanup_id"] = 7 }),
		"bad id":           with(func(m map[string]any) { m["cleanup_id"] = "not-a-uuid" }),
		"empty key":        with(func(m map[string]any) { m["private_key"] = "" }),
		"duplicate member": `{"cleanup_id":"` + cleanupHTTPID + `","cleanup_id":"` + cleanupHTTPID + `","private_key":"k"}`,
		"trailing json":    cleanupHTTPBody(cleanupHTTPMembers()) + "{}",
	} {
		fake := &cleanupFake{}
		rec := installServe(t, http.MethodPost, path, installOpID, body, NewProbeCleanupHandlers(fake, true).Submit)
		if rec.Code != http.StatusBadRequest || installErrorBody(t, rec).Code != "invalid_cleanup" || fake.calls() != 0 {
			t.Fatalf("%s: %d %s calls=%d", name, rec.Code, rec.Body.String(), fake.calls())
		}
	}
	fake := &cleanupFake{}
	rec := installServe(t, http.MethodPost, path, "bad", cleanupHTTPBody(cleanupHTTPMembers()), NewProbeCleanupHandlers(fake, true).Submit)
	if rec.Code != http.StatusBadRequest || fake.calls() != 0 {
		t.Fatalf("bad operation id: %d", rec.Code)
	}
	huge := cleanupHTTPBody(map[string]any{"cleanup_id": cleanupHTTPID, "private_key": strings.Repeat("k", probeCleanupMaxBodyBytes)})
	if rec := installServe(t, http.MethodPost, path, installOpID, huge, NewProbeCleanupHandlers(fake, true).Submit); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized: %d", rec.Code)
	}
}

func TestProbeCleanupHTTPErrors(t *testing.T) {
	path := installPath + "/" + installOpID + "/cleanup"
	leaky := errors.New("storage failure " + installLeakMarker)
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"validation", domain.ErrValidation, 400, "invalid_cleanup"},
		{"ssh input", domain.ErrProbeSSHInput, 400, "invalid_cleanup"},
		{"ineligible", fmt.Errorf("store %s: %w", installLeakMarker, domain.ErrProbeCleanupIneligible), 409, "cleanup_ineligible"},
		{"conflict", ports.ErrConflict, 409, "cleanup_conflict"},
		{"not found", ports.ErrNotFound, 404, "cleanup_not_found"},
		{"busy", domain.ErrProbeInstallBusy, 503, "cleanups_busy"},
		{"closed", domain.ErrProbeInstallClosed, 503, "cleanups_closed"},
		{"internal", domain.ErrInternal, 503, "cleanup_unavailable"},
		{"leaky", leaky, 503, "cleanup_unavailable"},
	} {
		h := NewProbeCleanupHandlers(&cleanupFake{err: tc.err}, true)
		for name, call := range map[string]func() int{
			"submit": func() int {
				rec := installServe(t, http.MethodPost, path, installOpID, cleanupHTTPBody(cleanupHTTPMembers()), h.Submit)
				if installErrorBody(t, rec).Code != tc.code {
					t.Fatalf("%s submit: %s", tc.name, rec.Body.String())
				}
				if strings.Contains(rec.Body.String(), installLeakMarker) {
					t.Fatalf("%s leaked", tc.name)
				}
				return rec.Code
			},
			"latest": func() int {
				rec := installServe(t, http.MethodGet, path, installOpID, "", h.Latest)
				if installErrorBody(t, rec).Code != tc.code {
					t.Fatalf("%s latest: %s", tc.name, rec.Body.String())
				}
				return rec.Code
			},
		} {
			if got := call(); got != tc.status {
				t.Fatalf("%s %s: %d, want %d", tc.name, name, got, tc.status)
			}
		}
	}
	// A receipt for another install, or a corrupt one, fails closed.
	other := cleanupHTTPReceipt(t, domain.ProbeCleanupCleaned, "")
	other.Identity.Install.OperationID = "a2222222-2222-4222-8222-222222222222"
	corrupt := cleanupHTTPReceipt(t, domain.ProbeCleanupCleaned, "")
	corrupt.ErrorCode = "outcome_unknown"
	for name, receipt := range map[string]*domain.ProbeCleanup{"other install": other, "corrupt": corrupt, "nil": nil} {
		rec := installServe(t, http.MethodGet, path, installOpID, "", NewProbeCleanupHandlers(&cleanupFake{receipt: receipt}, true).Latest)
		if rec.Code != http.StatusServiceUnavailable || installErrorBody(t, rec).Code != "cleanup_unavailable" {
			t.Fatalf("%s: %d %s", name, rec.Code, rec.Body.String())
		}
	}
}

func TestProbeCleanupHTTPSurfaceGates(t *testing.T) {
	path := installPath + "/" + installOpID + "/cleanup"
	var typedNil *cleanupFake
	for name, tc := range map[string]struct {
		h     *ProbeCleanupHandlers
		actor int64
		code  string
	}{
		"unauthenticated": {NewProbeCleanupHandlers(&cleanupFake{}, true), 0, "unauthenticated"},
		"disabled":        {NewProbeCleanupHandlers(&cleanupFake{}, false), 1, "cleanups_disabled"},
		"nil service":     {NewProbeCleanupHandlers(nil, true), 1, "cleanup_unavailable"},
		"typed nil":       {NewProbeCleanupHandlers(typedNil, true), 1, "cleanup_unavailable"},
	} {
		rec := installServeAs(t, tc.actor, http.MethodPost, path, installOpID, cleanupHTTPBody(cleanupHTTPMembers()), tc.h.Submit)
		if installErrorBody(t, rec).Code != tc.code {
			t.Fatalf("%s submit: %s", name, rec.Body.String())
		}
		rec = installServeAs(t, tc.actor, http.MethodGet, path, installOpID, "", tc.h.Latest)
		if installErrorBody(t, rec).Code != tc.code {
			t.Fatalf("%s latest: %s", name, rec.Body.String())
		}
	}
}
