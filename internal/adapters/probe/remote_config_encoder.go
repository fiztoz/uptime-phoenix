package probe

import (
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// RemoteConfigEncoder emits the complete remote dialect from the shared graph.
// It never exports hub-only acknowledgement links or administrative fields.
type RemoteConfigEncoder struct{}

var _ ports.RemoteProbeConfigEncoder = RemoteConfigEncoder{}

// EncodeRemote shares deterministic explicit DTO construction with the local
// encoder, then enforces remote identity and implemented edge capabilities.
func (RemoteConfigEncoder) EncodeRemote(definition domain.LocalProbeConfigDefinition) ([]byte, error) {
	return encodeProbeConfigDefinition(definition, true)
}
