package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestJoinURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		base, path, want string
	}{
		{"http://127.0.0.1:3000", "/api/config/export", "http://127.0.0.1:3000/api/config/export"},
		{"http://127.0.0.1:3000/", "/api/config/export", "http://127.0.0.1:3000/api/config/export"},
		{"http://127.0.0.1:3000/", "api/config/export", "http://127.0.0.1:3000/api/config/export"},
		{"https://phoenix.example.com", "/api/config/plan", "https://phoenix.example.com/api/config/plan"},
	}
	for _, tc := range cases {
		got := joinURL(tc.base, tc.path)
		if got != tc.want {
			t.Errorf("joinURL(%q, %q) = %q; want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

func TestSelectAuth(t *testing.T) {
	t.Parallel()

	a, err := selectAuth("jwt-token", "")
	if err != nil {
		t.Fatalf("token only: %v", err)
	}
	if a.value != "Bearer jwt-token" {
		t.Fatalf("token auth = %q; want Bearer jwt-token", a.value)
	}

	a, err = selectAuth("", "phx_secret")
	if err != nil {
		t.Fatalf("api-key only: %v", err)
	}
	if a.value != "ApiKey phx_secret" {
		t.Fatalf("api-key auth = %q; want ApiKey phx_secret", a.value)
	}

	// Token wins when both are set (session path first, matching middleware order).
	a, err = selectAuth("jwt", "phx_key")
	if err != nil {
		t.Fatalf("both: %v", err)
	}
	if a.value != "Bearer jwt" {
		t.Fatalf("both auth = %q; want Bearer jwt", a.value)
	}

	_, err = selectAuth("", "")
	if err == nil {
		t.Fatal("expected error when neither credential is set")
	}
	if !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("error = %v; want authentication required", err)
	}

	// Whitespace-only is treated as absent.
	_, err = selectAuth("  ", "\t")
	if err == nil {
		t.Fatal("expected error for whitespace-only credentials")
	}
}

func TestBuildApplyPayload_WrapsDocumentAndPrune(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	const src = "apiVersion: phoenix.dev/v1\nkind: Config\nspec:\n  tags: []\n"
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := buildApplyPayload(path, true)
	if err != nil {
		t.Fatalf("buildApplyPayload: %v", err)
	}
	var got map[string]any
	if err := yaml.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if got["prune"] != true {
		t.Fatalf("prune = %v; want true", got["prune"])
	}
	doc, ok := got["document"].(map[string]any)
	if !ok {
		t.Fatalf("document type = %T; want map", got["document"])
	}
	if doc["apiVersion"] != "phoenix.dev/v1" {
		t.Fatalf("apiVersion = %v", doc["apiVersion"])
	}
	if doc["kind"] != "Config" {
		t.Fatalf("kind = %v", doc["kind"])
	}
}
