package checker

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"golang.org/x/net/proxy"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// HTTPChecker performs HTTP(s) health checks with status code, keyword, and JSON query assertions.
type HTTPChecker struct{}

func init() { Register(HTTPChecker{}) }

func (HTTPChecker) Type() string { return "http" }

// Validate checks that url is present, parseable, and has an http/https scheme.
func (HTTPChecker) Validate(config map[string]any) error {
	rawURL, ok := config["url"].(string)
	if !ok || rawURL == "" {
		return fmt.Errorf("url is required and must be a string")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("url is not valid: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("url must have a host")
	}
	if err := validateJSONAssertion(config); err != nil {
		return err
	}
	return nil
}

const (
	jsonOperatorExists      = "exists"
	jsonOperatorNotExists   = "not_exists"
	jsonOperatorEquals      = "equals"
	jsonOperatorNotEquals   = "not_equals"
	jsonOperatorContains    = "contains"
	jsonOperatorNotContains = "not_contains"
)

// validateJSONAssertion checks the optional response-body assertion without
// requiring callers that only use status codes or keywords to configure one.
func validateJSONAssertion(config map[string]any) error {
	jsonQuery, _ := config["json_query"].(string)
	if strings.TrimSpace(jsonQuery) == "" {
		operator, _ := config["json_operator"].(string)
		expected, _ := config["expected_value"].(string)
		if (operator != "" && operator != jsonOperatorExists) || expected != "" {
			return fmt.Errorf("json_query is required when a JSON comparison is configured")
		}
		return nil
	}

	operator := jsonAssertionOperator(config)
	switch operator {
	case jsonOperatorExists, jsonOperatorNotExists:
		return nil
	case jsonOperatorEquals, jsonOperatorNotEquals, jsonOperatorContains, jsonOperatorNotContains:
		expected, ok := config["expected_value"].(string)
		if !ok || expected == "" {
			return fmt.Errorf("expected_value is required when json_operator is %q", operator)
		}
		return nil
	default:
		return fmt.Errorf("unsupported json_operator %q", operator)
	}
}

// jsonAssertionOperator preserves the original path-existence behavior while
// honoring expected_value from older Uptime Kuma imports as an equality check.
func jsonAssertionOperator(config map[string]any) string {
	operator, _ := config["json_operator"].(string)
	operator = strings.TrimSpace(operator)
	if operator != "" {
		return operator
	}
	if expected, ok := config["expected_value"].(string); ok && expected != "" {
		return jsonOperatorEquals
	}
	return jsonOperatorExists
}

// buildHTTPClient builds the *http.Client used for a single check: redirect
// policy from follow_redirects, transport routed through the monitor's
// configured proxy (if any — see buildProxyTransport), and TLS verification
// skipping from tls_ignore.
func buildHTTPClient(config map[string]any) (*http.Client, error) {
	client := &http.Client{}
	if follow, ok := config["follow_redirects"].(bool); ok && !follow {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}
	transport, err := buildProxyTransport(config)
	if err != nil {
		return nil, err
	}
	if ignore, ok := config["tls_ignore"].(bool); ok && ignore {
		if transport == nil {
			transport = &http.Transport{}
		}
		// Operator opt-in per monitor (Monitor.TLSIgnore / config tls_ignore).
		// Verification is skipped only when the operator explicitly enables it
		// for broken/self-signed targets. Handshake still runs so resp.TLS
		// peer certs remain available for tls_days_remaining / tls_issuer.
		transport.TLSClientConfig = operatorTLSIgnoreConfig()
	}
	if transport != nil {
		client.Transport = transport
	}
	return client, nil
}

// buildProxyTransport inspects the "_proxy" config fragment injected by the
// scheduler (see internal/adapters/scheduler/proxy_resolver.go — a checker
// only ever receives a config map, never the monitor, so it cannot resolve
// a proxy itself) and returns an *http.Transport routed through it.
// Returns (nil, nil) when no proxy is configured for this check.
//
// http/https proxies use the stdlib http.ProxyURL. socks5 uses
// golang.org/x/net/proxy, which was already a transitive dependency of this
// module before this feature (see go.mod) — using it directly here doesn't
// add anything new. socks4 is rejected: x/net/proxy has no SOCKS4 dialer,
// and ProxyService already refuses to store a socks4 proxy for the same
// reason, so reaching that branch here would indicate a data inconsistency.
func buildProxyTransport(config map[string]any) (*http.Transport, error) {
	raw, ok := config["_proxy"].(map[string]any)
	if !ok || raw == nil {
		return nil, nil
	}
	protocol, _ := raw["protocol"].(string)
	host, _ := raw["host"].(string)
	if host == "" {
		return nil, nil
	}
	port := intFromAny(raw["port"])

	var userInfo *url.Userinfo
	if authEnabled, _ := raw["auth"].(bool); authEnabled {
		username, _ := raw["username"].(string)
		password, _ := raw["password"].(string)
		userInfo = url.UserPassword(username, password)
	}
	proxyURL := &url.URL{
		Scheme: protocol,
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		User:   userInfo,
	}

	switch protocol {
	case "http", "https":
		return &http.Transport{Proxy: http.ProxyURL(proxyURL)}, nil
	case "socks5":
		dialer, err := proxy.FromURL(proxyURL, proxy.Direct)
		if err != nil {
			return nil, fmt.Errorf("configure socks5 proxy: %w", err)
		}
		return &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported proxy protocol %q", protocol)
	}
}

// intFromAny converts the numeric types that may end up in a config map
// (scheduler-built maps carry Go ints straight through; anything that has
// passed through JSON decodes as float64) into an int, defaulting to 0.
func intFromAny(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	default:
		return 0
	}
}

// maxSavedResponseBody caps HTTP response bodies stored for notifications /
// heartbeat history when save_body_on_* is enabled. Keeps DB rows and chat
// provider payloads within practical limits (Discord/Telegram ~4 KiB).
const maxSavedResponseBody = 4 * 1024

// Check performs an HTTP health check against the target URL.
// Config fields:
//   - url (required, string) — target URL
//   - method (optional, string, default "GET") — HTTP method
//   - headers (optional, map[string]any) — custom request headers
//   - body (optional, string) — request body
//   - timeout (optional, float64, default 10) — timeout in seconds
//   - accepted_statuscodes (optional, []any of strings) — e.g. ["200-299","301"]
//   - tls_ignore (optional, bool) — skip TLS certificate verification (self-signed / internal CAs)
//   - keyword (optional, string) — substring to find in response body
//   - json_query (optional, string) — gjson path to evaluate on response body
//   - json_operator (optional, string) — exists, not_exists, equals, not_equals, contains, or not_contains
//   - expected_value (optional, string) — comparison value for operators other than exists/not_exists
//   - auth_method (optional, string) — "none" (default), "basic", "bearer", "oauth2_cc"
//   - auth_username / auth_password (basic)
//   - auth_bearer_token (bearer)
//   - oauth2_token_url / oauth2_client_id / oauth2_client_secret / oauth2_scopes (oauth2 client credentials)
//   - save_body_on_error / save_body_on_success (optional, bool) — append truncated response body to Message
//
// Never returns an error — all failures are returned as StatusDown with the error in Message.
func (HTTPChecker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	// Extract and validate url.
	rawURL, _ := config["url"].(string)
	if rawURL == "" {
		return ports.CheckResult{Status: domain.StatusDown, Message: "url is required"}, nil
	}

	// Method: default GET, always uppercase.
	method, _ := config["method"].(string)
	if method == "" {
		method = "GET"
	}
	method = strings.ToUpper(method)

	// Timeout: default 10 seconds, minimum 1 second.
	timeoutSec := 10.0
	if timeoutVal, ok := config["timeout"]; ok {
		if tf, ok := timeoutVal.(float64); ok && tf > 0 {
			timeoutSec = tf
		}
	}
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
	defer cancel()

	// Body.
	body, _ := config["body"].(string)

	// Build request.
	req, err := http.NewRequestWithContext(ctx, method, rawURL, strings.NewReader(body))
	if err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("failed to create request: %s", err.Error()),
		}, nil
	}

	// Set custom headers.
	if headers, ok := config["headers"].(map[string]any); ok {
		for k, v := range headers {
			if vs, ok := v.(string); ok {
				req.Header.Set(k, vs)
			}
		}
	}

	// Execute request. Follow redirects by default (e.g. google.com → www.google.com).
	// Set follow_redirects=false in config to inspect the first response only.
	// Routed through the monitor's configured proxy, if any.
	start := time.Now()
	client, err := buildHTTPClient(config)
	if err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("proxy configuration error: %s", err.Error()),
		}, nil
	}

	// Auth is applied after headers so method-based Authorization wins over a
	// stale Authorization header the operator left in the free-form headers field.
	if err := applyHTTPAuth(ctx, client, req, config); err != nil {
		return ports.CheckResult{
			Status:  domain.StatusDown,
			Message: fmt.Sprintf("auth configuration error: %s", err.Error()),
		}, nil
	}

	resp, err := client.Do(req)
	latencyMs := time.Since(start).Milliseconds()

	if err != nil {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("request failed: %s", err.Error()),
		}, nil
	}
	defer func() { _ = resp.Body.Close() }()

	// Read body (limit to 10 MB to prevent OOM).
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 10*1024*1024))
	if err != nil {
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latencyMs,
			Message:   fmt.Sprintf("failed to read response body: %s", err.Error()),
		}, nil
	}
	bodyStr := string(bodyBytes)

	// Extract TLS certificate info for HTTPS responses.
	// tls_not_after is the exact certificate NotAfter (RFC3339) — callers must
	// not reconstruct expiry by adding rounded days to "now" (Sprint C F2.1).
	var metadata map[string]string
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		cert := resp.TLS.PeerCertificates[0]
		daysRemaining := int(math.Ceil(time.Until(cert.NotAfter).Hours() / 24))
		if daysRemaining < 0 {
			daysRemaining = 0
		}
		metadata = map[string]string{
			"tls_days_remaining": strconv.Itoa(daysRemaining),
			"tls_issuer":         cert.Issuer.String(),
			"tls_not_after":      cert.NotAfter.UTC().Format(time.RFC3339),
		}
	}

	finish := func(status domain.Status, message string) ports.CheckResult {
		result := ports.CheckResult{
			Status:    status,
			LatencyMs: latencyMs,
			Message:   message,
			Metadata:  metadata,
		}
		attachSavedResponseBody(config, status, bodyStr, &result)
		return result
	}

	// Validate status code against accepted rules.
	if acceptedCodes, ok := config["accepted_statuscodes"].([]any); ok && len(acceptedCodes) > 0 {
		if !isStatusCodeAccepted(resp.StatusCode, acceptedCodes) {
			return finish(domain.StatusDown, fmt.Sprintf("unexpected status code: %d", resp.StatusCode)), nil
		}
	} else {
		// Default: accept any 2xx.
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return finish(domain.StatusDown, fmt.Sprintf("unexpected status code: %d", resp.StatusCode)), nil
		}
	}

	// Check keyword presence in body.
	if keyword, ok := config["keyword"].(string); ok && keyword != "" {
		if !strings.Contains(bodyStr, keyword) {
			return finish(domain.StatusDown, fmt.Sprintf("keyword %q not found in response body", keyword)), nil
		}
	}

	// Check the optional JSON response assertion. A missing json_operator keeps
	// path-only monitors backward compatible; imported expected_value configs
	// implicitly use equality.
	if jsonQuery, ok := config["json_query"].(string); ok && jsonQuery != "" {
		if message := evaluateJSONAssertion(bodyStr, jsonQuery, config); message != "" {
			return finish(domain.StatusDown, message), nil
		}
	}

	return finish(domain.StatusUp, fmt.Sprintf("%d - OK", resp.StatusCode)), nil
}

// evaluateJSONAssertion returns an empty message when the assertion passes and
// an actionable heartbeat message when it fails.
func evaluateJSONAssertion(body, jsonQuery string, config map[string]any) string {
	if err := validateJSONAssertion(config); err != nil {
		return fmt.Sprintf("invalid JSON assertion: %s", err)
	}
	result := gjson.Get(body, jsonQuery)
	operator := jsonAssertionOperator(config)

	if operator == jsonOperatorNotExists {
		if result.Exists() {
			return fmt.Sprintf("JSON path %q exists but should not", jsonQuery)
		}
		return ""
	}
	if !result.Exists() {
		return fmt.Sprintf("JSON path %q was not found in the response body", jsonQuery)
	}
	if operator == jsonOperatorExists {
		return ""
	}

	expected, _ := config["expected_value"].(string)
	actual := jsonAssertionValue(result)
	switch operator {
	case jsonOperatorEquals:
		if actual != expected {
			return fmt.Sprintf("JSON path %q returned %q; expected %q", jsonQuery, compactJSONAssertionValue(actual), expected)
		}
	case jsonOperatorNotEquals:
		if actual == expected {
			return fmt.Sprintf("JSON path %q returned the disallowed value %q", jsonQuery, compactJSONAssertionValue(actual))
		}
	case jsonOperatorContains:
		if !strings.Contains(actual, expected) {
			return fmt.Sprintf("JSON path %q returned %q; expected it to contain %q", jsonQuery, compactJSONAssertionValue(actual), expected)
		}
	case jsonOperatorNotContains:
		if strings.Contains(actual, expected) {
			return fmt.Sprintf("JSON path %q returned a value containing disallowed text %q", jsonQuery, expected)
		}
	}
	return ""
}

func jsonAssertionValue(result gjson.Result) string {
	if result.Type == gjson.Null && result.Raw == "null" {
		return "null"
	}
	return result.String()
}

func compactJSONAssertionValue(value string) string {
	const maxRunes = 160
	runes := []rune(value)
	if len(runes) <= maxRunes {
		return value
	}
	return string(runes[:maxRunes]) + "…"
}

// applyHTTPAuth mutates req with the configured auth method.
// Supported methods: none (default), basic, bearer, oauth2_cc (client credentials).
// NTLM and mTLS are intentionally unsupported.
func applyHTTPAuth(ctx context.Context, client *http.Client, req *http.Request, config map[string]any) error {
	method, _ := config["auth_method"].(string)
	method = strings.ToLower(strings.TrimSpace(method))
	switch method {
	case "", "none":
		return nil
	case "basic":
		user, _ := config["auth_username"].(string)
		pass, _ := config["auth_password"].(string)
		if user == "" {
			return fmt.Errorf("auth_username is required for basic auth")
		}
		req.SetBasicAuth(user, pass)
		return nil
	case "bearer":
		token, _ := config["auth_bearer_token"].(string)
		if token == "" {
			// Also accept the shorter key some importers/UIs may use.
			token, _ = config["auth_token"].(string)
		}
		if token == "" {
			return fmt.Errorf("auth_bearer_token is required for bearer auth")
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	case "oauth2_cc", "oauth2", "oauth2_client_credentials":
		token, err := fetchOAuth2ClientCredentialsToken(ctx, client, config)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		return nil
	default:
		return fmt.Errorf("unsupported auth_method %q (supported: none, basic, bearer, oauth2_cc)", method)
	}
}

// fetchOAuth2ClientCredentialsToken exchanges client_id/client_secret for an
// access_token at oauth2_token_url (RFC 6749 §4.4). Scopes are optional.
func fetchOAuth2ClientCredentialsToken(ctx context.Context, client *http.Client, config map[string]any) (string, error) {
	tokenURL, _ := config["oauth2_token_url"].(string)
	clientID, _ := config["oauth2_client_id"].(string)
	clientSecret, _ := config["oauth2_client_secret"].(string)
	scopes, _ := config["oauth2_scopes"].(string)
	if tokenURL == "" {
		return "", fmt.Errorf("oauth2_token_url is required for oauth2_cc")
	}
	if clientID == "" || clientSecret == "" {
		return "", fmt.Errorf("oauth2_client_id and oauth2_client_secret are required for oauth2_cc")
	}

	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	if strings.TrimSpace(scopes) != "" {
		form.Set("scope", strings.TrimSpace(scopes))
	}

	tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build oauth2 token request: %w", err)
	}
	tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenReq.SetBasicAuth(clientID, clientSecret)

	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(tokenReq)
	if err != nil {
		return "", fmt.Errorf("oauth2 token request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read oauth2 token response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := strings.TrimSpace(string(raw))
		if len(snippet) > 200 {
			snippet = snippet[:200] + "…"
		}
		return "", fmt.Errorf("oauth2 token endpoint returned %d: %s", resp.StatusCode, snippet)
	}

	accessToken := gjson.GetBytes(raw, "access_token").String()
	if accessToken == "" {
		return "", fmt.Errorf("oauth2 token response missing access_token")
	}
	return accessToken, nil
}

// attachSavedResponseBody appends a truncated response body to Message (and
// Metadata["response_body"]) when the operator opted in via save_body_on_error
// or save_body_on_success. The truncated Message is what notifications surface
// via AlertContext.CheckOutput (wired from heartbeat Msg).
func attachSavedResponseBody(config map[string]any, status domain.Status, body string, result *ports.CheckResult) {
	if result == nil || body == "" {
		return
	}
	saveErr, _ := config["save_body_on_error"].(bool)
	saveOK, _ := config["save_body_on_success"].(bool)
	want := (status == domain.StatusDown && saveErr) || (status == domain.StatusUp && saveOK)
	if !want {
		return
	}

	truncated := body
	if len(truncated) > maxSavedResponseBody {
		truncated = truncated[:maxSavedResponseBody] + "…(truncated)"
	}
	if result.Metadata == nil {
		result.Metadata = map[string]string{}
	}
	result.Metadata["response_body"] = truncated
	result.Message = result.Message + "\n\nResponse body:\n" + truncated
}

// isStatusCodeAccepted returns true if the status code matches any of the acceptance rules.
// Each rule can be a range like "200-299" or an exact code like "301".
func isStatusCodeAccepted(code int, rules []any) bool {
	for _, rule := range rules {
		ruleStr, ok := rule.(string)
		if !ok {
			continue
		}
		if idx := strings.Index(ruleStr, "-"); idx != -1 {
			low, err1 := strconv.Atoi(ruleStr[:idx])
			high, err2 := strconv.Atoi(ruleStr[idx+1:])
			if err1 == nil && err2 == nil && code >= low && code <= high {
				return true
			}
		} else {
			if val, err := strconv.Atoi(ruleStr); err == nil && code == val {
				return true
			}
		}
	}
	return false
}

// operatorTLSIgnoreConfig builds a TLS client config for monitors that opt into
// skipping certificate verification (self-signed / broken lab hosts).
// MinVersion still enforces TLS 1.2+; only peer certificate trust is relaxed.
//
// This is never the global default — only monitors with tls_ignore / TLSIgnore
// set by an operator use this config (uptime checks against broken lab certs).
func operatorTLSIgnoreConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// codeql[go/disabled-certificate-check]: per-monitor operator opt-in (Monitor.TLSIgnore), not a global bypass
		InsecureSkipVerify: true,
	}
}
