package probe

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"time"
)

const (
	// MaxConfigChunkBytes limits bytes before canonical padded base64 encoding.
	MaxConfigChunkBytes = 256 << 10
	// MaxConfigChunks bounds staging bookkeeping.
	MaxConfigChunks = 1024
	// ConfigTransferTimeout is fixed from begin; retries never extend it.
	ConfigTransferTimeout = 60 * time.Second
	// MaxConfigErrors bounds redacted activation rejection details.
	MaxConfigErrors = 64
)

// ConfigTransferIdentity binds frames to a snapshot and positive revision.
type ConfigTransferIdentity struct {
	SnapshotID string  `json:"snapshot_id"`
	Revision   Decimal `json:"revision"`
}

// ConfigBegin declares bounded confidential content and its exact requirements.
type ConfigBegin struct {
	ConfigTransferIdentity
	ConfigSchemaVersion  int       `json:"config_schema_version"`
	TotalBytes           int       `json:"total_bytes"`
	ChunkCount           int       `json:"chunk_count"`
	SHA256               string    `json:"sha256"`
	RequiredCapabilities []string  `json:"required_capabilities"`
	EffectiveAt          Timestamp `json:"effective_at"`
}

// ConfigChunk holds original bytes; JSON uses canonical standard padded base64.
type ConfigChunk struct {
	ConfigTransferIdentity
	Index int    `json:"index"`
	Data  []byte `json:"data_base64"`
}

// ConfigCommit requests verification, not execution of a partially staged config.
type ConfigCommit struct {
	ConfigTransferIdentity
	SHA256 string `json:"sha256"`
}

// ConfigApplied may be sent only after durable atomic activation.
type ConfigApplied struct {
	ConfigTransferIdentity
	SHA256          string    `json:"sha256"`
	AppliedAt       Timestamp `json:"applied_at"`
	AssignmentCount int       `json:"assignment_count"`
}

// ConfigRejected is bounded diagnostic data, never raw validator output.
type ConfigRejected struct {
	ConfigTransferIdentity
	Errors []ConfigError `json:"errors"`
}

// ConfigError reports a bounded JSON pointer, machine code, and redacted message.
type ConfigError struct {
	Path    string `json:"path"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// DecodeConfigBegin validates metadata without allocating snapshot staging.
func DecodeConfigBegin(data []byte) (Envelope, ConfigBegin, error) {
	var begin ConfigBegin
	envelope, fields, err := telemetryFields(data, "config.begin")
	if err != nil {
		return envelope, begin, err
	}
	if err := decodeConfigTransferIdentity(fields, &begin.ConfigTransferIdentity); err != nil {
		return envelope, begin, err
	}
	if err := decodeRequiredFields(fields,
		field{"config_schema_version", &begin.ConfigSchemaVersion}, field{"total_bytes", &begin.TotalBytes},
		field{"chunk_count", &begin.ChunkCount}, field{"sha256", &begin.SHA256},
		field{"required_capabilities", &begin.RequiredCapabilities}, field{"effective_at", &begin.EffectiveAt},
	); err != nil {
		return envelope, begin, err
	}
	if begin.ConfigSchemaVersion != 1 || begin.TotalBytes <= 0 || begin.TotalBytes > MaxConfigSnapshotBytes || begin.ChunkCount <= 0 || begin.ChunkCount > MaxConfigChunks {
		return envelope, begin, errors.New("unsupported config schema or transfer bounds")
	}
	if begin.ChunkCount > begin.TotalBytes || begin.ChunkCount*MaxConfigChunkBytes < begin.TotalBytes || !validSHA256(begin.SHA256) {
		return envelope, begin, errors.New("invalid config hash or chunk coverage")
	}
	if err := validateCapabilities(begin.RequiredCapabilities); err != nil {
		return envelope, begin, err
	}
	if !containsCapability(begin.RequiredCapabilities, "snapshot.v1") {
		return envelope, begin, ErrUnsupportedCapability
	}
	return envelope, begin, nil
}

// DecodeConfigChunk validates canonical base64 and byte/index limits.
func DecodeConfigChunk(data []byte) (Envelope, ConfigChunk, error) {
	var chunk ConfigChunk
	envelope, fields, err := telemetryFields(data, "config.chunk")
	if err != nil {
		return envelope, chunk, err
	}
	if err := decodeConfigTransferIdentity(fields, &chunk.ConfigTransferIdentity); err != nil {
		return envelope, chunk, err
	}
	var encoded string
	if err := decodeRequiredFields(fields, field{"index", &chunk.Index}, field{"data_base64", &encoded}); err != nil {
		return envelope, chunk, err
	}
	if chunk.Index < 0 || chunk.Index >= MaxConfigChunks || len(encoded) == 0 || len(encoded) > base64.StdEncoding.EncodedLen(MaxConfigChunkBytes) {
		return envelope, chunk, errors.New("config chunk exceeds index/byte bounds")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) == 0 || len(decoded) > MaxConfigChunkBytes || base64.StdEncoding.EncodeToString(decoded) != encoded {
		return envelope, chunk, errors.New("config chunk requires bounded canonical padded base64")
	}
	chunk.Data = decoded
	return envelope, chunk, nil
}

// DecodeConfigCommit validates a request, not its staged bytes or durability.
func DecodeConfigCommit(data []byte) (Envelope, ConfigCommit, error) {
	var commit ConfigCommit
	envelope, fields, err := telemetryFields(data, "config.commit")
	if err != nil {
		return envelope, commit, err
	}
	if err := decodeConfigTransferIdentity(fields, &commit.ConfigTransferIdentity); err != nil {
		return envelope, commit, err
	}
	if err := required(fields, "sha256", &commit.SHA256); err != nil {
		return envelope, commit, err
	}
	if !validSHA256(commit.SHA256) {
		return envelope, commit, errors.New("invalid config commit hash")
	}
	return envelope, commit, nil
}

// DecodeConfigApplied validates receipt shape without claiming a DB commit.
func DecodeConfigApplied(data []byte) (Envelope, ConfigApplied, error) {
	var applied ConfigApplied
	envelope, fields, err := telemetryFields(data, "config.applied")
	if err != nil {
		return envelope, applied, err
	}
	if err := decodeConfigTransferIdentity(fields, &applied.ConfigTransferIdentity); err != nil {
		return envelope, applied, err
	}
	if err := decodeRequiredFields(fields, field{"sha256", &applied.SHA256}, field{"applied_at", &applied.AppliedAt}, field{"assignment_count", &applied.AssignmentCount}); err != nil {
		return envelope, applied, err
	}
	if !validSHA256(applied.SHA256) || applied.AssignmentCount < 0 || applied.AssignmentCount > MaxConfigAssignments {
		return envelope, applied, errors.New("invalid config receipt hash or count")
	}
	return envelope, applied, nil
}

// DecodeConfigRejected validates bounded redacted activation diagnostics.
func DecodeConfigRejected(data []byte) (Envelope, ConfigRejected, error) {
	var rejected ConfigRejected
	envelope, fields, err := telemetryFields(data, "config.rejected")
	if err != nil {
		return envelope, rejected, err
	}
	if err := decodeConfigTransferIdentity(fields, &rejected.ConfigTransferIdentity); err != nil {
		return envelope, rejected, err
	}
	rejected.Errors, err = decodeConfigList(fields["errors"], MaxConfigErrors, decodeConfigError)
	if err != nil {
		return envelope, rejected, err
	}
	if len(rejected.Errors) == 0 {
		return envelope, rejected, errors.New("config rejection requires at least one error")
	}
	return envelope, rejected, nil
}

func decodeConfigError(data []byte) (ConfigError, error) {
	var detail ConfigError
	if _, err := decodeConfigFields(data, &detail, "path code message", ""); err != nil {
		return detail, err
	}
	if !validConfigPath(detail.Path) || !validErrorCode(detail.Code) || len(detail.Message) > MaxMessageBytes {
		return detail, errors.New("invalid config rejection path, code, or message bounds")
	}
	return detail, nil
}

func validConfigPath(path string) bool {
	if len(path) == 0 || len(path) > 256 || path[0] != '/' {
		return false
	}
	for index := 0; index < len(path); index++ {
		if path[index] < ' ' || path[index] > '~' {
			return false
		}
		if path[index] == '~' {
			index++
			if index >= len(path) || path[index] != '0' && path[index] != '1' {
				return false
			}
		}
	}
	return true
}

func decodeConfigTransferIdentity(fields map[string]json.RawMessage, identity *ConfigTransferIdentity) error {
	if err := requiredUUID(fields, "snapshot_id", &identity.SnapshotID); err != nil {
		return err
	}
	if err := required(fields, "revision", &identity.Revision); err != nil {
		return err
	}
	if identity.Revision <= 0 {
		return errors.New("config revision must be positive")
	}
	return nil
}

// ErrConfigRevisionConflict prevents stale content or same-revision hash changes.
var ErrConfigRevisionConflict = errors.New("revision_conflict")

// CompareConfigRevision returns true only for a same-revision/same-hash retry.
// The caller supplies durable active metadata and must repeat this comparison
// in the activation transaction. Active revision zero requires an empty hash.
func CompareConfigRevision(activeRevision Decimal, activeHash string, revision Decimal, hash string) (bool, error) {
	if activeRevision < 0 || activeRevision == 0 && activeHash != "" || activeRevision > 0 && !validSHA256(activeHash) || revision <= 0 || !validSHA256(hash) {
		return false, errors.New("invalid configuration revision metadata")
	}
	if revision < activeRevision || revision == activeRevision && hash != activeHash {
		return false, ErrConfigRevisionConflict
	}
	return revision == activeRevision, nil
}
