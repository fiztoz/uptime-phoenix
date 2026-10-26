package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const (
	bootstrapTestPassword = "Tr0ub4dor&3-handler-secret"
	bootstrapTestKey      = "-----BEGIN OPENSSH PRIVATE KEY-----\\nhandler-test-key-material\\n-----END OPENSSH PRIVATE KEY-----\\n"
	bootstrapTestPin      = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
)

type bootstrapFake struct {
	calls    int
	got      domain.ProbeHostBootstrapRequest
	password string
	key      string
	result   domain.ProbeHostBootstrapResult
	err      error
}

func (f *bootstrapFake) Bootstrap(_ context.Context, req domain.ProbeHostBootstrapRequest) (domain.ProbeHostBootstrapResult, error) {
	f.calls++
	f.got = req
	f.password = string(req.Password)
	f.key = string(req.PrivateKey)
	return f.result, f.err
}

func bootstrapBody(overrides map[string]any, drop ...string) string {
	body := map[string]any{
		"ssh_address":          "203.0.113.20:22",
		"ssh_user":             "ops",
		"ssh_password":         bootstrapTestPassword,
		"host_key_fingerprint": bootstrapTestPin,
		"private_key":          strings.ReplaceAll(bootstrapTestKey, `\n`, "\n"),
		"helper_version":       "1.4.0",
		"helper_architecture":  "amd64",
		"helper_sha256":        strings.Repeat("b", 64),
		"helper_size":          4096,
	}
	for k, v := range overrides {
		body[k] = v
	}
	for _, k := range drop {
		delete(body, k)
	}
	data, _ := json.Marshal(body)
	return string(data)
}

func bootstrapServe(t *testing.T, h *ProbeHostBootstrapHandlers, actor int64, body string) *httptest.ResponseRecorder {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	e := echo.New()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/probe-host-bootstraps", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	c := e.NewContext(req, rec)
	if actor != 0 {
		c.Set(ContextUserIDKey, actor)
	}
	if err := h.Submit(c); err != nil {
		e.HTTPErrorHandler(err, c)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q (status %d)", got, rec.Code)
	}
	for _, secret := range []string{bootstrapTestPassword, "handler-test-key-material", "203.0.113.20", bootstrapTestPin} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("response leaks %q: %s", secret, rec.Body.String())
		}
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("log leaks %q: %s", secret, logs.String())
		}
	}
	return rec
}

func TestProbeHostBootstrapHandlerSuccess(t *testing.T) {
	fake := &bootstrapFake{result: domain.ProbeHostBootstrapResult{Helper: "installed", StageAuthority: "unchanged", AuthorizedKey: "added", Verified: true}}
	rec := bootstrapServe(t, NewProbeHostBootstrapHandlers(fake, true), 42, bootstrapBody(nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	want := `{"kind":"ssh_probe_host_bootstrap","status":"verified","helper":{"version":"1.4.0","architecture":"amd64","state":"installed"},"stage_authority":"unchanged","authorized_key":"added"}`
	if strings.TrimSpace(rec.Body.String()) != want {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if fake.calls != 1 || fake.password != bootstrapTestPassword || !strings.Contains(fake.key, "handler-test-key-material") {
		t.Fatal("service did not receive the transient secrets")
	}
	if fake.got.RequestedBy != 42 || fake.got.Host != (domain.ProbeSSHHost{Address: "203.0.113.20:22", User: "ops"}) ||
		fake.got.HostKeyFingerprint != bootstrapTestPin || fake.got.Helper.Size != 4096 || fake.got.Helper.Version != "1.4.0" {
		t.Fatalf("request=%+v", fake.got)
	}
	// The handler zeroes its owned copies once the response is decided.
	if !bytes.Equal(fake.got.Password, make([]byte, len(fake.got.Password))) || !bytes.Equal(fake.got.PrivateKey, make([]byte, len(fake.got.PrivateKey))) {
		t.Fatal("handler did not clear its password/key buffers")
	}
}

func TestProbeHostBootstrapHandlerStrictBody(t *testing.T) {
	bodies := map[string]string{
		"unknown key":   bootstrapBody(map[string]any{"password": "x"}),
		"case variant":  bootstrapBody(map[string]any{"SSH_Password": "x"}, "ssh_password"),
		"missing":       bootstrapBody(nil, "ssh_password"),
		"null":          bootstrapBody(map[string]any{"ssh_password": nil}),
		"number string": bootstrapBody(map[string]any{"helper_size": "4096"}),
		"negative size": bootstrapBody(map[string]any{"helper_size": -1}),
		"float size":    bootstrapBody(map[string]any{"helper_size": 1.5}),
		"trailing":      bootstrapBody(nil) + "{}",
		"array":         "[]",
		"duplicate":     strings.Replace(bootstrapBody(nil), `"ssh_user":"ops"`, `"ssh_user":"ops","ssh_user":"root"`, 1),
		"newline pass":  bootstrapBody(map[string]any{"ssh_password": "a\nb"}),
		"empty pass":    bootstrapBody(map[string]any{"ssh_password": ""}),
		"bad pin":       bootstrapBody(map[string]any{"host_key_fingerprint": "MD5:00"}),
		"bad address":   bootstrapBody(map[string]any{"ssh_address": "nohost"}),
		"bad helper":    bootstrapBody(map[string]any{"helper_architecture": "riscv64"}),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			fake := &bootstrapFake{}
			rec := bootstrapServe(t, NewProbeHostBootstrapHandlers(fake, true), 1, body)
			if rec.Code != http.StatusBadRequest || fake.calls != 0 || !strings.Contains(rec.Body.String(), `"code":"invalid_bootstrap"`) {
				t.Fatalf("status=%d calls=%d body=%s", rec.Code, fake.calls, rec.Body.String())
			}
		})
	}
	fake := &bootstrapFake{}
	huge := bootstrapBody(map[string]any{"private_key": strings.Repeat("k", probeHostBootstrapMaxBodyBytes)})
	if rec := bootstrapServe(t, NewProbeHostBootstrapHandlers(fake, true), 1, huge); rec.Code != http.StatusRequestEntityTooLarge || fake.calls != 0 {
		t.Fatalf("oversized status=%d", rec.Code)
	}
}

func TestProbeHostBootstrapHandlerGates(t *testing.T) {
	fake := &bootstrapFake{}
	if rec := bootstrapServe(t, NewProbeHostBootstrapHandlers(fake, true), 0, bootstrapBody(nil)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no actor status=%d", rec.Code)
	}
	if rec := bootstrapServe(t, NewProbeHostBootstrapHandlers(fake, false), 1, bootstrapBody(nil)); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "installs_disabled") {
		t.Fatalf("disabled status=%d body=%s", rec.Code, rec.Body.String())
	}
	var typedNil *bootstrapFake
	if rec := bootstrapServe(t, NewProbeHostBootstrapHandlers(typedNil, true), 1, bootstrapBody(nil)); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), "bootstrap_unavailable") {
		t.Fatalf("nil service status=%d", rec.Code)
	}
	if fake.calls != 0 {
		t.Fatal("gated request reached the service")
	}
}

func TestProbeHostBootstrapHandlerErrorMapping(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{&domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapTrust}, 422, "host_trust_failed"},
		{&domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapAuth}, 422, "host_password_rejected"},
		{&domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapPrivilege}, 422, "host_privilege_failed"},
		{&domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapKeyClash}, 422, "host_authorized_key_conflict"},
		{&domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapVerifyAuth}, 422, "verify_auth_failed"},
		{&domain.ProbeHostBootstrapError{Code: "password:" + bootstrapTestPassword}, 503, "bootstrap_unavailable"},
		{domain.ErrProbeInstallArtifact, 422, "artifact_rejected"},
		{domain.ErrProbeSSHInput, 400, "invalid_bootstrap"},
		{domain.ErrValidation, 400, "invalid_bootstrap"},
		{domain.ErrProbeHostBootstrapBusy, 503, "bootstraps_busy"},
		{domain.ErrProbeHostBootstrapClosed, 503, "bootstraps_closed"},
		{context.DeadlineExceeded, 504, "bootstrap_timeout"},
		{errors.New("ssh: unable to authenticate with " + bootstrapTestPassword), 503, "bootstrap_unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			rec := bootstrapServe(t, NewProbeHostBootstrapHandlers(&bootstrapFake{err: tc.err}, true), 1, bootstrapBody(nil))
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), `"code":"`+tc.code+`"`) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if tc.code == "bootstraps_busy" && rec.Header().Get("Retry-After") == "" {
				t.Fatal("busy without Retry-After")
			}
		})
	}
	for code := range probeHostBootstrapMessages {
		if !domain.ValidProbeHostBootstrapCode(code) {
			t.Fatalf("message for unknown code %q", code)
		}
	}
	// An unverified "success" never renders.
	fake := &bootstrapFake{result: domain.ProbeHostBootstrapResult{Helper: "installed", StageAuthority: "installed", AuthorizedKey: "added"}}
	if rec := bootstrapServe(t, NewProbeHostBootstrapHandlers(fake, true), 1, bootstrapBody(nil)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unverified result status=%d", rec.Code)
	}
}
