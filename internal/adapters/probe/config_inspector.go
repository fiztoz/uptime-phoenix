package probe

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ConfigInspector validates complete snapshot shape and hashes original bytes.
// It is intentionally separate from runtime validation and activation.
type ConfigInspector struct{}

var _ ports.ProbeConfigInspector = ConfigInspector{}

// Inspect selects the dialect from trusted target identity, never from the payload.
func (ConfigInspector) Inspect(document []byte, target domain.ProbeConfigTarget) (domain.ProbeConfigMetadata, error) {
	if !domain.ValidProbeConfigTarget(target) {
		return domain.ProbeConfigMetadata{}, domain.ErrValidation
	}
	var snapshot ConfigSnapshot
	var err error
	if target.ProbeID == domain.LocalProbeID {
		snapshot, err = DecodeLocalConfigSnapshot(document)
	} else {
		snapshot, err = DecodeConfigSnapshot(document)
	}
	if err != nil || snapshot.HubID != target.HubID || snapshot.ProbeID != target.ProbeID {
		return domain.ProbeConfigMetadata{}, domain.ErrValidation
	}
	hash := sha256.Sum256(document)
	return domain.NormalizeProbeConfigMetadata(domain.ProbeConfigMetadata{ProbeConfigTarget: target,
		Revision: int64(snapshot.Revision), SchemaVersion: snapshot.SchemaVersion, SHA256: hex.EncodeToString(hash[:]),
		CreatedAt: time.Time(snapshot.CreatedAt), EffectiveAt: time.Time(snapshot.EffectiveAt)}), nil
}
