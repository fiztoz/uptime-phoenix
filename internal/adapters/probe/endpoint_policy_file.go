package probe

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"os"
)

// LoadEndpointPolicy loads a bounded operator allow-list. Empty path retains
// public-address-only defaults. Special files and ambiguous JSON fail closed.
func LoadEndpointPolicy(path string) (EndpointPolicy, error) {
	if path == "" {
		return EndpointPolicy{}, nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64*1024 {
		return EndpointPolicy{}, errors.New("invalid probe endpoint policy file")
	}
	f, err := os.Open(path)
	if err != nil {
		return EndpointPolicy{}, errors.New("probe endpoint policy unavailable")
	}
	defer func() { _ = f.Close() }()
	return DecodeEndpointPolicy(f)
}

// DecodeEndpointPolicy accepts only a bounded JSON CIDR allow-list. Address
// exclusions such as metadata/link-local remain enforced by the pinned dialer.
func DecodeEndpointPolicy(r io.Reader) (EndpointPolicy, error) {
	invalid := errors.New("invalid probe endpoint policy")
	data, err := io.ReadAll(io.LimitReader(r, 64*1024+1))
	if err != nil || len(data) > 64*1024 {
		return EndpointPolicy{}, invalid
	}
	if _, err := decodeJSONObject(data); err != nil {
		return EndpointPolicy{}, invalid
	}
	var document struct {
		AllowedCIDRs []string `json:"allowed_cidrs"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil || len(document.AllowedCIDRs) > 128 {
		return EndpointPolicy{}, invalid
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return EndpointPolicy{}, invalid
	}
	policy := EndpointPolicy{}
	for _, raw := range document.AllowedCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil || prefix.Addr().Is4In6() || prefix != prefix.Masked() {
			return EndpointPolicy{}, invalid
		}
		policy.AllowedCIDRs = append(policy.AllowedCIDRs, prefix)
	}
	return policy, nil
}
