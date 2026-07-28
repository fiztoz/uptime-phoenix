package handlers

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// PushHandler handles inbound push heartbeats for "push" type monitors.
// Clients POST (or GET) to /api/push/<push_token> to report status.
type PushHandler struct {
	monitors *services.MonitorService
	hb       *services.HeartbeatService
}

// NewPushHandler creates the push ingest handler.
func NewPushHandler(monitors *services.MonitorService, hb *services.HeartbeatService) *PushHandler {
	return &PushHandler{monitors: monitors, hb: hb}
}

// Receive is the public push ingest endpoint.
// Supports:
//
//	POST /api/push/:token   with optional JSON { "status": "up|down", "msg": "...", "ping": 12 }
//	GET  /api/push/:token?status=up&msg=...&ping=12
//
// Optional signature: X-Signature: sha256=<hex-hmac> using the monitor's push_token (or config["hmac_secret"]) as key.
func (h *PushHandler) Receive(c echo.Context) error {
	token := strings.TrimSpace(c.Param("token"))
	if token == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "push token required"})
	}

	ctx := c.Request().Context()

	mon, err := h.monitors.GetByPushToken(ctx, token)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"error": "monitor not found"})
	}
	if !mon.Active {
		return c.JSON(http.StatusBadRequest, map[string]string{"error": "monitor is disabled"})
	}

	// Parse payload (JSON preferred, then query params)
	status := domain.StatusUp
	msg := "push received"
	var latency int64

	var body struct {
		Status string `json:"status"`
		Msg    string `json:"msg"`
		Ping   int64  `json:"ping"`
	}
	_ = c.Bind(&body) // best effort

	if body.Status != "" {
		if strings.EqualFold(body.Status, "down") {
			status = domain.StatusDown
		}
	}
	if body.Msg != "" {
		msg = body.Msg
	}
	if body.Ping > 0 {
		latency = body.Ping
	}

	// Query fallback (simple curl support)
	if qp := c.QueryParam("status"); qp != "" && body.Status == "" {
		if strings.EqualFold(qp, "down") {
			status = domain.StatusDown
		}
	}
	if qp := c.QueryParam("msg"); qp != "" && body.Msg == "" {
		msg = qp
	}
	if qp := c.QueryParam("ping"); qp != "" && latency == 0 {
		if v, _ := strconv.ParseInt(qp, 10, 64); v > 0 {
			latency = v
		}
	}

	// Optional HMAC verification (body or canonical string)
	sig := c.Request().Header.Get("X-Signature")
	if sig == "" {
		sig = c.Request().Header.Get("X-Hub-Signature-256")
	}
	secret := token
	if s, ok := mon.Config["hmac_secret"].(string); ok && s != "" {
		secret = s
	}
	if secret != "" && sig != "" {
		// For GET we sign a canonical string (token|status|msg). For POST the body can also be used.
		toSign := fmt.Sprintf("%s|%s|%s", token, status, msg)
		if !verifyPushHMAC([]byte(toSign), sig, secret) {
			return c.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		}
	}

	result := ports.CheckResult{
		Status:    status,
		Message:   msg,
		LatencyMs: latency,
	}

	if err := h.hb.Record(ctx, mon, result); err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"error": "failed to record heartbeat"})
	}

	return c.NoContent(http.StatusOK)
}

func verifyPushHMAC(data []byte, provided, secret string) bool {
	provided = strings.TrimPrefix(provided, "sha256=")
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(data)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(provided))
}
