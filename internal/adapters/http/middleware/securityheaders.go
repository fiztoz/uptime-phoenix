package middleware

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"regexp"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"
)

// SecurityHeadersConfig controls baseline HTTP security headers.
type SecurityHeadersConfig struct {
	// Production enables a stricter Content-Security-Policy for the embedded SPA.
	Production bool
	// ScriptHashes are CSP hash sources ('sha256-…') for the inline scripts in
	// the embedded SPA shell. In production they are the only inline scripts
	// the browser may run; see InlineScriptHashes.
	ScriptHashes []string
}

// devCSP allows inline scripts from Vite/Svelte builds without breaking HMR.
const devCSP = "default-src 'self' 'unsafe-inline' 'unsafe-eval'; connect-src 'self' ws: wss:; img-src 'self' data: blob:; frame-ancestors 'self'"

// ProductionCSP returns the production policy. Scripts are limited to the
// SPA's own files plus the hashed inline bootstrap; inline styles stay allowed
// because Svelte transitions and component style attributes need them, and
// they cannot run code. Images may come from https: and data: URLs because
// status-page logos and favicons are operator-supplied.
func ProductionCSP(scriptHashes []string) string {
	script := "script-src 'self'"
	for _, h := range scriptHashes {
		script += " " + h
	}
	return strings.Join([]string{
		"default-src 'self'",
		script,
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data: blob: https:",
		"font-src 'self' data:",
		"connect-src 'self'",
		"object-src 'none'",
		"frame-ancestors 'self'",
		"base-uri 'self'",
		"form-action 'self'",
	}, "; ")
}

// SecurityHeaders sets common hardening headers on every response.
func SecurityHeaders(cfg SecurityHeadersConfig) echo.MiddlewareFunc {
	csp := devCSP
	if cfg.Production {
		csp = ProductionCSP(cfg.ScriptHashes)
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			h := c.Response().Header()
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "SAMEORIGIN")
			h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
			h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
			h.Set("Content-Security-Policy", csp)
			return next(c)
		}
	}
}

// inlineScript matches a <script> element without a src attribute and
// captures its body exactly as the browser hashes it.
var inlineScript = regexp.MustCompile(`(?is)<script(\s[^>]*)?>(.*?)</script>`)

// InlineScriptHashes returns the sorted, de-duplicated CSP hash sources of
// every inline <script> in the HTML files of fsys. SvelteKit's static build
// boots the SPA from such a script, so a production CSP without these hashes
// renders a blank page.
func InlineScriptHashes(fsys fs.FS) ([]string, error) {
	var hashes []string
	err := fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".html") {
			return err
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		for _, m := range inlineScript.FindAllSubmatch(data, -1) {
			if strings.Contains(strings.ToLower(string(m[1])), "src=") {
				continue
			}
			sum := sha256.Sum256(m[2])
			hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(hashes)
	return slices.Compact(hashes), nil
}
