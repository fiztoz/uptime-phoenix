package checker

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestS3Checker_Type(t *testing.T) {
	if got := (S3Checker{}).Type(); got != "s3" {
		t.Fatalf("Type() = %q, want s3", got)
	}
}

func TestS3Checker_Validate(t *testing.T) {
	base := map[string]any{
		"bucket":     "my-bucket",
		"access_key": "AKIAEXAMPLE",
		"secret_key": "secret",
		"region":     "us-east-1",
	}
	tests := []struct {
		name    string
		mutate  func(map[string]any)
		wantErr string
	}{
		{name: "ok hyphen bucket", mutate: func(map[string]any) {}},
		{
			name: "ok underscore bucket path-style",
			mutate: func(c map[string]any) {
				c["bucket"] = "my_backup_bucket"
				c["path_style"] = true
				c["endpoint"] = "http://minio:9000"
			},
		},
		{
			name:    "missing bucket",
			mutate:  func(c map[string]any) { delete(c, "bucket") },
			wantErr: "bucket is required",
		},
		{
			name:    "missing access_key",
			mutate:  func(c map[string]any) { delete(c, "access_key") },
			wantErr: "access_key is required",
		},
		{
			name:    "missing secret_key",
			mutate:  func(c map[string]any) { delete(c, "secret_key") },
			wantErr: "secret_key is required",
		},
		{
			name:    "short bucket",
			mutate:  func(c map[string]any) { c["bucket"] = "ab" },
			wantErr: "between 3 and 63",
		},
		{
			name:    "invalid bucket char",
			mutate:  func(c map[string]any) { c["bucket"] = "my bucket" },
			wantErr: "invalid character",
		},
		{
			name: "underscore requires path-style",
			mutate: func(c map[string]any) {
				c["bucket"] = "my_bucket"
				c["path_style"] = false
			},
			wantErr: "path_style must be true",
		},
		{
			name: "head_object needs object_key",
			mutate: func(c map[string]any) {
				c["health_check"] = "head_object"
			},
			wantErr: "object_key is required",
		},
		{
			name:    "bad health_check",
			mutate:  func(c map[string]any) { c["health_check"] = "list_all" },
			wantErr: "health_check must be",
		},
		{
			name:    "endpoint with path",
			mutate:  func(c map[string]any) { c["endpoint"] = "http://minio:9000/bucket" },
			wantErr: "must not include a path",
		},
		{
			name:    "bad provider",
			mutate:  func(c map[string]any) { c["provider"] = "azure" },
			wantErr: "provider must be",
		},
	}

	c := S3Checker{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := cloneConfig(base)
			tt.mutate(cfg)
			err := c.Validate(cfg)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want substring %q", err, tt.wantErr)
			}
		})
	}
}

func TestS3Checker_Check_KnownGoodAndBad(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			_, _ = io.Copy(io.Discard, r.Body)
		}
		switch {
		case r.URL.Path == "/good-bucket" || r.URL.Path == "/good-bucket/canary":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/missing-bucket":
			w.WriteHeader(http.StatusNotFound)
		case r.URL.Path == "/denied-bucket":
			w.WriteHeader(http.StatusForbidden)
		case r.URL.Path == "/redirect-bucket":
			w.Header().Set("Location", "https://elsewhere.example/redirect-bucket")
			w.WriteHeader(http.StatusMovedPermanently)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := S3Checker{}
	tests := []struct {
		name       string
		cfg        map[string]any
		wantStatus domain.Status
		wantSubstr string
	}{
		{
			name: "head_bucket 200",
			cfg: map[string]any{
				"endpoint": srv.URL, "bucket": "good-bucket",
				"access_key": "AKIA", "secret_key": "secret",
				"path_style": true, "timeout": 5.0,
			},
			wantStatus: domain.StatusUp,
			wantSubstr: "ok",
		},
		{
			name: "head_object 200",
			cfg: map[string]any{
				"endpoint": srv.URL, "bucket": "good-bucket",
				"object_key": "canary", "health_check": "head_object",
				"access_key": "AKIA", "secret_key": "secret",
				"path_style": true, "timeout": 5.0,
			},
			wantStatus: domain.StatusUp,
			wantSubstr: "ok",
		},
		{
			name: "404 is DOWN",
			cfg: map[string]any{
				"endpoint": srv.URL, "bucket": "missing-bucket",
				"access_key": "AKIA", "secret_key": "secret",
				"path_style": true, "timeout": 5.0,
			},
			wantStatus: domain.StatusDown,
			wantSubstr: "404",
		},
		{
			name: "403 is DOWN",
			cfg: map[string]any{
				"endpoint": srv.URL, "bucket": "denied-bucket",
				"access_key": "AKIA", "secret_key": "secret",
				"path_style": true, "timeout": 5.0,
			},
			wantStatus: domain.StatusDown,
			wantSubstr: "403",
		},
		{
			name: "redirect is DOWN and is not followed",
			cfg: map[string]any{
				"endpoint": srv.URL, "bucket": "redirect-bucket",
				"access_key": "AKIA", "secret_key": "secret",
				"path_style": true, "timeout": 5.0,
			},
			wantStatus: domain.StatusDown,
			wantSubstr: "redirected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := c.Check(context.Background(), tt.cfg)
			if err != nil {
				t.Fatalf("Check() unexpected error: %v", err)
			}
			if result.Status != tt.wantStatus {
				t.Fatalf("status = %s, want %s (message: %s)", result.Status, tt.wantStatus, result.Message)
			}
			if !strings.Contains(result.Message, tt.wantSubstr) {
				t.Fatalf("message %q, want substring %q", result.Message, tt.wantSubstr)
			}
			if strings.Contains(result.Message, "secret") || strings.Contains(result.Message, "AKIA") {
				t.Fatalf("message leaked credentials: %q", result.Message)
			}
		})
	}
}

func TestS3Checker_Check_UnderscoreBucketUsesPathStyle(t *testing.T) {
	var gotHost, gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	result, err := (S3Checker{}).Check(context.Background(), map[string]any{
		"endpoint":   srv.URL,
		"bucket":     "my_backup_bucket",
		"access_key": "AKIA",
		"secret_key": "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		"timeout":    5.0,
	})
	if err != nil {
		t.Fatalf("Check() unexpected error: %v", err)
	}
	if result.Status != domain.StatusUp {
		t.Fatalf("status = %s, want UP (%s)", result.Status, result.Message)
	}
	if strings.Contains(gotHost, "my_backup_bucket") {
		t.Fatalf("host %q used virtual-hosted style with an underscore bucket", gotHost)
	}
	if gotPath != "/my_backup_bucket" {
		t.Fatalf("path = %q, want /my_backup_bucket", gotPath)
	}
	if !strings.HasPrefix(gotAuth, "AWS4-HMAC-SHA256 Credential=") {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if !strings.Contains(gotAuth, "/s3/aws4_request") {
		t.Fatalf("Authorization missing s3 scope: %q", gotAuth)
	}
	if result.Message != "" && strings.Contains(result.Message, "wJalrXUtnFEMI") {
		t.Fatalf("message leaked secret: %q", result.Message)
	}
}

func TestS3Checker_Check_HyphenBucketVirtualHosted(t *testing.T) {
	opts := s3Opts{
		Scheme:      "https",
		Host:        "s3.us-east-1.amazonaws.com",
		Region:      "us-east-1",
		Bucket:      "my-backup-bucket",
		AccessKey:   "AKIA",
		SecretKey:   "secret",
		PathStyle:   false,
		HealthCheck: s3HealthHeadBucket,
	}
	req, err := newS3Request(context.Background(), opts, time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if req.Host != "my-backup-bucket.s3.us-east-1.amazonaws.com" {
		t.Fatalf("host = %q", req.Host)
	}
	if req.URL.Path != "/" && req.URL.Path != "" {
		t.Fatalf("path = %q, want / for virtual-hosted HeadBucket", req.URL.Path)
	}
	if req.URL.Host != "my-backup-bucket.s3.us-east-1.amazonaws.com" {
		t.Fatalf("url host = %q", req.URL.Host)
	}
}

func TestS3Checker_Check_ConnectionRefused(t *testing.T) {
	result, err := (S3Checker{}).Check(context.Background(), map[string]any{
		"endpoint":   "http://127.0.0.1:1",
		"bucket":     "any-bucket",
		"access_key": "AKIA",
		"secret_key": "secret",
		"path_style": true,
		"timeout":    1.0,
	})
	if err != nil {
		t.Fatalf("Check() unexpected error: %v", err)
	}
	if result.Status != domain.StatusDown {
		t.Fatalf("status = %s, want DOWN", result.Status)
	}
	if strings.Contains(result.Message, "secret") {
		t.Fatalf("message leaked secret: %q", result.Message)
	}
}

func TestS3Checker_Check_TLSIgnore(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	tests := []struct {
		name       string
		tlsIgnore  any
		wantStatus domain.Status
	}{
		{name: "verify by default", wantStatus: domain.StatusDown},
		{name: "explicit false", tlsIgnore: false, wantStatus: domain.StatusDown},
		{name: "ignore", tlsIgnore: true, wantStatus: domain.StatusUp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := map[string]any{
				"endpoint":   srv.URL,
				"bucket":     "tls-bucket",
				"access_key": "AKIA",
				"secret_key": "secret",
				"path_style": true,
				"timeout":    5.0,
			}
			if tt.tlsIgnore != nil {
				cfg["tls_ignore"] = tt.tlsIgnore
			}
			result, err := (S3Checker{}).Check(context.Background(), cfg)
			if err != nil {
				t.Fatalf("Check() unexpected error: %v", err)
			}
			if result.Status != tt.wantStatus {
				t.Fatalf("status = %s, want %s (%s)", result.Status, tt.wantStatus, result.Message)
			}
		})
	}
}

func TestS3Checker_GetObjectStopsReading(t *testing.T) {
	payload := strings.Repeat("x", s3MaxGetBodyBytes+4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, payload)
	}))
	defer srv.Close()

	result, err := (S3Checker{}).Check(context.Background(), map[string]any{
		"endpoint":     srv.URL,
		"bucket":       "big-bucket",
		"object_key":   "blob",
		"health_check": "get_object",
		"access_key":   "AKIA",
		"secret_key":   "secret",
		"path_style":   true,
		"timeout":      5.0,
	})
	if err != nil {
		t.Fatalf("Check() unexpected error: %v", err)
	}
	if result.Status != domain.StatusUp {
		t.Fatalf("status = %s, want UP (%s)", result.Status, result.Message)
	}
}

func TestSignS3Request_OfficialGETVector(t *testing.T) {
	// AWS SigV4 header-based auth example (GET object + Range).
	// https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html
	req, err := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "examplebucket.s3.amazonaws.com"
	req.Header.Set("Range", "bytes=0-9")

	opts := s3Opts{
		AccessKey: "AKIAIOSFODNN7EXAMPLE",
		SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Region:    "us-east-1",
	}
	now := time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC)
	signS3Request(req, opts, now, emptyPayloadSHA256)

	got := req.Header.Get("Authorization")
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got != want {
		t.Fatalf("Authorization mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestS3URIEncode_PreservesHyphenAndUnderscore(t *testing.T) {
	if got := s3URIEncode("my-bucket_name", true); got != "my-bucket_name" {
		t.Fatalf("s3URIEncode = %q", got)
	}
	if got := s3URIEncode("a b", true); got != "a%20b" {
		t.Fatalf("s3URIEncode space = %q", got)
	}
}

func TestBucketNeedsPathStyle(t *testing.T) {
	if bucketNeedsPathStyle("my-bucket") {
		t.Fatal("hyphen bucket should allow virtual-hosted")
	}
	if !bucketNeedsPathStyle("my_bucket") {
		t.Fatal("underscore bucket must force path-style")
	}
}

func TestS3Checker_Register(t *testing.T) {
	got, ok := Get("s3")
	if !ok {
		t.Fatal("s3 checker was not registered")
	}
	if got.Type() != "s3" {
		t.Fatalf("registered type = %q", got.Type())
	}
}

func cloneConfig(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
