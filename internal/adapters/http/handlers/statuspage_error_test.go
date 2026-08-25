package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// Both unique columns behind mapSPError are user-chosen — status_pages.slug and
// status_page_cnames.domain — so a collision is a client error, not a server
// fault.
func TestMapSPError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantBody string
	}{
		{
			name:     "duplicate slug or domain is a conflict, not a server fault",
			err:      fmt.Errorf("create status page: %w", ports.ErrConflict),
			wantCode: http.StatusConflict,
			wantBody: "slug or custom domain already in use",
		},
		{
			// A duplicate monitor-add used to fall into the ErrConflict arm above
			// and tell the user "slug or custom domain already in use" — copy
			// written for a completely different conflict. It gets its own
			// sentinel and its own message.
			name:     "duplicate monitor link names the monitor, not the slug",
			err:      fmt.Errorf("add monitor: %w", ports.ErrMonitorAlreadyLinked),
			wantCode: http.StatusConflict,
			wantBody: "monitor is already linked to this status page",
		},
		{
			name:     "not found",
			err:      fmt.Errorf("wrapped: %w", ports.ErrNotFound),
			wantCode: http.StatusNotFound,
			wantBody: "not found",
		},
		{
			name:     "active incident must be resolved before deletion",
			err:      fmt.Errorf("wrapped: %w", domain.ErrIncidentActive),
			wantCode: http.StatusConflict,
			wantBody: "resolve the incident before deleting it",
		},
		{
			name:     "validation",
			err:      fmt.Errorf("%w: slug is required", domain.ErrValidation),
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "invalid public access code is forbidden without triggering login auth",
			err:      fmt.Errorf("verify status page access: %w", domain.ErrUnauthorized),
			wantCode: http.StatusForbidden,
			wantBody: "access denied",
		},
		{
			name:     "unknown errors stay 500 and do not leak the cause",
			err:      fmt.Errorf("some driver explosion"),
			wantCode: http.StatusInternalServerError,
			wantBody: "internal error",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := echo.New()
			rec := httptest.NewRecorder()
			c := e.NewContext(httptest.NewRequest(http.MethodPost, "/api/status-pages", nil), rec)

			if err := mapSPError(c, tt.err); err != nil {
				t.Fatalf("mapSPError returned an error: %v", err)
			}
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d; want %d (body: %s)", rec.Code, tt.wantCode, rec.Body.String())
			}
			if tt.wantBody != "" {
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("unmarshal body: %v", err)
				}
				if body["error"] != tt.wantBody {
					t.Errorf("error = %q; want %q", body["error"], tt.wantBody)
				}
			}
		})
	}
}
