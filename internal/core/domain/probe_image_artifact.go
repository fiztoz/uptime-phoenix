package domain

import (
	"errors"
	"net"
	"net/url"
	"regexp"
	"strings"
)

// MaxProbeImageMetadataSize bounds each digest-bound image metadata blob.
const MaxProbeImageMetadataSize int64 = 4 * 1024 * 1024

// MaxProbeImageLayers bounds one supported executable image manifest.
const MaxProbeImageLayers = 128

// Image errors are fixed; raw image configuration may contain sensitive data.
var (
	ErrProbeImageInput       = errors.New("invalid pinned probe image input")
	ErrProbeImageSize        = errors.New("probe image metadata size mismatch")
	ErrProbeImageDigest      = errors.New("probe image metadata digest mismatch")
	ErrProbeImageMetadata    = errors.New("probe image metadata contract mismatch")
	ErrProbeImageUnavailable = errors.New("probe image metadata unavailable")
)

var probeImagePathComponent = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*$`)

// ProbeImagePin binds a repository/platform/version to out-of-band trusted index bytes.
type ProbeImagePin struct {
	Repository   string
	Version      string
	Architecture string
	IndexSHA256  string
	IndexSize    int64
}

// Valid refuses mutable references and implicit Docker Hub normalization.
func (p ProbeImagePin) Valid() bool {
	fields := ProbeBinaryPin{Version: p.Version, Architecture: p.Architecture, SHA256: p.IndexSHA256, Size: p.IndexSize}
	if !fields.Valid() || p.IndexSize > MaxProbeImageMetadataSize || len(p.Repository) > 255 || strings.ToLower(p.Repository) != p.Repository {
		return false
	}
	registry, name, ok := strings.Cut(p.Repository, "/")
	if !ok || name == "" || strings.ContainsAny(p.Repository, "@?#\\ \t\r\n\x00") {
		return false
	}
	u, err := url.Parse("https://" + registry)
	if err != nil || u.User != nil || u.Host != registry || u.Path != "" {
		return false
	}
	host := u.Hostname()
	port := u.Port()
	canonical := host
	if strings.Contains(host, ":") {
		canonical = "[" + host + "]"
	}
	if port != "" {
		canonical = net.JoinHostPort(host, port)
	}
	if registry != canonical {
		return false
	}
	if port == "" {
		port = "443"
	}
	if !(ProbeSSHHost{Address: net.JoinHostPort(host, port), User: "artifact"}).Valid() {
		return false
	}
	if host != "localhost" && net.ParseIP(host) == nil && !strings.Contains(host, ".") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if !probeImagePathComponent.MatchString(part) {
			return false
		}
	}
	return true
}

// ProbeImageSelection is verified metadata only, not downloaded image/layer evidence.
type ProbeImageSelection struct {
	Pin            ProbeImagePin
	ManifestSHA256 string
	ConfigSHA256   string
	LayerCount     int
}

// Valid checks bounded output identity before callers can form an immutable reference.
func (s ProbeImageSelection) Valid() bool {
	if !s.Pin.Valid() || s.LayerCount < 1 || s.LayerCount > MaxProbeImageLayers {
		return false
	}
	p := ProbeBinaryPin{Version: s.Pin.Version, Architecture: s.Pin.Architecture, Size: 1, SHA256: s.ManifestSHA256}
	if !p.Valid() {
		return false
	}
	p.SHA256 = s.ConfigSHA256
	return p.Valid()
}

// Reference returns the exact selected platform manifest, never a tag.
func (s ProbeImageSelection) Reference() string {
	if !s.Valid() {
		return ""
	}
	return s.Pin.Repository + "@sha256:" + s.ManifestSHA256
}
