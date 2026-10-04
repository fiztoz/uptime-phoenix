package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func readProbeAssignmentBindings(ctx context.Context, path string) ([]domain.ProbeAssignmentBinding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return nil, nil
	}
	data, err := readPrivateProbeInput(path, 128<<10)
	if err != nil {
		return nil, err
	}
	defer clear(data)
	var entries []struct {
		ProbeID    string `json:"probe_id"`
		BindingKey string `json:"binding_key"`
		Kind       string `json:"kind"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&entries) != nil || entries == nil || len(entries) > 128 || d.Decode(new(any)) != io.EOF {
		return nil, domain.ErrValidation
	}
	out := make([]domain.ProbeAssignmentBinding, 0, len(entries))
	for _, entry := range entries {
		binding := domain.ProbeResourceBinding{BindingKey: entry.BindingKey, Kind: entry.Kind}
		if !domain.ValidHubID(entry.ProbeID) || !domain.ValidProbeResourceBinding(binding) {
			return nil, domain.ErrValidation
		}
		out = append(out, domain.ProbeAssignmentBinding{ProbeID: entry.ProbeID, ProbeResourceBinding: binding})
	}
	return out, nil
}
