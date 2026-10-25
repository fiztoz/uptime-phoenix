package provisioning

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// openProbeImageLayout opens the content inputs of a pinned image from an
// operator OCI image layout directory beneath root: the pinned index blob,
// then the manifest it selects for linux/<pin architecture>, its config and
// its ordered layers, each by digest under blobs/sha256. The parsing here
// only locates blobs; the content verifier is authoritative over every byte.
// On error every opened stream is closed.
func openProbeImageLayout(root *os.Root, pin domain.ProbeImagePin) (ports.ProbeImageContentInputs, error) {
	var zero ports.ProbeImageContentInputs
	dir := domain.ProbeImageLayoutDirName(pin)
	if root == nil || dir == "" {
		return zero, domain.ErrProbeInstallArtifact
	}
	blob := func(digest string) string { return dir + "/blobs/sha256/" + digest }
	indexBytes, err := readProbeImageLayoutBlob(root, blob(pin.IndexSHA256), domain.MaxProbeImageMetadataSize)
	if err != nil {
		return zero, err
	}
	manifestDigest, err := selectProbeImageManifest(indexBytes, pin.Architecture)
	if err != nil {
		return zero, err
	}
	manifestBytes, err := readProbeImageLayoutBlob(root, blob(manifestDigest), domain.MaxProbeImageMetadataSize)
	if err != nil {
		return zero, err
	}
	var manifest struct {
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
	}
	if json.Unmarshal(manifestBytes, &manifest) != nil || len(manifest.Layers) == 0 || len(manifest.Layers) > domain.MaxProbeImageLayers {
		return zero, domain.ErrProbeInstallArtifact
	}
	configDigest, ok := strings.CutPrefix(manifest.Config.Digest, "sha256:")
	if !ok || !probeHexDigest(configDigest) {
		return zero, domain.ErrProbeInstallArtifact
	}
	configBytes, err := readProbeImageLayoutBlob(root, blob(configDigest), domain.MaxProbeImageMetadataSize)
	if err != nil {
		return zero, err
	}
	layers := make([]io.ReadCloser, 0, len(manifest.Layers))
	closeLayers := func() {
		for _, layer := range layers {
			_ = layer.Close()
		}
	}
	for _, layer := range manifest.Layers {
		digest, ok := strings.CutPrefix(layer.Digest, "sha256:")
		if !ok || !probeHexDigest(digest) {
			closeLayers()
			return zero, domain.ErrProbeInstallArtifact
		}
		file, err := root.Open(blob(digest))
		if err != nil {
			closeLayers()
			return zero, domain.ErrProbeInstallArtifact
		}
		if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() {
			_ = file.Close()
			closeLayers()
			return zero, domain.ErrProbeInstallArtifact
		}
		layers = append(layers, &probeImageLayoutLayer{file: file})
	}
	return ports.ProbeImageContentInputs{
		Metadata: ports.ProbeImageInputs{
			Index:    io.NopCloser(bytes.NewReader(indexBytes)),
			Manifest: io.NopCloser(bytes.NewReader(manifestBytes)),
			Config:   io.NopCloser(bytes.NewReader(configBytes)),
		},
		Layers: layers,
	}, nil
}

// readProbeImageBlob reads one regular blob file beneath root, refusing more
// than limit bytes.
func readProbeImageLayoutBlob(root *os.Root, name string, limit int64) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, domain.ErrProbeInstallArtifact
	}
	defer func() { _ = file.Close() }()
	if info, err := file.Stat(); err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, domain.ErrProbeInstallArtifact
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(body)) > limit {
		return nil, domain.ErrProbeInstallArtifact
	}
	return body, nil
}

// selectProbeImageManifest finds the single linux/<architecture> manifest
// descriptor of an image index and returns its digest hex.
func selectProbeImageManifest(index []byte, architecture string) (string, error) {
	var parsed struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				OS           string `json:"os"`
				Architecture string `json:"architecture"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if json.Unmarshal(index, &parsed) != nil {
		return "", domain.ErrProbeInstallArtifact
	}
	found := ""
	for _, m := range parsed.Manifests {
		if m.Platform.OS != "linux" || m.Platform.Architecture != architecture {
			continue
		}
		digest, ok := strings.CutPrefix(m.Digest, "sha256:")
		if !ok || !probeHexDigest(digest) || found != "" {
			return "", domain.ErrProbeInstallArtifact
		}
		found = digest
	}
	if found == "" {
		return "", domain.ErrProbeInstallArtifact
	}
	return found, nil
}

func probeHexDigest(v string) bool {
	if len(v) != 64 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// probeImageLayoutLayer exposes only Read and Close of a layer blob file.
// Once it has reported EOF it keeps reporting EOF, even after Close: stream
// consumers may close a source at EOF and still drain it once more.
type probeImageLayoutLayer struct {
	file *os.File
	mu   sync.Mutex
	eof  bool
}

func (l *probeImageLayoutLayer) Read(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.eof {
		return 0, io.EOF
	}
	n, err := l.file.Read(p)
	if err == io.EOF {
		l.eof = true
	}
	return n, err
}

func (l *probeImageLayoutLayer) Close() error { return l.file.Close() }
