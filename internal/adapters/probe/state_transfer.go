package probe

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

const (
	// MaxStateChunkBytes limits a chunk before base64 encoding.
	MaxStateChunkBytes = 256 << 10
	// MaxStateChunks bounds staging bookkeeping even for tiny chunks.
	MaxStateChunks = 1024
	// StateTransferTimeout is measured from begin, never extended by retries.
	StateTransferTimeout = 60 * time.Second
)

// StateTransferIdentity binds each frame to a snapshot, stream, and active config.
// Authentication and negotiated connection generation are session responsibilities.
type StateTransferIdentity struct {
	SnapshotID     string  `json:"snapshot_id"`
	StreamID       string  `json:"stream_id"`
	ConfigRevision Decimal `json:"config_revision"`
}

// StateBegin declares bounded metadata before any chunks are allocated.
type StateBegin struct {
	StateTransferIdentity
	StateSchemaVersion int       `json:"state_schema_version"`
	CreatedAt          Timestamp `json:"created_at"`
	LastCreatedSeq     Decimal   `json:"last_created_seq"`
	TotalBytes         int       `json:"total_bytes"`
	ChunkCount         int       `json:"chunk_count"`
	SHA256             string    `json:"sha256"`
}

// StateChunk holds decoded bytes. JSON serialization uses standard padded base64.
type StateChunk struct {
	StateTransferIdentity
	Index int    `json:"index"`
	Data  []byte `json:"data_base64"`
}

// StateCommit requests verification of a complete staging transfer.
type StateCommit struct {
	StateTransferIdentity
	SHA256 string `json:"sha256"`
}

// StateApplied describes an application receipt, never a history cursor ACK.
// A sender may produce it only after the projection transaction commits.
type StateApplied struct {
	StateTransferIdentity
	SHA256     string    `json:"sha256"`
	AppliedAt  Timestamp `json:"applied_at"`
	StateCount int       `json:"state_count"`
}

// DecodeStateBegin validates transfer bounds without allocating snapshot storage.
func DecodeStateBegin(data []byte) (Envelope, StateBegin, error) {
	var begin StateBegin
	envelope, fields, err := telemetryFields(data, "state.begin")
	if err != nil {
		return envelope, begin, err
	}
	if err := decodeStateTransferIdentity(fields, &begin.StateTransferIdentity); err != nil {
		return envelope, begin, err
	}
	if err := decodeRequiredFields(fields,
		field{"state_schema_version", &begin.StateSchemaVersion}, field{"created_at", &begin.CreatedAt},
		field{"last_created_seq", &begin.LastCreatedSeq}, field{"total_bytes", &begin.TotalBytes},
		field{"chunk_count", &begin.ChunkCount}, field{"sha256", &begin.SHA256},
	); err != nil {
		return envelope, begin, err
	}
	if begin.StateSchemaVersion != 1 {
		return envelope, begin, errors.New("unsupported state_schema_version")
	}
	if begin.TotalBytes <= 0 || begin.TotalBytes > MaxStateSnapshotBytes || begin.ChunkCount <= 0 || begin.ChunkCount > MaxStateChunks {
		return envelope, begin, errors.New("state transfer exceeds byte/chunk bounds")
	}
	if begin.ChunkCount > begin.TotalBytes || begin.ChunkCount*MaxStateChunkBytes < begin.TotalBytes {
		return envelope, begin, errors.New("state chunk_count cannot cover total_bytes")
	}
	if !validSHA256(begin.SHA256) {
		return envelope, begin, errors.New("invalid state sha256")
	}
	return envelope, begin, nil
}

// DecodeStateChunk enforces canonical base64 and limits before decoding bytes.
func DecodeStateChunk(data []byte) (Envelope, StateChunk, error) {
	var chunk StateChunk
	envelope, fields, err := telemetryFields(data, "state.chunk")
	if err != nil {
		return envelope, chunk, err
	}
	if err := decodeStateTransferIdentity(fields, &chunk.StateTransferIdentity); err != nil {
		return envelope, chunk, err
	}
	var encoded string
	if err := decodeRequiredFields(fields, field{"index", &chunk.Index}, field{"data_base64", &encoded}); err != nil {
		return envelope, chunk, err
	}
	if chunk.Index < 0 || chunk.Index >= MaxStateChunks || len(encoded) == 0 || len(encoded) > base64.StdEncoding.EncodedLen(MaxStateChunkBytes) {
		return envelope, chunk, errors.New("state chunk exceeds index/byte bounds")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) == 0 || len(decoded) > MaxStateChunkBytes || base64.StdEncoding.EncodeToString(decoded) != encoded {
		return envelope, chunk, errors.New("state chunk requires bounded canonical padded base64")
	}
	chunk.Data = decoded
	return envelope, chunk, nil
}

// DecodeStateCommit validates a commit frame, not the staged bytes or durability.
func DecodeStateCommit(data []byte) (Envelope, StateCommit, error) {
	var commit StateCommit
	envelope, fields, err := telemetryFields(data, "state.commit")
	if err != nil {
		return envelope, commit, err
	}
	if err := decodeStateTransferIdentity(fields, &commit.StateTransferIdentity); err != nil {
		return envelope, commit, err
	}
	if err := required(fields, "sha256", &commit.SHA256); err != nil {
		return envelope, commit, err
	}
	if !validSHA256(commit.SHA256) {
		return envelope, commit, errors.New("invalid state sha256")
	}
	return envelope, commit, nil
}

// DecodeStateApplied validates receipt framing without claiming a transaction
// committed. The receiving session must correlate it with its pending snapshot.
func DecodeStateApplied(data []byte) (Envelope, StateApplied, error) {
	var applied StateApplied
	envelope, fields, err := telemetryFields(data, "state.applied")
	if err != nil {
		return envelope, applied, err
	}
	if err := decodeStateTransferIdentity(fields, &applied.StateTransferIdentity); err != nil {
		return envelope, applied, err
	}
	if err := decodeRequiredFields(fields,
		field{"sha256", &applied.SHA256}, field{"applied_at", &applied.AppliedAt}, field{"state_count", &applied.StateCount},
	); err != nil {
		return envelope, applied, err
	}
	if !validSHA256(applied.SHA256) || applied.StateCount < 0 || applied.StateCount > MaxSnapshotStates {
		return envelope, applied, errors.New("invalid state receipt hash or count")
	}
	return envelope, applied, nil
}

func decodeStateTransferIdentity(fields map[string]json.RawMessage, identity *StateTransferIdentity) error {
	if err := requiredUUID(fields, "snapshot_id", &identity.SnapshotID); err != nil {
		return err
	}
	if err := requiredUUID(fields, "stream_id", &identity.StreamID); err != nil {
		return err
	}
	if err := required(fields, "config_revision", &identity.ConfigRevision); err != nil {
		return err
	}
	if identity.ConfigRevision <= 0 {
		return errors.New("state config_revision must be positive")
	}
	return nil
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
