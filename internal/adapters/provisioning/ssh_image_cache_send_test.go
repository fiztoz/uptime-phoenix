package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// imageCacheArchive is a fake closed archive with fixed bytes.
type imageCacheArchive struct {
	descriptor domain.ProbeImageArchiveDescriptor
	body       []byte
	opens      int
}

func (a *imageCacheArchive) Descriptor() domain.ProbeImageArchiveDescriptor { return a.descriptor }
func (a *imageCacheArchive) Open(context.Context) (io.ReadCloser, error) {
	a.opens++
	return io.NopCloser(bytes.NewReader(a.body)), nil
}
func (a *imageCacheArchive) Close(context.Context) error { return nil }

// dockerImageCacheIntent turns the fixture's systemd identity into a Docker
// image_cache intent for archive.
func dockerImageCacheIntent(t *testing.T, f *sendTFixture, archive *imageCacheArchive) domain.ProbeHostActionIntent {
	t.Helper()
	intent := hostActSendIntentFor(f, domain.ProbeHostActionImageCache)
	intent.Identity.Mode, intent.Identity.Binary = domain.ProbeInstallDocker, domain.ProbeBinaryPin{}
	intent.Identity.Image = archive.descriptor.Content.Selection
	d := archive.descriptor
	intent.Payload = domain.ProbeHostActionPayload{SHA256: d.ArchiveSHA256, Size: d.ArchiveSize, LayoutIndexSHA256: d.LayoutIndexSHA256}
	if !intent.Valid() {
		t.Fatal("docker image_cache fixture intent invalid")
	}
	return intent
}

func newImageCacheArchive(t *testing.T) *imageCacheArchive {
	t.Helper()
	body := bytes.Repeat([]byte{0x5a}, 2048)
	selection := domain.ProbeImageSelection{
		Pin:            domain.ProbeImagePin{Repository: "ghcr.io/example/probe", Version: "1.2.3", Architecture: "arm64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 512},
		ManifestSHA256: strings.Repeat("b", 64), ConfigSHA256: strings.Repeat("d", 64), LayerCount: 1,
	}
	d := domain.ProbeImageArchiveDescriptor{
		Content:           domain.ProbeImageContentVerification{Selection: selection, LayerBytes: 100, UncompressedBytes: 200},
		LayoutIndexSHA256: strings.Repeat("f", 64), LayoutIndexSize: 300,
		ArchiveSHA256: sendTPin(body).SHA256, ArchiveSize: int64(len(body)),
	}
	if !d.Valid() {
		t.Fatal("archive descriptor fixture invalid")
	}
	return &imageCacheArchive{descriptor: d, body: body}
}

func TestSendImageCacheStreamsFrameGrantPrefixAndArchive(t *testing.T) {
	archive := newImageCacheArchive(t)
	var intent domain.ProbeHostActionIntent
	h := newHostActSendHarness(t, "", func(helper *hostActSendHelper, _ *hostActSendAuthorizer) {
		// The emulator parses only v1 frames; this test asserts the exact
		// v2 bytes itself.
		helper.verify = false
		helper.tail = func(*sendTFixture, domain.ProbeHostActionIntent, []byte) string {
			return string(EncodeProbeHostActionReceipt(intent))
		}
	})
	intent = dockerImageCacheIntent(t, h.f, archive)
	h.intent = intent
	ack, err := h.sender.SendImageCache(t.Context(), intent, archive, h.auth)
	if err != nil || ack.Intent != intent {
		t.Fatalf("SendImageCache = %+v %v", ack, err)
	}
	h.helper.waitIdle(t, h.f)
	requests := h.auth.seenRequests()
	if len(requests) != 1 || requests[0].Intent != intent {
		t.Fatalf("authorizer requests %+v", requests)
	}
	frame, err := EncodeProbeHostActionFrame(intent)
	if err != nil || !bytes.HasPrefix(frame, []byte("PHXACT2\n")) {
		t.Fatalf("frame %v", err)
	}
	signature := ed25519.Sign(h.helper.authority.priv, requests[0].SigningMessage())
	want := append(append(append([]byte(nil), frame...), hostActFrameGrantMagic...), signature...)
	want = append(want, EncodeProbeImagePayloadPrefix(int64(len(archive.body)))...)
	want = append(want, archive.body...)
	if got := h.f.stdinBytes(); !bytes.Equal(got, want) {
		t.Fatalf("server stdin = %d bytes, want %d", len(got), len(want))
	}
	if archive.opens != 1 {
		t.Fatalf("archive opened %d times", archive.opens)
	}
}

func TestSendImageCacheRefusesMismatchedArchiveBeforeIO(t *testing.T) {
	h := newHostActSendHarness(t, "", nil)
	archive := newImageCacheArchive(t)
	intent := dockerImageCacheIntent(t, h.f, archive)
	for name, edit := range map[string]func(*domain.ProbeHostActionIntent, *imageCacheArchive){
		"payload digest differs": func(i *domain.ProbeHostActionIntent, _ *imageCacheArchive) {
			i.Payload.SHA256 = strings.Repeat("0", 64)
		},
		"layout index differs": func(i *domain.ProbeHostActionIntent, _ *imageCacheArchive) {
			i.Payload.LayoutIndexSHA256 = strings.Repeat("0", 64)
		},
		"image selection differs": func(i *domain.ProbeHostActionIntent, _ *imageCacheArchive) {
			i.Identity.Image.ConfigSHA256 = strings.Repeat("e", 64)
		},
		"wrong kind": func(i *domain.ProbeHostActionIntent, _ *imageCacheArchive) {
			i.Kind, i.Payload = domain.ProbeHostActionContainer, domain.ProbeHostActionPayload{}
		},
	} {
		i, a := intent, *archive
		edit(&i, &a)
		if _, err := h.sender.SendImageCache(t.Context(), i, &a, h.auth); err != ErrSSHBinaryStageInput {
			t.Fatalf("%s: err %v", name, err)
		}
	}
	if _, err := h.sender.SendHostAction(t.Context(), intent, h.auth); err != ErrSSHBinaryStageInput {
		t.Fatalf("SendHostAction accepted image_cache: %v", err)
	}
	if h.f.execCalls.Load() != 0 || len(h.auth.seenRequests()) != 0 {
		t.Fatal("a refused image_cache reached the host or the authorizer")
	}
}
