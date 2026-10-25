//go:build linux || darwin

package bootstrap

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func stageAuthorityDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "authority")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func runStageAuthority(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := RunProbeStageAuthorityKey(t.Context(), args, &out, &stderr)
	return code, out.String(), stderr.String()
}

func TestRunProbeStageAuthorityKeyInitCheckExportRoundTrip(t *testing.T) {
	t.Setenv("PROBE_STAGE_AUTHORITY_KEY_FILE", "")
	path := filepath.Join(stageAuthorityDir(t), "stage-authority.key")

	if code, out, stderr := runStageAuthority(t, "init", "--file", path); code != 0 || stderr != "" || !strings.Contains(out, "created") {
		t.Fatalf("init code=%d out=%q err=%q", code, out, stderr)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 || info.Size() != 32 {
		t.Fatalf("key file %v err=%v", info, err)
	}
	seed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	signer, err := LoadProbeStageGrantSigner(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	keyID := domain.ProbeBinaryStageAuthorityKeyID(signer.PublicKey())
	code, out, stderr := runStageAuthority(t, "check", "--file", path)
	if code != 0 || stderr != "" || !strings.Contains(out, keyID) {
		t.Fatalf("check code=%d out=%q err=%q", code, out, stderr)
	}

	code, out, stderr = runStageAuthority(t, "public-key", "--file", path)
	if code != 0 || stderr != "" {
		t.Fatalf("public-key code=%d err=%q", code, stderr)
	}
	public, err := provisioning.ParseProbeBinaryStageAuthorityKey([]byte(out))
	if err != nil || public != signer.PublicKey() {
		t.Fatalf("exported public key %q does not parse to the signer's key: %v", out, err)
	}
	// No command ever prints the private seed, in raw or hex form.
	if strings.Contains(out, string(seed)) || strings.Contains(out, hex.EncodeToString(seed)) {
		t.Fatal("private seed printed")
	}

	// The exported trust file verifies a grant this signer issues, exactly as
	// the authorized helper verifies it after loading the host file.
	intent := helperIntent()
	intent.Identity.Host.User = "root"
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	request := domain.ProbeBinaryStageAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
		AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(public), Nonce: nonce, MarkerSHA256: intent.MarkerSHA256(),
	}}
	admission := domain.ProbeBinaryStageAdmission{Request: request, AdmittedAt: time.Now().UTC().Truncate(time.Microsecond)}
	grant, err := signer.SignStageAdmission(t.Context(), admission)
	if err != nil || !grant.ValidFor(public) {
		t.Fatalf("grant from loaded key does not verify under exported trust file: %v", err)
	}

	// init never replaces an existing key.
	if code, _, stderr := runStageAuthority(t, "init", "--file", path); code != 1 || stderr == "" {
		t.Fatalf("second init code=%d err=%q", code, stderr)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(after, seed) {
		t.Fatal("init replaced an existing key")
	}
}

func TestRunProbeStageAuthorityKeyUsesEnvironmentPath(t *testing.T) {
	path := filepath.Join(stageAuthorityDir(t), "stage-authority.key")
	t.Setenv("PROBE_STAGE_AUTHORITY_KEY_FILE", path)
	if code, _, stderr := runStageAuthority(t, "init"); code != 0 {
		t.Fatalf("init from env code=%d err=%q", code, stderr)
	}
	if code, out, _ := runStageAuthority(t, "public-key"); code != 0 || len(out) != provisioning.ProbeBinaryStageAuthorityKeyFileSize {
		t.Fatalf("public-key from env code=%d out=%q", code, out)
	}
}

func TestRunProbeStageAuthorityKeyRefusesUnprotectedOrMalformedKeys(t *testing.T) {
	t.Setenv("PROBE_STAGE_AUTHORITY_KEY_FILE", "")
	dir := stageAuthorityDir(t)
	write := func(name string, body []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, body, mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	seed := bytes.Repeat([]byte{9}, 32)
	cases := map[string]string{
		"world readable": write("readable.key", seed, 0o644),
		"short":          write("short.key", seed[:31], 0o600),
		"long":           write("long.key", append(append([]byte(nil), seed...), '\n'), 0o600),
		"missing":        filepath.Join(dir, "absent.key"),
		"directory":      dir,
	}
	for name, path := range cases {
		for _, verb := range []string{"check", "public-key"} {
			code, out, stderr := runStageAuthority(t, verb, "--file", path)
			if code != 1 || out != "" || stderr == "" || strings.Contains(stderr, path) {
				t.Fatalf("%s %s: code=%d out=%q err=%q", name, verb, code, out, stderr)
			}
		}
		if _, err := LoadProbeStageGrantSigner(t.Context(), path); err == nil {
			t.Fatalf("%s: loaded", name)
		}
	}
	if _, err := LoadProbeStageGrantSigner(t.Context(), ""); err == nil {
		t.Fatal("empty path loaded")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := LoadProbeStageGrantSigner(canceled, write("ok.key", seed, 0o600)); err == nil {
		t.Fatal("canceled context loaded")
	}
}

func TestRunProbeStageAuthorityKeyUsage(t *testing.T) {
	t.Setenv("PROBE_STAGE_AUTHORITY_KEY_FILE", "")
	for _, args := range [][]string{nil, {"rotate"}, {"check"}, {"check", "--file"}, {"check", "--file", "x", "extra"}, {"check", "--bogus"}} {
		if code, out, stderr := runStageAuthority(t, args...); code != 2 || out != "" || stderr != probeStageAuthorityUsage {
			t.Fatalf("args %q code=%d out=%q err=%q", args, code, out, stderr)
		}
	}
	for _, args := range [][]string{{"--help"}, {"help"}, {"check", "--help"}} {
		if code, out, stderr := runStageAuthority(t, args...); code != 0 || out != probeStageAuthorityUsage || stderr != "" {
			t.Fatalf("help %q code=%d out=%q err=%q", args, code, out, stderr)
		}
	}
}
