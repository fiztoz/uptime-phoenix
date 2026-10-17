package provisioning

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/testutil"
)

type imageTrackedInput struct {
	io.Reader
	closed  atomic.Int32
	closeFn func() error
}

func (r *imageTrackedInput) Close() error {
	r.closed.Add(1)
	if r.closeFn != nil {
		return r.closeFn()
	}
	return nil
}
func imageInputs(f *testutil.ProbeImageFixture) (ports.ProbeImageInputs, []*imageTrackedInput) {
	all := []*imageTrackedInput{{Reader: bytes.NewReader(f.IndexBytes)}, {Reader: bytes.NewReader(f.ManifestBytes)}, {Reader: bytes.NewReader(f.ConfigBytes)}}
	return ports.ProbeImageInputs{Index: all[0], Manifest: all[1], Config: all[2]}, all
}
func assertImageInputsClosed(t *testing.T, all []*imageTrackedInput) {
	t.Helper()
	for i, r := range all {
		if r.closed.Load() != 1 {
			t.Fatalf("input %d closed %d times", i, r.closed.Load())
		}
	}
}
func TestProbeImageMetadataChains(t *testing.T) {
	for _, arch := range []string{"amd64", "arm64"} {
		for _, docker := range []bool{false, true} {
			format := "oci"
			if docker {
				format = "docker"
			}
			t.Run(arch+"/"+format, func(t *testing.T) {
				f := testutil.NewProbeImageFixture(t, arch, docker)
				// BuildKit appends attestation manifests with unknown/unknown platforms.
				f.ChangeIndex(t, func(o map[string]any) {
					o["manifests"] = append(o["manifests"].([]any), map[string]any{"mediaType": probeOCIManifest, "digest": "sha256:" + strings.Repeat("f", 64), "size": 100, "platform": map[string]any{"os": "unknown", "architecture": "unknown"}, "annotations": map[string]any{"vnd.docker.reference.type": "attestation-manifest"}}, map[string]any{"mediaType": probeOCIManifest, "digest": "sha256:" + strings.Repeat("e", 64), "size": 100, "platform": map[string]any{"os": "linux", "architecture": "s390x"}})
				})
				if arch == "arm64" {
					f.ChangeIndex(t, func(o map[string]any) {
						o["manifests"].([]any)[0].(map[string]any)["platform"].(map[string]any)["variant"] = "v8"
					})
					f.ChangeConfig(t, func(o map[string]any) { o["variant"] = "v8" })
				}
				before := [][]byte{bytes.Clone(f.IndexBytes), bytes.Clone(f.ManifestBytes), bytes.Clone(f.ConfigBytes)}
				in, all := imageInputs(f)
				result, err := NewProbeImageMetadataPreparer().PrepareImage(t.Context(), f.Pin, in)
				if err != nil || result == nil || !result.Valid() || result.Pin != f.Pin || result.ManifestSHA256 != testutil.ImageSHA256(f.ManifestBytes) || result.ConfigSHA256 != testutil.ImageSHA256(f.ConfigBytes) || result.LayerCount != 1 || result.Reference() != f.Pin.Repository+"@sha256:"+testutil.ImageSHA256(f.ManifestBytes) {
					t.Fatalf("exact verified chain lost: %v", err)
				}
				assertImageInputsClosed(t, all)
				for i, b := range [][]byte{f.IndexBytes, f.ManifestBytes, f.ConfigBytes} {
					if !bytes.Equal(b, before[i]) {
						t.Fatal("borrowed source bytes mutated")
					}
				}
			})
		}
	}
}

func TestProbeImageDigestAndSizeRefusals(t *testing.T) {
	for _, stage := range []string{"index", "manifest", "config"} {
		for _, kind := range []string{"digest", "short", "long"} {
			t.Run(stage+"/"+kind, func(t *testing.T) {
				f := testutil.NewProbeImageFixture(t, "amd64", false)
				target := &f.IndexBytes
				if stage == "manifest" {
					target = &f.ManifestBytes
				}
				if stage == "config" {
					target = &f.ConfigBytes
				}
				want := domain.ErrProbeImageSize
				switch kind {
				case "digest":
					// Keep valid, structurally acceptable JSON: only digest authority changes.
					offset := bytes.Index(*target, []byte("sha256:")) + len("sha256:")
					if (*target)[offset] == 'a' {
						(*target)[offset] = 'b'
					} else {
						(*target)[offset] = 'a'
					}
					want = domain.ErrProbeImageDigest
				case "short":
					*target = (*target)[:len(*target)-1]
				case "long":
					*target = append(*target, ' ')
				}
				in, all := imageInputs(f)
				got, err := NewProbeImageMetadataPreparer().PrepareImage(t.Context(), f.Pin, in)
				if got != nil || !errors.Is(err, want) {
					t.Fatalf("%s/%s bypassed exact bytes: %v", stage, kind, err)
				}
				assertImageInputsClosed(t, all)
			})
		}
	}
}

func TestProbeImageMetadataRefusals(t *testing.T) {
	cases := map[string]func(*testutil.ProbeImageFixture){
		"index_schema": func(f *testutil.ProbeImageFixture) {
			f.ChangeIndex(t, func(o map[string]any) { o["schemaVersion"] = 1 })
		},
		"index_type": func(f *testutil.ProbeImageFixture) {
			f.ChangeIndex(t, func(o map[string]any) { o["mediaType"] = probeOCIManifest })
		},
		"index_subject": func(f *testutil.ProbeImageFixture) { f.ChangeIndex(t, func(o map[string]any) { o["subject"] = nil }) },
		"index_case_alias": func(f *testutil.ProbeImageFixture) {
			f.ChangeIndex(t, func(o map[string]any) { o["SchemaVersion"] = 2 })
		},
		"no_target": func(f *testutil.ProbeImageFixture) { f.Pin.Architecture = "arm64" },
		"duplicate_target": func(f *testutil.ProbeImageFixture) {
			f.ChangeIndex(t, func(o map[string]any) { v := o["manifests"].([]any); o["manifests"] = append(v, v[0]) })
		},
		"unsupported_variant": func(f *testutil.ProbeImageFixture) {
			f.ChangeIndex(t, func(o map[string]any) {
				o["manifests"].([]any)[0].(map[string]any)["platform"].(map[string]any)["variant"] = "v3"
			})
		},
		"variant_case_alias": func(f *testutil.ProbeImageFixture) {
			f.ChangeIndex(t, func(o map[string]any) {
				o["manifests"].([]any)[0].(map[string]any)["platform"].(map[string]any)["Variant"] = "v3"
			})
		},
		"platform_features": func(f *testutil.ProbeImageFixture) {
			f.ChangeIndex(t, func(o map[string]any) {
				o["manifests"].([]any)[0].(map[string]any)["platform"].(map[string]any)["os.features"] = []any{"unsupported"}
			})
		},
		"descriptor_urls": func(f *testutil.ProbeImageFixture) {
			f.ChangeIndex(t, func(o map[string]any) {
				o["manifests"].([]any)[0].(map[string]any)["urls"] = []any{"https://untrusted.invalid/blob"}
			})
		},
		"embedded_descriptor": func(f *testutil.ProbeImageFixture) {
			f.ChangeIndex(t, func(o map[string]any) { o["manifests"].([]any)[0].(map[string]any)["data"] = "e30=" })
		},
		"huge_manifest": func(f *testutil.ProbeImageFixture) {
			f.ChangeIndex(t, func(o map[string]any) {
				o["manifests"].([]any)[0].(map[string]any)["size"] = domain.MaxProbeImageMetadataSize + 1
			})
		},
		"manifest_type": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) { o["mediaType"] = probeDockerManifest })
		},
		"manifest_artifact": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) { o["artifactType"] = "application/example" })
		},
		"manifest_subject": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) { o["subject"] = nil })
		},
		"config_type": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) { o["config"].(map[string]any)["mediaType"] = probeDockerConfig })
		},
		"config_urls": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) { o["config"].(map[string]any)["urls"] = nil })
		},
		"huge_config": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) { o["config"].(map[string]any)["size"] = domain.MaxProbeImageMetadataSize + 1 })
		},
		"foreign_layer": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) {
				o["layers"].([]any)[0].(map[string]any)["mediaType"] = "application/vnd.docker.image.rootfs.foreign.diff.tar.gzip"
			})
		},
		"layer_urls": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) {
				o["layers"].([]any)[0].(map[string]any)["urls"] = []any{"https://untrusted.invalid/blob"}
			})
		},
		"layer_bad_digest": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) {
				o["layers"].([]any)[0].(map[string]any)["digest"] = "sha256:" + strings.Repeat("Z", 64)
			})
		},
		"layer_size": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) { o["layers"].([]any)[0].(map[string]any)["size"] = int64(16<<30) + 1 })
		},
		"layer_zero": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) { o["layers"].([]any)[0].(map[string]any)["size"] = 0 })
		},
		"zero_layers": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) { o["layers"] = []any{} })
		},
		"too_many_layers": func(f *testutil.ProbeImageFixture) {
			f.ChangeManifest(t, func(o map[string]any) {
				a := o["layers"].([]any)
				for len(a) <= domain.MaxProbeImageLayers {
					a = append(a, a[0])
				}
				o["layers"] = a
			})
		},
	}
	configCases := map[string]func(map[string]any){
		"config_os":           func(o map[string]any) { o["os"] = "windows" },
		"config_architecture": func(o map[string]any) { o["architecture"] = "arm64" },
		"config_variant":      func(o map[string]any) { o["variant"] = "v3" },
		"config_case_alias":   func(o map[string]any) { o["Architecture"] = "amd64" },
		"rootfs_type":         func(o map[string]any) { o["rootfs"].(map[string]any)["type"] = "other" },
		"rootfs_count":        func(o map[string]any) { o["rootfs"].(map[string]any)["diff_ids"] = []any{} },
		"rootfs_digest":       func(o map[string]any) { o["rootfs"].(map[string]any)["diff_ids"] = []any{"sha512:abc"} },
	}
	runtimeCases := map[string]func(map[string]any){
		"root_user":          func(o map[string]any) { o["User"] = "0" },
		"entrypoint":         func(o map[string]any) { o["Entrypoint"] = []any{"/bin/sh"} },
		"entrypoint_args":    func(o map[string]any) { o["Entrypoint"] = []any{"/uptime-phoenix-probe", "run"} },
		"default_command":    func(o map[string]any) { o["Cmd"] = []any{"run"} },
		"healthcheck":        func(o map[string]any) { o["Healthcheck"] = map[string]any{"Test": []any{"CMD", "other"}} },
		"anonymous_volume":   func(o map[string]any) { o["Volumes"] = map[string]any{"/untracked": map[string]any{}} },
		"onbuild":            func(o map[string]any) { o["OnBuild"] = []any{"RUN other"} },
		"runtime_case_alias": func(o map[string]any) { o["entrypoint"] = []any{"/bin/sh"} },
		"component": func(o map[string]any) {
			o["Labels"].(map[string]any)["org.opencontainers.image.title"] = "Uptime Phoenix"
		},
		"source": func(o map[string]any) {
			o["Labels"].(map[string]any)["org.opencontainers.image.source"] = "https://untrusted.invalid"
		},
		"version": func(o map[string]any) { o["Labels"].(map[string]any)["org.opencontainers.image.version"] = "0.4.6" },
	}
	for name, fn := range configCases {
		cases[name] = func(f *testutil.ProbeImageFixture) { f.ChangeConfig(t, fn) }
	}
	for name, fn := range runtimeCases {
		cases[name] = func(f *testutil.ProbeImageFixture) {
			f.ChangeConfig(t, func(o map[string]any) { fn(o["config"].(map[string]any)) })
		}
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := testutil.NewProbeImageFixture(t, "amd64", false)
			change(f)
			in, all := imageInputs(f)
			got, err := NewProbeImageMetadataPreparer().PrepareImage(t.Context(), f.Pin, in)
			if got != nil || !errors.Is(err, domain.ErrProbeImageMetadata) {
				t.Fatalf("invalid metadata accepted: %v", err)
			}
			if strings.Contains(err.Error(), "sensitive-") {
				t.Fatal("raw config diagnostic escaped")
			}
			assertImageInputsClosed(t, all)
		})
	}
}

func TestProbeImageStrictJSON(t *testing.T) {
	for name, raw := range map[string][]byte{
		"duplicate":         []byte(`{"os":"linux","os":"linux"}`),
		"escaped_duplicate": []byte(`{"os":"linux","\u006fs":"linux"}`),
		"nested_duplicate":  []byte(`{"x":{"a":1,"a":2}}`),
		"trailing":          []byte(`{} {}`),
		"utf8":              append([]byte(`{"x":"`), 0xff, '"', '}'),
		"deep":              []byte(strings.Repeat("[", 34) + strings.Repeat("]", 34)),
		"tokens":            []byte(`{"x":[` + strings.Repeat("0,", 66000) + "0]}"),
		"null":              []byte("null"),
		"array":             []byte("[]"),
	} {
		for _, stage := range []string{"index", "manifest", "config"} {
			t.Run(name+"/"+stage, func(t *testing.T) {
				f := testutil.NewProbeImageFixture(t, "amd64", false)
				original, key, value := f.IndexBytes, "schemaVersion", "2"
				if stage == "manifest" {
					original = f.ManifestBytes
				} else if stage == "config" {
					original, key, value = f.ConfigBytes, "os", `"linux"`
				}
				mutated := raw
				// Keep the rest of the executable chain valid so the JSON guard,
				// not an unrelated missing metadata field, must refuse the input.
				switch name {
				case "duplicate", "escaped_duplicate":
					if name == "escaped_duplicate" {
						if key == "os" {
							key = `\u006fs`
						} else {
							key = `sch\u0065maVersion`
						}
					}
					mutated = []byte(`{"` + key + `":` + value + `,` + string(original[1:]))
				case "nested_duplicate", "deep", "tokens":
					mutated = []byte(string(original[:len(original)-1]) + `,"extension":` + string(raw) + `}`)
				}
				switch stage {
				case "index":
					f.SetIndexBytes(mutated)
				case "manifest":
					f.SetManifestBytes(t, mutated)
				case "config":
					f.SetConfigBytes(t, mutated)
				}
				in, all := imageInputs(f)
				got, err := NewProbeImageMetadataPreparer().PrepareImage(t.Context(), f.Pin, in)
				if got != nil || !errors.Is(err, domain.ErrProbeImageMetadata) {
					t.Fatalf("unsafe JSON accepted: %v", err)
				}
				assertImageInputsClosed(t, all)
			})
		}
	}
	// Unknown bounded extensions are ignored, never reinterpreted as runtime data.
	f := testutil.NewProbeImageFixture(t, "amd64", false)
	f.ChangeConfig(t, func(o map[string]any) { o["extension"] = map[string]any{"uninterpreted": "value"} })
	in, _ := imageInputs(f)
	if _, err := NewProbeImageMetadataPreparer().PrepareImage(t.Context(), f.Pin, in); err != nil {
		t.Fatal(err)
	}
}

type imageReadSignaler struct {
	r       io.Reader
	started chan struct{}
	once    atomic.Bool
}

func (r *imageReadSignaler) Read(b []byte) (int, error) {
	if r.once.CompareAndSwap(false, true) {
		close(r.started)
	}
	return r.r.Read(b)
}

func TestProbeImageInputOwnershipAndCancellation(t *testing.T) {
	for _, stage := range []string{"index", "manifest", "config"} {
		t.Run(stage, func(t *testing.T) {
			f := testutil.NewProbeImageFixture(t, "amd64", false)
			in, all := imageInputs(f)
			r, w := io.Pipe()
			defer w.Close()
			started := make(chan struct{})
			blocked := &imageTrackedInput{Reader: &imageReadSignaler{r: r, started: started}, closeFn: r.Close}
			switch stage {
			case "index":
				in.Index = blocked
				all[0] = blocked
			case "manifest":
				in.Manifest = blocked
				all[1] = blocked
			case "config":
				in.Config = blocked
				all[2] = blocked
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				got, err := NewProbeImageMetadataPreparer().PrepareImage(ctx, f.Pin, in)
				if got != nil {
					err = errors.New("canceled image returned a result")
				}
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("input not reached")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("not canceled: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation did not close/join blocked input")
			}
			assertImageInputsClosed(t, all)
		})
	}
	for _, kind := range []string{"nil_context", "canceled", "invalid_pin", "nil_stream"} {
		t.Run(kind, func(t *testing.T) {
			f := testutil.NewProbeImageFixture(t, "amd64", false)
			in, all := imageInputs(f)
			ctx := t.Context()
			switch kind {
			case "nil_context":
				ctx = nil
			case "canceled":
				c, cancel := context.WithCancel(ctx)
				cancel()
				ctx = c
			case "invalid_pin":
				f.Pin.Version = "latest"
			case "nil_stream":
				in.Manifest = nil
				all = append(all[:1], all[2:]...)
			}
			if got, err := NewProbeImageMetadataPreparer().PrepareImage(ctx, f.Pin, in); got != nil || err == nil {
				t.Fatal("bad input succeeded")
			}
			assertImageInputsClosed(t, all)
		})
	}
}

type imageErrorReader struct{}

func (imageErrorReader) Read([]byte) (int, error) { return 0, errors.New("sensitive-source-marker") }

func TestProbeImageSourceErrorSecrecy(t *testing.T) {
	f := testutil.NewProbeImageFixture(t, "amd64", false)
	in, all := imageInputs(f)
	all[0].Reader = imageErrorReader{}
	got, err := NewProbeImageMetadataPreparer().PrepareImage(t.Context(), f.Pin, in)
	if got != nil || err != domain.ErrProbeImageUnavailable || strings.Contains(err.Error(), "sensitive-source-marker") {
		t.Fatal("raw source error escaped")
	}
	assertImageInputsClosed(t, all)
}

type imageParsingContext struct {
	context.Context
	checks atomic.Int32
	cancel context.CancelFunc
}

func (c *imageParsingContext) Err() error {
	if c.checks.Add(1) > 20 {
		c.cancel()
	}
	return c.Context.Err()
}
func TestProbeImageParsingCancellation(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx := &imageParsingContext{Context: parent, cancel: cancel}
	f := testutil.NewProbeImageFixture(t, "amd64", false)
	in, all := imageInputs(f)
	got, err := NewProbeImageMetadataPreparer().PrepareImage(ctx, f.Pin, in)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("parser never checked cancellation", err)
	}
	assertImageInputsClosed(t, all)
}

func TestProbeImageMetadataSourcePolicy(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("local metadata source is supported on Linux/macOS only")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenProbeImageMetadataSource(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	for _, p := range []string{dir, filepath.Join(dir, "missing"), "/dev/null"} {
		if f, err := OpenProbeImageMetadataSource(t.Context(), p); err == nil {
			_ = f.Close()
			t.Fatal("nonregular source accepted")
		}
	}
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(domain.MaxProbeImageMetadataSize + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if f, err := OpenProbeImageMetadataSource(t.Context(), path); err == nil {
		_ = f.Close()
		t.Fatal("oversized metadata source accepted")
	}
}
