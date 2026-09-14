package probe

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStateTransferGoldenOutOfOrderAndRetries(t *testing.T) {
	now := time.Now()
	transfer, err := NewStateTransfer(readFixture(t, "valid", "state-begin-empty.json"), now)
	if err != nil {
		t.Fatal(err)
	}
	for _, index := range []string{"1", "0", "1"} {
		if err := transfer.AddChunk(readFixture(t, "valid", "state-chunk-empty-"+index+".json"), now.Add(59*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	commit := readFixture(t, "valid", "state-commit-empty.json")
	snapshot, err := transfer.Commit(commit, now.Add(StateTransferTimeout-time.Nanosecond))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.States == nil || len(snapshot.States) != 0 || snapshot.LastCreatedSeq != 0 || snapshot.ConfigRevision != 12 {
		t.Fatalf("empty snapshot evidence changed: %+v", snapshot)
	}
	if transfer.chunks != nil || transfer.totalBytes != 0 || !transfer.closed {
		t.Fatal("successful commit retained staging memory")
	}
	if _, err := transfer.Commit(commit, now.Add(StateTransferTimeout)); err == nil {
		t.Fatal("consumed staging transfer falsely acted as a durable application receipt")
	}
}

func TestStateTransferReconstructsSourceEvidence(t *testing.T) {
	for _, name := range []string{"first-warning", "first-recovery", "max-sequence", "stale"} {
		t.Run(name, func(t *testing.T) {
			original := readFixture(t, "valid", "state-snapshot-"+name+".json")
			begin, chunks, commit := stateTestFrames(t, original)
			now := time.Now()
			transfer := mustStateTransfer(t, begin, now)
			for _, chunk := range chunks {
				if err := transfer.AddChunk(chunk, now); err != nil {
					t.Fatal(err)
				}
			}
			snapshot, err := transfer.Commit(commit, now)
			if err != nil {
				t.Fatal(err)
			}
			expected, err := DecodeStateSnapshot(original)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(marshalStateTest(t, snapshot), marshalStateTest(t, expected)) {
				t.Fatal("chunk reconstruction changed source evidence")
			}
		})
	}
}

func TestStateTransferChunkFailuresDiscardStaging(t *testing.T) {
	now := time.Now()
	begin, chunks, commit := stateTestFrames(t, readFixture(t, "valid", "state-snapshot-first-warning.json"))
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		at     time.Time
	}{
		{"different snapshot", func(v map[string]any) {
			v["payload"].(map[string]any)["snapshot_id"] = "7c73a9c5-77ad-4dc0-926d-123bbab94540"
		}, now},
		{"different stream", func(v map[string]any) {
			v["payload"].(map[string]any)["stream_id"] = "7c73a9c5-77ad-4dc0-926d-123bbab94540"
		}, now},
		{"different config", func(v map[string]any) { v["payload"].(map[string]any)["config_revision"] = "13" }, now},
		{"different connection", func(v map[string]any) { v["connection_generation"] = "8" }, now},
		{"undeclared index", func(v map[string]any) { v["payload"].(map[string]any)["index"] = 2 }, now},
		{"conflicting duplicate", func(v map[string]any) {
			p := v["payload"].(map[string]any)
			p["index"], p["data_base64"] = 0, base64.StdEncoding.EncodeToString([]byte("different bytes"))
		}, now},
		{"declared total exceeded", func(v map[string]any) {
			v["payload"].(map[string]any)["data_base64"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), MaxStateChunkBytes))
		}, now},
		{"malformed chunk", func(v map[string]any) { v["payload"].(map[string]any)["data_base64"] = false }, now},
		{"expired at deadline", func(map[string]any) {}, now.Add(StateTransferTimeout)},
	} {
		t.Run(test.name, func(t *testing.T) {
			transfer := mustStateTransfer(t, begin, now)
			if err := transfer.AddChunk(chunks[0], now); err != nil {
				t.Fatal(err)
			}
			bad := mutateJSON(t, chunks[1], test.mutate)
			if err := transfer.AddChunk(bad, test.at); err == nil {
				t.Fatal("invalid chunk accepted")
			}
			assertStateTransferDiscarded(t, transfer, chunks[1], commit, now)
		})
	}
}

func TestStateTransferCommitFailuresNeverReturnPartialState(t *testing.T) {
	now := time.Now()
	begin, chunks, commit := stateTestFrames(t, readFixture(t, "valid", "state-snapshot-first-warning.json"))
	for _, test := range []struct {
		name     string
		mutate   func(map[string]any)
		complete bool
		at       time.Time
	}{
		{"incomplete", func(map[string]any) {}, false, now},
		{"hash differs from begin", func(v map[string]any) { v["payload"].(map[string]any)["sha256"] = strings.Repeat("0", 64) }, true, now},
		{"different snapshot", func(v map[string]any) {
			v["payload"].(map[string]any)["snapshot_id"] = "7c73a9c5-77ad-4dc0-926d-123bbab94540"
		}, true, now},
		{"different stream", func(v map[string]any) {
			v["payload"].(map[string]any)["stream_id"] = "7c73a9c5-77ad-4dc0-926d-123bbab94540"
		}, true, now},
		{"different revision", func(v map[string]any) { v["payload"].(map[string]any)["config_revision"] = "13" }, true, now},
		{"different connection", func(v map[string]any) { v["connection_generation"] = "8" }, true, now},
		{"malformed commit", func(v map[string]any) { delete(v["payload"].(map[string]any), "sha256") }, true, now},
		{"expired", func(map[string]any) {}, true, now.Add(StateTransferTimeout)},
	} {
		t.Run(test.name, func(t *testing.T) {
			transfer := mustStateTransfer(t, begin, now)
			for i, chunk := range chunks {
				if i > 0 && !test.complete {
					break
				}
				if err := transfer.AddChunk(chunk, now); err != nil {
					t.Fatal(err)
				}
			}
			bad := mutateJSON(t, commit, test.mutate)
			snapshot, err := transfer.Commit(bad, test.at)
			if err == nil || snapshot.States != nil {
				t.Fatalf("invalid commit returned partial state: %v", err)
			}
			assertStateTransferDiscarded(t, transfer, chunks[1], commit, now)
		})
	}
}

func TestStateTransferChecksHashSchemaAndBeginMetadata(t *testing.T) {
	original := readFixture(t, "valid", "state-snapshot-first-warning.json")
	for _, name := range []string{"bytes changed", "size understated", "size overstated", "stream_id", "config_revision", "last_created_seq", "created_at", "invalid schema", "duplicate key"} {
		t.Run(name, func(t *testing.T) {
			content := original
			if name == "invalid schema" {
				content = mutateJSON(t, content, func(v map[string]any) { firstSnapshotState(v)["status"] = "UNKNOWN" })
			}
			if name == "duplicate key" {
				content = []byte(strings.Replace(string(content), `"monitor_id": 42`, `"monitor_id": 42, "monitor_id": 42`, 1))
			}
			begin, chunks, commit := stateTestFrames(t, content)
			switch name {
			case "bytes changed":
				chunks[1] = mutateJSON(t, chunks[1], func(v map[string]any) {
					p := v["payload"].(map[string]any)
					decoded, err := base64.StdEncoding.DecodeString(p["data_base64"].(string))
					if err != nil {
						t.Fatal(err)
					}
					decoded[len(decoded)-1] = ' '
					p["data_base64"] = base64.StdEncoding.EncodeToString(decoded)
				})
			case "size understated", "size overstated":
				begin = mutateJSON(t, begin, func(v map[string]any) {
					size := len(content) + 1
					if name == "size understated" {
						size = len(content) - 1
					}
					v["payload"].(map[string]any)["total_bytes"] = size
				})
			case "stream_id", "config_revision", "last_created_seq", "created_at":
				value := map[string]string{"stream_id": "7c73a9c5-77ad-4dc0-926d-123bbab94540", "config_revision": "13", "last_created_seq": "102", "created_at": "2026-09-14T09:01:00Z"}[name]
				for _, frame := range []*[]byte{&begin, &chunks[0], &chunks[1], &commit} {
					*frame = mutateJSON(t, *frame, func(v map[string]any) { v["payload"].(map[string]any)[name] = value })
				}
			}
			now := time.Now()
			transfer := mustStateTransfer(t, begin, now)
			var chunkError error
			for _, chunk := range chunks {
				if chunkError = transfer.AddChunk(chunk, now); chunkError != nil {
					break
				}
			}
			if name != "size understated" && chunkError != nil {
				t.Fatalf("failed before intended commit check: %v", chunkError)
			}
			if name == "size understated" && chunkError == nil {
				t.Fatal("accepted more bytes than declared")
			}
			if snapshot, err := transfer.Commit(commit, now); err == nil || snapshot.States != nil {
				t.Fatalf("invalid content returned usable state: %v", err)
			}
			assertStateTransferDiscarded(t, transfer, chunks[1], commit, now)
		})
	}
}

func TestStateTransferCancellationAndFixedDeadline(t *testing.T) {
	begin, chunks, commit := stateTestFrames(t, readFixture(t, "valid", "state-snapshot-empty.json"))
	now := time.Now()
	transfer := mustStateTransfer(t, begin, now)
	if err := transfer.AddChunk(chunks[0], now); err != nil {
		t.Fatal(err)
	}
	if err := transfer.AddChunk(chunks[0], now.Add(59*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := transfer.AddChunk(chunks[1], now.Add(StateTransferTimeout)); err == nil {
		t.Fatal("duplicate chunk extended the transfer deadline")
	}
	assertStateTransferDiscarded(t, transfer, chunks[0], commit, now)
	fresh := mustStateTransfer(t, begin, now)
	fresh.Discard()
	fresh.Discard()
	assertStateTransferDiscarded(t, fresh, chunks[0], commit, now)
	var zero StateTransfer
	assertStateTransferDiscarded(t, &zero, chunks[0], commit, now)
}

func TestStateFrameRequiredFieldsAndWireRoundTrip(t *testing.T) {
	for _, name := range []string{"state-begin-empty.json", "state-chunk-empty-0.json", "state-commit-empty.json", "state-applied-empty.json"} {
		t.Run(name, func(t *testing.T) {
			fixture := readFixture(t, "valid", name)
			envelope, payload, err := decodeStateTestFrame(fixture)
			if err != nil {
				t.Fatal(err)
			}
			envelope.Payload = marshalStateTest(t, payload)
			if _, _, err := decodeStateTestFrame(marshalStateTest(t, envelope)); err != nil {
				t.Fatalf("wire roundtrip failed: %v", err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(envelope.Payload, &fields); err != nil {
				t.Fatal(err)
			}
			for field := range fields {
				for _, absent := range []bool{true, false} {
					bad := mutateJSON(t, fixture, func(v map[string]any) {
						p := v["payload"].(map[string]any)
						if absent {
							delete(p, field)
						} else {
							p[field] = nil
						}
					})
					if _, _, err := decodeStateTestFrame(bad); err == nil {
						t.Fatalf("accepted absent=%v field=%s", absent, field)
					}
				}
			}
		})
	}
	wrong := readFixture(t, "valid", "batch-up.json")
	for _, decode := range []func([]byte) error{
		func(v []byte) error { _, _, err := DecodeStateBegin(v); return err },
		func(v []byte) error { _, _, err := DecodeStateChunk(v); return err },
		func(v []byte) error { _, _, err := DecodeStateCommit(v); return err },
		func(v []byte) error { _, _, err := DecodeStateApplied(v); return err },
	} {
		if err := decode(wrong); !errors.Is(err, ErrUnsupportedPayload) {
			t.Fatalf("wrong typed decoder did not fail explicitly: %v", err)
		}
	}
}

func TestStateChunkSizeBound(t *testing.T) {
	fixture := readFixture(t, "valid", "state-chunk-empty-0.json")
	for _, size := range []int{1, MaxStateChunkBytes, MaxStateChunkBytes + 1} {
		data := mutateJSON(t, fixture, func(v map[string]any) {
			v["payload"].(map[string]any)["data_base64"] = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), size))
		})
		_, chunk, err := DecodeStateChunk(data)
		if size <= MaxStateChunkBytes && (err != nil || len(chunk.Data) != size) || size > MaxStateChunkBytes && err == nil {
			t.Fatalf("chunk byte bound %d: %v", size, err)
		}
	}
}

func TestStateBeginAllocationBounds(t *testing.T) {
	fixture := readFixture(t, "valid", "state-begin-empty.json")
	for _, test := range []struct {
		bytes, chunks int
		valid         bool
	}{
		{1, 1, true}, {MaxStateSnapshotBytes, 64, true}, {MaxStateSnapshotBytes, MaxStateChunks, true},
		{MaxStateSnapshotBytes + 1, 65, false}, {MaxStateSnapshotBytes, 63, false},
		{0, 1, false}, {1, 0, false}, {2, 3, false}, {1025, MaxStateChunks + 1, false},
		{-1, 1, false}, {1, -1, false},
	} {
		data := mutateJSON(t, fixture, func(v map[string]any) {
			p := v["payload"].(map[string]any)
			p["total_bytes"], p["chunk_count"] = test.bytes, test.chunks
		})
		transfer, err := NewStateTransfer(data, time.Now())
		if (err == nil) != test.valid {
			t.Fatalf("bytes=%d chunks=%d: %v", test.bytes, test.chunks, err)
		}
		if err == nil {
			if len(transfer.chunks) != test.chunks || transfer.totalBytes != 0 {
				t.Fatal("begin allocated bytes before receiving chunks")
			}
			transfer.Discard()
		}
	}
}

func stateTestFrames(t *testing.T, content []byte) ([]byte, [][]byte, []byte) {
	t.Helper()
	var metadata StateSnapshot
	if err := json.Unmarshal(content, &metadata); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(content)
	identity := StateTransferIdentity{SnapshotID: "1c73a9c5-77ad-4dc0-926d-123bbab94540", StreamID: metadata.StreamID, ConfigRevision: metadata.ConfigRevision}
	begin := StateBegin{StateTransferIdentity: identity, StateSchemaVersion: 1, CreatedAt: metadata.CreatedAt, LastCreatedSeq: metadata.LastCreatedSeq, TotalBytes: len(content), ChunkCount: 2, SHA256: hex.EncodeToString(hash[:])}
	frame := func(kind string, payload any) []byte {
		return marshalStateTest(t, Envelope{ProtocolVersion: 1, Type: kind, MessageID: "637c39e5-0d90-41db-82c8-4a8bc65f16e5", SentAt: metadata.CreatedAt, ConnectionGeneration: 7, Payload: marshalStateTest(t, payload)})
	}
	chunks := [][]byte{
		frame("state.chunk", StateChunk{StateTransferIdentity: identity, Index: 0, Data: content[:len(content)/2]}),
		frame("state.chunk", StateChunk{StateTransferIdentity: identity, Index: 1, Data: content[len(content)/2:]}),
	}
	return frame("state.begin", begin), chunks, frame("state.commit", StateCommit{StateTransferIdentity: identity, SHA256: begin.SHA256})
}

func mustStateTransfer(t *testing.T, begin []byte, now time.Time) *StateTransfer {
	t.Helper()
	transfer, err := NewStateTransfer(begin, now)
	if err != nil {
		t.Fatal(err)
	}
	return transfer
}

func assertStateTransferDiscarded(t *testing.T, transfer *StateTransfer, chunk, commit []byte, now time.Time) {
	t.Helper()
	if transfer.chunks != nil || transfer.totalBytes != 0 {
		t.Fatal("failed/canceled transfer retained staged bytes")
	}
	if err := transfer.AddChunk(chunk, now); err == nil {
		t.Fatal("aborted transfer accepted another chunk")
	}
	if snapshot, err := transfer.Commit(commit, now); err == nil || snapshot.States != nil {
		t.Fatal("aborted transfer returned usable state")
	}
}

func marshalStateTest(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeStateTestFrame(data []byte) (Envelope, any, error) {
	envelope, err := DecodeEnvelope(data)
	if err != nil {
		return envelope, nil, err
	}
	switch envelope.Type {
	case "state.begin":
		return DecodeStateBegin(data)
	case "state.chunk":
		return DecodeStateChunk(data)
	case "state.commit":
		return DecodeStateCommit(data)
	case "state.applied":
		return DecodeStateApplied(data)
	default:
		return envelope, nil, ErrUnsupportedPayload
	}
}
