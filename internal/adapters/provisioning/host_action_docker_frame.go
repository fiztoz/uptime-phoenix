package provisioning

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"unicode/utf8"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// Docker host-action wire format (frame v2). Systemd identities keep v1:
//
//	PHXACT2\n <kind length byte> <kind> PHXDKR1\n <u32 big-endian N> <N bytes strict JSON>
//
// and an image_cache action then streams, after its grant frame:
//
//	PHXIMG1\n <u64 big-endian size> <size archive bytes>
const (
	hostActionDockerMagic = "PHXACT2\n"
	dockerHeaderMagic     = "PHXDKR1\n"
	// ProbeDockerHeaderPrefixSize is the PHXDKR1 magic plus a u32 length.
	ProbeDockerHeaderPrefixSize = 12
	// MaxProbeDockerHeaderSize bounds the JSON body.
	MaxProbeDockerHeaderSize = 4096
	imagePayloadMagic        = "PHXIMG1\n"
	// ProbeImagePayloadPrefixSize is the PHXIMG1 magic plus a u64 size.
	ProbeImagePayloadPrefixSize = 16
)

// probeDockerHeaderKeys is the exact canonical key set of a Docker header.
var probeDockerHeaderKeys = []string{
	"operation_id", "owner_id", "attempt_id", "ssh_address", "ssh_user",
	"host_key_fingerprint", "client_key_fingerprint", "requested_by", "layout_version", "mode",
	"image_repository", "image_version", "image_architecture", "image_index_sha256", "image_index_size",
	"image_manifest_sha256", "image_config_sha256", "image_layer_count",
	"payload_sha256", "payload_size", "payload_layout_index_sha256",
}

// probeDockerIntegerKeys are the keys whose values are JSON integers.
var probeDockerIntegerKeys = map[string]bool{
	"requested_by": true, "layout_version": true, "image_index_size": true,
	"image_layer_count": true, "payload_size": true,
}

// encodeProbeDockerActionFrame writes the v2 frame for a valid Docker intent.
func encodeProbeDockerActionFrame(intent domain.ProbeHostActionIntent) ([]byte, error) {
	r, img, p := intent.Identity, intent.Identity.Image, intent.Payload
	values := map[string]any{
		"operation_id": r.OperationID, "owner_id": intent.OwnerID, "attempt_id": intent.AttemptID,
		"ssh_address": r.Host.Address, "ssh_user": r.Host.User,
		"host_key_fingerprint": r.HostKeyFingerprint, "client_key_fingerprint": r.ClientKeyFingerprint,
		"requested_by": r.RequestedBy, "layout_version": r.LayoutVersion, "mode": r.Mode,
		"image_repository": img.Pin.Repository, "image_version": img.Pin.Version, "image_architecture": img.Pin.Architecture,
		"image_index_sha256": img.Pin.IndexSHA256, "image_index_size": img.Pin.IndexSize,
		"image_manifest_sha256": img.ManifestSHA256, "image_config_sha256": img.ConfigSHA256, "image_layer_count": img.LayerCount,
		"payload_sha256": p.SHA256, "payload_size": p.Size, "payload_layout_index_sha256": p.LayoutIndexSHA256,
	}
	var body bytes.Buffer
	body.WriteByte('{')
	for i, key := range probeDockerHeaderKeys {
		if i > 0 {
			body.WriteByte(',')
		}
		k, _ := json.Marshal(key)
		v, err := json.Marshal(values[key])
		if err != nil {
			return nil, domain.ErrProbeBinaryStageWriteInput
		}
		body.Write(k)
		body.WriteByte(':')
		body.Write(v)
	}
	body.WriteByte('}')
	if body.Len() > MaxProbeDockerHeaderSize {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	frame := make([]byte, 0, ProbeHostActionPrefixSize+len(intent.Kind)+ProbeDockerHeaderPrefixSize+body.Len())
	frame = append(frame, hostActionDockerMagic...)
	frame = append(frame, byte(len(intent.Kind)))
	frame = append(frame, intent.Kind...)
	frame = append(frame, dockerHeaderMagic...)
	frame = binary.BigEndian.AppendUint32(frame, uint32(body.Len()))
	return append(frame, body.Bytes()...), nil
}

// ProbeHostActionHeaderLength reads the 12-byte header prefix that follows
// the kind and returns the JSON length. docker selects the v2 (PHXDKR1)
// header; otherwise it is the v1 binary-stage header (PHXSTG1).
func ProbeHostActionHeaderLength(prefix []byte, docker bool) (int, error) {
	if !docker {
		return ProbeBinaryStageHeaderLength(prefix)
	}
	if len(prefix) != ProbeDockerHeaderPrefixSize || string(prefix[:len(dockerHeaderMagic)]) != dockerHeaderMagic {
		return 0, domain.ErrProbeBinaryStageWriteInput
	}
	n := binary.BigEndian.Uint32(prefix[len(dockerHeaderMagic):])
	if n == 0 || n > MaxProbeDockerHeaderSize {
		return 0, domain.ErrProbeBinaryStageWriteInput
	}
	return int(n), nil
}

// decodeProbeDockerActionIntent decodes an exact PHXDKR1 header (prefix plus
// JSON) with the kind into a valid Docker host-action intent.
func decodeProbeDockerActionIntent(kind, header []byte) (domain.ProbeHostActionIntent, error) {
	var zero domain.ProbeHostActionIntent
	if len(header) < ProbeDockerHeaderPrefixSize {
		return zero, domain.ErrProbeBinaryStageWriteInput
	}
	n, err := ProbeHostActionHeaderLength(header[:ProbeDockerHeaderPrefixSize], true)
	if err != nil || len(header) != ProbeDockerHeaderPrefixSize+n {
		return zero, domain.ErrProbeBinaryStageWriteInput
	}
	values, ok := parseProbeDockerHeader(header[ProbeDockerHeaderPrefixSize:])
	if !ok {
		return zero, domain.ErrProbeBinaryStageWriteInput
	}
	str := func(k string) string { return values[k].(string) }
	num := func(k string) int64 { return values[k].(int64) }
	layout, layers := num("layout_version"), num("image_layer_count")
	if int64(int(layout)) != layout || int64(int(layers)) != layers {
		return zero, domain.ErrProbeBinaryStageWriteInput
	}
	intent := domain.ProbeHostActionIntent{
		Identity: domain.ProbeInstallIdentity{
			OperationID:          str("operation_id"),
			Host:                 domain.ProbeSSHHost{Address: str("ssh_address"), User: str("ssh_user")},
			HostKeyFingerprint:   str("host_key_fingerprint"),
			ClientKeyFingerprint: str("client_key_fingerprint"),
			RequestedBy:          num("requested_by"),
			LayoutVersion:        int(layout),
			Mode:                 str("mode"),
			Image: domain.ProbeImageSelection{
				Pin: domain.ProbeImagePin{
					Repository: str("image_repository"), Version: str("image_version"), Architecture: str("image_architecture"),
					IndexSHA256: str("image_index_sha256"), IndexSize: num("image_index_size"),
				},
				ManifestSHA256: str("image_manifest_sha256"),
				ConfigSHA256:   str("image_config_sha256"),
				LayerCount:     int(layers),
			},
		},
		OwnerID:   str("owner_id"),
		AttemptID: str("attempt_id"),
		Kind:      string(kind),
		Payload: domain.ProbeHostActionPayload{
			SHA256: str("payload_sha256"), Size: num("payload_size"), LayoutIndexSHA256: str("payload_layout_index_sha256"),
		},
	}
	if intent.Identity.Mode != domain.ProbeInstallDocker || !intent.Valid() {
		return zero, domain.ErrProbeBinaryStageWriteInput
	}
	return intent, nil
}

// parseProbeDockerHeader requires one flat object with exactly the canonical
// key spellings, no duplicates, strings for string keys and canonical
// integers for integer keys, and nothing after it.
func parseProbeDockerHeader(body []byte) (map[string]any, bool) {
	if len(body) == 0 || len(body) > MaxProbeDockerHeaderSize || !utf8.Valid(body) {
		return nil, false
	}
	allowed := make(map[string]bool, len(probeDockerHeaderKeys))
	for _, k := range probeDockerHeaderKeys {
		allowed[k] = true
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, false
	}
	values := make(map[string]any, len(probeDockerHeaderKeys))
	for dec.More() {
		before := dec.InputOffset()
		keyTok, err := dec.Token()
		after := dec.InputOffset()
		if err != nil {
			return nil, false
		}
		key, ok := keyTok.(string)
		if !ok || !allowed[key] || !canonicalStageHeaderKeyLiteral(body, before, after, key) {
			return nil, false
		}
		if _, dup := values[key]; dup {
			return nil, false
		}
		val, err := dec.Token()
		if err != nil {
			return nil, false
		}
		if probeDockerIntegerKeys[key] {
			var n int64
			if !takeStageInt64(&n, val) {
				return nil, false
			}
			values[key] = n
			continue
		}
		s, ok := val.(string)
		if !ok {
			return nil, false
		}
		values[key] = s
	}
	if end, err := dec.Token(); err != nil || end != json.Delim('}') || len(values) != len(probeDockerHeaderKeys) {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return values, true
}

// EncodeProbeImagePayloadPrefix writes the PHXIMG1 prefix for an image_cache
// payload of size bytes, or nil for an out-of-range size.
func EncodeProbeImagePayloadPrefix(size int64) []byte {
	if size <= 0 || size > domain.MaxProbeImageArchiveBytes {
		return nil
	}
	return binary.BigEndian.AppendUint64([]byte(imagePayloadMagic), uint64(size))
}

// ProbeImagePayloadSize reads an exact 16-byte payload prefix and returns the
// declared size, which must be in range and equal want (the intent's
// payload size).
func ProbeImagePayloadSize(prefix []byte, want int64) (int64, error) {
	if len(prefix) != ProbeImagePayloadPrefixSize || string(prefix[:len(imagePayloadMagic)]) != imagePayloadMagic {
		return 0, domain.ErrProbeBinaryStageWriteInput
	}
	n := binary.BigEndian.Uint64(prefix[len(imagePayloadMagic):])
	if n == 0 || n > uint64(domain.MaxProbeImageArchiveBytes) || int64(n) != want {
		return 0, domain.ErrProbeBinaryStageWriteInput
	}
	return int64(n), nil
}
