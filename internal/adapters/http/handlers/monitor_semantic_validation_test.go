package handlers_test

import (
	"encoding/json"
	"net/http"
	"testing"
)

// TestMonitorSemanticValidation pins the monitor part of #118: the API
// accepted a timeout not below the interval and accepted-status-code patterns
// the checker can never match ("abc", "600", "300-200"), returning 201.
func TestMonitorSemanticValidation(t *testing.T) {
	base := func(extra map[string]any) map[string]any {
		b := map[string]any{
			"name": "m", "type": "http", "interval": 60, "timeout": 30,
			"config": map[string]any{"url": "https://example.com"},
		}
		for k, v := range extra {
			b[k] = v
		}
		return b
	}
	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"timeout above interval", base(map[string]any{"timeout": 90})},
		{"timeout equal to interval", base(map[string]any{"timeout": 60})},
		{"non-numeric status code", base(map[string]any{"accepted_statuscodes": []string{"abc"}})},
		{"out-of-range status code", base(map[string]any{"accepted_statuscodes": []string{"600"}})},
		{"reversed status range", base(map[string]any{"accepted_statuscodes": []string{"300-200"}})},
		{"half-open status range", base(map[string]any{"accepted_statuscodes": []string{"200-"}})},
		{"padded exact status", base(map[string]any{"accepted_statuscodes": []string{" 200"}})},
		{"padded status range", base(map[string]any{"accepted_statuscodes": []string{"200 - 299"}})},
		{"dns bogus record type", map[string]any{"name": "d", "type": "dns", "interval": 60, "timeout": 30,
			"config": map[string]any{"hostname": "example.com", "resolve_type": "BOGUS"}}},
		{"dns non-string record type", map[string]any{"name": "d", "type": "dns", "interval": 60, "timeout": 30,
			"config": map[string]any{"hostname": "example.com", "resolve_type": 5}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newMonitorHarness(t)
			rec := h.do(t, http.MethodPost, "/api/monitors", tc.body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST = %d; want 400 (body %s)", rec.Code, rec.Body)
			}
		})
	}

	t.Run("short interval with omitted timeout gets a timeout below it", func(t *testing.T) {
		h := newMonitorHarness(t)
		rec := h.do(t, http.MethodPost, "/api/monitors", base(map[string]any{"interval": 20, "timeout": 0}))
		if rec.Code != http.StatusCreated {
			t.Fatalf("POST = %d; want 201 (body %s)", rec.Code, rec.Body)
		}
		var got map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &got)
		if to, _ := got["timeout"].(float64); to <= 0 || to >= 20 {
			t.Fatalf("timeout = %v; want 0 < timeout < 20", got["timeout"])
		}
	})

	t.Run("update cannot shrink the interval under the stored timeout", func(t *testing.T) {
		h := newMonitorHarness(t)
		rec := h.do(t, http.MethodPost, "/api/monitors", base(nil))
		if rec.Code != http.StatusCreated {
			t.Fatalf("create = %d", rec.Code)
		}
		var created map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &created)
		id := floatToIntStr(created["id"].(float64))
		if rec := h.do(t, http.MethodPut, "/api/monitors/"+id, map[string]any{"interval": 20}); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT interval 20 over timeout 30 = %d; want 400", rec.Code)
		}
		for _, rule := range []string{"abc", " 200", "200 - 299"} {
			if rec := h.do(t, http.MethodPut, "/api/monitors/"+id, map[string]any{"accepted_statuscodes": []string{rule}}); rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT status code %q = %d; want 400", rule, rec.Code)
			}
		}
		if rec := h.do(t, http.MethodPut, "/api/monitors/"+id, map[string]any{"active": false}); rec.Code != http.StatusOK {
			t.Fatalf("PUT pause = %d; want 200", rec.Code)
		}
	})
}
