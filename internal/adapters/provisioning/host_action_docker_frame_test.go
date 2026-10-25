package provisioning

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func dockerFrameIntent(t *testing.T, kind string) domain.ProbeHostActionIntent {
	t.Helper()
	fp := "SHA256:" + strings.Repeat("A", 43)
	intent := domain.ProbeHostActionIntent{
		Identity: domain.ProbeInstallIdentity{
			OperationID: "11111111-1111-4111-8111-111111111111", Host: domain.ProbeSSHHost{Address: "edge.example:22", User: "root"},
			HostKeyFingerprint: fp, ClientKeyFingerprint: fp, RequestedBy: 7, LayoutVersion: 1, Mode: domain.ProbeInstallDocker,
			Image: domain.ProbeImageSelection{
				Pin:            domain.ProbeImagePin{Repository: "ghcr.io/example/probe", Version: "1.2.3", Architecture: "arm64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 512},
				ManifestSHA256: strings.Repeat("b", 64), ConfigSHA256: strings.Repeat("d", 64), LayerCount: 2,
			},
		},
		OwnerID: "22222222-2222-4222-8222-222222222222", AttemptID: "33333333-3333-4333-8333-333333333333", Kind: kind,
	}
	if kind == domain.ProbeHostActionImageCache {
		intent.Payload = domain.ProbeHostActionPayload{SHA256: strings.Repeat("e", 64), Size: 1 << 20, LayoutIndexSHA256: strings.Repeat("f", 64)}
	}
	if !intent.Valid() {
		t.Fatalf("fixture %s intent invalid", kind)
	}
	return intent
}

// splitDockerFrame returns prefix, kind and header of an encoded frame.
func splitDockerFrame(t *testing.T, frame []byte) ([]byte, []byte, []byte) {
	t.Helper()
	prefix := frame[:ProbeHostActionPrefixSize]
	n, err := ProbeHostActionKindLength(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return prefix, frame[ProbeHostActionPrefixSize : ProbeHostActionPrefixSize+n], frame[ProbeHostActionPrefixSize+n:]
}

func TestDockerHostActionFrameRoundTrip(t *testing.T) {
	for _, kind := range []string{domain.ProbeHostActionImageCache, domain.ProbeHostActionDataDirectory, domain.ProbeHostActionContainer,
		domain.ProbeActivationProbeInit, domain.ProbeActivationServiceStart, domain.ProbeHostActionCleanup} {
		intent := dockerFrameIntent(t, kind)
		frame, err := EncodeProbeHostActionFrame(intent)
		if err != nil || !bytes.HasPrefix(frame, []byte("PHXACT2\n")) {
			t.Fatalf("%s: encode %v %q", kind, err, frame[:min(len(frame), 9)])
		}
		prefix, k, header := splitDockerFrame(t, frame)
		if !ProbeHostActionFrameIsDocker(prefix) {
			t.Fatalf("%s: not detected as v2", kind)
		}
		n, err := ProbeHostActionHeaderLength(header[:ProbeDockerHeaderPrefixSize], true)
		if err != nil || n != len(header)-ProbeDockerHeaderPrefixSize {
			t.Fatalf("%s: header length %d %v", kind, n, err)
		}
		got, err := DecodeProbeHostActionIntent(k, header, true)
		if err != nil || got != intent {
			t.Fatalf("%s: decode %+v %v", kind, got, err)
		}
		// The version must match the identity: a docker header is not a v1 header.
		if _, err := DecodeProbeHostActionIntent(k, header, false); err == nil {
			t.Fatalf("%s: docker header decoded as v1", kind)
		}
	}
	// Systemd identities keep the unchanged v1 frame.
	systemd := dockerFrameIntent(t, domain.ProbeHostActionDataDirectory)
	systemd.Identity.Mode, systemd.Identity.Image = domain.ProbeInstallSystemd, domain.ProbeImageSelection{}
	systemd.Identity.Binary = domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "arm64", SHA256: strings.Repeat("a", 64), Size: 10}
	frame, err := EncodeProbeHostActionFrame(systemd)
	if err != nil || !bytes.HasPrefix(frame, []byte("PHXACT1\n")) {
		t.Fatalf("systemd frame %v", err)
	}
	prefix, k, header := splitDockerFrame(t, frame)
	if ProbeHostActionFrameIsDocker(prefix) {
		t.Fatal("v1 detected as v2")
	}
	if _, err := DecodeProbeHostActionIntent(k, header, true); err == nil {
		t.Fatal("v1 header decoded as docker")
	}
}

func TestDockerHostActionHeaderIsStrict(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeHostActionImageCache)
	frame, _ := EncodeProbeHostActionFrame(intent)
	_, kind, header := splitDockerFrame(t, frame)
	body := string(header[ProbeDockerHeaderPrefixSize:])
	rebuild := func(b string) []byte {
		return append(binary.BigEndian.AppendUint32([]byte("PHXDKR1\n"), uint32(len(b))), b...)
	}
	if _, err := DecodeProbeHostActionIntent(kind, rebuild(body), true); err != nil {
		t.Fatalf("baseline refused: %v", err)
	}
	for name, mutated := range map[string]string{
		"missing key":         strings.Replace(body, `"payload_size":1048576,`, ``, 1),
		"duplicate key":       strings.Replace(body, `{`, `{"mode":"docker",`, 1),
		"unknown key":         strings.Replace(body, `{`, `{"extra":"x",`, 1),
		"case variant":        strings.Replace(body, `"mode"`, `"Mode"`, 1),
		"escaped key":         strings.Replace(body, `"mode"`, `"mod\u0065"`, 1),
		"integer as string":   strings.Replace(body, `"image_layer_count":2`, `"image_layer_count":"2"`, 1),
		"leading zero":        strings.Replace(body, `"image_layer_count":2`, `"image_layer_count":02`, 1),
		"float":               strings.Replace(body, `"image_layer_count":2`, `"image_layer_count":2.0`, 1),
		"nested":              strings.Replace(body, `"mode":"docker"`, `"mode":{"x":1}`, 1),
		"systemd mode":        strings.Replace(body, `"mode":"docker"`, `"mode":"systemd"`, 1),
		"payload size change": strings.Replace(body, `"payload_size":1048576`, `"payload_size":0`, 1),
		"trailing data":       body + " {}",
	} {
		if mutated == body {
			t.Fatalf("%s: mutation did not apply", name)
		}
		if _, err := DecodeProbeHostActionIntent(kind, rebuild(mutated), true); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// A kind that does not carry a payload refuses one in the header.
	if _, err := DecodeProbeHostActionIntent([]byte(domain.ProbeHostActionContainer), rebuild(body), true); err == nil {
		t.Error("container accepted a payload")
	}
	for name, prefix := range map[string][]byte{
		"wrong magic": append([]byte("PHXDKR2\n"), 0, 0, 0, 10),
		"zero length": append([]byte("PHXDKR1\n"), 0, 0, 0, 0),
		"over limit":  binary.BigEndian.AppendUint32([]byte("PHXDKR1\n"), MaxProbeDockerHeaderSize+1),
	} {
		if _, err := ProbeHostActionHeaderLength(prefix, true); err == nil {
			t.Errorf("header prefix %s accepted", name)
		}
	}
}

func TestProbeImagePayloadPrefix(t *testing.T) {
	prefix := EncodeProbeImagePayloadPrefix(4096)
	if len(prefix) != ProbeImagePayloadPrefixSize {
		t.Fatalf("prefix %q", prefix)
	}
	if n, err := ProbeImagePayloadSize(prefix, 4096); err != nil || n != 4096 {
		t.Fatalf("size %d %v", n, err)
	}
	if _, err := ProbeImagePayloadSize(prefix, 4097); err == nil {
		t.Fatal("size other than the intent's accepted")
	}
	if EncodeProbeImagePayloadPrefix(0) != nil || EncodeProbeImagePayloadPrefix(domain.MaxProbeImageArchiveBytes+1) != nil {
		t.Fatal("out-of-range sizes encoded")
	}
	bad := append([]byte("PHXIMG2\n"), prefix[8:]...)
	if _, err := ProbeImagePayloadSize(bad, 4096); err == nil {
		t.Fatal("wrong payload magic accepted")
	}
}
