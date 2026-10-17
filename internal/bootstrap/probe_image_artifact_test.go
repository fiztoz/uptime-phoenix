package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/testutil"
)

func imageVerificationArgs(t *testing.T, f *testutil.ProbeImageFixture) ([]string, []string) {
	t.Helper()
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "index"), filepath.Join(dir, "manifest"), filepath.Join(dir, "config")}
	for i, b := range [][]byte{f.IndexBytes, f.ManifestBytes, f.ConfigBytes} {
		if err := os.WriteFile(paths[i], b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return []string{"--repository", f.Pin.Repository, "--version", f.Pin.Version, "--architecture", f.Pin.Architecture, "--index-sha256", f.Pin.IndexSHA256, "--index-size", strconv.FormatInt(f.Pin.IndexSize, 10), "--index", paths[0], "--manifest", paths[1], "--config", paths[2]}, paths
}

func TestProbeImageVerificationCLI(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("local metadata source is supported on Linux/macOS only")
	}
	for _, arch := range []string{"amd64", "arm64"} {
		t.Run(arch, func(t *testing.T) {
			f := testutil.NewProbeImageFixture(t, arch, false)
			args, paths := imageVerificationArgs(t, f)
			var out, stderr bytes.Buffer
			if code := RunProbeImageVerification(t.Context(), args, &out, &stderr); code != 0 || stderr.Len() != 0 {
				t.Fatalf("verification failed: %d %s", code, stderr.String())
			}
			var view map[string]json.RawMessage
			if err := json.Unmarshal(out.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			if len(view) != 14 || string(view["state"]) != `"image_metadata_verified"` || string(view["version_source"]) != `"digest_bound_config_label"` || string(view["layers_verified"]) != "false" || string(view["runtime_version_verified"]) != "false" || string(view["image_reference"]) != `"`+f.Pin.Repository+"@sha256:"+testutil.ImageSHA256(f.ManifestBytes)+`"` {
				t.Fatal("invented or unsafe DTO", out.String())
			}
			for i, path := range paths {
				if bytes.Contains(out.Bytes(), []byte(path)) {
					t.Fatal("source path leaked")
				}
				b, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(b, [][]byte{f.IndexBytes, f.ManifestBytes, f.ConfigBytes}[i]) {
					t.Fatal("source changed")
				}
			}
			if strings.Contains(out.String(), "sensitive-") {
				t.Fatal("raw configuration escaped")
			}
		})
	}
}

func TestProbeImageVerificationCLIRefusals(t *testing.T) {
	f := testutil.NewProbeImageFixture(t, "amd64", false)
	args, _ := imageVerificationArgs(t, f)
	for _, kind := range []string{"help", "invalid_pin", "bad_flag", "trailing", "bad_digest", "bad_size", "missing_source", "bad_version", "canceled", "nil_context"} {
		t.Run(kind, func(t *testing.T) {
			argv := append([]string(nil), args...)
			var ctx context.Context = t.Context()
			want := 1
			switch kind {
			case "help":
				argv = []string{"--help"}
				want = 0
			case "invalid_pin":
				argv[1] = "probe:latest"
				want = 2
			case "bad_flag":
				argv = []string{"--unknown=sensitive-flag-marker"}
				want = 2
			case "trailing":
				argv = append(argv, "extra")
				want = 2
			case "bad_digest":
				argv[7] = strings.Repeat("0", 64)
			case "bad_size":
				argv[9] = strconv.FormatInt(f.Pin.IndexSize+1, 10)
			case "missing_source":
				argv[15] = filepath.Join(t.TempDir(), "sensitive-source-marker")
			case "bad_version":
				argv[3] = "0.4.6"
			case "canceled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "nil_context":
				ctx = nil
			}
			var out, stderr bytes.Buffer
			code := RunProbeImageVerification(ctx, argv, &out, &stderr)
			if code != want {
				t.Fatalf("exit=%d want=%d: %s", code, want, stderr.String())
			}
			if want != 0 && out.Len() != 0 {
				t.Fatal("failure emitted success payload")
			}
			if strings.Contains(stderr.String(), "sensitive-") {
				t.Fatal("private diagnostics escaped")
			}
			if kind == "bad_version" && !strings.Contains(stderr.String(), "metadata contract mismatch") {
				t.Fatal("image config version label was not enforced")
			}
		})
	}
}

type imageFailWriter struct{}

func (imageFailWriter) Write([]byte) (int, error) { return 0, errors.New("sensitive-writer-marker") }
func TestProbeImageVerificationOutputError(t *testing.T) {
	f := testutil.NewProbeImageFixture(t, "amd64", false)
	args, _ := imageVerificationArgs(t, f)
	var stderr bytes.Buffer
	if code := RunProbeImageVerification(t.Context(), args, imageFailWriter{}, &stderr); code != 1 || strings.Contains(stderr.String(), "sensitive-") {
		t.Fatal("output failure not sanitized")
	}
}
