package handlers

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestConfigFleetGateErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{services.ErrFleetNotAssignmentAware, 409, "worker_fleet_unaware"},
		{services.ErrFleetReadinessUnavailable, 503, "worker_readiness_unavailable"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			response := httptest.NewRecorder()
			ctx := echo.New().NewContext(httptest.NewRequest("POST", "/api/config/apply", nil), response)
			if err := mapConfigError(ctx, fmt.Errorf("%w: private-worker-name", tc.err)); err != nil {
				t.Fatal(err)
			}
			if response.Code != tc.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body)
			}
			var body map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["code"] != tc.code {
				t.Fatalf("body=%v", body)
			}
		})
	}
}
