package provisioning

import (
	"context"
	"errors"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func preflightTestReport() domain.ProbeInstallHostReport {
	return domain.ProbeInstallHostReport{Kernel: "Linux", Machine: "aarch64", Systemd: true, Root: true, Authority: true, ExistingInstalls: 2}
}

func TestProbeInstallHostReportRoundTrip(t *testing.T) {
	report := preflightTestReport()
	encoded := EncodeProbeInstallHostReport(report)
	want := "phoenix-install-preflight-v1\nkernel=Linux\nmachine=aarch64\nsystemd=1\ndocker=0\nroot=1\nauthority=1\nexisting=2\n"
	if string(encoded) != want {
		t.Fatalf("encoded %q", encoded)
	}
	got, err := ParseProbeInstallHostReport(string(encoded))
	if err != nil || got != report {
		t.Fatalf("parsed %+v %v", got, err)
	}
	if EncodeProbeInstallHostReport(domain.ProbeInstallHostReport{Kernel: "Linux"}) != nil {
		t.Fatal("encoded an invalid report")
	}
}

func TestProbeInstallHostReportParseIsStrict(t *testing.T) {
	good := string(EncodeProbeInstallHostReport(preflightTestReport()))
	for name, text := range map[string]string{
		"empty":            "",
		"no trailing LF":   strings.TrimSuffix(good, "\n"),
		"extra line":       good + "extra=1\n",
		"wrong magic":      strings.Replace(good, "-v1", "-v2", 1),
		"reordered":        strings.Replace(good, "systemd=1\ndocker=0", "docker=0\nsystemd=1", 1),
		"bool two":         strings.Replace(good, "root=1", "root=2", 1),
		"bool word":        strings.Replace(good, "systemd=1", "systemd=yes", 1),
		"leading zero":     strings.Replace(good, "existing=2", "existing=02", 1),
		"negative":         strings.Replace(good, "existing=2", "existing=-1", 1),
		"over the cap":     strings.Replace(good, "existing=2", "existing=65", 1),
		"plus sign":        strings.Replace(good, "existing=2", "existing=+2", 1),
		"kernel space":     strings.Replace(good, "kernel=Linux", "kernel=Linux x", 1),
		"machine empty":    strings.Replace(good, "machine=aarch64", "machine=", 1),
		"machine slash":    strings.Replace(good, "machine=aarch64", "machine=a/b", 1),
		"CRLF":             strings.ReplaceAll(good, "\n", "\r\n"),
		"duplicate kernel": strings.Replace(good, "machine=aarch64", "kernel=Linux", 1),
	} {
		if _, err := ParseProbeInstallHostReport(text); !errors.Is(err, domain.ErrProbeSSHDiscovery) {
			t.Errorf("%s: err %v", name, err)
		}
	}
}

func preflightRefusalCode(t *testing.T, err error) string {
	t.Helper()
	var refusal *domain.ProbeInstallPreflightError
	if !errors.As(err, &refusal) || !errors.Is(err, domain.ErrProbeInstallPreflight) {
		t.Fatalf("err %v is not a preflight refusal", err)
	}
	return refusal.Code
}

func TestInspectInstallHostRunsOnlyTheFixedCommand(t *testing.T) {
	clientKey, clientSigner := deploymentKey(t)
	srv := newHKServer(t, clientSigner.PublicKey())
	srv.mu.Lock()
	srv.exec = func(command string) ([]byte, uint32) {
		if command != SSHProbeInstallPreflightCommand {
			return nil, 2
		}
		return EncodeProbeInstallHostReport(preflightTestReport()), 0
	}
	srv.mu.Unlock()
	// The ED25519 pin also exercises the learned host-key family.
	insp := sessionTestInspector(t, clientKey, ssh.FingerprintSHA256(srv.ed25519.PublicKey()))
	report, err := insp.InspectInstallHost(t.Context(), domain.ProbeSSHHost{Address: srv.address, User: "root"})
	if err != nil || report != preflightTestReport() {
		t.Fatalf("report %+v err %v", report, err)
	}
	srv.mu.Lock()
	commands := append([]string(nil), srv.commands...)
	srv.mu.Unlock()
	if len(commands) != 1 || commands[0] != SSHProbeInstallPreflightCommand {
		t.Fatalf("commands %q", commands)
	}
}

func TestInspectInstallHostClassifiesEveryFailure(t *testing.T) {
	clientKey, clientSigner := deploymentKey(t)
	host := func(srv *hkServer) domain.ProbeSSHHost {
		return domain.ProbeSSHHost{Address: srv.address, User: "root"}
	}
	t.Run("bad host key never authenticates", func(t *testing.T) {
		srv := newHKServer(t, clientSigner.PublicKey())
		_, other := deploymentKey(t)
		insp := sessionTestInspector(t, clientKey, ssh.FingerprintSHA256(other.PublicKey()))
		_, err := insp.InspectInstallHost(t.Context(), host(srv))
		if code := preflightRefusalCode(t, err); code != domain.ProbeInstallPreflightTrust {
			t.Fatalf("code %q", code)
		}
		if srv.auths.Load() != 0 || srv.execs.Load() != 0 {
			t.Fatalf("auths=%d execs=%d", srv.auths.Load(), srv.execs.Load())
		}
	})
	t.Run("refused key is auth failure", func(t *testing.T) {
		_, stranger := deploymentKey(t)
		srv := newHKServer(t, stranger.PublicKey())
		insp := sessionTestInspector(t, clientKey, ssh.FingerprintSHA256(srv.ecdsa.PublicKey()))
		_, err := insp.InspectInstallHost(t.Context(), host(srv))
		if code := preflightRefusalCode(t, err); code != domain.ProbeInstallPreflightAuth {
			t.Fatalf("code %q", code)
		}
		if srv.execs.Load() != 0 {
			t.Fatal("executed without authentication")
		}
	})
	t.Run("missing helper", func(t *testing.T) {
		srv := newHKServer(t, clientSigner.PublicKey()) // default: exit 1, no output
		insp := sessionTestInspector(t, clientKey, ssh.FingerprintSHA256(srv.ecdsa.PublicKey()))
		_, err := insp.InspectInstallHost(t.Context(), host(srv))
		if code := preflightRefusalCode(t, err); code != domain.ProbeInstallPreflightHelper {
			t.Fatalf("code %q", code)
		}
	})
	t.Run("malformed report", func(t *testing.T) {
		srv := newHKServer(t, clientSigner.PublicKey())
		srv.mu.Lock()
		srv.exec = func(string) ([]byte, uint32) { return []byte("phoenix-install-preflight-v1\nkernel=Linux\n"), 0 }
		srv.mu.Unlock()
		insp := sessionTestInspector(t, clientKey, ssh.FingerprintSHA256(srv.ecdsa.PublicKey()))
		_, err := insp.InspectInstallHost(t.Context(), host(srv))
		if code := preflightRefusalCode(t, err); code != domain.ProbeInstallPreflightHelper {
			t.Fatalf("code %q", code)
		}
	})
	t.Run("oversized output", func(t *testing.T) {
		srv := newHKServer(t, clientSigner.PublicKey())
		srv.mu.Lock()
		srv.exec = func(string) ([]byte, uint32) { return []byte(strings.Repeat("x", maxDiscoverySize+1)), 0 }
		srv.mu.Unlock()
		insp := sessionTestInspector(t, clientKey, ssh.FingerprintSHA256(srv.ecdsa.PublicKey()))
		_, err := insp.InspectInstallHost(t.Context(), host(srv))
		if code := preflightRefusalCode(t, err); code != domain.ProbeInstallPreflightHelper {
			t.Fatalf("code %q", code)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		insp := sessionTestInspector(t, clientKey, ssh.FingerprintSHA256(clientSigner.PublicKey()))
		_, err := insp.InspectInstallHost(t.Context(), domain.ProbeSSHHost{Address: "127.0.0.1:1", User: "root"})
		if code := preflightRefusalCode(t, err); code != domain.ProbeInstallPreflightUnreachable {
			t.Fatalf("code %q", code)
		}
	})
	t.Run("canceled context passes through", func(t *testing.T) {
		insp := sessionTestInspector(t, clientKey, ssh.FingerprintSHA256(clientSigner.PublicKey()))
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := insp.InspectInstallHost(ctx, domain.ProbeSSHHost{Address: "127.0.0.1:1", User: "root"})
		if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrProbeInstallPreflight) {
			t.Fatalf("err %v", err)
		}
	})
}
