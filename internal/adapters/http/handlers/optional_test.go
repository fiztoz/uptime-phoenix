package handlers

import (
	"encoding/json"
	"testing"
)

func TestOptionalNullableInt64_OmitVsNullVsValue(t *testing.T) {
	type body struct {
		ID optionalNullableInt64 `json:"id"`
	}

	t.Run("omitted", func(t *testing.T) {
		var b body
		if err := json.Unmarshal([]byte(`{}`), &b); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if b.ID.set {
			t.Fatal("omitted key must leave set=false")
		}
		if b.ID.value != nil {
			t.Fatalf("omitted key value = %v; want nil", b.ID.value)
		}
	})

	t.Run("null", func(t *testing.T) {
		var b body
		if err := json.Unmarshal([]byte(`{"id":null}`), &b); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !b.ID.set {
			t.Fatal("explicit null must set set=true")
		}
		if b.ID.value != nil {
			t.Fatalf("explicit null value = %v; want nil", b.ID.value)
		}
	})

	t.Run("number", func(t *testing.T) {
		var b body
		if err := json.Unmarshal([]byte(`{"id":42}`), &b); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if !b.ID.set {
			t.Fatal("number must set set=true")
		}
		if b.ID.value == nil || *b.ID.value != 42 {
			t.Fatalf("number value = %v; want 42", b.ID.value)
		}
	})

	t.Run("invalid", func(t *testing.T) {
		var b body
		if err := json.Unmarshal([]byte(`{"id":"nope"}`), &b); err == nil {
			t.Fatal("non-integer must fail")
		}
	})
}
