package probe_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
)

func TestHelperProcess_LockHolder(t *testing.T) {
	if os.Getenv("GO_TEST_SUBPROCESS") != "lock_holder" {
		return
	}
	dir := os.Getenv("PROBE_TEST_DIR")
	if dir == "" {
		os.Exit(2)
	}

	identity, err := probe.OpenRuntimeIdentity(context.Background(), dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "open error: %v\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = identity.Close()
	}()

	// Signal parent that lock is held
	fmt.Println("LOCKED")

	// Wait until killed or context canceled
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	os.Exit(0)
}

func TestRuntimeIdentity_InitializeAndOpen(t *testing.T) {
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "probe-data")

	ctx := context.Background()

	// Initialize new identity
	ident1, err := probe.InitializeRuntimeIdentity(ctx, dataDir)
	if err != nil {
		t.Fatalf("InitializeRuntimeIdentity failed: %v", err)
	}

	if ident1.ProbeID == "" || ident1.StreamID == "" || ident1.Fingerprint == "" {
		t.Fatalf("unexpected empty field in identity: %+v", ident1)
	}
	if len(ident1.Fingerprint) != 64 {
		t.Fatalf("expected 64-char fingerprint, got %d chars: %s", len(ident1.Fingerprint), ident1.Fingerprint)
	}
	canonicalExpected, err := filepath.EvalSymlinks(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	canonicalExpected, _ = filepath.Abs(canonicalExpected)
	if ident1.DataDir != canonicalExpected {
		t.Fatalf("expected DataDir %q, got %q", canonicalExpected, ident1.DataDir)
	}
	if len(ident1.Certificate.Certificate) == 0 {
		t.Fatal("expected non-empty certificate in identity")
	}

	// Verify directory permissions 0700
	fi, err := os.Stat(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0700 {
		t.Fatalf("expected 0700 directory perms, got %04o", fi.Mode().Perm())
	}

	// Verify file permissions 0600
	for _, fn := range []string{"identity.json", "tls.pem", "probe.lock"} {
		fPath := filepath.Join(dataDir, fn)
		fi, err := os.Stat(fPath)
		if err != nil {
			t.Fatalf("stat %s: %v", fn, err)
		}
		if fi.Mode().Perm() != 0600 {
			t.Fatalf("expected 0600 perms on %s, got %04o", fn, fi.Mode().Perm())
		}
	}

	// Close ident1
	if err := ident1.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// Reopen with OpenRuntimeIdentity
	ident2, err := probe.OpenRuntimeIdentity(ctx, dataDir)
	if err != nil {
		t.Fatalf("OpenRuntimeIdentity failed: %v", err)
	}
	defer func() {
		_ = ident2.Close()
	}()

	if ident2.ProbeID != ident1.ProbeID {
		t.Fatalf("ProbeID mismatch: got %q, want %q", ident2.ProbeID, ident1.ProbeID)
	}
	if ident2.StreamID != ident1.StreamID {
		t.Fatalf("StreamID mismatch: got %q, want %q", ident2.StreamID, ident1.StreamID)
	}
	if ident2.Fingerprint != ident1.Fingerprint {
		t.Fatalf("Fingerprint mismatch: got %q, want %q", ident2.Fingerprint, ident1.Fingerprint)
	}
}

func TestRuntimeIdentity_IdempotentInitialize(t *testing.T) {
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "probe-data")

	ctx := context.Background()

	ident1, err := probe.InitializeRuntimeIdentity(ctx, dataDir)
	if err != nil {
		t.Fatalf("first Initialize failed: %v", err)
	}
	if err := ident1.Close(); err != nil {
		t.Fatal(err)
	}

	// Second initialize should load and return the same identity
	ident2, err := probe.InitializeRuntimeIdentity(ctx, dataDir)
	if err != nil {
		t.Fatalf("second Initialize failed: %v", err)
	}
	defer func() {
		_ = ident2.Close()
	}()

	if ident2.ProbeID != ident1.ProbeID || ident2.StreamID != ident1.StreamID || ident2.Fingerprint != ident1.Fingerprint {
		t.Fatalf("idempotent Initialize produced different identity: got %+v, want %+v", ident2, ident1)
	}
}

func TestRuntimeIdentity_UninitializedOpenFails(t *testing.T) {
	tempDir := t.TempDir()
	ctx := context.Background()

	// Nonexistent directory
	_, err := probe.OpenRuntimeIdentity(ctx, filepath.Join(tempDir, "nonexistent"))
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("expected does not exist error, got %v", err)
	}

	// Empty directory
	emptyDir := filepath.Join(tempDir, "empty")
	if err := os.MkdirAll(emptyDir, 0700); err != nil {
		t.Fatal(err)
	}
	_, err = probe.OpenRuntimeIdentity(ctx, emptyDir)
	if err == nil || !strings.Contains(err.Error(), "missing identity.json") {
		t.Fatalf("expected missing identity.json error, got %v", err)
	}
}

func TestRuntimeIdentity_PartialInitializationRecovery(t *testing.T) {
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "partial-data")
	if err := os.MkdirAll(dataDir, 0700); err != nil {
		t.Fatal(err)
	}

	// Generate valid tls.pem directly in the directory, simulating crash before identity.json
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(12345),
		Subject: pkix.Name{
			CommonName: "partial-probe",
		},
		NotBefore:             now.Add(-1 * time.Hour),
		NotAfter:              now.Add(365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		t.Fatal(err)
	}
	privBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	tlsBytes := append(
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes}),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})...,
	)
	if err := os.WriteFile(filepath.Join(dataDir, "tls.pem"), tlsBytes, 0600); err != nil {
		t.Fatal(err)
	}

	expectedSum := sha256.Sum256(derBytes)
	expectedFP := hex.EncodeToString(expectedSum[:])

	ctx := context.Background()
	ident, err := probe.InitializeRuntimeIdentity(ctx, dataDir)
	if err != nil {
		t.Fatalf("Initialize failed during partial recovery: %v", err)
	}
	defer func() {
		_ = ident.Close()
	}()

	if ident.Fingerprint != expectedFP {
		t.Fatalf("expected fingerprint %s, got %s", expectedFP, ident.Fingerprint)
	}

	// Verify identity.json now exists
	if _, err := os.Stat(filepath.Join(dataDir, "identity.json")); err != nil {
		t.Fatalf("expected identity.json to be created, got error: %v", err)
	}
}

func TestRuntimeIdentity_CorruptManifestRejection(t *testing.T) {
	ctx := context.Background()

	setupValid := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "data")
		ident, err := probe.InitializeRuntimeIdentity(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := ident.Close(); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("corrupt json", func(t *testing.T) {
		dir := setupValid(t)
		if err := os.WriteFile(filepath.Join(dir, "identity.json"), []byte("{invalid-json"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "parse identity manifest") {
			t.Fatalf("expected parse identity manifest error, got %v", err)
		}
	})

	t.Run("unsupported version", func(t *testing.T) {
		dir := setupValid(t)
		raw, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["version"] = 99
		raw, _ = json.Marshal(m)
		_ = os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0600)

		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "unsupported identity manifest version") {
			t.Fatalf("expected unsupported version error, got %v", err)
		}
	})

	t.Run("nil uuid", func(t *testing.T) {
		dir := setupValid(t)
		raw, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["probe_id"] = "00000000-0000-0000-0000-000000000000"
		raw, _ = json.Marshal(m)
		_ = os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0600)

		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "invalid probe_id") {
			t.Fatalf("expected invalid probe_id error, got %v", err)
		}
	})

	t.Run("uppercase uuid", func(t *testing.T) {
		dir := setupValid(t)
		raw, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["probe_id"] = strings.ToUpper(m["probe_id"].(string))
		raw, _ = json.Marshal(m)
		_ = os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0600)

		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "invalid probe_id") {
			t.Fatalf("expected invalid probe_id error for uppercase, got %v", err)
		}
	})

	t.Run("invalid fingerprint length", func(t *testing.T) {
		dir := setupValid(t)
		raw, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["certificate_fingerprint"] = "abcd"
		raw, _ = json.Marshal(m)
		_ = os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0600)

		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "invalid fingerprint") {
			t.Fatalf("expected invalid fingerprint error, got %v", err)
		}
	})

	t.Run("fingerprint mismatch", func(t *testing.T) {
		dir := setupValid(t)
		raw, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		m["certificate_fingerprint"] = strings.Repeat("f", 64)
		raw, _ = json.Marshal(m)
		_ = os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0600)

		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
			t.Fatalf("expected fingerprint mismatch error, got %v", err)
		}
	})

	t.Run("duplicate keys", func(t *testing.T) {
		dir := setupValid(t)
		sentinelSecret := "SENTINEL_ATTACKER_SECRET_KEY_DO_NOT_LEAK"
		dupJSON := `{"version": 1, "probe_id": "11111111-1111-4111-8111-111111111111", "` + sentinelSecret + `": 1, "` + sentinelSecret + `": 2, "stream_id": "33333333-3333-4333-8333-333333333333", "certificate_fingerprint": "` + strings.Repeat("a", 64) + `"}`
		_ = os.WriteFile(filepath.Join(dir, "identity.json"), []byte(dupJSON), 0600)

		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "duplicate key in identity manifest") {
			t.Fatalf("expected duplicate key in identity manifest error, got %v", err)
		}
		if strings.Contains(err.Error(), sentinelSecret) {
			t.Fatalf("duplicate key error leaked attacker secret key text: %v", err)
		}
	})

	t.Run("trailing data", func(t *testing.T) {
		dir := setupValid(t)
		raw, _ := os.ReadFile(filepath.Join(dir, "identity.json"))
		raw = append(raw, []byte(` {"extra": 1}`)...)
		_ = os.WriteFile(filepath.Join(dir, "identity.json"), raw, 0600)

		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "trailing data") {
			t.Fatalf("expected trailing data error, got %v", err)
		}
	})

	t.Run("oversize manifest", func(t *testing.T) {
		dir := setupValid(t)
		// 4 KiB + 1 byte
		oversizeJSON := `{"version": 1, "probe_id": "11111111-1111-4111-8111-111111111111", "stream_id": "33333333-3333-4333-8333-333333333333", "certificate_fingerprint": "` + strings.Repeat("a", 64) + `", "padding": "` + strings.Repeat("x", 4096) + `"}`
		_ = os.WriteFile(filepath.Join(dir, "identity.json"), []byte(oversizeJSON), 0600)

		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "exceeds maximum") && !strings.Contains(err.Error(), "oversize") {
			t.Fatalf("expected oversize manifest error, got %v", err)
		}
	})
}

func TestRuntimeIdentity_CorruptOrMissingTLSRejection(t *testing.T) {
	ctx := context.Background()

	setupValid := func(t *testing.T) string {
		dir := filepath.Join(t.TempDir(), "data")
		ident, err := probe.InitializeRuntimeIdentity(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := ident.Close(); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	t.Run("missing tls.pem", func(t *testing.T) {
		dir := setupValid(t)
		_ = os.Remove(filepath.Join(dir, "tls.pem"))
		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "missing tls.pem") {
			t.Fatalf("expected missing tls.pem error, got %v", err)
		}
	})

	t.Run("corrupt tls.pem", func(t *testing.T) {
		dir := setupValid(t)
		_ = os.WriteFile(filepath.Join(dir, "tls.pem"), []byte("garbage data"), 0600)
		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "invalid certificate or private key") {
			t.Fatalf("expected invalid cert/key error, got %v", err)
		}
	})

	t.Run("expired certificate", func(t *testing.T) {
		dir := setupValid(t)

		priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		now := time.Now().UTC()
		template := &x509.Certificate{
			SerialNumber: big.NewInt(999),
			Subject: pkix.Name{
				CommonName: "expired-probe",
			},
			NotBefore:             now.Add(-2 * time.Hour),
			NotAfter:              now.Add(-1 * time.Hour),
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			BasicConstraintsValid: true,
		}
		derBytes, _ := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
		privBytes, _ := x509.MarshalECPrivateKey(priv)
		tlsBytes := append(
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes}),
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})...,
		)
		_ = os.WriteFile(filepath.Join(dir, "tls.pem"), tlsBytes, 0600)

		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "certificate has expired") {
			t.Fatalf("expected certificate has expired error, got %v", err)
		}
	})

	t.Run("not yet valid certificate", func(t *testing.T) {
		dir := setupValid(t)

		priv, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		now := time.Now().UTC()
		template := &x509.Certificate{
			SerialNumber: big.NewInt(999),
			Subject: pkix.Name{
				CommonName: "future-probe",
			},
			NotBefore:             now.Add(1 * time.Hour),
			NotAfter:              now.Add(2 * time.Hour),
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			BasicConstraintsValid: true,
		}
		derBytes, _ := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
		privBytes, _ := x509.MarshalECPrivateKey(priv)
		tlsBytes := append(
			pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: privBytes}),
			pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})...,
		)
		_ = os.WriteFile(filepath.Join(dir, "tls.pem"), tlsBytes, 0600)

		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "certificate is not yet valid") {
			t.Fatalf("expected certificate is not yet valid error, got %v", err)
		}
	})

	t.Run("oversize tls.pem", func(t *testing.T) {
		dir := setupValid(t)
		// Read valid tls.pem and append padding > 64 KiB
		data, _ := os.ReadFile(filepath.Join(dir, "tls.pem"))
		data = append(data, []byte("# padding\n")...)
		data = append(data, bytes.Repeat([]byte("a"), 65536)...)
		_ = os.WriteFile(filepath.Join(dir, "tls.pem"), data, 0600)

		_, err := probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "exceeds maximum") && !strings.Contains(err.Error(), "oversize") {
			t.Fatalf("expected oversize tls error, got %v", err)
		}
	})
}

func TestRuntimeIdentity_SymlinkAndPermissionRejection(t *testing.T) {
	ctx := context.Background()

	t.Run("parent is symlink canonicalization", func(t *testing.T) {
		baseDir := t.TempDir()
		realParent := filepath.Join(baseDir, "real-parent")
		if err := os.MkdirAll(realParent, 0700); err != nil {
			t.Fatal(err)
		}
		symParent := filepath.Join(baseDir, "sym-parent")
		if err := os.Symlink(realParent, symParent); err != nil {
			t.Fatal(err)
		}

		targetData := filepath.Join(symParent, "probe-data")
		ident, err := probe.InitializeRuntimeIdentity(ctx, targetData)
		if err != nil {
			t.Fatalf("initialize under parent symlink failed: %v", err)
		}
		defer func() {
			_ = ident.Close()
		}()

		// Canonical directory should resolve parent symlink to realParent/probe-data
		expectedCanonical, err := filepath.EvalSymlinks(targetData)
		if err != nil {
			t.Fatal(err)
		}
		expectedCanonical, _ = filepath.Abs(expectedCanonical)
		if ident.DataDir != expectedCanonical {
			t.Fatalf("expected canonical DataDir %q, got %q", expectedCanonical, ident.DataDir)
		}
	})

	t.Run("directory is symlink", func(t *testing.T) {
		realDir := filepath.Join(t.TempDir(), "real-data")
		_ = os.MkdirAll(realDir, 0700)
		symDir := filepath.Join(t.TempDir(), "sym-data")
		if err := os.Symlink(realDir, symDir); err != nil {
			t.Fatal(err)
		}
		_, err := probe.InitializeRuntimeIdentity(ctx, symDir)
		if err == nil || !strings.Contains(err.Error(), "must not be a symlink") {
			t.Fatalf("expected symlink directory error, got %v", err)
		}
	})

	t.Run("directory permissive permissions", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "permissive-data")
		_ = os.MkdirAll(dir, 0700)
		_ = os.Chmod(dir, 0777)
		_, err := probe.InitializeRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "untrusted directory permissions") {
			t.Fatalf("expected untrusted directory permissions error, got %v", err)
		}
	})

	t.Run("manifest is symlink", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		ident, err := probe.InitializeRuntimeIdentity(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		_ = ident.Close()

		realManifest := filepath.Join(dir, "identity.json")
		otherManifest := filepath.Join(dir, "other.json")
		_ = os.Rename(realManifest, otherManifest)
		_ = os.Symlink(otherManifest, realManifest)

		_, err = probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("expected manifest symlink rejection, got %v", err)
		}
	})

	t.Run("tls.pem is symlink", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		ident, err := probe.InitializeRuntimeIdentity(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		_ = ident.Close()

		realTLS := filepath.Join(dir, "tls.pem")
		otherTLS := filepath.Join(dir, "other.pem")
		_ = os.Rename(realTLS, otherTLS)
		_ = os.Symlink(otherTLS, realTLS)

		_, err = probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("expected tls symlink rejection, got %v", err)
		}
	})

	t.Run("lock is symlink", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		ident, err := probe.InitializeRuntimeIdentity(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		_ = ident.Close()

		realLock := filepath.Join(dir, "probe.lock")
		otherLock := filepath.Join(dir, "other.lock")
		_ = os.Rename(realLock, otherLock)
		_ = os.Symlink(otherLock, realLock)

		_, err = probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "symlink") && !strings.Contains(err.Error(), "regular file") && !strings.Contains(err.Error(), "open lock file") {
			t.Fatalf("expected lock symlink rejection, got %v", err)
		}
	})

	t.Run("lock is permissive", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		ident, err := probe.InitializeRuntimeIdentity(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		_ = ident.Close()

		_ = os.Chmod(filepath.Join(dir, "probe.lock"), 0644)
		_, err = probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "permissive") {
			t.Fatalf("expected lock permissive rejection, got %v", err)
		}
	})

	t.Run("lock is fifo", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		fifoPath := filepath.Join(dir, "probe.lock")
		if err := syscall.Mkfifo(fifoPath, 0600); err != nil {
			t.Fatal(err)
		}

		ctxTimeout, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()

		_, err := probe.OpenRuntimeIdentity(ctxTimeout, dir)
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("expected fifo rejection as non-regular file, got %v", err)
		}
	})

	t.Run("permissive file permissions", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "data")
		ident, err := probe.InitializeRuntimeIdentity(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		_ = ident.Close()

		_ = os.Chmod(filepath.Join(dir, "tls.pem"), 0644)
		_, err = probe.OpenRuntimeIdentity(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "permissions too permissive") {
			t.Fatalf("expected permissions too permissive error, got %v", err)
		}
	})
}

func TestRuntimeIdentity_ContextCancellation(t *testing.T) {
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "cancel-data")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := probe.InitializeRuntimeIdentity(ctx, dataDir)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on Initialize, got %v", err)
	}

	_, err = probe.OpenRuntimeIdentity(ctx, dataDir)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled on Open, got %v", err)
	}
}

func TestRuntimeIdentity_SecretsNotExposedInErrors(t *testing.T) {
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "data")

	ident, err := probe.InitializeRuntimeIdentity(context.Background(), dataDir)
	if err != nil {
		t.Fatal(err)
	}
	_ = ident.Close()

	fakeSecretKey := "SUPER_SECRET_PRIVATE_KEY_BYTES_DO_NOT_LEAK"
	corruptContent := fmt.Sprintf("-----BEGIN EC PRIVATE KEY-----\n%s\n-----END EC PRIVATE KEY-----\n", fakeSecretKey)
	_ = os.WriteFile(filepath.Join(dataDir, "tls.pem"), []byte(corruptContent), 0600)

	_, err = probe.OpenRuntimeIdentity(context.Background(), dataDir)
	if err == nil {
		t.Fatal("expected error on corrupt TLS, got nil")
	}
	if strings.Contains(err.Error(), fakeSecretKey) {
		t.Fatalf("error leaked secret key material: %v", err)
	}
}

func TestRuntimeIdentity_SubprocessLockContentionAndRelease(t *testing.T) {
	tempDir := t.TempDir()
	dataDir := filepath.Join(tempDir, "lock-data")

	// Initialize identity
	ident, err := probe.InitializeRuntimeIdentity(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("initialize failed: %v", err)
	}
	if err := ident.Close(); err != nil {
		t.Fatal(err)
	}

	// Launch subprocess A to hold the lock
	cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcess_LockHolder")
	cmd.Env = append(os.Environ(), "GO_TEST_SUBPROCESS=lock_holder", "PROBE_TEST_DIR="+dataDir)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatalf("start subprocess: %v", err)
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	// Wait for subprocess to acquire lock
	buf := make([]byte, 7) // "LOCKED\n"
	n, err := stdout.Read(buf)
	if err != nil || string(buf[:n]) != "LOCKED\n" {
		t.Fatalf("subprocess failed to signal lock: %v, output: %q", err, string(buf[:n]))
	}

	// Attempt to open the identity while locked by subprocess -> must fail with lock contention
	_, err = probe.OpenRuntimeIdentity(context.Background(), dataDir)
	if err == nil || !strings.Contains(err.Error(), "locked by another process") {
		t.Fatalf("expected lock contention error, got: %v", err)
	}

	// Attempt to initialize while locked by subprocess -> must fail with lock contention
	_, err = probe.InitializeRuntimeIdentity(context.Background(), dataDir)
	if err == nil || !strings.Contains(err.Error(), "locked by another process") {
		t.Fatalf("expected lock contention error on initialize, got: %v", err)
	}

	// Kill subprocess (simulating crash)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill subprocess: %v", err)
	}
	_ = cmd.Wait()

	// Parent process should now immediately be able to acquire the lock
	identAfterCrash, err := probe.OpenRuntimeIdentity(context.Background(), dataDir)
	if err != nil {
		t.Fatalf("expected successful lock acquisition after crash, got: %v", err)
	}
	defer func() {
		_ = identAfterCrash.Close()
	}()

	if identAfterCrash.ProbeID != ident.ProbeID {
		t.Fatalf("identity ProbeID changed after crash restart: got %s, want %s", identAfterCrash.ProbeID, ident.ProbeID)
	}
}
