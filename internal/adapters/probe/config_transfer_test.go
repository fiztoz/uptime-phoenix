package probe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestConfigTransferOriginalBytesOutOfOrderAndRetries(t *testing.T) {
	for _, fixture := range []string{"config-snapshot-empty.json", "config-snapshot-complete.json", "config-snapshot-max-versions.json"} {
		t.Run(fixture, func(t *testing.T) {
			content := readFixture(t, "valid", fixture)
			begin, chunks, commit := configTestFrames(t, content)
			now := time.Now()
			transfer := mustConfigTransfer(t, begin, now, configTestTarget())
			for i := len(chunks) - 1; i >= 0; i-- {
				for retry := 0; retry < 2; retry++ {
					if err := transfer.AddChunk(chunks[i], now.Add(time.Second)); err != nil {
						t.Fatal(err)
					}
				}
			}
			if transfer.totalBytes != len(content) || transfer.received != len(chunks) {
				t.Fatal("retries inflated staging")
			}
			snapshot, err := transfer.Commit(commit, now.Add(2*time.Second))
			if err != nil {
				t.Fatal(err)
			}
			want, err := DecodeConfigSnapshot(content)
			if err != nil || !reflect.DeepEqual(snapshot, want) {
				t.Fatal("assembled configuration changed")
			}
			assertConfigDiscarded(t, transfer, chunks[0], commit, now)
		})
	}
}

func TestConfigTransferChunkFailuresDiscardStaging(t *testing.T) {
	begin, chunks, commit := configTestFrames(t, readFixture(t, "valid", "config-snapshot-empty.json"))
	now := time.Now()
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"snapshot", func(f map[string]any) {
			f["payload"].(map[string]any)["snapshot_id"] = "1ac3ab1e-655a-4f36-b542-08e492af1111"
		}},
		{"revision", func(f map[string]any) { f["payload"].(map[string]any)["revision"] = "13" }},
		{"generation", func(f map[string]any) { f["connection_generation"] = "8" }},
		{"index", func(f map[string]any) { f["payload"].(map[string]any)["index"] = 3 }},
		{"conflicting duplicate", func(f map[string]any) { f["payload"].(map[string]any)["data_base64"] = "eA==" }},
		{"wrong type", func(f map[string]any) { f["type"] = "state.chunk" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			transfer := mustConfigTransfer(t, begin, now, configTestTarget())
			if err := transfer.AddChunk(chunks[0], now); err != nil {
				t.Fatal(err)
			}
			if err := transfer.AddChunk(mutateJSON(t, chunks[0], test.change), now); err == nil {
				t.Fatal("invalid chunk accepted")
			}
			assertConfigDiscarded(t, transfer, chunks[0], commit, now)
		})
	}
	transfer := mustConfigTransfer(t, begin, now, configTestTarget())
	if err := transfer.AddChunk([]byte(`{`), now); err == nil {
		t.Fatal("malformed chunk accepted")
	}
	assertConfigDiscarded(t, transfer, chunks[0], commit, now)
}

func TestConfigTransferCommitFailuresNeverReturnSnapshot(t *testing.T) {
	content := readFixture(t, "valid", "config-snapshot-complete.json")
	begin, chunks, commit := configTestFrames(t, content)
	now := time.Now()
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"snapshot", func(f map[string]any) {
			f["payload"].(map[string]any)["snapshot_id"] = "1ac3ab1e-655a-4f36-b542-08e492af1111"
		}},
		{"revision", func(f map[string]any) { f["payload"].(map[string]any)["revision"] = "13" }},
		{"generation", func(f map[string]any) { f["connection_generation"] = "8" }},
		{"hash", func(f map[string]any) { f["payload"].(map[string]any)["sha256"] = strings.Repeat("0", 64) }},
		{"wrong type", func(f map[string]any) { f["type"] = "config.applied" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			transfer := filledConfigTransfer(t, begin, chunks, now, configTestTarget())
			assertConfigCommitFails(t, transfer, mutateJSON(t, commit, test.change), now)
			assertConfigDiscarded(t, transfer, chunks[0], commit, now)
		})
	}
	for _, test := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"effective time", func(p map[string]any) { p["effective_at"] = "2020-01-01T00:00:00Z" }},
		{"wrong hash", func(p map[string]any) { p["sha256"] = strings.Repeat("0", 64) }},
		{"too many declared bytes", func(p map[string]any) { p["total_bytes"] = len(content) + 1 }},
		{"too few declared bytes", func(p map[string]any) { p["total_bytes"] = len(content) - 1 }},
		{"omitted capability", func(p map[string]any) { p["required_capabilities"] = []string{"snapshot.v1"} }},
		{"extra capability", func(p map[string]any) {
			p["required_capabilities"] = append(p["required_capabilities"].([]any), "checker.tcp.v1")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			changed := mutateJSON(t, begin, func(f map[string]any) { test.change(f["payload"].(map[string]any)) })
			transfer := mustConfigTransfer(t, changed, now, configTestTarget())
			var chunkErr error
			for _, chunk := range chunks {
				if chunkErr = transfer.AddChunk(chunk, now); chunkErr != nil {
					break
				}
			}
			// Match the declared hash so corruption reaches the original-byte check.
			attemptCommit := commit
			if test.name == "wrong hash" {
				attemptCommit = changePayload(t, commit, map[string]any{"sha256": strings.Repeat("0", 64)})
			}
			assertConfigCommitFails(t, transfer, attemptCommit, now)
		})
	}
	transfer := mustConfigTransfer(t, begin, now, configTestTarget())
	if err := transfer.AddChunk(chunks[0], now); err != nil {
		t.Fatal(err)
	}
	assertConfigCommitFails(t, transfer, commit, now)
}

func TestConfigTransferRejectsWrongTargetAndLocalResource(t *testing.T) {
	begin, chunks, commit := configTestFrames(t, readFixture(t, "valid", "config-snapshot-complete.json"))
	now := time.Now()
	for _, test := range []struct {
		name       string
		change     func(*ConfigTarget)
		beginFails bool
	}{
		{"other hub", func(target *ConfigTarget) { target.HubID = "a1123604-32c5-40aa-89f9-b62f93dceac2" }, false},
		{"other probe", func(target *ConfigTarget) { target.ProbeID = "a645246b-b176-4422-8ae5-b79629ee6a29" }, false},
		{"stale lease", func(target *ConfigTarget) { target.ConnectionGeneration = 8 }, true},
		{"missing capability", func(target *ConfigTarget) { target.Capabilities = []string{"snapshot.v1"} }, true},
		{"unknown required capability", func(target *ConfigTarget) { target.Capabilities = []string{"snapshot.v1", "checker.http.v2"} }, true},
		{"missing binding", func(target *ConfigTarget) { target.ResourceBindings = nil }, false},
		{"wrong binding kind", func(target *ConfigTarget) { target.ResourceBindings[0].Kind = "docker_api" }, false},
		{"duplicate binding", func(target *ConfigTarget) {
			target.ResourceBindings = append(target.ResourceBindings, target.ResourceBindings[0])
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			target := configTestTarget()
			test.change(&target)
			transfer, err := NewConfigTransfer(begin, now, target)
			if test.beginFails {
				if err == nil || transfer != nil {
					t.Fatal("invalid begin/target accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, chunk := range chunks {
				if err := transfer.AddChunk(chunk, now); err != nil {
					t.Fatal(err)
				}
			}
			assertConfigCommitFails(t, transfer, commit, now)
		})
	}
	target := configTestTarget()
	transfer := filledConfigTransfer(t, begin, chunks, now, target)
	target.ResourceBindings[0].Kind = "docker_api"
	target.Capabilities[0] = "unknown.v1"
	if _, err := transfer.Commit(commit, now); err != nil {
		t.Fatalf("caller mutation changed frozen target: %v", err)
	}
}

func TestConfigTransferRejectsInvalidReconstructedSchema(t *testing.T) {
	valid := readFixture(t, "valid", "config-snapshot-empty.json")
	for _, content := range [][]byte{
		[]byte(strings.Replace(string(valid), `"schema_version": 1`, `"schema_version": 2`, 1)),
		[]byte(strings.Replace(string(valid), `"schema_version": 1`, `"schema_version": 1,"schema_version":1`, 1)),
		readFixture(t, "invalid", "config-snapshot-missing-channel.json"),
	} {
		begin, chunks, commit := configTestFrames(t, content)
		now := time.Now()
		transfer := filledConfigTransfer(t, begin, chunks, now, configTestTarget())
		assertConfigCommitFails(t, transfer, commit, now)
	}
}

func TestConfigTransferFixedDeadlineAndCancellation(t *testing.T) {
	begin, chunks, commit := configTestFrames(t, readFixture(t, "valid", "config-snapshot-empty.json"))
	now := time.Now()
	transfer := filledConfigTransfer(t, begin, chunks, now, configTestTarget())
	if err := transfer.AddChunk(chunks[0], now.Add(ConfigTransferTimeout-time.Nanosecond)); err != nil {
		t.Fatal(err)
	}
	assertConfigCommitFails(t, transfer, commit, now.Add(ConfigTransferTimeout))
	transfer = mustConfigTransfer(t, begin, now, configTestTarget())
	if err := transfer.AddChunk(chunks[0], now.Add(ConfigTransferTimeout)); err == nil {
		t.Fatal("expired chunk accepted")
	}
	assertConfigDiscarded(t, transfer, chunks[0], commit, now)
	transfer = filledConfigTransfer(t, begin, chunks, now, configTestTarget())
	transfer.Discard()
	transfer.Discard()
	assertConfigDiscarded(t, transfer, chunks[0], commit, now)
}

func TestConfigRevisionComparison(t *testing.T) {
	hash, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, test := range []struct {
		active     Decimal
		activeHash string
		revision   Decimal
		hash       string
		retry      bool
		conflict   bool
		valid      bool
	}{
		{0, "", 1, hash, false, false, true}, {12, hash, 13, other, false, false, true}, {12, hash, 12, hash, true, false, true},
		{12, hash, 12, other, false, true, false}, {12, hash, 11, hash, false, true, false}, {math.MaxInt64, hash, math.MaxInt64, hash, true, false, true},
		{0, hash, 1, hash, false, false, false}, {1, "", 2, hash, false, false, false}, {-1, "", 1, hash, false, false, false}, {0, "", 0, hash, false, false, false}, {0, "", 1, "", false, false, false},
	} {
		retry, err := CompareConfigRevision(test.active, test.activeHash, test.revision, test.hash)
		if retry != test.retry || (err == nil) != test.valid || errors.Is(err, ErrConfigRevisionConflict) != test.conflict {
			t.Fatalf("revision comparison: retry=%v err=%v", retry, err)
		}
	}
}

func configTestTarget() ConfigTarget {
	return ConfigTarget{HubID: "b1123604-32c5-40aa-89f9-b62f93dceac2", ProbeID: "e645246b-b176-4422-8ae5-b79629ee6a29", ConnectionGeneration: 7,
		Capabilities:     []string{"snapshot.v1", "watchdog.v1", "checker.http.v1", "checker.docker.v1", "checker.tcp.v1", "notifier.webhook.v1", "notifier.discord.v1", "notifier.smtp.v1"},
		ResourceBindings: []ResourceBinding{{BindingKey: "docker-local", Kind: "docker_socket"}}}
}

func configTestFrames(t *testing.T, content []byte) ([]byte, [][]byte, []byte) {
	t.Helper()
	// Decode metadata permissively so malformed full documents can test assembly.
	var snapshot ConfigSnapshot
	if err := json.Unmarshal(content, &snapshot); err != nil {
		t.Fatal(err)
	}
	required := configCapabilities(snapshot)
	capabilities := make([]string, 0, len(required))
	for capability := range required {
		capabilities = append(capabilities, capability)
	}
	sort.Strings(capabilities)
	hash := sha256.Sum256(content)
	identity := ConfigTransferIdentity{SnapshotID: "9ac3ab1e-655a-4f36-b542-08e492af1111", Revision: snapshot.Revision}
	begin := ConfigBegin{ConfigTransferIdentity: identity, ConfigSchemaVersion: 1, TotalBytes: len(content), ChunkCount: 2, SHA256: hex.EncodeToString(hash[:]), RequiredCapabilities: capabilities, EffectiveAt: snapshot.EffectiveAt}
	chunks := [][]byte{configTestFrame(t, "config.chunk", ConfigChunk{ConfigTransferIdentity: identity, Index: 0, Data: content[:len(content)/2]}), configTestFrame(t, "config.chunk", ConfigChunk{ConfigTransferIdentity: identity, Index: 1, Data: content[len(content)/2:]})}
	return configTestFrame(t, "config.begin", begin), chunks, configTestFrame(t, "config.commit", ConfigCommit{ConfigTransferIdentity: identity, SHA256: begin.SHA256})
}

func configTestFrame(t *testing.T, kind string, payload any) []byte {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	frame, err := json.Marshal(Envelope{ProtocolVersion: 1, Type: kind, MessageID: "1c73a9c5-77ad-4dc0-926d-123bbab94540", SentAt: Timestamp(time.Unix(0, 0).UTC()), ConnectionGeneration: 7, Payload: data})
	if err != nil {
		t.Fatal(err)
	}
	return frame
}

func mustConfigTransfer(t *testing.T, begin []byte, now time.Time, target ConfigTarget) *ConfigTransfer {
	t.Helper()
	transfer, err := NewConfigTransfer(begin, now, target)
	if err != nil {
		t.Fatal(err)
	}
	return transfer
}

func filledConfigTransfer(t *testing.T, begin []byte, chunks [][]byte, now time.Time, target ConfigTarget) *ConfigTransfer {
	t.Helper()
	transfer := mustConfigTransfer(t, begin, now, target)
	for _, chunk := range chunks {
		if err := transfer.AddChunk(chunk, now); err != nil {
			t.Fatal(err)
		}
	}
	return transfer
}

func assertConfigCommitFails(t *testing.T, transfer *ConfigTransfer, commit []byte, now time.Time) {
	t.Helper()
	snapshot, err := transfer.Commit(commit, now)
	if err == nil || !reflect.DeepEqual(snapshot, ConfigSnapshot{}) {
		t.Fatal("failed commit returned usable config")
	}
}

func assertConfigDiscarded(t *testing.T, transfer *ConfigTransfer, chunk, commit []byte, now time.Time) {
	t.Helper()
	if !transfer.closed || transfer.chunks != nil || transfer.bindings != nil || transfer.totalBytes != 0 || transfer.received != 0 {
		t.Fatal("staging not discarded")
	}
	if err := transfer.AddChunk(chunk, now); err == nil {
		t.Fatal("closed transfer accepted chunk")
	}
	assertConfigCommitFails(t, transfer, commit, now)
}
