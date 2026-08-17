package domain

import "testing"

func TestMonitorTarget_S3OmitsCredentials(t *testing.T) {
	m := &Monitor{
		Type: "s3",
		Config: map[string]any{
			"endpoint":   "https://s3.us-east-1.amazonaws.com",
			"bucket":     "my-backup_bucket",
			"access_key": "AKIAEXAMPLE",
			"secret_key": "wJalrXUtnFEMI",
		},
	}
	got := m.Target()
	if got != "https://s3.us-east-1.amazonaws.com/my-backup_bucket" {
		t.Fatalf("Target() = %q", got)
	}
	if got != S3DisplayTarget(m.Config) {
		t.Fatalf("Target() != S3DisplayTarget")
	}
}

func TestS3DisplayTarget_BucketOnly(t *testing.T) {
	got := S3DisplayTarget(map[string]any{"bucket": "my-bucket"})
	if got != "my-bucket" {
		t.Fatalf("S3DisplayTarget = %q", got)
	}
}
