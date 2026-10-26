//go:build m8bootstrap

package provisioning

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// TestM8HostBootstrapOpenSSH is the opt-in real-host acceptance for the
// password bootstrap: the PRODUCTION layout (/usr/local/bin, /etc/phoenix-probe,
// ~root/.ssh) on a disposable Linux host running real OpenSSH and sudo, the
// real phoenix-probe-admin release as the helper, and verification through the
// real forced authorized-dispatch -> install-preflight. Never point it at a
// host you care about: it writes root-owned files.
//
// Inputs (all required; the test fails, never skips, when one is missing):
//
//	M8BOOT_ADDR      host:port of the disposable sshd
//	M8BOOT_HOST_FP   its SHA256: host key fingerprint, verified out of band
//	M8BOOT_USER      a non-root sudoer that may log in with a password
//	M8BOOT_PASSWORD  that user's login and sudo password
//	M8BOOT_HELPER    path to phoenix-probe-admin built for the host (linux, CGO 0)
//	M8BOOT_VERSION   release version used for the helper file name
//	M8BOOT_ARCH      amd64 | arm64
func TestM8HostBootstrapOpenSSH(t *testing.T) {
	env := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			t.Fatalf("%s is required for the m8bootstrap acceptance gate", name)
		}
		return v
	}
	addr, hostFP, user, password := env("M8BOOT_ADDR"), env("M8BOOT_HOST_FP"), env("M8BOOT_USER"), env("M8BOOT_PASSWORD")
	helperPath, version, arch := env("M8BOOT_HELPER"), env("M8BOOT_VERSION"), env("M8BOOT_ARCH")
	data, err := os.ReadFile(helperPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	pin := domain.ProbeBinaryPin{Version: version, Architecture: arch, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))}
	artifacts := t.TempDir()
	if err := os.WriteFile(filepath.Join(artifacts, domain.HelperReleaseName(pin)), data, 0o600); err != nil {
		t.Fatal(err)
	}
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	var authority [32]byte
	copy(authority[:], public)
	policy := probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32")}}
	b, err := NewSSHProbeHostBootstrapper(artifacts, policy, authority)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	key, _ := deploymentKey(t)
	request := func() domain.ProbeHostBootstrapRequest {
		return domain.ProbeHostBootstrapRequest{
			Host: domain.ProbeSSHHost{Address: addr, User: user}, Password: []byte(password),
			HostKeyFingerprint: hostFP, PrivateKey: append([]byte(nil), key...), Helper: pin, RequestedBy: 1,
		}
	}

	first, err := b.Bootstrap(t.Context(), request())
	if err != nil {
		t.Fatalf("first bootstrap: %v", err)
	}
	if first != (domain.ProbeHostBootstrapResult{Helper: "installed", StageAuthority: "installed", AuthorizedKey: "added", Verified: true}) {
		t.Fatalf("first=%+v", first)
	}
	again, err := b.Bootstrap(t.Context(), request())
	if err != nil {
		t.Fatalf("rerun: %v", err)
	}
	if again != (domain.ProbeHostBootstrapResult{Helper: "unchanged", StageAuthority: "unchanged", AuthorizedKey: "unchanged", Verified: true}) {
		t.Fatalf("rerun=%+v", again)
	}

	// A wrong sudo/login password is a fixed refusal with nothing changed.
	bad := request()
	bad.Password = []byte(password + "-wrong")
	if _, err := b.Bootstrap(t.Context(), bad); bootstrapCode(err) != domain.ProbeHostBootstrapAuth {
		t.Fatalf("wrong password: %v", err)
	}
	// A different hub (another stage authority) is refused, not silently re-keyed.
	other, err := NewSSHProbeHostBootstrapper(artifacts, policy, [32]byte{1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if _, err := other.Bootstrap(t.Context(), request()); bootstrapCode(err) != domain.ProbeHostBootstrapAuthorityClash {
		t.Fatalf("other authority: %v", err)
	}
	// A wrong pin never sends the password.
	pinned := request()
	pinned.HostKeyFingerprint = "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	if _, err := b.Bootstrap(t.Context(), pinned); bootstrapCode(err) != domain.ProbeHostBootstrapTrust {
		t.Fatalf("wrong pin: %v", err)
	}
	t.Log("m8bootstrap acceptance passed")
}
