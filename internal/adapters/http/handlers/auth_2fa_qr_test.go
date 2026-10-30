package handlers_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"net/http"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
)

// TestSetup2FAReturnsQRImageTheCSPAllows pins #121: the settings page
// rendered the otpauth:// provisioning URL as <img src>, which is not an
// image and is blocked by the CSP img-src directive ('self' data: blob:).
// setup-2fa must hand the UI a data:image/png QR code it can render.
func TestSetup2FAReturnsQRImageTheCSPAllows(t *testing.T) {
	h := newHarness(t)
	register := h.do(t, http.MethodPost, "/api/auth/register", map[string]string{
		"setup_token": testSetupToken,
		"username":    "alice",
		"password":    "supersecret",
	}, "")
	if register.Code != http.StatusCreated {
		t.Fatalf("register status = %d; body=%s", register.Code, register.Body.String())
	}
	var reg handlers.LoginResponse
	if err := json.Unmarshal(register.Body.Bytes(), &reg); err != nil {
		t.Fatalf("decode register: %v", err)
	}

	setup := h.do(t, http.MethodPost, "/api/auth/setup-2fa", nil, reg.Token)
	if setup.Code != http.StatusOK {
		t.Fatalf("setup-2fa status = %d; body=%s", setup.Code, setup.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(setup.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode setup-2fa: %v", err)
	}
	img, _ := body["qr_image"].(string)
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(img, prefix) {
		t.Fatalf("qr_image = %.40q; want a %q URL the CSP img-src allows", img, prefix)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(img, prefix))
	if err != nil {
		t.Fatalf("qr_image is not valid base64: %v", err)
	}
	decoded, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("qr_image is not a PNG: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() < 128 || b.Dx() != b.Dy() {
		t.Fatalf("qr_image bounds = %v; want a square of at least 128px", b)
	}
	if url, _ := body["qr_url"].(string); !strings.HasPrefix(url, "otpauth://totp/") {
		t.Fatalf("qr_url = %.20q; want the otpauth provisioning URL kept for API clients", url)
	}
}
