package probe

import (
	"encoding/json"
	"errors"
	"strings"
)

const (
	// MaxCapabilities bounds runtime inventory and required capability sets.
	MaxCapabilities = 128
	// MaxCapabilityBytes bounds a single exact capability name.
	MaxCapabilityBytes = 128
	// MaxResourceBindings bounds locally declared Docker resources.
	MaxResourceBindings = 128
	// MaxBindingKeyBytes bounds a resource key, never a host path or URL.
	MaxBindingKeyBytes = 128
	// MaxAgentVersionBytes bounds diagnostic build text.
	MaxAgentVersionBytes = 128
	// HeartbeatSeconds is the fixed V1 application health interval.
	HeartbeatSeconds = 15
)

var (
	// ErrHandshakeIdentity indicates a frame claiming a different installation/probe.
	ErrHandshakeIdentity = errors.New("handshake_identity_mismatch")
	// ErrStreamResetRequired requires explicit administrative stream reconciliation.
	ErrStreamResetRequired = errors.New("stream_reset_required")
	// ErrHandshakeGeneration indicates a frame outside the expected lease generation.
	ErrHandshakeGeneration = errors.New("handshake_generation_mismatch")
	// ErrIncompatibleProtocol indicates that the advertised range cannot use V1.
	ErrIncompatibleProtocol = errors.New("incompatible_protocol")
	// ErrUnsupportedCapability indicates a missing or malformed exact requirement.
	ErrUnsupportedCapability = errors.New("unsupported_capability")
	// ErrCursorAhead indicates that the hub cursor exceeds the source high-water mark.
	ErrCursorAhead = errors.New("committed_cursor_ahead")
	// ErrConfigRevisionAhead prevents a restored hub from reactivating older config.
	ErrConfigRevisionAhead = errors.New("active_config_revision_ahead")
)

// SessionIdentity carries explicit remote installation, registration, and epoch IDs.
type SessionIdentity struct {
	HubID    string `json:"hub_id"`
	ProbeID  string `json:"probe_id"`
	StreamID string `json:"stream_id"`
}

// ResourceBinding advertises a local key and kind without its path or credentials.
type ResourceBinding struct {
	BindingKey string `json:"binding_key"`
	Kind       string `json:"kind"`
}

// Hello reports durable source inventory before generation negotiation.
type Hello struct {
	SessionIdentity
	AgentVersion     string            `json:"agent_version"`
	ProtocolMin      int32             `json:"protocol_min"`
	ProtocolMax      int32             `json:"protocol_max"`
	ConfigRevision   Decimal           `json:"config_revision"`
	FirstRetainedSeq Decimal           `json:"first_retained_seq"`
	LastCreatedSeq   Decimal           `json:"last_created_seq"`
	Capabilities     []string          `json:"capabilities"`
	ResourceBindings []ResourceBinding `json:"resource_bindings"`
}

// Welcome declares the hub's durable progress and fixed V1 session limits.
type Welcome struct {
	SessionIdentity
	SelectedProtocol      int       `json:"selected_protocol"`
	ConnectionGeneration  Decimal   `json:"connection_generation"`
	CommittedSeq          Decimal   `json:"committed_seq"`
	DesiredConfigRevision Decimal   `json:"desired_config_revision"`
	HeartbeatSeconds      int       `json:"heartbeat_seconds"`
	MaxFrameBytes         int       `json:"max_frame_bytes"`
	MaxBatchEvents        int       `json:"max_batch_events"`
	MaxBatchBytes         int       `json:"max_batch_bytes"`
	HubTime               Timestamp `json:"hub_time"`
}

// DecodeHello validates bounded inventory, not authentication or compatibility
// with a particular hub's requirements. Unknown advertised capabilities survive.
func DecodeHello(data []byte) (Envelope, Hello, error) {
	var hello Hello
	envelope, fields, err := telemetryFields(data, "hello")
	if err != nil {
		return envelope, hello, err
	}
	if err := decodeSessionIdentity(fields, &hello.SessionIdentity); err != nil {
		return envelope, hello, err
	}
	var bindings []json.RawMessage
	if err := decodeRequiredFields(fields,
		field{"agent_version", &hello.AgentVersion}, field{"protocol_min", &hello.ProtocolMin},
		field{"protocol_max", &hello.ProtocolMax}, field{"config_revision", &hello.ConfigRevision},
		field{"first_retained_seq", &hello.FirstRetainedSeq}, field{"last_created_seq", &hello.LastCreatedSeq},
		field{"capabilities", &hello.Capabilities}, field{"resource_bindings", &bindings},
	); err != nil {
		return envelope, hello, err
	}
	if !validAgentVersion(hello.AgentVersion) || hello.ProtocolMin <= 0 || hello.ProtocolMax < hello.ProtocolMin {
		return envelope, hello, errors.New("invalid agent version or protocol range")
	}
	if hello.FirstRetainedSeq > hello.LastCreatedSeq {
		return envelope, hello, errors.New("retained sequence exceeds created high-water mark")
	}
	if err := validateCapabilities(hello.Capabilities); err != nil {
		return envelope, hello, err
	}
	if len(bindings) > MaxResourceBindings {
		return envelope, hello, errors.New("too many resource bindings")
	}
	hello.ResourceBindings = make([]ResourceBinding, 0, len(bindings))
	seen := make(map[string]struct{}, len(bindings))
	for _, raw := range bindings {
		binding, err := decodeResourceBinding(raw)
		if err != nil {
			return envelope, hello, err
		}
		if _, exists := seen[binding.BindingKey]; exists {
			return envelope, hello, errors.New("duplicate resource binding key")
		}
		seen[binding.BindingKey] = struct{}{}
		hello.ResourceBindings = append(hello.ResourceBindings, binding)
	}
	return envelope, hello, nil
}

// DecodeWelcome validates fixed V1 limits and matching payload/envelope generation.
// Durable identity, cursor, and configuration comparisons require the hello pair.
func DecodeWelcome(data []byte) (Envelope, Welcome, error) {
	var welcome Welcome
	envelope, fields, err := telemetryFields(data, "welcome")
	if err != nil {
		return envelope, welcome, err
	}
	if err := decodeSessionIdentity(fields, &welcome.SessionIdentity); err != nil {
		return envelope, welcome, err
	}
	if err := decodeRequiredFields(fields,
		field{"selected_protocol", &welcome.SelectedProtocol}, field{"connection_generation", &welcome.ConnectionGeneration},
		field{"committed_seq", &welcome.CommittedSeq}, field{"desired_config_revision", &welcome.DesiredConfigRevision},
		field{"heartbeat_seconds", &welcome.HeartbeatSeconds}, field{"max_frame_bytes", &welcome.MaxFrameBytes},
		field{"max_batch_events", &welcome.MaxBatchEvents}, field{"max_batch_bytes", &welcome.MaxBatchBytes},
		field{"hub_time", &welcome.HubTime},
	); err != nil {
		return envelope, welcome, err
	}
	if welcome.SelectedProtocol != 1 {
		return envelope, welcome, ErrIncompatibleProtocol
	}
	if welcome.ConnectionGeneration != envelope.ConnectionGeneration {
		return envelope, welcome, ErrHandshakeGeneration
	}
	if welcome.HeartbeatSeconds != HeartbeatSeconds || welcome.MaxFrameBytes != MaxFrameBytes || welcome.MaxBatchEvents != MaxBatchEvents || welcome.MaxBatchBytes != MaxBatchBytes {
		return envelope, welcome, errors.New("unsupported V1 session limits")
	}
	return envelope, welcome, nil
}

// HandshakeExpectation is trusted caller input, never populated from the peer.
// Its stream and generation come from durable registration/current lease state;
// required capabilities come from the caller's supported configuration contract.
type HandshakeExpectation struct {
	HubID                string
	ProbeID              string
	StreamID             string
	ConnectionGeneration Decimal
	RequiredCapabilities []string
}

// Handshake is a validated transcript, not an authenticated or activated session.
type Handshake struct {
	Hello   Hello
	Welcome Welcome
}

// ValidateHandshake decodes both frames and compares them with trusted expectations.
// It performs no I/O, acquires no lease, and grants no queue/configuration mutation.
// The caller must authenticate TLS/credentials and enforce session deadlines/fencing.
func ValidateHandshake(helloFrame, welcomeFrame []byte, expected HandshakeExpectation) (Handshake, error) {
	_, hello, err := DecodeHello(helloFrame)
	if err != nil {
		return Handshake{}, err
	}
	_, welcome, err := DecodeWelcome(welcomeFrame)
	if err != nil {
		return Handshake{}, err
	}
	if hello.HubID != expected.HubID || welcome.HubID != expected.HubID || hello.ProbeID != expected.ProbeID || welcome.ProbeID != expected.ProbeID {
		return Handshake{}, ErrHandshakeIdentity
	}
	if hello.StreamID != expected.StreamID || welcome.StreamID != expected.StreamID {
		return Handshake{}, ErrStreamResetRequired
	}
	if welcome.ConnectionGeneration != expected.ConnectionGeneration {
		return Handshake{}, ErrHandshakeGeneration
	}
	if hello.ProtocolMin > 1 || hello.ProtocolMax < 1 {
		return Handshake{}, ErrIncompatibleProtocol
	}
	if err := validateCapabilities(expected.RequiredCapabilities); err != nil {
		return Handshake{}, ErrUnsupportedCapability
	}
	available := make(map[string]struct{}, len(hello.Capabilities))
	for _, capability := range hello.Capabilities {
		available[capability] = struct{}{}
	}
	if _, exists := available["snapshot.v1"]; !exists {
		return Handshake{}, ErrUnsupportedCapability
	}
	for _, capability := range expected.RequiredCapabilities {
		if _, exists := available[capability]; !exists {
			return Handshake{}, ErrUnsupportedCapability
		}
	}
	if welcome.CommittedSeq > hello.LastCreatedSeq {
		return Handshake{}, ErrCursorAhead
	}
	if welcome.DesiredConfigRevision < hello.ConfigRevision {
		return Handshake{}, ErrConfigRevisionAhead
	}
	return Handshake{Hello: hello, Welcome: welcome}, nil
}

func decodeSessionIdentity(fields map[string]json.RawMessage, identity *SessionIdentity) error {
	if err := requiredUUID(fields, "hub_id", &identity.HubID); err != nil {
		return err
	}
	if err := requiredUUID(fields, "probe_id", &identity.ProbeID); err != nil {
		return err
	}
	return requiredUUID(fields, "stream_id", &identity.StreamID)
}

func validAgentVersion(value string) bool {
	if len(value) == 0 || len(value) > MaxAgentVersionBytes {
		return false
	}
	for _, character := range value {
		if character < '!' || character > '~' {
			return false
		}
	}
	return true
}

func validateCapabilities(capabilities []string) error {
	if len(capabilities) > MaxCapabilities {
		return errors.New("too many capabilities")
	}
	seen := make(map[string]struct{}, len(capabilities))
	for _, capability := range capabilities {
		if len(capability) == 0 || len(capability) > MaxCapabilityBytes {
			return errors.New("invalid capability name length")
		}
		for _, component := range strings.Split(capability, ".") {
			if len(component) == 0 {
				return errors.New("empty capability name component")
			}
			for _, character := range component {
				if !bindingCharacter(character) {
					return errors.New("invalid capability name character")
				}
			}
		}
		if _, exists := seen[capability]; exists {
			return errors.New("duplicate capability name")
		}
		seen[capability] = struct{}{}
	}
	return nil
}

func decodeResourceBinding(data []byte) (ResourceBinding, error) {
	var binding ResourceBinding
	fields, err := objectFields(data)
	if err != nil {
		return binding, err
	}
	if err := decodeRequiredFields(fields, field{"binding_key", &binding.BindingKey}, field{"kind", &binding.Kind}); err != nil {
		return binding, err
	}
	if !validBindingKey(binding.BindingKey) || (binding.Kind != "docker_socket" && binding.Kind != "docker_api") {
		return binding, errors.New("invalid resource binding key or kind")
	}
	return binding, nil
}

func validBindingKey(key string) bool {
	if len(key) == 0 || len(key) > MaxBindingKeyBytes || key[0] == '-' || key[0] == '_' {
		return false
	}
	for _, character := range key {
		if !bindingCharacter(character) {
			return false
		}
	}
	return true
}

func bindingCharacter(character rune) bool {
	return character >= 'a' && character <= 'z' || character >= '0' && character <= '9' || character == '_' || character == '-'
}
