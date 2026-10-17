package provisioning

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeBinaryPreparer verifies a private copy of a local release binary.
// BaseDir is a trusted local staging parent, never an HTTP input or source path.
type ProbeBinaryPreparer struct{ baseDir string }

// NewProbeBinaryPreparer selects a staging parent; empty uses the OS temp dir.
func NewProbeBinaryPreparer(baseDir string) *ProbeBinaryPreparer {
	return &ProbeBinaryPreparer{baseDir: baseDir}
}

var _ ports.ProbeBinaryPreparer = (*ProbeBinaryPreparer)(nil)

// PrepareBinary consumes and closes input, even on invalid pins or cancellation.
// The closer must unblock Read. Only the copied snapshot is inspected/returned.
func (p *ProbeBinaryPreparer) PrepareBinary(ctx context.Context, pin domain.ProbeBinaryPin, input io.ReadCloser) (prepared ports.PreparedProbeBinary, err error) {
	if input == nil {
		return nil, domain.ErrProbeArtifactInput
	}
	var once sync.Once
	closeInput := func() { once.Do(func() { _ = input.Close() }) }
	defer closeInput()
	if ctx == nil || !pin.Valid() {
		return nil, domain.ErrProbeArtifactInput
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	joined := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(joined); closeInput() })
	defer func() {
		if !stop() {
			<-joined
		}
	}()
	dir, e := os.MkdirTemp(p.baseDir, "phoenix-probe-artifact-")
	if e != nil {
		return nil, domain.ErrProbeArtifactStorage
	}
	identity, e := os.Lstat(dir)
	if e != nil {
		return nil, domain.ErrProbeArtifactStorage
	}
	owned := &preparedProbeBinary{pin: pin, dir: dir, identity: identity, readers: make(map[*artifactReader]struct{})}
	defer func() {
		if prepared == nil {
			_ = owned.Close(context.WithoutCancel(ctx))
		}
	}()
	file, e := os.OpenFile(filepath.Join(dir, "probe"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if e != nil {
		return nil, domain.ErrProbeArtifactStorage
	}
	defer func() { _ = file.Close() }()
	hash := sha256.New()
	n, e := io.CopyBuffer(io.MultiWriter(file, hash), io.LimitReader(artifactContextReader{ctx, input}, pin.Size+1), make([]byte, 64*1024))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if e != nil {
		return nil, domain.ErrProbeArtifactStorage
	}
	if n != pin.Size {
		return nil, domain.ErrProbeArtifactSize
	}
	if hex.EncodeToString(hash.Sum(nil)) != pin.SHA256 {
		return nil, domain.ErrProbeArtifactDigest
	}
	if !validProbeBinary(ctx, file, pin) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, domain.ErrProbeArtifactMetadata
	}
	if e = file.Sync(); e != nil {
		return nil, domain.ErrProbeArtifactStorage
	}
	if e = file.Chmod(0400); e != nil {
		return nil, domain.ErrProbeArtifactStorage
	}
	owned.fileIdentity, e = file.Stat()
	if e != nil {
		return nil, domain.ErrProbeArtifactStorage
	}
	if e = file.Close(); e != nil {
		return nil, domain.ErrProbeArtifactStorage
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return owned, nil
}

type artifactContextReader struct {
	ctx   context.Context
	input io.Reader
}

func (r artifactContextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.input.Read(p)
}

type artifactContextReaderAt struct {
	ctx   context.Context
	input io.ReaderAt
}

func (r artifactContextReaderAt) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.input.ReadAt(p, offset)
}

func validProbeBinary(ctx context.Context, input io.ReaderAt, pin domain.ProbeBinaryPin) bool {
	r := artifactContextReaderAt{ctx: ctx, input: input}
	f, err := elf.NewFile(r)
	if err != nil {
		return false
	}
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB || f.Type != elf.ET_EXEC || (f.OSABI != elf.ELFOSABI_NONE && f.OSABI != elf.ELFOSABI_LINUX) {
		return false
	}
	want := elf.EM_X86_64
	if pin.Architecture == "arm64" {
		want = elf.EM_AARCH64
	}
	if f.Machine != want {
		return false
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return false
		}
	}
	info, err := buildinfo.Read(r)
	if err != nil || info.Path != "github.com/fiztoz/uptime-phoenix/cmd/probe" || info.Main.Path != "github.com/fiztoz/uptime-phoenix" {
		return false
	}
	settings := make(map[string]string, len(info.Settings))
	for _, s := range info.Settings {
		if _, exists := settings[s.Key]; exists {
			return false
		}
		settings[s.Key] = s.Value
	}
	// -trimpath release builds omit -ldflags. Version provenance is the trusted
	// operator pin, not a fabricated inference from strings or build settings.
	return settings["GOOS"] == "linux" && settings["GOARCH"] == pin.Architecture && settings["CGO_ENABLED"] == "0"
}

type preparedProbeBinary struct {
	mu           sync.Mutex
	pin          domain.ProbeBinaryPin
	dir          string
	identity     os.FileInfo
	fileIdentity os.FileInfo
	closed       bool
	readers      map[*artifactReader]struct{}
}

func (b *preparedProbeBinary) Pin() domain.ProbeBinaryPin { return b.pin }
func (b *preparedProbeBinary) Open(ctx context.Context) (io.ReadCloser, error) {
	if ctx == nil {
		return nil, domain.ErrProbeArtifactInput
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil, domain.ErrProbeArtifactClosed
	}
	current, err := os.Lstat(b.dir)
	if err != nil || !os.SameFile(current, b.identity) {
		return nil, domain.ErrProbeArtifactStorage
	}
	f, err := os.Open(filepath.Join(b.dir, "probe"))
	if err != nil {
		return nil, domain.ErrProbeArtifactStorage
	}
	info, err := f.Stat()
	if err != nil || !os.SameFile(info, b.fileIdentity) || info.Size() != b.pin.Size {
		_ = f.Close()
		return nil, domain.ErrProbeArtifactStorage
	}
	r := &artifactReader{file: f, owner: b, ctx: ctx}
	b.readers[r] = struct{}{}
	r.stop = context.AfterFunc(ctx, func() { _ = r.Close() })
	return r, nil
}
func (b *preparedProbeBinary) Close(ctx context.Context) error {
	b.mu.Lock()
	b.closed = true
	readers := make([]*artifactReader, 0, len(b.readers))
	for r := range b.readers {
		readers = append(readers, r)
	}
	b.mu.Unlock()
	for _, r := range readers {
		_ = r.Close()
	}
	current, err := os.Lstat(b.dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !current.IsDir() || !os.SameFile(current, b.identity) {
		return domain.ErrProbeArtifactStorage
	}
	if os.RemoveAll(b.dir) != nil {
		return domain.ErrProbeArtifactStorage
	}
	return nil
}

type artifactReader struct {
	file  *os.File
	owner *preparedProbeBinary
	ctx   context.Context
	stop  func() bool
	once  sync.Once
	err   error
}

func (r *artifactReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.file.Read(p)
	if err == nil || errors.Is(err, io.EOF) {
		return n, err
	}
	if r.ctx.Err() != nil {
		return n, r.ctx.Err()
	}
	if errors.Is(err, os.ErrClosed) {
		return n, domain.ErrProbeArtifactClosed
	}
	return n, domain.ErrProbeArtifactStorage
}
func (r *artifactReader) Close() error {
	r.once.Do(func() {
		// Close can run from the context callback before Open publishes stop.
		// Stop is installed while holding owner.mu, so read it under that lock.
		r.owner.mu.Lock()
		stop := r.stop
		delete(r.owner.readers, r)
		r.owner.mu.Unlock()
		if stop != nil {
			stop()
		}
		if r.file.Close() != nil {
			r.err = domain.ErrProbeArtifactStorage
		}
	})
	return r.err
}
