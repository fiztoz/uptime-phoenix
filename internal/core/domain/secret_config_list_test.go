package domain

import (
	"reflect"
	"testing"
)

func TestRedactSecretConfig_HMACSecretAndHeadersString(t *testing.T) {
	t.Parallel()
	cfg := map[string]any{
		"hmac_secret": "k",
		"headers":     `{"Authorization":"Bearer x"}`,
	}
	out := RedactSecretConfig(cfg)
	if out["hmac_secret"] != SecretConfigPlaceholder || out["headers"] != SecretConfigPlaceholder {
		t.Fatalf("redacted = %v; want both masked", out)
	}
	// A headers string with no credential header stays readable.
	plain := RedactSecretConfig(map[string]any{"headers": `{"Accept":"application/json"}`})
	if plain["headers"] != `{"Accept":"application/json"}` {
		t.Fatalf("plain headers string = %v; want unchanged", plain["headers"])
	}
	// Text that is not a JSON object cannot be inspected, so it is masked.
	raw := RedactSecretConfig(map[string]any{"headers": "Authorization: Bearer x"})
	if raw["headers"] != SecretConfigPlaceholder {
		t.Fatalf("non-JSON headers string = %v; want masked", raw["headers"])
	}
	back := RestoreSecretConfig(cfg, out)
	if !reflect.DeepEqual(back, cfg) {
		t.Fatalf("round trip = %v; want %v", back, cfg)
	}
}

// Restoring list entries by index after an insert or delete hands one
// entry's secret to another. Entries are matched by their masked form; the
// index is used only when the list kept its length; otherwise the
// placeholder stays and the caller refuses the update.
func TestRestoreSecretConfig_ListEntries(t *testing.T) {
	t.Parallel()
	stored := map[string]any{"targets": []any{
		map[string]any{"name": "a", "password": "pa"},
		map[string]any{"name": "b", "password": "pb"},
	}}
	masked := RedactSecretConfig(stored)["targets"].([]any)

	// Entry "a" removed: "b" must keep pb, not inherit pa by position.
	got := RestoreSecretConfig(stored, map[string]any{"targets": []any{masked[1]}})
	want := []any{map[string]any{"name": "b", "password": "pb"}}
	if !reflect.DeepEqual(got["targets"], want) {
		t.Fatalf("after delete = %v; want %v", got["targets"], want)
	}

	// New entry inserted in front: the masked entries still find their own.
	newEntry := map[string]any{"name": "c", "password": "pc"}
	got = RestoreSecretConfig(stored, map[string]any{"targets": []any{newEntry, masked[0], masked[1]}})
	want = []any{newEntry, stored["targets"].([]any)[0], stored["targets"].([]any)[1]}
	if !reflect.DeepEqual(got["targets"], want) {
		t.Fatalf("after insert = %v; want %v", got["targets"], want)
	}

	// Same length, ambiguous masked strings: index is the only identity.
	strs := map[string]any{"tokens": []any{"t1", "t2"}}
	got = RestoreSecretConfig(strs, RedactSecretConfig(strs))
	if !reflect.DeepEqual(got, strs) {
		t.Fatalf("same-length strings = %v; want %v", got, strs)
	}

	// Length changed and the placeholder matches several entries: refuse.
	got = RestoreSecretConfig(strs, map[string]any{"tokens": []any{SecretConfigPlaceholder}})
	if !ConfigHasSecretPlaceholder(got) {
		t.Fatalf("ambiguous placeholder restored to %v; want it left for the caller to refuse", got)
	}
}
