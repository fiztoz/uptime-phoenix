package provisioning

import (
	"bytes"
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type sessionTStage struct{}

func (sessionTStage) Authorize(context.Context, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageGrant, error) {
	return domain.ProbeBinaryStageGrant{}, errors.New("not used")
}

type sessionTActions struct{}

func (sessionTActions) Authorize(context.Context, domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
	return domain.ProbeHostActionGrant{}, errors.New("not used")
}

func sessionTPolicy() probe.EndpointPolicy {
	return probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
}

func sessionTFactory(t *testing.T, dir string) *SSHProbeInstallSessionFactory {
	t.Helper()
	f, err := NewSSHProbeInstallSessionFactory(dir, sessionTPolicy(), sessionTStage{}, sessionTActions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func TestSSHProbeInstallSessionFactoryConstructorRefusals(t *testing.T) {
	dir := t.TempDir()
	var typedStage *struct{ sessionTStage }
	for name, build := range map[string]func() (*SSHProbeInstallSessionFactory, error){
		"empty dir": func() (*SSHProbeInstallSessionFactory, error) {
			return NewSSHProbeInstallSessionFactory("", sessionTPolicy(), sessionTStage{}, sessionTActions{})
		},
		"missing dir": func() (*SSHProbeInstallSessionFactory, error) {
			return NewSSHProbeInstallSessionFactory(filepath.Join(dir, "absent"), sessionTPolicy(), sessionTStage{}, sessionTActions{})
		},
		"nil stage": func() (*SSHProbeInstallSessionFactory, error) {
			return NewSSHProbeInstallSessionFactory(dir, sessionTPolicy(), nil, sessionTActions{})
		},
		"typed-nil stg": func() (*SSHProbeInstallSessionFactory, error) {
			return NewSSHProbeInstallSessionFactory(dir, sessionTPolicy(), typedStage, sessionTActions{})
		},
		"nil actions": func() (*SSHProbeInstallSessionFactory, error) {
			return NewSSHProbeInstallSessionFactory(dir, sessionTPolicy(), sessionTStage{}, nil)
		},
	} {
		if f, err := build(); err == nil || f != nil {
			t.Fatalf("%s: factory=%v err=%v", name, f, err)
		}
	}
}

func TestSSHProbeInstallSessionFactoryBuildsVerifiedSession(t *testing.T) {
	data := buildArtifactFixture(t, "linux", "arm64", "cmd/probe", "0")
	pin := artifactPin(data, "arm64")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pin.ReleaseName()), data, 0o600); err != nil {
		t.Fatal(err)
	}
	f := sessionTFactory(t, dir)
	key, signer := deploymentKey(t)
	_, hostSigner := deploymentKey(t)
	hostFP := ssh.FingerprintSHA256(hostSigner.PublicKey())

	session, clientFP, err := f.NewInstallSession(t.Context(), key, hostFP, pin)
	if err != nil || session == nil {
		t.Fatalf("session: %v", err)
	}
	if clientFP != ssh.FingerprintSHA256(signer.PublicKey()) {
		t.Fatalf("client fingerprint %q", clientFP)
	}
	// The borrowed key buffer is not retained or mutated.
	if !bytes.Contains(key, []byte("PRIVATE KEY")) {
		t.Fatal("key buffer mutated")
	}
	// A kind the receipt does not hold in flight never reaches the network.
	outcome, err := session.ApplyResource(t.Context(), domain.ProbeInstallOperation{}, "data_directory")
	if err != nil || outcome.State != domain.ProbeInstallResourceNotStarted {
		t.Fatalf("apply on empty receipt: %+v %v", outcome, err)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(t.Context()); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestSSHProbeInstallSessionFactoryRejectsArtifacts(t *testing.T) {
	data := buildArtifactFixture(t, "linux", "amd64", "cmd/probe", "0")
	pin := artifactPin(data, "amd64")
	key, _ := deploymentKey(t)
	_, hostSigner := deploymentKey(t)
	hostFP := ssh.FingerprintSHA256(hostSigner.PublicKey())

	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, pin.ReleaseName()), data, 0o600); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(dir string) domain.ProbeBinaryPin{
		"missing release file": func(string) domain.ProbeBinaryPin { return pin },
		"digest mismatch": func(dir string) domain.ProbeBinaryPin {
			_ = os.WriteFile(filepath.Join(dir, pin.ReleaseName()), append([]byte(nil), data[:len(data)-1]...), 0o600)
			return pin
		},
		"wrong architecture pin": func(dir string) domain.ProbeBinaryPin {
			other := pin
			other.Architecture = "arm64"
			_ = os.WriteFile(filepath.Join(dir, other.ReleaseName()), data, 0o600)
			return other
		},
		"directory instead of file": func(dir string) domain.ProbeBinaryPin {
			_ = os.Mkdir(filepath.Join(dir, pin.ReleaseName()), 0o700)
			return pin
		},
		"symlink escaping the artifact dir": func(dir string) domain.ProbeBinaryPin {
			_ = os.Symlink(filepath.Join(outside, pin.ReleaseName()), filepath.Join(dir, pin.ReleaseName()))
			return pin
		},
		"invalid pin": func(string) domain.ProbeBinaryPin { return domain.ProbeBinaryPin{} },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			f := sessionTFactory(t, dir)
			session, fp, err := f.NewInstallSession(t.Context(), key, hostFP, setup(dir))
			if !errors.Is(err, domain.ErrProbeInstallArtifact) || session != nil || fp != "" {
				t.Fatalf("session=%v fp=%q err=%v", session, fp, err)
			}
		})
	}
}

func TestSSHProbeInstallSessionFactoryRejectsKeysAndTrust(t *testing.T) {
	data := buildArtifactFixture(t, "linux", "amd64", "cmd/probe", "0")
	pin := artifactPin(data, "amd64")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pin.ReleaseName()), data, 0o600); err != nil {
		t.Fatal(err)
	}
	f := sessionTFactory(t, dir)
	key, _ := deploymentKey(t)
	_, hostSigner := deploymentKey(t)
	hostFP := ssh.FingerprintSHA256(hostSigner.PublicKey())
	for name, tc := range map[string]struct {
		key  []byte
		host string
	}{
		"garbage key":        {[]byte("not a key"), hostFP},
		"empty key":          {nil, hostFP},
		"malformed host pin": {key, "SHA256:short"},
		"md5 host pin":       {key, "MD5:aa:bb"},
	} {
		session, fp, err := f.NewInstallSession(t.Context(), tc.key, tc.host, pin)
		if !errors.Is(err, domain.ErrProbeSSHInput) || session != nil || fp != "" {
			t.Fatalf("%s: session=%v fp=%q err=%v", name, session, fp, err)
		}
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if session, _, err := f.NewInstallSession(canceled, key, hostFP, pin); session != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: session=%v err=%v", session, err)
	}
}
