package checker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestHTTPChecker_Type(t *testing.T) {
	c := HTTPChecker{}
	if got := c.Type(); got != "http" {
		t.Errorf("Type() = %q, want %q", got, "http")
	}
}

func TestHTTPChecker_Validate(t *testing.T) {
	tests := []struct {
		name    string
		config  map[string]any
		wantErr bool
	}{
		{"valid url", map[string]any{"url": "https://example.com"}, false},
		{"missing url", map[string]any{}, true},
		{"empty url", map[string]any{"url": ""}, true},
		{"nil config", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := HTTPChecker{}
			err := c.Validate(tt.config)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestHTTPChecker_Check_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":     srv.URL,
		"timeout": 5.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Errorf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
	if result.Message != "200 - OK" {
		t.Errorf("Check() message = %q, want %q", result.Message, "200 - OK")
	}
	if result.LatencyMs < 0 {
		t.Errorf("Check() latency = %d, want >= 0", result.LatencyMs)
	}
}

func TestHTTPChecker_Check_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":     srv.URL,
		"timeout": 5.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN", result.Status)
	}
}

func TestHTTPChecker_Check_AcceptedStatusCodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":                  srv.URL,
		"accepted_statuscodes": []any{"200-299", "404"},
		"timeout":              5.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Errorf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
}

func TestHTTPChecker_Check_Keyword(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"service": "phoenix", "status": "running"}`))
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":     srv.URL,
		"keyword": "phoenix",
		"timeout": 5.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Errorf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
}

func TestHTTPChecker_Check_KeywordNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"service": "phoenix"}`))
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":     srv.URL,
		"keyword": "this-string-definitely-does-not-exist-xyzzy",
		"timeout": 5.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN (message: %s)", result.Status, result.Message)
	}
}

func TestHTTPChecker_Check_JSONQuery(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"slideshow": {"title": "Sample Slide Show"}}`))
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":        srv.URL,
		"json_query": "slideshow.title",
		"timeout":    5.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Errorf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
}

func TestHTTPChecker_Check_JSONQueryNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"slideshow": {"title": "Sample"}}`))
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":        srv.URL,
		"json_query": "nonexistent.path",
		"timeout":    5.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN (message: %s)", result.Status, result.Message)
	}
}

func TestHTTPChecker_Check_FollowsRedirects(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer final.Close()

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL, http.StatusMovedPermanently)
	}))
	defer redirect.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":     redirect.URL,
		"timeout": 5.0,
	})
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Errorf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
}

func TestHTTPChecker_Check_InvalidHost(t *testing.T) {
	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":     "http://this-host-definitely-does-not-exist.invalid/",
		"timeout": 5.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN (message: %s)", result.Status, result.Message)
	}
}

func TestHTTPChecker_Check_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":     srv.URL,
		"timeout": 1.0,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Errorf("Check() status = %v, want DOWN due to timeout (message: %s)", result.Status, result.Message)
	}
	if !strings.Contains(strings.ToLower(result.Message), "timeout") && !strings.Contains(strings.ToLower(result.Message), "context deadline exceeded") {
		t.Logf("timeout message: %s", result.Message)
	}
}

// TestHTTPChecker_Check_TLSIgnore pivots a single self-signed TLS server on
// the tls_ignore flag: verification on (default or explicit false) must fail
// the check, verification skipped must pass it. This is the effect assertion
// for Monitor.TLSIgnore — before it was honored, all three cases were DOWN.
func TestHTTPChecker_Check_TLSIgnore(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer srv.Close()

	tests := []struct {
		name       string
		config     map[string]any
		wantStatus string
	}{
		{"verification on by default", map[string]any{"url": srv.URL, "timeout": 5.0}, "DOWN"},
		{"explicit tls_ignore false", map[string]any{"url": srv.URL, "timeout": 5.0, "tls_ignore": false}, "DOWN"},
		{"tls_ignore true", map[string]any{"url": srv.URL, "timeout": 5.0, "tls_ignore": true}, "UP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := HTTPChecker{}
			result, err := c.Check(context.Background(), tt.config)
			if err != nil {
				t.Errorf("Check() returned unexpected error: %v", err)
			}
			if result.Status.String() != tt.wantStatus {
				t.Errorf("Check() status = %v, want %s (message: %s)", result.Status, tt.wantStatus, result.Message)
			}
		})
	}
}

// TestHTTPChecker_Check_TLSIgnoreKeepsCertMetadata asserts that skipping
// verification does not lose the certificate metadata capture: the handshake
// still completes, so tls_days_remaining / tls_issuer must be populated.
func TestHTTPChecker_Check_TLSIgnoreKeepsCertMetadata(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":        srv.URL,
		"timeout":    5.0,
		"tls_ignore": true,
	})
	if err != nil {
		t.Errorf("Check() returned unexpected error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Fatalf("Check() status = %v, want UP (message: %s)", result.Status, result.Message)
	}
	days, ok := result.Metadata["tls_days_remaining"]
	if !ok {
		t.Fatal("metadata missing tls_days_remaining when skipping verification")
	}
	if n, convErr := strconv.Atoi(days); convErr != nil || n <= 0 {
		t.Errorf("tls_days_remaining = %q, want a positive integer (httptest cert is long-lived)", days)
	}
	if issuer := result.Metadata["tls_issuer"]; issuer == "" {
		t.Error("metadata missing tls_issuer when skipping verification")
	}
	if notAfter := result.Metadata["tls_not_after"]; notAfter == "" {
		t.Error("metadata missing tls_not_after (exact NotAfter RFC3339 required for cert alerts)")
	}
}

func TestHTTPChecker_Check_BasicAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "alice" || pass != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":           srv.URL,
		"timeout":       5.0,
		"auth_method":   "basic",
		"auth_username": "alice",
		"auth_password": "s3cret",
	})
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Fatalf("status = %v, want UP (msg: %s)", result.Status, result.Message)
	}
}

func TestHTTPChecker_Check_BearerAuth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":               srv.URL,
		"timeout":           5.0,
		"auth_method":       "bearer",
		"auth_bearer_token": "tok-123",
	})
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Fatalf("status = %v, want UP (msg: %s)", result.Status, result.Message)
	}
}

func TestHTTPChecker_Check_OAuth2ClientCredentials(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "cid" || pass != "csec" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("ParseForm: %v", err)
		}
		if r.Form.Get("grant_type") != "client_credentials" {
			t.Errorf("grant_type = %q", r.Form.Get("grant_type"))
		}
		if r.Form.Get("scope") != "read write" {
			t.Errorf("scope = %q", r.Form.Get("scope"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-xyz","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("/api", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer at-xyz" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":                  srv.URL + "/api",
		"timeout":              5.0,
		"auth_method":          "oauth2_cc",
		"oauth2_token_url":     srv.URL + "/token",
		"oauth2_client_id":     "cid",
		"oauth2_client_secret": "csec",
		"oauth2_scopes":        "read write",
	})
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Fatalf("status = %v, want UP (msg: %s)", result.Status, result.Message)
	}
}

func TestHTTPChecker_Check_SaveBodyOnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"db down"}`))
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":                srv.URL,
		"timeout":            5.0,
		"save_body_on_error": true,
	})
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if result.Status.String() != "DOWN" {
		t.Fatalf("status = %v, want DOWN", result.Status)
	}
	if !strings.Contains(result.Message, `{"error":"db down"}`) {
		t.Errorf("message should include response body, got %q", result.Message)
	}
	if result.Metadata["response_body"] != `{"error":"db down"}` {
		t.Errorf("metadata response_body = %q", result.Metadata["response_body"])
	}
}

func TestHTTPChecker_Check_SaveBodyOnSuccessOffByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("secret payload"))
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":     srv.URL,
		"timeout": 5.0,
	})
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Fatalf("status = %v, want UP", result.Status)
	}
	if strings.Contains(result.Message, "secret payload") {
		t.Errorf("body must not be saved by default: %q", result.Message)
	}
	if result.Metadata != nil && result.Metadata["response_body"] != "" {
		t.Errorf("metadata must not include response_body by default")
	}
}

func TestHTTPChecker_Check_SaveBodyOnSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("healthy"))
	}))
	defer srv.Close()

	c := HTTPChecker{}
	result, err := c.Check(context.Background(), map[string]any{
		"url":                  srv.URL,
		"timeout":              5.0,
		"save_body_on_success": true,
	})
	if err != nil {
		t.Fatalf("Check() error: %v", err)
	}
	if result.Status.String() != "UP" {
		t.Fatalf("status = %v, want UP", result.Status)
	}
	if !strings.Contains(result.Message, "healthy") {
		t.Errorf("message should include response body, got %q", result.Message)
	}
}
