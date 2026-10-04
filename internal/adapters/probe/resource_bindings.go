package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const maxResourceFileBytes = 128 << 10

// LocalResourceBindings is an immutable operator-owned Docker resource map.
// Inventory exposes only references; endpoints never enter a hub snapshot.
type LocalResourceBindings struct {
	endpoints map[ResourceBinding]string
}

// LoadResourceBindings loads an optional private regular JSON file. Changing
// bindings requires a restart; accepted snapshots are revalidated on cold load.
func LoadResourceBindings(ctx context.Context, path string) (*LocalResourceBindings, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if path == "" {
		return &LocalResourceBindings{}, nil
	}
	invalid := errors.New("invalid private probe resource binding file")
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > maxResourceFileBytes {
		return nil, invalid
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, invalid
	}
	defer func() { _ = f.Close() }()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, invalid
	}
	data, err := io.ReadAll(io.LimitReader(f, maxResourceFileBytes+1))
	if err != nil || len(data) > maxResourceFileBytes || validateJSON(data) != nil {
		return nil, invalid
	}
	defer clear(data)
	var entries []struct {
		BindingKey string `json:"binding_key"`
		Kind       string `json:"kind"`
		Endpoint   string `json:"endpoint"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&entries) != nil || entries == nil || len(entries) > MaxResourceBindings {
		return nil, invalid
	}
	out := &LocalResourceBindings{endpoints: make(map[ResourceBinding]string, len(entries))}
	keys := make(map[string]bool, len(entries))
	for _, entry := range entries {
		binding := ResourceBinding{BindingKey: entry.BindingKey, Kind: entry.Kind}
		if keys[binding.BindingKey] || !domain.ValidProbeResourceBinding(domain.ProbeResourceBinding(binding)) || !validResourceEndpoint(binding.Kind, entry.Endpoint) {
			return nil, invalid
		}
		keys[binding.BindingKey] = true
		out.endpoints[binding] = entry.Endpoint
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func validResourceEndpoint(kind, endpoint string) bool {
	if len(endpoint) == 0 || len(endpoint) > 4096 || strings.ContainsAny(endpoint, "\r\n\t ") {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || u.RawPath != "" {
		return false
	}
	if kind == "docker_socket" {
		return u.Scheme == "unix" && u.Host == "" && filepath.IsAbs(u.Path) && filepath.Clean(u.Path) == u.Path && u.Path != "/"
	}
	// Match the existing checker's plain TCP Docker transport. TLS/client-key
	// options are not supported by that checker and must not be advertised.
	if kind != "docker_api" || u.Scheme != "tcp" || u.Path != "" {
		return false
	}
	host, port, err := net.SplitHostPort(u.Host)
	n, parseErr := strconv.Atoi(port)
	return err == nil && host != "" && parseErr == nil && n > 0 && n <= 65535 && strconv.Itoa(n) == port
}

// Inventory returns a sorted independent wire view without endpoints.
// Availability means explicitly configured transport access; daemon/container
// outages remain ordinary DOWN checks and do not disable their own monitoring.
func (b *LocalResourceBindings) Inventory() []ResourceBinding {
	out := make([]ResourceBinding, 0)
	if b != nil {
		for binding := range b.endpoints {
			out = append(out, binding)
		}
	}
	slices.SortFunc(out, func(a, b ResourceBinding) int { return strings.Compare(a.BindingKey, b.BindingKey) })
	return out
}

func (b *LocalResourceBindings) resolve(binding ResourceBinding) (string, error) {
	if b != nil {
		if endpoint, ok := b.endpoints[binding]; ok {
			return endpoint, nil
		}
	}
	return "", ErrUnsupportedCapability
}
