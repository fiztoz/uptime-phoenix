package provisioning

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/testutil"
)

// writeImageLayout writes the fixture as an operator OCI layout beneath dir
// and returns the layout directory.
func writeImageLayout(t *testing.T, dir string, f *testutil.ProbeImageContentFixture) string {
	t.Helper()
	layout := filepath.Join(dir, domain.ProbeImageLayoutDirName(f.Pin))
	blobs := filepath.Join(layout, "blobs", "sha256")
	if err := os.MkdirAll(blobs, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(b []byte) {
		if err := os.WriteFile(filepath.Join(blobs, testutil.ImageSHA256(b)), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(f.IndexBytes)
	write(f.ManifestBytes)
	write(f.ConfigBytes)
	for _, layer := range f.Layers {
		write(layer)
	}
	return layout
}

func TestOpenProbeImageLayoutFeedsTheVerifier(t *testing.T) {
	for _, docker := range []bool{false, true} {
		f := testutil.NewProbeImageContentFixture(t, "amd64", docker, true, 2)
		dir := t.TempDir()
		writeImageLayout(t, dir, f)
		root, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		inputs, err := openProbeImageLayout(root, f.Pin)
		if err != nil {
			t.Fatalf("docker=%v open: %v", docker, err)
		}
		got, err := NewProbeImageContentVerifier().VerifyImageContent(t.Context(), f.Pin, inputs)
		if err != nil || got == nil || got.Selection.LayerCount != 2 || got.Selection.Pin != f.Pin {
			t.Fatalf("docker=%v verify: %+v %v", docker, got, err)
		}
		_ = root.Close()
	}
}

func TestOpenProbeImageLayoutRefusals(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 1)
	for name, setup := range map[string]func(t *testing.T, dir string) domain.ProbeImagePin{
		"no layout directory": func(*testing.T, string) domain.ProbeImagePin { return f.Pin },
		"missing layer blob": func(t *testing.T, dir string) domain.ProbeImagePin {
			layout := writeImageLayout(t, dir, f)
			if err := os.Remove(filepath.Join(layout, "blobs", "sha256", testutil.ImageSHA256(f.Layers[0]))); err != nil {
				t.Fatal(err)
			}
			return f.Pin
		},
		"no manifest for the architecture": func(t *testing.T, dir string) domain.ProbeImagePin {
			writeImageLayout(t, dir, f)
			pin := f.Pin
			pin.Architecture = "arm64"
			return pin
		},
		"layer replaced by a directory": func(t *testing.T, dir string) domain.ProbeImagePin {
			layout := writeImageLayout(t, dir, f)
			path := filepath.Join(layout, "blobs", "sha256", testutil.ImageSHA256(f.Layers[0]))
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
			return f.Pin
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			pin := setup(t, dir)
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			if _, err := openProbeImageLayout(root, pin); err != domain.ErrProbeInstallArtifact {
				t.Fatalf("err %v, want ErrProbeInstallArtifact", err)
			}
		})
	}
}

func TestNewDockerInstallSessionPacksTheVerifiedImage(t *testing.T) {
	f := testutil.NewProbeImageContentFixture(t, "amd64", false, true, 2)
	dir := t.TempDir()
	writeImageLayout(t, dir, f)
	factory := sessionTFactory(t, dir)
	key, signer := deploymentKey(t)
	_, host := deploymentKey(t)
	session, fingerprint, selection, err := factory.NewDockerInstallSession(t.Context(), key, ssh.FingerprintSHA256(host.PublicKey()), f.Pin)
	if err != nil || session == nil {
		t.Fatalf("session: %v", err)
	}
	defer func() { _ = session.Close(t.Context()) }()
	if fingerprint != ssh.FingerprintSHA256(signer.PublicKey()) || !selection.Valid() || selection.Pin != f.Pin || selection.LayerCount != 2 {
		t.Fatalf("fingerprint %q selection %+v", fingerprint, selection)
	}
	executor := session.(*sshProbeInstallSession).SSHProbeInstallExecutor
	d := executor.archive.Descriptor()
	if !d.Valid() || d.Content.Selection != selection {
		t.Fatalf("archive descriptor %+v", d)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A tampered layer is an artifact refusal before any session exists.
	layout := filepath.Join(dir, domain.ProbeImageLayoutDirName(f.Pin))
	if err := os.WriteFile(filepath.Join(layout, "blobs", "sha256", testutil.ImageSHA256(f.Layers[0])), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, _, _, err := factory.NewDockerInstallSession(t.Context(), key, ssh.FingerprintSHA256(host.PublicKey()), f.Pin); s != nil || err != domain.ErrProbeInstallArtifact {
		t.Fatalf("tampered layer: %v %v", s, err)
	}
	if s, _, _, err := factory.NewDockerInstallSession(t.Context(), []byte("not a key"), ssh.FingerprintSHA256(host.PublicKey()), f.Pin); s != nil || err != domain.ErrProbeSSHInput {
		t.Fatalf("bad key: %v %v", s, err)
	}
}
