package probe

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestConfigFramesRequireFieldsAndRoundTrip(t *testing.T) {
	for _, kind := range []string{"begin", "chunk", "commit", "applied", "rejected"} {
		t.Run(kind, func(t *testing.T) {
			fixture := readFixture(t, "valid", "config-"+kind+"-empty.json")
			var frame map[string]any
			if err := json.Unmarshal(fixture, &frame); err != nil {
				t.Fatal(err)
			}
			for key := range frame["payload"].(map[string]any) {
				for _, absent := range []bool{true, false} {
					changed := mutateJSON(t, fixture, func(f map[string]any) {
						payload := f["payload"].(map[string]any)
						if absent {
							delete(payload, key)
						} else {
							payload[key] = nil
						}
					})
					if _, _, err := decodeConfigTestFrame(kind, changed); err == nil {
						t.Fatalf("accepted absent/null %s", key)
					}
				}
			}
			envelope, payload, err := decodeConfigTestFrame(kind, changePayload(t, fixture, map[string]any{"future_optional": true}))
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "future_optional") {
				t.Fatal("unknown field survived typed decoder")
			}
			envelope.Payload = encoded
			encoded, err = json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := decodeConfigTestFrame(kind, encoded); err != nil {
				t.Fatalf("round trip: %v", err)
			}
			oversize := mutateJSON(t, fixture, func(f map[string]any) { f["future"] = strings.Repeat("x", MaxFrameBytes) })
			if _, _, err := decodeConfigTestFrame(kind, oversize); err == nil {
				t.Fatal("oversize frame accepted")
			}
			wrong := mutateJSON(t, fixture, func(f map[string]any) { f["type"] = "health" })
			if _, _, err := decodeConfigTestFrame(kind, wrong); !errors.Is(err, ErrUnsupportedPayload) {
				t.Fatalf("wrong decoder: %v", err)
			}
		})
	}
	fixture := readFixture(t, "valid", "config-rejected-empty.json")
	for _, key := range []string{"path", "code", "message"} {
		for _, absent := range []bool{true, false} {
			frame := mutateJSON(t, fixture, func(f map[string]any) {
				detail := f["payload"].(map[string]any)["errors"].([]any)[0].(map[string]any)
				if absent {
					delete(detail, key)
				} else {
					detail[key] = nil
				}
			})
			if _, _, err := DecodeConfigRejected(frame); err == nil {
				t.Fatalf("absent/null error field %s", key)
			}
		}
	}
}

func TestConfigFrameByteCountAndDiagnosticBounds(t *testing.T) {
	begin := readFixture(t, "valid", "config-begin-empty.json")
	for _, test := range []struct {
		bytes  int
		chunks int
		valid  bool
	}{
		{MaxConfigSnapshotBytes, 64, true}, {MaxConfigSnapshotBytes, MaxConfigChunks, true},
		{MaxConfigSnapshotBytes + 1, 64, false}, {MaxConfigSnapshotBytes, 63, false}, {1, 2, false}, {0, 1, false}, {1, 0, false}, {MaxConfigSnapshotBytes, MaxConfigChunks + 1, false},
	} {
		_, _, err := DecodeConfigBegin(changePayload(t, begin, map[string]any{"total_bytes": test.bytes, "chunk_count": test.chunks}))
		if (err == nil) != test.valid {
			t.Fatalf("bytes=%d chunks=%d: %v", test.bytes, test.chunks, err)
		}
	}
	chunk := readFixture(t, "valid", "config-chunk-empty.json")
	for _, size := range []int{1, MaxConfigChunkBytes, MaxConfigChunkBytes + 1} {
		data := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", size)))
		_, _, err := DecodeConfigChunk(changePayload(t, chunk, map[string]any{"data_base64": data}))
		if (err == nil) != (size <= MaxConfigChunkBytes) {
			t.Fatalf("chunk size %d: %v", size, err)
		}
	}
	rejected := readFixture(t, "valid", "config-rejected-empty.json")
	for _, count := range []int{MaxConfigErrors, MaxConfigErrors + 1} {
		details := make([]ConfigError, count)
		for i := range details {
			details[i] = ConfigError{Path: "/assignments/0", Code: "invalid_config", Message: ""}
		}
		_, _, err := DecodeConfigRejected(changePayload(t, rejected, map[string]any{"errors": details}))
		if (err == nil) != (count == MaxConfigErrors) {
			t.Fatalf("diagnostics count %d: %v", count, err)
		}
	}
	for _, test := range []struct {
		detail ConfigError
		valid  bool
	}{
		{ConfigError{Path: "/" + strings.Repeat("a", 255), Code: strings.Repeat("a", MaxErrorCodeBytes), Message: strings.Repeat("é", MaxMessageBytes/2)}, true},
		{ConfigError{Path: "/a~0b~1c", Code: "invalid_config", Message: ""}, true},
		{ConfigError{Path: "/" + strings.Repeat("a", 256), Code: "invalid_config", Message: ""}, false},
		{ConfigError{Path: "/a~", Code: "invalid_config", Message: ""}, false},
		{ConfigError{Path: "/a\n", Code: "invalid_config", Message: ""}, false},
		{ConfigError{Path: "/a", Code: strings.Repeat("a", MaxErrorCodeBytes+1), Message: ""}, false},
		{ConfigError{Path: "/a", Code: "invalid_config", Message: strings.Repeat("é", MaxMessageBytes/2+1)}, false},
	} {
		_, _, err := DecodeConfigRejected(changePayload(t, rejected, map[string]any{"errors": []ConfigError{test.detail}}))
		if (err == nil) != test.valid {
			t.Fatalf("diagnostic bounds valid=%v: %v", test.valid, err)
		}
	}
}

func decodeConfigTestFrame(kind string, data []byte) (Envelope, any, error) {
	switch kind {
	case "begin":
		return DecodeConfigBegin(data)
	case "chunk":
		return DecodeConfigChunk(data)
	case "commit":
		return DecodeConfigCommit(data)
	case "applied":
		return DecodeConfigApplied(data)
	default:
		return DecodeConfigRejected(data)
	}
}
