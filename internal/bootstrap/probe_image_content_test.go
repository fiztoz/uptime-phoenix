package bootstrap

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/testutil"
)

// imageContentArgs writes fixture metadata and ordered layer files and returns the
// exact CLI argv plus every source path in open order (index, manifest, config, layers).
func imageContentArgs(t *testing.T, f *testutil.ProbeImageContentFixture) ([]string, []string) {
	t.Helper()
	dir := t.TempDir()
	names := []string{"index", "manifest", "config"}
	blobs := [][]byte{f.IndexBytes, f.ManifestBytes, f.ConfigBytes}
	paths := make([]string, 0, 3+len(f.Layers))
	for i, b := range blobs {
		p := filepath.Join(dir, names[i])
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	for i, b := range f.Layers {
		p := filepath.Join(dir, "layer-"+strconv.Itoa(i))
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	args := make([]string, 0, 16+2*(len(paths)-3))
	args = append(args, "--repository", f.Pin.Repository, "--version", f.Pin.Version, "--architecture", f.Pin.Architecture, "--index-sha256", f.Pin.IndexSHA256, "--index-size", strconv.FormatInt(f.Pin.IndexSize, 10), "--index", paths[0], "--manifest", paths[1], "--config", paths[2])
	for _, p := range paths[3:] {
		args = append(args, "--layer", p)
	}
	return args, paths
}

func imageContentSetFlag(args []string, name, value string) []string {
	out := append([]string(nil), args...)
	for i, a := range out {
		if a == "--"+name && i+1 < len(out) {
			out[i+1] = value
			return out
		}
	}
	return append(out, "--"+name, value)
}

func imageContentBlobs(f *testutil.ProbeImageContentFixture) [][]byte {
	return append([][]byte{f.IndexBytes, f.ManifestBytes, f.ConfigBytes}, f.Layers...)
}

func TestProbeImageContentVerificationCLI(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("local image sources are supported on Linux/macOS only")
	}
	for _, tc := range []struct {
		name         string
		arch         string
		docker, gzip bool
		layers       int
		multiMember  bool
	}{
		{name: "oci_gzip_two_layers", arch: "amd64", gzip: true, layers: 2},
		{name: "oci_tar_one_layer", arch: "arm64", layers: 1},
		{name: "docker_gzip_one_layer", arch: "arm64", docker: true, gzip: true, layers: 1},
		{name: "docker_tar_two_layers", arch: "amd64", docker: true, layers: 2},
		{name: "oci_gzip_multimember", arch: "amd64", gzip: true, layers: 1, multiMember: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := testutil.NewProbeImageContentFixture(t, tc.arch, tc.docker, tc.gzip, tc.layers)
			if tc.multiMember {
				halves := make([][]byte, 2)
				halves[0], halves[1] = f.Uncompressed[0][:len(f.Uncompressed[0])/2], f.Uncompressed[0][len(f.Uncompressed[0])/2:]
				var joined bytes.Buffer
				for _, part := range halves {
					g := gzip.NewWriter(&joined)
					if _, err := g.Write(part); err != nil {
						t.Fatal(err)
					}
					if err := g.Close(); err != nil {
						t.Fatal(err)
					}
				}
				f.Layers[0] = append([]byte(nil), joined.Bytes()...)
				f.RebindContent(t)
			}
			args, paths := imageContentArgs(t, f)
			var out, stderr bytes.Buffer
			if code := RunProbeImageContentVerification(t.Context(), args, &out, &stderr); code != 0 || stderr.Len() != 0 {
				t.Fatalf("verification failed: %d %s", code, stderr.String())
			}
			var view map[string]json.RawMessage
			if err := json.Unmarshal(out.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			layerBytes, uncompressed := int64(0), int64(0)
			for i := range f.Layers {
				layerBytes += int64(len(f.Layers[i]))
				uncompressed += int64(len(f.Uncompressed[i]))
			}
			if len(view) != 19 ||
				string(view["state"]) != `"image_content_digests_verified"` ||
				string(view["version_source"]) != `"digest_bound_config_label"` ||
				string(view["layer_digests_verified"]) != "true" ||
				string(view["diff_ids_verified"]) != "true" ||
				string(view["rootfs_verified"]) != "false" ||
				string(view["runtime_version_verified"]) != "false" ||
				string(view["content_retained"]) != "false" ||
				string(view["layer_count"]) != strconv.Itoa(tc.layers) ||
				string(view["layer_bytes"]) != strconv.FormatInt(layerBytes, 10) ||
				string(view["uncompressed_bytes"]) != strconv.FormatInt(uncompressed, 10) ||
				string(view["image_reference"]) != `"`+f.Pin.Repository+"@sha256:"+testutil.ImageSHA256(f.ManifestBytes)+`"` {
				t.Fatal("invented or unsafe DTO", out.String())
			}
			for i, path := range paths {
				if bytes.Contains(out.Bytes(), []byte(path)) {
					t.Fatal("source path leaked")
				}
				b, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(b, imageContentBlobs(f)[i]) {
					t.Fatal("source changed")
				}
			}
			if strings.Contains(out.String(), "sensitive-") {
				t.Fatal("raw configuration escaped")
			}
		})
	}
}

func TestProbeImageContentVerificationCLIRefusals(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("local image sources are supported on Linux/macOS only")
	}
	for _, kind := range []string{"help", "invalid_pin", "bad_flag", "trailing", "no_layer", "excess_layer", "empty_layer", "missing_source", "missing_layer", "oversize_layer", "bad_layer_digest", "bad_layer_size", "corrupt_gzip", "truncated_gzip", "trailing_gzip", "bad_diff_id", "zstd_chain", "over_limit_size", "layer_count_mismatch", "canceled", "nil_context"} {
		t.Run(kind, func(t *testing.T) {
			f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
			var ctx context.Context = t.Context()
			want := 1
			var allowed []string
			args, paths := imageContentArgs(t, f)
			argv := args
			switch kind {
			case "help":
				argv = []string{"--help"}
				want = 0
			case "invalid_pin":
				argv = imageContentSetFlag(args, "repository", "probe:latest")
				want = 2
			case "bad_flag":
				argv = []string{"--unknown=sensitive-flag-marker"}
				want = 2
			case "trailing":
				argv = append(append([]string(nil), args...), "extra")
				want = 2
			case "no_layer":
				argv = args[:len(args)-2]
				want = 2
			case "excess_layer":
				argv = args[:len(args)-2]
				for i := 0; i <= domain.MaxProbeImageLayers; i++ {
					argv = append(argv, "--layer", paths[3])
				}
				want = 2
			case "empty_layer":
				argv = imageContentSetFlag(args, "layer", "")
				want = 2
			case "missing_source":
				argv = imageContentSetFlag(args, "config", filepath.Join(t.TempDir(), "sensitive-source-marker"))
			case "missing_layer":
				argv = imageContentSetFlag(args, "layer", filepath.Join(t.TempDir(), "sensitive-layer-marker"))
				allowed = []string{domain.ErrProbeImageInput.Error(), domain.ErrProbeImageUnavailable.Error()}
			case "oversize_layer":
				if err := os.Truncate(paths[3], domain.MaxProbeImageLayerBytes+1); err != nil {
					t.Fatal(err)
				}
				allowed = []string{domain.ErrProbeImageInput.Error(), domain.ErrProbeImageContentLimit.Error()}
			case "bad_layer_digest":
				b, err := os.ReadFile(paths[3])
				if err != nil {
					t.Fatal(err)
				}
				b[len(b)-1] ^= 0xff
				if err := os.WriteFile(paths[3], b, 0o600); err != nil {
					t.Fatal(err)
				}
				allowed = []string{domain.ErrProbeImageDigest.Error()}
			case "bad_layer_size":
				f = testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
				f.ChangeManifest(t, func(m map[string]any) {
					layer := m["layers"].([]any)[0].(map[string]any)
					layer["size"] = layer["size"].(int) + 1
				})
				args, _ = imageContentArgs(t, f)
				argv = args
				allowed = []string{domain.ErrProbeImageSize.Error()}
			case "corrupt_gzip":
				b := append([]byte(nil), f.Layers[0]...)
				b[len(b)/2] ^= 0xff
				f.Layers[0] = b
				f.RebindContent(t)
				args, _ = imageContentArgs(t, f)
				argv = args
				allowed = []string{domain.ErrProbeImageContentEncoding.Error()}
			case "truncated_gzip":
				f.Layers[0] = f.Layers[0][:len(f.Layers[0])/2]
				f.RebindContent(t)
				args, _ = imageContentArgs(t, f)
				argv = args
				allowed = []string{domain.ErrProbeImageContentEncoding.Error()}
			case "trailing_gzip":
				f.Layers[0] = append(f.Layers[0], 0x00)
				f.RebindContent(t)
				args, _ = imageContentArgs(t, f)
				argv = args
				allowed = []string{domain.ErrProbeImageContentEncoding.Error()}
			case "bad_diff_id":
				f.ChangeConfig(t, func(c map[string]any) {
					rootfs := c["rootfs"].(map[string]any)
					rootfs["diff_ids"].([]any)[0] = "sha256:" + strings.Repeat("a", 64)
				})
				args, _ = imageContentArgs(t, f)
				argv = args
				allowed = []string{domain.ErrProbeImageContentDiffID.Error()}
			case "zstd_chain":
				f.ChangeManifest(t, func(m map[string]any) {
					m["layers"].([]any)[0].(map[string]any)["mediaType"] = "application/vnd.oci.image.layer.v1.tar+zstd"
				})
				args, _ = imageContentArgs(t, f)
				argv = args
				allowed = []string{domain.ErrProbeImageContentEncoding.Error()}
			case "over_limit_size":
				f.ChangeManifest(t, func(m map[string]any) {
					m["layers"].([]any)[0].(map[string]any)["size"] = int(domain.MaxProbeImageLayerBytes + 1)
				})
				args, _ = imageContentArgs(t, f)
				argv = args
				allowed = []string{domain.ErrProbeImageContentLimit.Error()}
			case "layer_count_mismatch":
				argv = append(append([]string(nil), args...), "--layer", paths[3])
			case "canceled":
				canceled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = canceled
				allowed = []string{"Probe image content verification canceled or timed out"}
			case "nil_context":
				ctx = nil
				allowed = []string{domain.ErrProbeImageInput.Error()}
			}
			var out, stderr bytes.Buffer
			code := RunProbeImageContentVerification(ctx, argv, &out, &stderr)
			if code != want {
				t.Fatalf("exit=%d want=%d: %s", code, want, stderr.String())
			}
			if want == 0 {
				if stderr.Len() != 0 || !strings.Contains(out.String(), "No registry, daemon") {
					t.Fatal("help did not stay local and fixed")
				}
				return
			}
			if out.Len() != 0 {
				t.Fatal("failure emitted success payload")
			}
			if strings.Contains(stderr.String(), "sensitive-") {
				t.Fatal("private diagnostics escaped")
			}
			if want == 2 {
				return
			}
			message := strings.TrimSpace(stderr.String())
			if len(allowed) > 0 {
				found := false
				for _, want := range allowed {
					if message == want {
						found = true
					}
				}
				if !found {
					t.Fatalf("message %q not in fixed safe set %v", message, allowed)
				}
				return
			}
			for _, safe := range []string{domain.ErrProbeImageInput.Error(), domain.ErrProbeImageSize.Error(), domain.ErrProbeImageDigest.Error(), domain.ErrProbeImageMetadata.Error(), domain.ErrProbeImageUnavailable.Error(), domain.ErrProbeImageContentLimit.Error(), domain.ErrProbeImageContentEncoding.Error(), domain.ErrProbeImageContentDiffID.Error(), "Probe image content verification failed", "Probe image content verification canceled or timed out"} {
				if message == safe {
					return
				}
			}
			t.Fatalf("unfixed diagnostic %q", message)
		})
	}
}

type imageContentFailWriter struct{}

func (imageContentFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("sensitive-writer-marker")
}

func TestProbeImageContentVerificationOutputError(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("local image sources are supported on Linux/macOS only")
	}
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
	args, _ := imageContentArgs(t, f)
	var stderr bytes.Buffer
	if code := RunProbeImageContentVerification(t.Context(), args, imageContentFailWriter{}, &stderr); code != 1 || strings.Contains(stderr.String(), "sensitive-") {
		t.Fatal("output failure not sanitized")
	}
}

// TestProbeImageContentVerificationCLIClosesSources proves partial opens and delegated
// streams are closed on every path: a leaking CLI would grow the fd table per run.
func TestProbeImageContentVerificationCLIClosesSources(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("local image sources are supported on Linux/macOS only")
	}
	// macOS /dev/fd is not reliably enumerable. Fstat probes use no new
	// descriptors and keep this effect assertion active on both supported OSes.
	var limit syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &limit); err != nil {
		t.Fatal(err)
	}
	bound := min(limit.Cur, uint64(65536))
	countFDs := func() int {
		count := 0
		for fd := uint64(0); fd < bound; fd++ {
			var stat syscall.Stat_t
			err := syscall.Fstat(int(fd), &stat)
			if err == nil {
				count++
			} else if err != syscall.EBADF {
				t.Fatal(err)
			}
		}
		if uint64(count) == bound {
			t.Fatal("FD probe range exhausted; cannot establish closure coverage")
		}
		return count
	}
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 2)
	args, paths := imageContentArgs(t, f)
	broken := imageContentSetFlag(args, "layer", filepath.Join(t.TempDir(), "sensitive-layer-marker"))
	var out, stderr bytes.Buffer
	if code := RunProbeImageContentVerification(t.Context(), args, &out, &stderr); code != 0 {
		t.Fatalf("warm success failed: %d %s", code, stderr.String())
	}
	if code := RunProbeImageContentVerification(t.Context(), broken, &out, &stderr); code != 1 {
		t.Fatal("warm failure failed")
	}
	before := countFDs()
	for i := 0; i < 32; i++ {
		out.Reset()
		stderr.Reset()
		if code := RunProbeImageContentVerification(t.Context(), args, &out, &stderr); code != 0 {
			t.Fatalf("success run leaked error: %d %s", code, stderr.String())
		}
		out.Reset()
		stderr.Reset()
		if code := RunProbeImageContentVerification(t.Context(), broken, &out, &stderr); code != 1 {
			t.Fatal("failure run returned success")
		}
	}
	if after := countFDs(); after > before {
		t.Fatalf("verification leaked file descriptors: %d -> %d", before, after)
	}
	for i, path := range paths {
		b, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(b, imageContentBlobs(f)[i]) {
			t.Fatal("source changed")
		}
	}
}
