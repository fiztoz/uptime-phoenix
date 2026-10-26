package middleware_test

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/middleware"
)

func cspFor(t *testing.T, cfg middleware.SecurityHeadersConfig) string {
	t.Helper()
	e := echo.New()
	e.Use(middleware.SecurityHeaders(cfg))
	e.GET("/", func(c echo.Context) error { return c.String(http.StatusOK, "ok") })
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	return rec.Header().Get("Content-Security-Policy")
}

func directive(csp, name string) string {
	for _, d := range strings.Split(csp, ";") {
		d = strings.TrimSpace(d)
		if d == name || strings.HasPrefix(d, name+" ") {
			return d
		}
	}
	return ""
}

func TestSecurityHeaders_ProductionCSPBlocksInlineAndEvalScripts(t *testing.T) {
	hash := "'sha256-abc='"
	csp := cspFor(t, middleware.SecurityHeadersConfig{Production: true, ScriptHashes: []string{hash}})

	script := directive(csp, "script-src")
	if script != "script-src 'self' "+hash {
		t.Fatalf("script-src = %q, want only 'self' and the inline hash", script)
	}
	for _, name := range []string{"default-src", "script-src", "connect-src"} {
		d := directive(csp, name)
		if strings.Contains(d, "'unsafe-inline'") || strings.Contains(d, "'unsafe-eval'") {
			t.Errorf("%s allows unsafe sources: %q", name, d)
		}
	}
	for _, want := range []string{"object-src 'none'", "base-uri 'self'", "frame-ancestors 'self'", "form-action 'self'"} {
		if directive(csp, strings.Fields(want)[0]) != want {
			t.Errorf("missing %q in %q", want, csp)
		}
	}
}

func TestSecurityHeaders_DevCSPStaysPermissive(t *testing.T) {
	csp := cspFor(t, middleware.SecurityHeadersConfig{})
	if !strings.Contains(directive(csp, "default-src"), "'unsafe-eval'") {
		t.Fatalf("dev CSP should keep 'unsafe-eval' for the Vite dev server: %q", csp)
	}
}

func TestInlineScriptHashes_HashesOnlyInlineScripts(t *testing.T) {
	body := "\n\t\t\t__sveltekit = {};\n\t\t"
	fsys := fstest.MapFS{
		"index.html":       {Data: []byte(`<html><head><script type="module" src="/_app/start.js"></script></head><body><script>` + body + `</script></body></html>`)},
		"login/index.html": {Data: []byte(`<SCRIPT>` + body + `</SCRIPT>`)},
		"_app/start.js":    {Data: []byte(`<script>not html</script>`)},
	}
	got, err := middleware.InlineScriptHashes(fsys)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	want := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	if len(got) != 1 || got[0] != want {
		t.Fatalf("hashes = %v, want [%s]", got, want)
	}
}
