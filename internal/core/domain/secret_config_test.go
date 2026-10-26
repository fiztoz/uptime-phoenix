package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestRedactSecretConfig_MasksCredentialsAtAnyDepth(t *testing.T) {
	cfg := map[string]any{
		"url":           "https://user:pa55@example.com/health",
		"method":        "GET",
		"auth_password": "s3cret",
		"push_token":    "tok",
		"empty_secret":  "",
		"password":      "",
		"timeout":       30,
		"headers": map[string]any{
			"Authorization": "Bearer abc",
			"X-Api-Key":     "k",
			"Accept":        "application/json",
		},
		"targets": []any{
			map[string]any{"name": "a", "webhook": "https://hooks.example/x"},
		},
	}
	got := RedactSecretConfig(cfg)

	for _, k := range []string{"auth_password", "push_token"} {
		if got[k] != SecretConfigPlaceholder {
			t.Errorf("%s = %v; want placeholder", k, got[k])
		}
	}
	if got["password"] != "" {
		t.Errorf("empty secret should stay empty so the UI can tell 'unset', got %v", got["password"])
	}
	if got["method"] != "GET" || got["timeout"] != 30 {
		t.Errorf("non-secret values changed: %v", got)
	}
	if u, _ := got["url"].(string); strings.Contains(u, "pa55") || !strings.Contains(u, "user") {
		t.Errorf("url userinfo password not masked: %q", u)
	}
	h := got["headers"].(map[string]any)
	if h["Authorization"] != SecretConfigPlaceholder || h["X-Api-Key"] != SecretConfigPlaceholder {
		t.Errorf("credential headers not masked: %v", h)
	}
	if h["Accept"] != "application/json" {
		t.Errorf("ordinary header masked: %v", h)
	}
	if w := got["targets"].([]any)[0].(map[string]any)["webhook"]; w != SecretConfigPlaceholder {
		t.Errorf("nested webhook not masked: %v", w)
	}
	// The input must not be mutated: it is the stored monitor.
	if cfg["auth_password"] != "s3cret" || cfg["headers"].(map[string]any)["Authorization"] != "Bearer abc" {
		t.Fatalf("RedactSecretConfig mutated its input: %v", cfg)
	}
}

func TestRedactSecretConfig_RevealKeepsNamedKey(t *testing.T) {
	got := RedactSecretConfig(map[string]any{"push_token": "tok", "auth_password": "p"}, "push_token")
	if got["push_token"] != "tok" {
		t.Errorf("revealed key masked: %v", got["push_token"])
	}
	if got["auth_password"] != SecretConfigPlaceholder {
		t.Errorf("unrevealed key leaked: %v", got["auth_password"])
	}
}

func TestRestoreSecretConfig_RoundTrip(t *testing.T) {
	stored := map[string]any{
		"url":           "https://user:pa55@example.com/health",
		"auth_password": "s3cret",
		"headers":       map[string]any{"Authorization": "Bearer abc"},
		"targets":       []any{map[string]any{"webhook": "https://hooks.example/x"}},
	}
	masked := RedactSecretConfig(stored)
	if !ConfigHasSecretPlaceholder(masked) {
		t.Fatal("masked config should carry placeholders")
	}
	restored := RestoreSecretConfig(stored, masked)
	if !reflect.DeepEqual(restored, stored) {
		t.Fatalf("round trip lost data:\n got %#v\nwant %#v", restored, stored)
	}
	if ConfigHasSecretPlaceholder(restored) {
		t.Fatal("restored config still carries a placeholder")
	}
}

func TestRestoreSecretConfig_NewValueWinsAndOrphanPlaceholderDropped(t *testing.T) {
	stored := map[string]any{"auth_password": "old"}
	incoming := map[string]any{"auth_password": "new", "api_key": SecretConfigPlaceholder}
	got := RestoreSecretConfig(stored, incoming)
	if got["auth_password"] != "new" {
		t.Errorf("new secret not applied: %v", got["auth_password"])
	}
	if _, ok := got["api_key"]; ok {
		t.Errorf("placeholder with no stored value must be dropped, got %v", got["api_key"])
	}
}

func TestRedactURLPassword(t *testing.T) {
	cases := map[string]string{
		"https://example.com/x":         "https://example.com/x",
		"redis://:pw@cache:6379/0":      "redis://:" + SecretConfigPlaceholder + "@cache:6379/0",
		"not a url":                     "not a url",
		"https://user@example.com/path": "https://user@example.com/path",
	}
	for in, want := range cases {
		got := RedactURLPassword(in)
		if strings.Contains(got, "pw@") || (want != in && !strings.Contains(got, "cache:6379")) {
			t.Errorf("RedactURLPassword(%q) = %q", in, got)
		}
		if want == in && got != in {
			t.Errorf("RedactURLPassword(%q) changed a password-less value to %q", in, got)
		}
	}
}

func TestRedactNotificationConfig_MasksDeliveryURL(t *testing.T) {
	stored := map[string]any{"url": "https://hooks.example/T0K3N", "method": "POST", "bot_token": "b"}
	got := RedactNotificationConfig(stored)
	if got["url"] != SecretConfigPlaceholder || got["bot_token"] != SecretConfigPlaceholder {
		t.Fatalf("notification credentials not masked: %v", got)
	}
	if got["method"] != "POST" {
		t.Fatalf("ordinary value masked: %v", got)
	}
	if back := RestoreSecretConfig(stored, got); !reflect.DeepEqual(back, stored) {
		t.Fatalf("round trip = %v; want %v", back, stored)
	}
}
