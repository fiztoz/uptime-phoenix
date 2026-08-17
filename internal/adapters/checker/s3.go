// Package checker implements the ports.Checker interface for all monitor types.
package checker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	s3HealthHeadBucket = "head_bucket"
	s3HealthHeadObject = "head_object"
	s3HealthGetObject  = "get_object"

	s3ProviderAWS     = "aws"
	s3ProviderMinio   = "minio"
	s3ProviderGeneric = "generic"

	s3DefaultRegion   = "us-east-1"
	s3DefaultTimeout  = 10.0
	s3MaxGetBodyBytes = 64 * 1024
	s3MinBucketLen    = 3
	s3MaxBucketLen    = 63
)

// S3Checker probes S3-compatible object storage for reachability only.
// Availability is a signed HeadBucket, HeadObject, or GetObject. There is no
// usage/quota measurement — the S3 API has no cheap size call, and listing a
// bucket to sum sizes is rejected as a product choice.
type S3Checker struct{}

func init() { Register(S3Checker{}) }

// Type returns the monitor type identifier.
func (S3Checker) Type() string { return "s3" }

type s3Opts struct {
	Provider     string
	Endpoint     string
	Scheme       string
	Host         string
	Region       string
	Bucket       string
	ObjectKey    string
	AccessKey    string
	SecretKey    string
	SessionToken string
	PathStyle    bool
	HealthCheck  string
}

// Validate checks required credentials and that the health-check preset is
// one of the fixed values. Bucket names may contain '-' and '_' (S3-compatible
// stores); '_' is not a valid DNS label, so virtual-hosted addressing is
// rejected for those names.
func (S3Checker) Validate(config map[string]any) error {
	opts, err := parseS3Opts(config)
	if err != nil {
		return err
	}
	if err := validateS3BucketName(opts.Bucket); err != nil {
		return err
	}
	if opts.AccessKey == "" {
		return fmt.Errorf("access_key is required")
	}
	if opts.SecretKey == "" {
		return fmt.Errorf("secret_key is required")
	}
	switch opts.HealthCheck {
	case s3HealthHeadBucket:
	case s3HealthHeadObject, s3HealthGetObject:
		if opts.ObjectKey == "" {
			return fmt.Errorf("object_key is required when health_check is %q", opts.HealthCheck)
		}
	default:
		return fmt.Errorf("health_check must be %q, %q, or %q", s3HealthHeadBucket, s3HealthHeadObject, s3HealthGetObject)
	}
	if pathStyleExplicitlyFalse(config) && bucketNeedsPathStyle(opts.Bucket) {
		return fmt.Errorf("path_style must be true when the bucket name contains characters that are not valid in a DNS hostname (for example '_')")
	}
	if opts.Endpoint != "" {
		if _, _, err := parseS3Endpoint(opts.Endpoint); err != nil {
			return err
		}
	}
	switch opts.Provider {
	case "", s3ProviderAWS, s3ProviderMinio, s3ProviderGeneric:
	default:
		return fmt.Errorf("provider must be %q, %q, or %q", s3ProviderAWS, s3ProviderMinio, s3ProviderGeneric)
	}
	return nil
}

// Check performs a signed S3 health probe.
//
// Config fields:
//   - bucket (required) — may contain '-' and '_'
//   - access_key / secret_key (required)
//   - session_token (optional STS)
//   - endpoint (optional) — http(s)://host:port; required for MinIO / generic
//   - region (optional, default us-east-1)
//   - provider (optional) — aws | minio | generic
//   - path_style (optional bool) — default true when endpoint is set or the
//     bucket cannot be a DNS label; forced true for '_' in the bucket name
//   - health_check (optional) — head_bucket (default), head_object, get_object
//   - object_key (required for head_object / get_object)
//   - timeout (optional, default 10)
//   - tls_ignore (optional) — injected from Monitor.TLSIgnore
//
// Never returns an error — target failures are StatusDown. Secrets never
// appear in Message.
func (S3Checker) Check(ctx context.Context, config map[string]any) (ports.CheckResult, error) {
	opts, err := parseS3Opts(config)
	if err != nil {
		return ports.CheckResult{Status: domain.StatusDown, Message: err.Error()}, nil
	}
	if opts.AccessKey == "" || opts.SecretKey == "" || opts.Bucket == "" {
		return ports.CheckResult{Status: domain.StatusDown, Message: "bucket, access_key, and secret_key are required"}, nil
	}

	timeoutSec := s3DefaultTimeout
	if t, ok := config["timeout"].(float64); ok && t > 0 {
		timeoutSec = t
	}
	if timeoutSec < 1 {
		timeoutSec = 1
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSec*float64(time.Second)))
	defer cancel()

	client, err := buildS3HTTPClient(config)
	if err != nil {
		return ports.CheckResult{Status: domain.StatusDown, Message: fmt.Sprintf("http client: %s", err.Error())}, nil
	}

	req, err := newS3Request(ctx, opts, time.Now().UTC())
	if err != nil {
		return ports.CheckResult{Status: domain.StatusDown, Message: err.Error()}, nil
	}

	start := time.Now()
	resp, err := client.Do(req)
	latency := elapsedMillis(start)
	if err != nil {
		return ports.CheckResult{
			Status:     domain.StatusDown,
			LatencyMs:  latency,
			DurationMs: latency,
			Message:    fmt.Sprintf("s3 %s failed: %s", opts.HealthCheck, sanitizeS3Err(err)),
		}, nil
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, s3MaxGetBodyBytes+1))

	result := interpretS3Response(resp, opts, latency)
	result.DurationMs = latency
	return result, nil
}

func buildS3HTTPClient(config map[string]any) (*http.Client, error) {
	client, err := buildHTTPClient(config)
	if err != nil {
		return nil, err
	}
	// Signed requests cannot be replayed against a redirected host.
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return client, nil
}

func parseS3Opts(config map[string]any) (s3Opts, error) {
	if config == nil {
		return s3Opts{}, fmt.Errorf("config is required")
	}
	opts := s3Opts{
		Provider:     strings.ToLower(strings.TrimSpace(configString(config, "provider"))),
		Endpoint:     strings.TrimSpace(configString(config, "endpoint")),
		Region:       strings.TrimSpace(configString(config, "region")),
		Bucket:       strings.TrimSpace(configString(config, "bucket")),
		ObjectKey:    strings.Trim(strings.TrimSpace(configString(config, "object_key")), "/"),
		AccessKey:    strings.TrimSpace(configString(config, "access_key")),
		SecretKey:    configString(config, "secret_key"),
		SessionToken: configString(config, "session_token"),
		HealthCheck:  strings.ToLower(strings.TrimSpace(configString(config, "health_check"))),
	}
	if opts.Region == "" {
		opts.Region = s3DefaultRegion
	}
	if opts.HealthCheck == "" {
		opts.HealthCheck = s3HealthHeadBucket
	}
	if opts.Provider == "" {
		if opts.Endpoint != "" {
			opts.Provider = s3ProviderGeneric
		} else {
			opts.Provider = s3ProviderAWS
		}
	}

	scheme, host, err := resolveS3Endpoint(opts.Endpoint, opts.Region)
	if err != nil {
		return s3Opts{}, err
	}
	opts.Scheme = scheme
	opts.Host = host
	opts.PathStyle = resolveS3PathStyle(config, opts.Endpoint, opts.Bucket)
	return opts, nil
}

func resolveS3Endpoint(endpoint, region string) (scheme, host string, err error) {
	if strings.TrimSpace(endpoint) == "" {
		return "https", "s3." + region + ".amazonaws.com", nil
	}
	return parseS3Endpoint(endpoint)
}

func parseS3Endpoint(raw string) (scheme, host string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("endpoint is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("endpoint is not a valid URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", "", fmt.Errorf("endpoint scheme must be http or https")
	}
	if u.Host == "" {
		return "", "", fmt.Errorf("endpoint must have a host")
	}
	if u.Path != "" && u.Path != "/" {
		return "", "", fmt.Errorf("endpoint must not include a path")
	}
	return u.Scheme, u.Host, nil
}

func resolveS3PathStyle(config map[string]any, endpoint, bucket string) bool {
	if bucketNeedsPathStyle(bucket) {
		return true
	}
	if _, ok := config["path_style"]; ok {
		return configBool(config, "path_style")
	}
	return strings.TrimSpace(endpoint) != ""
}

func pathStyleExplicitlyFalse(config map[string]any) bool {
	if config == nil {
		return false
	}
	if _, ok := config["path_style"]; !ok {
		return false
	}
	return !configBool(config, "path_style")
}

func bucketNeedsPathStyle(bucket string) bool {
	for _, r := range bucket {
		if r == '_' || (!unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '-' && r != '.') {
			return true
		}
	}
	return false
}

func validateS3BucketName(bucket string) error {
	if bucket == "" {
		return fmt.Errorf("bucket is required")
	}
	if len(bucket) < s3MinBucketLen || len(bucket) > s3MaxBucketLen {
		return fmt.Errorf("bucket must be between %d and %d characters", s3MinBucketLen, s3MaxBucketLen)
	}
	for i, r := range bucket {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			continue
		}
		return fmt.Errorf("bucket contains invalid character %q at position %d (letters, digits, '-', '_', and '.' are allowed)", r, i+1)
	}
	return nil
}

func newS3Request(ctx context.Context, opts s3Opts, now time.Time) (*http.Request, error) {
	method := http.MethodHead
	if opts.HealthCheck == s3HealthGetObject {
		method = http.MethodGet
	}

	host := opts.Host
	if !opts.PathStyle {
		host = opts.Bucket + "." + opts.Host
	}
	rawURL := opts.Scheme + "://" + host + s3RequestPath(opts)
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: invalid URL")
	}
	req.Host = host
	req.Header.Set("Host", host)
	signS3Request(req, opts, now, unsignedPayloadHash)
	return req, nil
}

func s3RequestPath(opts s3Opts) string {
	var segments []string
	if opts.PathStyle {
		segments = append(segments, opts.Bucket)
	}
	if opts.HealthCheck != s3HealthHeadBucket && opts.ObjectKey != "" {
		segments = append(segments, strings.Split(opts.ObjectKey, "/")...)
	}
	if len(segments) == 0 {
		return "/"
	}
	encoded := make([]string, len(segments))
	for i, seg := range segments {
		encoded[i] = s3URIEncode(seg, true)
	}
	return "/" + strings.Join(encoded, "/")
}

func interpretS3Response(resp *http.Response, opts s3Opts, latency int64) ports.CheckResult {
	target := s3ResultTarget(opts)
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return ports.CheckResult{
			Status:    domain.StatusUp,
			LatencyMs: latency,
			Message:   fmt.Sprintf("s3 %s %s ok", opts.HealthCheck, target),
		}
	case http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
		loc := strings.TrimSpace(resp.Header.Get("Location"))
		msg := fmt.Sprintf("s3 %s %s redirected (%d) — wrong region or addressing style", opts.HealthCheck, target, resp.StatusCode)
		if loc != "" {
			msg += "; location=" + loc
		}
		return ports.CheckResult{Status: domain.StatusDown, LatencyMs: latency, Message: msg}
	case http.StatusForbidden:
		msg := fmt.Sprintf("s3 %s %s denied (403)", opts.HealthCheck, target)
		if looksLikeClockSkew(resp) {
			msg += "; request time too skewed — check NTP on this node"
		}
		return ports.CheckResult{Status: domain.StatusDown, LatencyMs: latency, Message: msg}
	case http.StatusNotFound:
		what := "bucket"
		if opts.HealthCheck != s3HealthHeadBucket {
			what = "object"
		}
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latency,
			Message:   fmt.Sprintf("s3 %s %s not found (404) — %s missing or wrong region", opts.HealthCheck, target, what),
		}
	default:
		return ports.CheckResult{
			Status:    domain.StatusDown,
			LatencyMs: latency,
			Message:   fmt.Sprintf("s3 %s %s HTTP %d", opts.HealthCheck, target, resp.StatusCode),
		}
	}
}

func s3ResultTarget(opts s3Opts) string {
	if opts.ObjectKey != "" && opts.HealthCheck != s3HealthHeadBucket {
		return opts.Bucket + "/" + opts.ObjectKey
	}
	return opts.Bucket
}

func looksLikeClockSkew(resp *http.Response) bool {
	if strings.Contains(strings.ToLower(resp.Header.Get("x-amz-error-code")), "requesttimetooskewed") {
		return true
	}
	if strings.Contains(strings.ToLower(resp.Status), "requesttimetooskewed") {
		return true
	}
	return false
}

func sanitizeS3Err(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	// Drop anything that looks like a query credential leak if a caller
	// ever puts keys in the endpoint. Signing uses headers, not the URL.
	if i := strings.Index(msg, "X-Amz-"); i >= 0 {
		return strings.TrimSpace(msg[:i]) + "…"
	}
	return msg
}

func configString(config map[string]any, key string) string {
	if config == nil {
		return ""
	}
	s, _ := config[key].(string)
	return s
}
