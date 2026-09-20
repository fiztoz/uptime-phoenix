package probe

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

const (
	identityManifestVersion = 1
	lockFileName            = "probe.lock"
	identityFileName        = "identity.json"
	tlsFileName             = "tls.pem"
)

// RuntimeIdentity represents the stable, persistent identity of a probe instance
// with exclusive ownership of its local data directory.
type RuntimeIdentity struct {
	ProbeID     string
	StreamID    string
	Fingerprint string // lowercase SHA-256 leaf DER
	DataDir     string // absolute canonical directory
	Certificate tls.Certificate
	lock        *dirLock
	root        *os.Root
}

// identityManifest defines the persisted on-disk format for identity.json.
type identityManifest struct {
	Version     int    `json:"version"`
	ProbeID     string `json:"probe_id"`
	StreamID    string `json:"stream_id"`
	Fingerprint string `json:"certificate_fingerprint"`
}

// Close releases the exclusive directory lock and held directory handles.
// The lock file is never unlinked, preserving inode stability across processes.
func (identity *RuntimeIdentity) Close() error {
	if identity == nil {
		return nil
	}
	var lockErr, rootErr error
	if identity.lock != nil {
		lockErr = identity.lock.release()
		identity.lock = nil
	}
	if identity.root != nil {
		rootErr = identity.root.Close()
		identity.root = nil
	}
	if lockErr != nil {
		return lockErr
	}
	return rootErr
}

// InitializeRuntimeIdentity performs explicit operator initialization of the probe
// runtime identity in dataDir. If the directory already contains a valid identity,
// it loads and validates it idempotently. If a prior initialization crashed after
// writing tls.pem, it finishes initialization using the existing valid TLS file.
func InitializeRuntimeIdentity(ctx context.Context, dataDir string) (*RuntimeIdentity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	canonicalDir, root, dirFile, err := prepareDataDir(ctx, dataDir, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		if dirFile != nil {
			_ = dirFile.Close()
		}
	}()

	lock, err := acquireDirLock(dirFile)
	if err != nil {
		_ = root.Close()
		return nil, err
	}

	releaseOnFailure := true
	defer func() {
		if releaseOnFailure {
			if lock != nil {
				_ = lock.release()
			}
			if root != nil {
				_ = root.Close()
			}
		}
	}()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Case 1: Manifest already exists -> load and validate idempotently
	if _, err := root.Lstat(identityFileName); err == nil {
		identity, err := loadAndValidateIdentity(ctx, canonicalDir, root, lock, true)
		if err != nil {
			return nil, fmt.Errorf("validate existing identity: %w", err)
		}
		releaseOnFailure = false
		return identity, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, probeIOError("stat identity manifest", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Case 2 & 3: Manifest does not exist. Check if tls.pem already exists (partial initialization recovery)
	var (
		tlsCert     tls.Certificate
		fingerprint string
	)

	if _, err := root.Lstat(tlsFileName); err == nil {
		// Existing TLS file: validate and reuse without regenerating
		cert, fp, err := readAndValidateTLSFile(root, tlsFileName)
		if err != nil {
			return nil, fmt.Errorf("partial initialization recovery failed: invalid existing tls.pem: %w", err)
		}
		tlsCert = cert
		fingerprint = fp
	} else if errors.Is(err, os.ErrNotExist) {
		// Generate new TLS material
		cert, certPEM, privPEM, fp, err := generateRuntimeTLS()
		if err != nil {
			return nil, fmt.Errorf("generate runtime TLS: %w", err)
		}
		tlsCert = cert
		fingerprint = fp

		combinedPEM := append(privPEM, certPEM...)
		if err := publishFileNoReplace(root, tlsFileName, combinedPEM); err != nil {
			return nil, fmt.Errorf("publish tls.pem: %w", err)
		}
	} else {
		return nil, probeIOError("stat tls.pem", err)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	probeUUID, err := uuid.NewRandom()
	if err != nil {
		return nil, errors.New("generate probe id failed")
	}
	streamUUID, err := uuid.NewRandom()
	if err != nil {
		return nil, errors.New("generate stream id failed")
	}

	probeID := strings.ToLower(probeUUID.String())
	streamID := strings.ToLower(streamUUID.String())

	manifest := identityManifest{
		Version:     identityManifestVersion,
		ProbeID:     probeID,
		StreamID:    streamID,
		Fingerprint: fingerprint,
	}

	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, errors.New("marshal identity manifest failed")
	}
	manifestData = append(manifestData, '\n')

	if err := publishFileNoReplace(root, identityFileName, manifestData); err != nil {
		return nil, fmt.Errorf("publish identity.json: %w", err)
	}

	releaseOnFailure = false
	return &RuntimeIdentity{
		ProbeID:     probeID,
		StreamID:    streamID,
		Fingerprint: fingerprint,
		DataDir:     canonicalDir,
		Certificate: tlsCert,
		lock:        lock,
		root:        root,
	}, nil
}

// OpenRuntimeIdentity opens and validates an existing runtime identity from dataDir.
// It fails if the directory is uninitialized, corrupt, expired, or locked by another process.
func OpenRuntimeIdentity(ctx context.Context, dataDir string) (*RuntimeIdentity, error) {
	return openRuntimeIdentity(ctx, dataDir, true)
}

// OpenRuntimeIdentityAnchor validates and locks the immutable bootstrap files
// without selecting a certificate for serving. Its certificate may be expired.
// The caller MUST authenticate and validate the durable active TLS identity
// before listening; a missing/corrupt active selection cannot use this as fallback.
func OpenRuntimeIdentityAnchor(ctx context.Context, dataDir string) (*RuntimeIdentity, error) {
	return openRuntimeIdentity(ctx, dataDir, false)
}

func openRuntimeIdentity(ctx context.Context, dataDir string, validateTime bool) (*RuntimeIdentity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	canonicalDir, root, dirFile, err := prepareDataDir(ctx, dataDir, false)
	if err != nil {
		return nil, err
	}
	defer func() {
		if dirFile != nil {
			_ = dirFile.Close()
		}
	}()

	lock, err := acquireDirLock(dirFile)
	if err != nil {
		_ = root.Close()
		return nil, err
	}

	releaseOnFailure := true
	defer func() {
		if releaseOnFailure {
			if lock != nil {
				_ = lock.release()
			}
			if root != nil {
				_ = root.Close()
			}
		}
	}()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	identity, err := loadAndValidateIdentity(ctx, canonicalDir, root, lock, validateTime)
	if err != nil {
		return nil, err
	}

	releaseOnFailure = false
	return identity, nil
}

func prepareDataDir(ctx context.Context, dataDir string, createIfMissing bool) (string, *os.Root, *os.File, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, nil, err
	}
	if strings.TrimSpace(dataDir) == "" {
		return "", nil, nil, errors.New("data directory path cannot be empty")
	}

	cleanDir := filepath.Clean(dataDir)
	fi, err := os.Lstat(cleanDir)
	if errors.Is(err, os.ErrNotExist) {
		if !createIfMissing {
			return "", nil, nil, errors.New("uninitialized data directory: does not exist")
		}
		if err := os.MkdirAll(cleanDir, 0700); err != nil {
			return "", nil, nil, probeIOError("create data directory", err)
		}
	} else if err != nil {
		return "", nil, nil, probeIOError("stat data directory", err)
	} else {
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", nil, nil, errors.New("data directory must not be a symlink")
		}
		if !fi.IsDir() {
			return "", nil, nil, errors.New("data directory path is not a directory")
		}
	}

	// Canonicalize through parent symlinks
	evalDir, err := filepath.EvalSymlinks(cleanDir)
	if err != nil {
		return "", nil, nil, probeIOError("canonicalize data directory", err)
	}
	absDir, err := filepath.Abs(evalDir)
	if err != nil {
		return "", nil, nil, probeIOError("canonicalize data directory", err)
	}

	// Verify the canonical directory itself is not a symlink and is a directory
	canonicalFi, err := os.Lstat(absDir)
	if err != nil {
		return "", nil, nil, probeIOError("stat canonical data directory", err)
	}
	if canonicalFi.Mode()&os.ModeSymlink != 0 {
		return "", nil, nil, errors.New("data directory must not be a symlink")
	}
	if !canonicalFi.IsDir() {
		return "", nil, nil, errors.New("data directory path is not a directory")
	}

	// Open os.Root
	root, err := os.OpenRoot(absDir)
	if err != nil {
		return "", nil, nil, probeIOError("open data directory root", err)
	}

	dirFile, err := root.Open(".")
	if err != nil {
		_ = root.Close()
		return "", nil, nil, probeIOError("open data directory handle", err)
	}

	dirInfo, err := dirFile.Stat()
	if err != nil {
		_ = dirFile.Close()
		_ = root.Close()
		return "", nil, nil, probeIOError("inspect data directory handle", err)
	}

	if !dirInfo.IsDir() {
		_ = dirFile.Close()
		_ = root.Close()
		return "", nil, nil, errors.New("data directory path is not a directory")
	}
	if dirInfo.Mode().Perm() != 0700 {
		_ = dirFile.Close()
		_ = root.Close()
		return "", nil, nil, fmt.Errorf("untrusted directory permissions: %04o (must be 0700)", dirInfo.Mode().Perm())
	}
	if !probeCurrentOwner(dirInfo) {
		_ = dirFile.Close()
		_ = root.Close()
		return "", nil, nil, errors.New("untrusted directory ownership: must be owned by process user")
	}

	return absDir, root, dirFile, nil
}

func loadAndValidateIdentity(ctx context.Context, dir string, root *os.Root, lock *dirLock, validateTime bool) (*RuntimeIdentity, error) {
	manifestBytes, err := readDescriptorBounded(root, identityFileName, 4096)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("uninitialized data directory: missing %s", identityFileName)
		}
		return nil, fmt.Errorf("read %s: %w", identityFileName, err)
	}

	manifest, err := parseAndValidateManifest(manifestBytes)
	if err != nil {
		return nil, err
	}

	if manifest.Version != identityManifestVersion {
		return nil, fmt.Errorf("unsupported identity manifest version: %d (expected %d)", manifest.Version, identityManifestVersion)
	}

	pID, err := uuid.Parse(manifest.ProbeID)
	if err != nil || pID == uuid.Nil || manifest.ProbeID != strings.ToLower(pID.String()) {
		return nil, fmt.Errorf("invalid probe_id in identity manifest: must be canonical lowercase non-zero UUID")
	}

	sID, err := uuid.Parse(manifest.StreamID)
	if err != nil || sID == uuid.Nil || manifest.StreamID != strings.ToLower(sID.String()) {
		return nil, fmt.Errorf("invalid stream_id in identity manifest: must be canonical lowercase non-zero UUID")
	}

	if err := validateFingerprintString(manifest.Fingerprint); err != nil {
		return nil, fmt.Errorf("invalid fingerprint in identity manifest: %w", err)
	}

	tlsCert, fp, err := readTLSFile(root, tlsFileName, validateTime)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("corrupt identity: missing %s", tlsFileName)
		}
		return nil, err
	}

	if subtle.ConstantTimeCompare([]byte(fp), []byte(manifest.Fingerprint)) != 1 {
		return nil, errors.New("certificate fingerprint mismatch between identity.json and tls.pem")
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	return &RuntimeIdentity{
		ProbeID:     manifest.ProbeID,
		StreamID:    manifest.StreamID,
		Fingerprint: manifest.Fingerprint,
		DataDir:     dir,
		Certificate: tlsCert,
		lock:        lock,
		root:        root,
	}, nil
}

func readDescriptorBounded(root *os.Root, filename string, maxBytes int64) ([]byte, error) {
	file, err := root.OpenFile(filename, os.O_RDONLY|probeNonblock|probeNoFollow, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", filename, os.ErrNotExist)
		}
		return nil, probeIOError("open "+filename, err)
	}
	defer func() {
		_ = file.Close()
	}()

	info, err := file.Stat()
	if err != nil {
		return nil, probeIOError("stat "+filename, err)
	}

	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", filename)
	}
	if info.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("%s permissions too permissive: %04o (must be 0600)", filename, info.Mode().Perm())
	}
	if !probeTrustedOwner(info) {
		return nil, fmt.Errorf("%s untrusted owner", filename)
	}

	if info.Size() > maxBytes {
		return nil, fmt.Errorf("%s exceeds maximum allowed size (%d bytes)", filename, maxBytes)
	}

	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, probeIOError("read "+filename, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("%s exceeds maximum allowed size (%d bytes)", filename, maxBytes)
	}

	return data, nil
}

func parseAndValidateManifest(data []byte) (*identityManifest, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	tok, err := dec.Token()
	if err != nil {
		return nil, errors.New("parse identity manifest failed: invalid JSON")
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil, errors.New("parse identity manifest failed: expected JSON object")
	}

	seenKeys := make(map[string]bool)
	var (
		version     *int
		probeID     *string
		streamID    *string
		fingerprint *string
	)

	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, errors.New("parse identity manifest failed: invalid JSON token")
		}
		key, ok := tok.(string)
		if !ok {
			return nil, errors.New("parse identity manifest failed: invalid object key")
		}
		if seenKeys[key] {
			return nil, errors.New("duplicate key in identity manifest")
		}
		seenKeys[key] = true

		switch key {
		case "version":
			var v int
			if err := dec.Decode(&v); err != nil {
				return nil, errors.New("parse identity manifest failed: invalid version")
			}
			version = &v
		case "probe_id":
			var s string
			if err := dec.Decode(&s); err != nil {
				return nil, errors.New("parse identity manifest failed: invalid probe_id")
			}
			probeID = &s
		case "stream_id":
			var s string
			if err := dec.Decode(&s); err != nil {
				return nil, errors.New("parse identity manifest failed: invalid stream_id")
			}
			streamID = &s
		case "certificate_fingerprint":
			var s string
			if err := dec.Decode(&s); err != nil {
				return nil, errors.New("parse identity manifest failed: invalid certificate_fingerprint")
			}
			fingerprint = &s
		default:
			var discard any
			if err := dec.Decode(&discard); err != nil {
				return nil, errors.New("parse identity manifest failed: invalid field value")
			}
		}
	}

	tok, err = dec.Token()
	if err != nil {
		return nil, errors.New("parse identity manifest failed: expected closing brace")
	}
	delim, ok = tok.(json.Delim)
	if !ok || delim != '}' {
		return nil, errors.New("parse identity manifest failed: expected closing brace")
	}

	if dec.More() {
		return nil, errors.New("trailing data in identity manifest")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data in identity manifest")
	}

	if version == nil || probeID == nil || streamID == nil || fingerprint == nil {
		return nil, errors.New("missing required fields in identity manifest")
	}

	return &identityManifest{
		Version:     *version,
		ProbeID:     *probeID,
		StreamID:    *streamID,
		Fingerprint: *fingerprint,
	}, nil
}

func readAndValidateTLSFile(root *os.Root, filename string) (tls.Certificate, string, error) {
	return readTLSFile(root, filename, true)
}

func readTLSFile(root *os.Root, filename string, validateTime bool) (tls.Certificate, string, error) {
	data, err := readDescriptorBounded(root, filename, 65536)
	if err != nil {
		return tls.Certificate{}, "", err
	}

	cert, err := tls.X509KeyPair(data, data)
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("parse tls material: invalid certificate or private key")
	}

	if len(cert.Certificate) == 0 {
		return tls.Certificate{}, "", errors.New("parse tls material: missing certificate block")
	}

	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return tls.Certificate{}, "", fmt.Errorf("parse leaf certificate: %w", err)
	}
	cert.Leaf = leaf

	now := time.Now().UTC()
	if validateTime && now.Before(leaf.NotBefore) {
		return tls.Certificate{}, "", fmt.Errorf("certificate is not yet valid: not before %s", leaf.NotBefore.Format(time.RFC3339))
	}
	if validateTime && !now.Before(leaf.NotAfter) {
		return tls.Certificate{}, "", fmt.Errorf("certificate has expired: not after %s", leaf.NotAfter.Format(time.RFC3339))
	}

	sum := sha256.Sum256(cert.Certificate[0])
	fingerprint := hex.EncodeToString(sum[:])

	return cert, fingerprint, nil
}

func generateRuntimeTLS() (tls.Certificate, []byte, []byte, string, error) {
	return generateRuntimeTLSFor(time.Now().UTC(), 365)
}

func generateRuntimeTLSFor(now time.Time, validForDays int) (tls.Certificate, []byte, []byte, string, error) {
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, nil, "", fmt.Errorf("generate ecdsa key: %w", err)
	}

	serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, nil, nil, "", fmt.Errorf("generate certificate serial: %w", err)
	}

	now = now.UTC().Truncate(time.Second)
	notBefore := now.Add(-5 * time.Minute) // allow small initial clock skew
	notAfter := now.Add(time.Duration(validForDays) * 24 * time.Hour)

	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "uptime-phoenix-probe",
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, template, &priv.PublicKey, priv)
	if err != nil {
		return tls.Certificate{}, nil, nil, "", fmt.Errorf("create self-signed certificate: %w", err)
	}

	privBytes, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		return tls.Certificate{}, nil, nil, "", fmt.Errorf("marshal ec private key: %w", err)
	}

	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "EC PRIVATE KEY",
		Bytes: privBytes,
	})

	certPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: derBytes,
	})

	cert, err := tls.X509KeyPair(certPEM, privPEM)
	if err != nil {
		return tls.Certificate{}, nil, nil, "", fmt.Errorf("assemble keypair: %w", err)
	}
	// Populate explicitly: supported Go versions allow x509keypairleaf=0, which
	// otherwise leaves Leaf nil even though the key pair parsed successfully.
	cert.Leaf, err = x509.ParseCertificate(derBytes)
	if err != nil {
		return tls.Certificate{}, nil, nil, "", fmt.Errorf("parse generated certificate: %w", err)
	}

	sum := sha256.Sum256(derBytes)
	fingerprint := hex.EncodeToString(sum[:])

	return cert, certPEM, privPEM, fingerprint, nil
}

func publishFileNoReplace(root *os.Root, filename string, data []byte) error {
	if _, err := root.Lstat(filename); err == nil {
		return fmt.Errorf("%s already exists: %w", filename, os.ErrExist)
	} else if !errors.Is(err, os.ErrNotExist) {
		return probeIOError("stat "+filename, err)
	}

	var suffix [16]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return errors.New("generate staging name failed")
	}
	staging := "." + filename + ".tmp." + hex.EncodeToString(suffix[:])

	stagingFile, err := root.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return probeIOError("create staging file", err)
	}
	defer func() {
		_ = root.Remove(staging)
	}()

	if err := stagingFile.Chmod(0600); err != nil {
		_ = stagingFile.Close()
		return probeIOError("protect staging file", err)
	}

	if n, err := stagingFile.Write(data); err != nil || n != len(data) {
		_ = stagingFile.Close()
		return errors.New("write staging file failed")
	}

	if err := stagingFile.Sync(); err != nil {
		_ = stagingFile.Close()
		return probeIOError("sync staging file", err)
	}

	if err := stagingFile.Close(); err != nil {
		return probeIOError("close staging file", err)
	}

	if err := root.Link(staging, filename); err != nil {
		return probeIOError("publish "+filename, err)
	}

	_ = root.Remove(staging)

	dir, err := root.Open(".")
	if err != nil {
		return probeIOError("open directory for sync", err)
	}
	defer func() {
		_ = dir.Close()
	}()

	if err := dir.Sync(); err != nil {
		return probeIOError("sync directory", err)
	}

	return nil
}

func validateFingerprintString(fp string) error {
	if len(fp) != 64 {
		return errors.New("fingerprint must be exactly 64 characters")
	}
	for i := 0; i < 64; i++ {
		b := fp[i]
		if !((b >= '0' && b <= '9') || (b >= 'a' && b <= 'f')) {
			return errors.New("fingerprint must be lowercase hexadecimal")
		}
	}
	return nil
}

func probeIOError(operation string, err error) error {
	for _, kind := range []error{fs.ErrNotExist, fs.ErrExist, fs.ErrPermission, fs.ErrInvalid} {
		if errors.Is(err, kind) {
			return fmt.Errorf("%s: %w", operation, kind)
		}
	}
	if errors.Is(err, syscall.ELOOP) || strings.Contains(err.Error(), "escapes from parent") || strings.Contains(err.Error(), "symlink") {
		return fmt.Errorf("%s: cannot follow symlink: %w", operation, fs.ErrInvalid)
	}
	return errors.New(operation + " failed")
}
