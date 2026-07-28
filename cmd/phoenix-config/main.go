// Command phoenix-config is a small HTTP client for Phoenix config-as-code
// endpoints (validate, plan, apply, export). It never embeds the server.
//
// Usage:
//
//	phoenix-config validate --file config.yaml
//	phoenix-config plan     --file config.yaml [--prune]
//	phoenix-config apply    --file config.yaml [--prune] [--yes]
//	phoenix-config export   [--out config.yaml]
//
// Auth (admin session JWT or write-scoped API key):
//
//	--token / PHOENIX_TOKEN     Authorization: Bearer <jwt>
//	--api-key / PHOENIX_API_KEY Authorization: ApiKey <key>
//
// Base URL:
//
//	--url / PHOENIX_URL         default http://127.0.0.1:3000
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultURL = "http://127.0.0.1:3000"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) < 1 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		printUsage(os.Stderr)
		if len(args) < 1 {
			return 2
		}
		return 0
	}

	cmd := args[0]
	switch cmd {
	case "validate", "plan", "apply", "export":
	default:
		fmt.Fprintf(os.Stderr, "phoenix-config: unknown command %q\n\n", cmd)
		printUsage(os.Stderr)
		return 2
	}

	fs := flag.NewFlagSet("phoenix-config "+cmd, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	var (
		baseURL string
		token   string
		apiKey  string
		timeout time.Duration
		file    string
		out     string
		prune   bool
		yes     bool
	)
	fs.StringVar(&baseURL, "url", envOr("PHOENIX_URL", defaultURL), "Phoenix base URL (env PHOENIX_URL)")
	fs.StringVar(&token, "token", os.Getenv("PHOENIX_TOKEN"), "session JWT (env PHOENIX_TOKEN)")
	fs.StringVar(&apiKey, "api-key", os.Getenv("PHOENIX_API_KEY"), "API key (env PHOENIX_API_KEY)")
	fs.DurationVar(&timeout, "timeout", 60*time.Second, "HTTP client timeout")
	fs.StringVar(&file, "file", "", "path to config YAML (required for validate/plan/apply)")
	fs.StringVar(&out, "out", "", "write export YAML to this path (mode 0600); default stdout")
	fs.BoolVar(&prune, "prune", false, "include prune deletes in plan/apply")
	fs.BoolVar(&yes, "yes", false, "confirm apply (required)")

	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}

	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		fmt.Fprintln(os.Stderr, "phoenix-config: --url / PHOENIX_URL is empty")
		return 2
	}

	auth, err := selectAuth(token, apiKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phoenix-config: %v\n", err)
		return 2
	}

	client := &http.Client{Timeout: timeout}
	ctx := context.Background()

	switch cmd {
	case "validate":
		if file == "" {
			fmt.Fprintln(os.Stderr, "phoenix-config validate: --file is required")
			return 2
		}
		return cmdValidate(ctx, client, baseURL, auth, file)
	case "plan":
		if file == "" {
			fmt.Fprintln(os.Stderr, "phoenix-config plan: --file is required")
			return 2
		}
		return cmdPlan(ctx, client, baseURL, auth, file, prune)
	case "apply":
		if file == "" {
			fmt.Fprintln(os.Stderr, "phoenix-config apply: --file is required")
			return 2
		}
		if !yes {
			fmt.Fprintln(os.Stderr, "phoenix-config apply: refusing to apply without --yes")
			return 2
		}
		return cmdApply(ctx, client, baseURL, auth, file, prune)
	case "export":
		return cmdExport(ctx, client, baseURL, auth, out)
	default:
		return 2
	}
}

func printUsage(w io.Writer) {
	// Best-effort write to stderr/stdout; CLI usage text failures are not actionable.
	_, _ = fmt.Fprintf(w, `Usage:
  phoenix-config validate --file <config.yaml>
  phoenix-config plan     --file <config.yaml> [--prune]
  phoenix-config apply    --file <config.yaml> [--prune] [--yes]
  phoenix-config export   [--out <config.yaml>]

Common flags / env:
  --url      Phoenix base URL (default %s, env PHOENIX_URL)
  --token    session JWT (env PHOENIX_TOKEN) → Authorization: Bearer …
  --api-key  write-scoped API key (env PHOENIX_API_KEY) → Authorization: ApiKey …
  --timeout  HTTP timeout (default 60s)

Auth requires an admin principal. Prefer env vars over flags so secrets stay out of shell history.
Never commit real webhook URLs or passwords in the config document.
`, defaultURL)
}

// writeOut writes to stdout, ignoring write errors (broken pipe on head/less).
func writeOut(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stdout, format, args...)
}

// authHeader is the Authorization header value (scheme + credential).
// Secrets must never be logged.
type authHeader struct {
	value string
}

func selectAuth(token, apiKey string) (authHeader, error) {
	token = strings.TrimSpace(token)
	apiKey = strings.TrimSpace(apiKey)
	switch {
	case token != "":
		return authHeader{value: "Bearer " + token}, nil
	case apiKey != "":
		return authHeader{value: "ApiKey " + apiKey}, nil
	default:
		return authHeader{}, errors.New("authentication required: set --token / PHOENIX_TOKEN or --api-key / PHOENIX_API_KEY")
	}
}

func joinURL(base, path string) string {
	base = strings.TrimRight(base, "/")
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return base + path
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func cmdValidate(ctx context.Context, client *http.Client, base string, auth authHeader, file string) int {
	body, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phoenix-config: read %s: %v\n", file, err)
		return 1
	}
	status, respBody, err := doRequest(ctx, client, http.MethodPost, joinURL(base, "/api/config/validate"), auth, "application/yaml", body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phoenix-config: %v\n", err)
		return 1
	}
	if status < 200 || status >= 300 {
		printHTTPError(status, respBody)
		return 1
	}

	var result struct {
		Valid  bool     `json:"valid"`
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		fmt.Fprintf(os.Stderr, "phoenix-config: decode validate response: %v\n", err)
		fmt.Fprintf(os.Stderr, "%s\n", respBody)
		return 1
	}
	if result.Valid {
		writeOut("valid: true\n")
		return 0
	}
	writeOut("valid: false\n")
	for _, e := range result.Errors {
		writeOut("  - %s\n", e)
	}
	return 1
}

func cmdPlan(ctx context.Context, client *http.Client, base string, auth authHeader, file string, prune bool) int {
	payload, err := buildApplyPayload(file, prune)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phoenix-config: %v\n", err)
		return 1
	}
	status, respBody, err := doRequest(ctx, client, http.MethodPost, joinURL(base, "/api/config/plan"), auth, "application/yaml", payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phoenix-config: %v\n", err)
		return 1
	}
	if status < 200 || status >= 300 {
		printHTTPError(status, respBody)
		return 1
	}
	pretty, err := prettyJSON(respBody)
	if err != nil {
		// Fall back to raw body if not JSON.
		writeOut("%s\n", respBody)
	} else {
		writeOut("%s\n", pretty)
	}
	// Exit 1 when the plan reports invalid even on HTTP 200.
	var plan struct {
		Valid bool `json:"valid"`
	}
	if err := json.Unmarshal(respBody, &plan); err == nil && !plan.Valid {
		return 1
	}
	return 0
}

func cmdApply(ctx context.Context, client *http.Client, base string, auth authHeader, file string, prune bool) int {
	payload, err := buildApplyPayload(file, prune)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phoenix-config: %v\n", err)
		return 1
	}
	status, respBody, err := doRequest(ctx, client, http.MethodPost, joinURL(base, "/api/config/apply"), auth, "application/yaml", payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phoenix-config: %v\n", err)
		return 1
	}
	if status < 200 || status >= 300 {
		printHTTPError(status, respBody)
		// Still print body as JSON to stdout when present so CI can inspect plan.
		if len(respBody) > 0 {
			if pretty, err := prettyJSON(respBody); err == nil {
				writeOut("%s\n", pretty)
			}
		}
		return 1
	}
	pretty, err := prettyJSON(respBody)
	if err != nil {
		writeOut("%s\n", respBody)
	} else {
		writeOut("%s\n", pretty)
	}
	return 0
}

func cmdExport(ctx context.Context, client *http.Client, base string, auth authHeader, out string) int {
	status, respBody, err := doRequest(ctx, client, http.MethodGet, joinURL(base, "/api/config/export"), auth, "", nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "phoenix-config: %v\n", err)
		return 1
	}
	if status < 200 || status >= 300 {
		printHTTPError(status, respBody)
		return 1
	}
	if out == "" {
		if _, err := os.Stdout.Write(respBody); err != nil {
			fmt.Fprintf(os.Stderr, "phoenix-config: write stdout: %v\n", err)
			return 1
		}
		if len(respBody) > 0 && respBody[len(respBody)-1] != '\n' {
			writeOut("\n")
		}
		return 0
	}
	if err := os.WriteFile(out, respBody, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "phoenix-config: write %s: %v\n", out, err)
		return 1
	}
	// Re-assert mode in case umask widened it.
	if err := os.Chmod(out, 0o600); err != nil {
		fmt.Fprintf(os.Stderr, "phoenix-config: chmod %s: %v\n", out, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "wrote %s\n", out)
	return 0
}

// buildApplyPayload reads a bare ConfigDocument YAML and wraps it as
// {document: <doc>, prune: <bool>} matching decodeConfigApplyRequest.
func buildApplyPayload(file string, prune bool) ([]byte, error) {
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse YAML: %w", err)
	}
	if doc == nil {
		return nil, errors.New("document is empty")
	}
	wrapper := map[string]any{
		"document": doc,
		"prune":    prune,
	}
	out, err := yaml.Marshal(wrapper)
	if err != nil {
		return nil, fmt.Errorf("encode request: %w", err)
	}
	return out, nil
}

func doRequest(ctx context.Context, client *http.Client, method, url string, auth authHeader, contentType string, body []byte) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", auth.value)
	req.Header.Set("Accept", "application/json, application/yaml, text/yaml, */*")
	if contentType != "" && body != nil {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read response: %w", err)
	}
	return resp.StatusCode, respBody, nil
}

func printHTTPError(status int, body []byte) {
	fmt.Fprintf(os.Stderr, "HTTP %d\n", status)
	if len(body) > 0 {
		fmt.Fprintf(os.Stderr, "%s\n", body)
	}
}

func prettyJSON(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.MarshalIndent(v, "", "  ")
}
