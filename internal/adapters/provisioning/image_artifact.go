package provisioning

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	probeOCIIndex       = "application/vnd.oci.image.index.v1+json"
	probeDockerIndex    = "application/vnd.docker.distribution.manifest.list.v2+json"
	probeOCIManifest    = "application/vnd.oci.image.manifest.v1+json"
	probeDockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
	probeOCIConfig      = "application/vnd.oci.image.config.v1+json"
	probeDockerConfig   = "application/vnd.docker.container.image.v1+json"
)

// ProbeImageMetadataPreparer captures and verifies bounded local image metadata.
// It has no registry, Docker daemon, SSH, filesystem-write or execution capability.
type ProbeImageMetadataPreparer struct{}

// NewProbeImageMetadataPreparer constructs an offline digest-chain verifier.
func NewProbeImageMetadataPreparer() *ProbeImageMetadataPreparer {
	return &ProbeImageMetadataPreparer{}
}

// PrepareImage owns all streams on entry, including refusals and cancellation.
func (p *ProbeImageMetadataPreparer) PrepareImage(ctx context.Context, pin domain.ProbeImagePin, in ports.ProbeImageInputs) (result *domain.ProbeImageSelection, err error) {
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			for _, r := range []io.ReadCloser{in.Index, in.Manifest, in.Config} {
				if r != nil {
					_ = r.Close()
				}
			}
		})
	}
	defer closeAll()
	if ctx == nil || !pin.Valid() || in.Index == nil || in.Manifest == nil || in.Config == nil {
		return nil, domain.ErrProbeImageInput
	}
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(joined); closeAll() })
	defer func() {
		if !stop() {
			<-joined
		}
		if ctx.Err() != nil {
			result = nil
			err = ctx.Err()
		}
	}()
	index, err := readProbeImageBlob(ctx, in.Index, pin.IndexSize, pin.IndexSHA256)
	if err != nil {
		return nil, err
	}
	selected, err := selectProbeImage(ctx, index, pin.Architecture)
	if err != nil {
		return nil, err
	}
	manifest, err := readProbeImageBlob(ctx, in.Manifest, selected.size, selected.sha)
	if err != nil {
		return nil, err
	}
	configDescriptor, layers, err := probeImageManifest(ctx, manifest, selected.media)
	if err != nil {
		return nil, err
	}
	config, err := readProbeImageBlob(ctx, in.Config, configDescriptor.size, configDescriptor.sha)
	if err != nil {
		return nil, err
	}
	if err = validateProbeImageConfig(ctx, config, pin, len(layers)); err != nil {
		return nil, err
	}
	return &domain.ProbeImageSelection{Pin: pin, ManifestSHA256: selected.sha, ConfigSHA256: configDescriptor.sha, LayerCount: len(layers)}, nil
}

type probeImageContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r probeImageContextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

func readProbeImageBlob(ctx context.Context, r io.Reader, size int64, digest string) ([]byte, error) {
	if size <= 0 || size > domain.MaxProbeImageMetadataSize {
		return nil, domain.ErrProbeImageSize
	}
	b, err := io.ReadAll(io.LimitReader(probeImageContextReader{ctx: ctx, r: r}, size+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, domain.ErrProbeImageUnavailable
	}
	if int64(len(b)) != size {
		return nil, domain.ErrProbeImageSize
	}
	sum := sha256.Sum256(b)
	if hex.EncodeToString(sum[:]) != digest {
		return nil, domain.ErrProbeImageDigest
	}
	if err := validateProbeImageJSON(ctx, b); err != nil {
		return nil, err
	}
	return b, nil
}

// The guard is deliberately independent of Go's case-insensitive struct binding.
// Decoded duplicate keys (including escaped spellings) are refused at every level.
func validateProbeImageJSON(ctx context.Context, b []byte) error {
	if !utf8.Valid(b) {
		return domain.ErrProbeImageMetadata
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	tokens := 0
	next := func() (json.Token, error) {
		tokens++
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if tokens > 65536 {
			return nil, domain.ErrProbeImageMetadata
		}
		return dec.Token()
	}
	var value func(int) error
	value = func(depth int) error {
		if depth > 32 {
			return domain.ErrProbeImageMetadata
		}
		token, err := next()
		if err != nil {
			return err
		}
		switch token {
		case json.Delim('{'):
			keys := make(map[string]struct{})
			for dec.More() {
				k, err := next()
				if err != nil {
					return err
				}
				key, ok := k.(string)
				if !ok {
					return domain.ErrProbeImageMetadata
				}
				if _, ok := keys[key]; ok {
					return domain.ErrProbeImageMetadata
				}
				keys[key] = struct{}{}
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := next()
			if err != nil {
				return err
			}
			if end != json.Delim('}') {
				return domain.ErrProbeImageMetadata
			}
		case json.Delim('['):
			for dec.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
			end, err := next()
			if err != nil {
				return err
			}
			if end != json.Delim(']') {
				return domain.ErrProbeImageMetadata
			}
		}
		return nil
	}
	if err := value(0); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return domain.ErrProbeImageMetadata
	}
	if _, err := next(); err != io.EOF {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return domain.ErrProbeImageMetadata
	}
	return nil
}

type probeImageObject map[string]json.RawMessage

func probeImageObjectFrom(ctx context.Context, b []byte, fields ...string) (probeImageObject, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var obj probeImageObject
	if json.Unmarshal(b, &obj) != nil || obj == nil {
		return nil, domain.ErrProbeImageMetadata
	}
	for key := range obj {
		for _, field := range fields {
			if key != field && strings.EqualFold(key, field) {
				return nil, domain.ErrProbeImageMetadata
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return obj, nil
}
func (o probeImageObject) string(key string) (string, bool) {
	var v string
	b, ok := o[key]
	if !ok || bytes.Equal(bytes.TrimSpace(b), []byte("null")) || json.Unmarshal(b, &v) != nil {
		return "", false
	}
	return v, true
}
func (o probeImageObject) number(key string) (int64, bool) {
	var v int64
	b, ok := o[key]
	if !ok || bytes.Equal(bytes.TrimSpace(b), []byte("null")) || json.Unmarshal(b, &v) != nil {
		return 0, false
	}
	return v, true
}
func (o probeImageObject) array(key string) ([]json.RawMessage, bool) {
	var v []json.RawMessage
	b, ok := o[key]
	if !ok || json.Unmarshal(b, &v) != nil || v == nil {
		return nil, false
	}
	return v, true
}
func (o probeImageObject) absent(keys ...string) bool {
	for _, k := range keys {
		if _, ok := o[k]; ok {
			return false
		}
	}
	return true
}
func (o probeImageObject) emptyOSVersion() bool {
	if _, ok := o["os.version"]; !ok {
		return true
	}
	v, ok := o.string("os.version")
	return ok && v == ""
}
func (o probeImageObject) optionalEmptyArray(key string) bool {
	b, ok := o[key]
	if !ok || string(bytes.TrimSpace(b)) == "null" {
		return true
	}
	v, ok := o.array(key)
	return ok && len(v) == 0
}

func probeImageSHA256(value string) (string, bool) {
	sha, ok := strings.CutPrefix(value, "sha256:")
	if !ok || len(sha) != 64 || strings.ToLower(sha) != sha {
		return "", false
	}
	_, err := hex.DecodeString(sha)
	return sha, err == nil
}

type probeImageDescriptor struct {
	sha, media string
	size       int64
	platform   json.RawMessage
}

func parseProbeImageDescriptor(ctx context.Context, b []byte) (probeImageDescriptor, error) {
	var out probeImageDescriptor
	o, err := probeImageObjectFrom(ctx, b, "mediaType", "digest", "size", "platform", "urls", "data", "artifactType")
	if err != nil {
		return out, err
	}
	media, mok := o.string("mediaType")
	digest, dok := o.string("digest")
	size, sok := o.number("size")
	sha, ok := probeImageSHA256(digest)
	if !mok || !dok || !sok || !ok || size <= 0 || size > 16<<30 || !o.absent("urls", "data", "artifactType") {
		return out, domain.ErrProbeImageMetadata
	}
	return probeImageDescriptor{sha: sha, media: media, size: size, platform: o["platform"]}, nil
}

func probeImageBaseline(ctx context.Context, b []byte, arch string) (bool, error) {
	o, err := probeImageObjectFrom(ctx, b, "os", "architecture", "variant", "os.version", "os.features")
	if err != nil {
		return false, err
	}
	os, ok := o.string("os")
	a, aok := o.string("architecture")
	if !ok || !aok || os == "" || a == "" {
		return false, domain.ErrProbeImageMetadata
	}
	if os != "linux" || a != arch {
		return false, nil
	}
	variant := ""
	if _, present := o["variant"]; present {
		var vok bool
		variant, vok = o.string("variant")
		if !vok {
			return false, domain.ErrProbeImageMetadata
		}
	}
	if (arch == "amd64" && variant != "") || (arch == "arm64" && variant != "" && variant != "v8") || !o.emptyOSVersion() || !o.optionalEmptyArray("os.features") {
		return false, domain.ErrProbeImageMetadata
	}
	return true, nil
}

func selectProbeImage(ctx context.Context, b []byte, arch string) (probeImageDescriptor, error) {
	var selected probeImageDescriptor
	o, err := probeImageObjectFrom(ctx, b, "schemaVersion", "mediaType", "manifests", "artifactType", "subject")
	if err != nil {
		return selected, err
	}
	schema, sok := o.number("schemaVersion")
	media, mok := o.string("mediaType")
	list, lok := o.array("manifests")
	if !sok || schema != 2 || !mok || (media != probeOCIIndex && media != probeDockerIndex) || !lok || len(list) == 0 || len(list) > 256 || !o.absent("artifactType", "subject") {
		return selected, domain.ErrProbeImageMetadata
	}
	found := false
	for _, raw := range list {
		desc, err := parseProbeImageDescriptor(ctx, raw)
		if err != nil {
			return selected, err
		}
		if desc.media != probeOCIManifest && desc.media != probeDockerManifest || desc.size > domain.MaxProbeImageMetadataSize {
			return selected, domain.ErrProbeImageMetadata
		}
		matches, err := probeImageBaseline(ctx, desc.platform, arch)
		if err != nil {
			return selected, err
		}
		if !matches {
			continue
		}
		if found {
			return selected, domain.ErrProbeImageMetadata
		}
		selected = desc
		found = true
	}
	if !found {
		return selected, domain.ErrProbeImageMetadata
	}
	return selected, nil
}

func probeImageManifest(ctx context.Context, b []byte, expectedMedia string) (probeImageDescriptor, []probeImageDescriptor, error) {
	var config probeImageDescriptor
	o, err := probeImageObjectFrom(ctx, b, "schemaVersion", "mediaType", "config", "layers", "artifactType", "subject")
	if err != nil {
		return config, nil, err
	}
	schema, sok := o.number("schemaVersion")
	media, mok := o.string("mediaType")
	layers, lok := o.array("layers")
	if !sok || schema != 2 || !mok || media != expectedMedia || !lok || len(layers) == 0 || len(layers) > domain.MaxProbeImageLayers || !o.absent("artifactType", "subject") {
		return config, nil, domain.ErrProbeImageMetadata
	}
	config, err = parseProbeImageDescriptor(ctx, o["config"])
	if err != nil {
		return config, nil, err
	}
	expectedConfig := probeOCIConfig
	if media == probeDockerManifest {
		expectedConfig = probeDockerConfig
	}
	if config.media != expectedConfig || config.size > domain.MaxProbeImageMetadataSize || len(config.platform) != 0 {
		return config, nil, domain.ErrProbeImageMetadata
	}
	out := make([]probeImageDescriptor, 0, len(layers))
	for _, raw := range layers {
		layer, err := parseProbeImageDescriptor(ctx, raw)
		if err != nil {
			return config, nil, err
		}
		allowed := layer.media == "application/vnd.oci.image.layer.v1.tar" || layer.media == "application/vnd.oci.image.layer.v1.tar+gzip" || layer.media == "application/vnd.oci.image.layer.v1.tar+zstd"
		if media == probeDockerManifest {
			allowed = layer.media == "application/vnd.docker.image.rootfs.diff.tar.gzip" || layer.media == "application/vnd.docker.image.rootfs.diff.tar"
		}
		if !allowed || len(layer.platform) != 0 {
			return config, nil, domain.ErrProbeImageMetadata
		}
		out = append(out, layer)
	}
	return config, out, nil
}

func validateProbeImageConfig(ctx context.Context, b []byte, pin domain.ProbeImagePin, layers int) error {
	o, err := probeImageObjectFrom(ctx, b, "architecture", "os", "variant", "os.version", "os.features", "config", "rootfs")
	if err != nil {
		return err
	}
	matches, err := probeImageBaseline(ctx, b, pin.Architecture)
	if err != nil {
		return err
	}
	if !matches {
		return domain.ErrProbeImageMetadata
	}
	runtime, err := probeImageObjectFrom(ctx, o["config"], "User", "Entrypoint", "Cmd", "Labels", "Healthcheck", "Volumes", "OnBuild")
	if err != nil {
		return err
	}
	user, uok := runtime.string("User")
	entry, eok := runtime.array("Entrypoint")
	var executable string
	if !uok || user != "65532" || !eok || len(entry) != 1 || json.Unmarshal(entry[0], &executable) != nil || executable != "/uptime-phoenix-probe" || !runtime.optionalEmptyArray("Cmd") || !runtime.optionalEmptyArray("OnBuild") {
		return domain.ErrProbeImageMetadata
	}
	if raw, ok := runtime["Healthcheck"]; ok && string(bytes.TrimSpace(raw)) != "null" {
		return domain.ErrProbeImageMetadata
	}
	if raw, ok := runtime["Volumes"]; ok && string(bytes.TrimSpace(raw)) != "null" {
		volumes, err := probeImageObjectFrom(ctx, raw)
		if err != nil {
			return err
		}
		if len(volumes) != 0 {
			return domain.ErrProbeImageMetadata
		}
	}
	labels, err := probeImageObjectFrom(ctx, runtime["Labels"])
	if err != nil {
		return err
	}
	for key, want := range map[string]string{"org.opencontainers.image.title": "Uptime Phoenix Probe", "org.opencontainers.image.source": "https://github.com/fiztoz/uptime-phoenix", "org.opencontainers.image.version": pin.Version} {
		got, ok := labels.string(key)
		if !ok || got != want {
			return domain.ErrProbeImageMetadata
		}
	}
	root, err := probeImageObjectFrom(ctx, o["rootfs"], "type", "diff_ids")
	if err != nil {
		return err
	}
	kind, ok := root.string("type")
	diffs, dok := root.array("diff_ids")
	if !ok || kind != "layers" || !dok || len(diffs) != layers {
		return domain.ErrProbeImageMetadata
	}
	for _, raw := range diffs {
		var digest string
		if json.Unmarshal(raw, &digest) != nil {
			return domain.ErrProbeImageMetadata
		}
		if _, ok := probeImageSHA256(digest); !ok {
			return domain.ErrProbeImageMetadata
		}
	}
	return ctx.Err()
}

var _ ports.ProbeImagePreparer = (*ProbeImageMetadataPreparer)(nil)
