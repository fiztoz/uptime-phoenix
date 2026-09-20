package probe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// ConfigTarget comes from trusted installation/session/runtime state, not config.
// Capabilities describe implemented runtime support, not unverified advertisements.
type ConfigTarget struct {
	HubID                string
	ProbeID              string
	ConnectionGeneration Decimal
	Capabilities         []string
	ResourceBindings     []ResourceBinding
}

// ConfigTransfer stages confidential bytes for one session reader. It is not
// concurrent. The owner supplies one local monotonic clock and discards staging
// on session cancellation/idle expiry. Assembly grants no activation authority.
type ConfigTransfer struct {
	begin      ConfigBegin
	target     ConfigTarget
	bindings   map[string]ResourceBinding
	deadline   time.Time
	chunks     [][]byte
	totalBytes int
	received   int
	closed     bool
}

// NewConfigTransfer checks metadata, current generation, and runtime requirements.
// Its fixed deadline begins here; no active configuration is changed.
func NewConfigTransfer(data []byte, now time.Time, target ConfigTarget) (*ConfigTransfer, error) {
	envelope, begin, err := DecodeConfigBegin(data)
	if err != nil {
		return nil, err
	}
	if target.HubID == "" || target.ProbeID == "" || envelope.ConnectionGeneration != target.ConnectionGeneration {
		return nil, errors.New("invalid config target or session generation")
	}
	if err := validateCapabilities(target.Capabilities); err != nil {
		return nil, err
	}
	for _, required := range begin.RequiredCapabilities {
		if !containsCapability(target.Capabilities, required) {
			return nil, ErrUnsupportedCapability
		}
	}
	if len(target.ResourceBindings) > MaxResourceBindings {
		return nil, errors.New("runtime resource inventory exceeds limit")
	}
	bindings, err := configIndex(target.ResourceBindings, func(b ResourceBinding) string { return b.BindingKey })
	if err != nil {
		return nil, err
	}
	for _, binding := range target.ResourceBindings {
		if !validBindingKey(binding.BindingKey) || binding.Kind != "docker_socket" && binding.Kind != "docker_api" {
			return nil, errors.New("invalid runtime resource binding")
		}
	}
	// Retain scalar identity only; caller mutation cannot change the frozen target.
	target.Capabilities = nil
	target.ResourceBindings = nil
	return &ConfigTransfer{begin: begin, target: target, bindings: bindings,
		deadline: now.Add(ConfigTransferTimeout), chunks: make([][]byte, begin.ChunkCount)}, nil
}

// Discard releases confidential staging and makes this transfer terminal.
func (c *ConfigTransfer) Discard() {
	c.chunks = nil
	c.bindings = nil
	c.totalBytes = 0
	c.received = 0
	c.closed = true
}

// AddChunk accepts out-of-order and identical retries without extending lifetime.
// Any error discards staging; conflicting duplicates cannot alter accepted bytes.
func (c *ConfigTransfer) AddChunk(data []byte, now time.Time) (err error) {
	defer func() {
		if err != nil {
			c.Discard()
		}
	}()
	if err := c.checkOpen(now); err != nil {
		return err
	}
	envelope, chunk, err := DecodeConfigChunk(data)
	if err != nil {
		return err
	}
	if chunk.ConfigTransferIdentity != c.begin.ConfigTransferIdentity || envelope.ConnectionGeneration != c.target.ConnectionGeneration || chunk.Index >= len(c.chunks) {
		return errors.New("config chunk identity, generation, or index mismatch")
	}
	if previous := c.chunks[chunk.Index]; previous != nil {
		if !bytes.Equal(previous, chunk.Data) {
			return errors.New("conflicting duplicate config chunk")
		}
		return nil
	}
	if len(chunk.Data) > c.begin.TotalBytes-c.totalBytes {
		return errors.New("config chunks exceed declared bytes")
	}
	c.chunks[chunk.Index] = chunk.Data
	c.totalBytes += len(chunk.Data)
	c.received++
	return nil
}

// Commit verifies original bytes, schema/references, target, and capability union.
// It consumes staging on success or failure. A returned snapshot still needs all
// runtime validators and a fenced, revision-checked atomic activation transaction.
func (c *ConfigTransfer) Commit(data []byte, now time.Time) (ConfigSnapshot, error) {
	snapshot, _, err := c.CommitDocument(data, now)
	return snapshot, err
}

// CommitDocument also returns the exact authenticated bytes for protected storage.
// Re-marshaling the DTO would change whitespace/unknown fields and invalidate
// the hub's original hash. This does not authorize activation by itself.
func (c *ConfigTransfer) CommitDocument(data []byte, now time.Time) (ConfigSnapshot, []byte, error) {
	defer c.Discard()
	if err := c.checkOpen(now); err != nil {
		return ConfigSnapshot{}, nil, err
	}
	envelope, commit, err := DecodeConfigCommit(data)
	if err != nil {
		return ConfigSnapshot{}, nil, err
	}
	if commit.ConfigTransferIdentity != c.begin.ConfigTransferIdentity || envelope.ConnectionGeneration != c.target.ConnectionGeneration || commit.SHA256 != c.begin.SHA256 {
		return ConfigSnapshot{}, nil, errors.New("config commit identity, generation, or hash mismatch")
	}
	if c.received != c.begin.ChunkCount || c.totalBytes != c.begin.TotalBytes {
		return ConfigSnapshot{}, nil, errors.New("config commit requires all chunks and exact byte total")
	}
	content := make([]byte, 0, c.totalBytes)
	for _, chunk := range c.chunks {
		content = append(content, chunk...)
	}
	hash := sha256.Sum256(content)
	if hex.EncodeToString(hash[:]) != c.begin.SHA256 {
		return ConfigSnapshot{}, nil, errors.New("config snapshot hash mismatch")
	}
	snapshot, err := DecodeConfigSnapshot(content)
	if err != nil {
		return ConfigSnapshot{}, nil, err
	}
	if snapshot.HubID != c.target.HubID || snapshot.ProbeID != c.target.ProbeID || snapshot.Revision != c.begin.Revision || !time.Time(snapshot.EffectiveAt).Equal(time.Time(c.begin.EffectiveAt)) {
		return ConfigSnapshot{}, nil, errors.New("config snapshot target or metadata mismatch")
	}
	required := configCapabilities(snapshot)
	if len(required) != len(c.begin.RequiredCapabilities) {
		return ConfigSnapshot{}, nil, errors.New("config capability union differs from begin")
	}
	for _, capability := range c.begin.RequiredCapabilities {
		if !required[capability] {
			return ConfigSnapshot{}, nil, errors.New("config capability union differs from begin")
		}
	}
	for _, assignment := range snapshot.Assignments {
		for _, binding := range assignment.ResourceBindings {
			if local, exists := c.bindings[binding.BindingKey]; !exists || local.Kind != binding.Kind {
				return ConfigSnapshot{}, nil, errors.New("config requires an unavailable local resource binding")
			}
		}
	}
	return snapshot, content, nil
}

func (c *ConfigTransfer) checkOpen(now time.Time) error {
	if c.closed || c.chunks == nil {
		return errors.New("config transfer is closed")
	}
	if !now.Before(c.deadline) {
		return errors.New("config transfer expired")
	}
	return nil
}
