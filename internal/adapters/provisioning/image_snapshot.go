package provisioning

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"sync"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// ProbeImageSnapshotPreparer retains private copied image bytes, not an OCI layout.
// Its staging parent is trusted local configuration, never a caller source path.
type ProbeImageSnapshotPreparer struct {
	baseDir  string
	verifier *ProbeImageContentVerifier
}

// NewProbeImageSnapshotPreparer uses private temporary storage under baseDir.
// An empty baseDir selects the operating system temporary directory.
func NewProbeImageSnapshotPreparer(baseDir string) *ProbeImageSnapshotPreparer {
	return &ProbeImageSnapshotPreparer{baseDir: baseDir, verifier: NewProbeImageContentVerifier()}
}

// PrepareImageSnapshot consumes inputs and verifies private copies before return.
func (p *ProbeImageSnapshotPreparer) PrepareImageSnapshot(ctx context.Context, pin domain.ProbeImagePin, in ports.ProbeImageContentInputs) (prepared ports.PreparedProbeImage, err error) {
	layers := append([]io.ReadCloser(nil), in.Layers...)
	sources, valid := imageSnapshotSources(in.Metadata, layers)
	var owned *preparedProbeImage
	var stop func() bool
	joined := make(chan struct{})
	closeSources := func() {
		for _, s := range sources {
			s.close()
		}
	}
	defer func() {
		closeSources()
		if stop != nil && !stop() {
			<-joined
		}
		if !imageSnapshotNil(ctx) && ctx.Err() != nil {
			prepared, err = nil, ctx.Err()
		}
		if prepared == nil && owned != nil {
			cleanupErr := owned.Close(context.Background())
			if cleanupErr != nil {
				err = domain.ErrProbeImageSnapshotStorage
			}
		}
	}()
	if p == nil || imageSnapshotNil(ctx) || !pin.Valid() || !valid || len(layers) < 1 || len(layers) > domain.MaxProbeImageLayers {
		return nil, domain.ErrProbeImageInput
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	stop = context.AfterFunc(ctx, func() { defer close(joined); closeSources() })
	dir, e := os.MkdirTemp(p.baseDir, "phoenix-probe-image-")
	if e != nil {
		return nil, domain.ErrProbeImageSnapshotStorage
	}
	identity, e := os.Lstat(dir)
	if e != nil {
		// No pathname recursion: at most the newly created empty directory.
		_ = os.Remove(dir)
		return nil, domain.ErrProbeImageSnapshotStorage
	}
	owned = &preparedProbeImage{dir: dir, identity: identity, readers: make(map[*imageSnapshotReader]struct{})}
	owned.root, e = os.OpenRoot(dir)
	if e != nil {
		return nil, domain.ErrProbeImageSnapshotStorage
	}
	wrapped := make([]io.ReadCloser, len(sources))
	for i, source := range sources {
		name, maxBytes := fmt.Sprintf("layer-%03d", i-3), domain.MaxProbeImageLayerBytes
		if i < 3 {
			name, maxBytes = []string{"index", "manifest", "config"}[i], domain.MaxProbeImageMetadataSize
		}
		file := &imageSnapshotFile{name: name, limit: maxBytes}
		owned.files = append(owned.files, file)
		wrapped[i] = &imageSnapshotCopyReader{source: source, file: file, owner: owned, layer: i >= 3}
	}
	verifier := p.verifier
	if verifier == nil {
		verifier = NewProbeImageContentVerifier()
	}
	first, e := verifier.VerifyImageContent(ctx, pin, imageSnapshotBundle(wrapped))
	if owned.copyFailed {
		return nil, domain.ErrProbeImageSnapshotStorage
	}
	if e != nil {
		return nil, e
	}
	if first == nil || !first.Valid() || first.Selection.Pin != pin || first.LayerBytes != owned.layerBytes {
		return nil, domain.ErrProbeImageMetadata
	}
	for _, file := range owned.files {
		if file.writer == nil || file.writer.Sync() != nil || file.writer.Chmod(0400) != nil {
			return nil, domain.ErrProbeImageSnapshotStorage
		}
		file.identity, e = file.writer.Stat()
		if e != nil || file.identity.Size() != file.size {
			return nil, domain.ErrProbeImageSnapshotStorage
		}
		if file.writer.Close() != nil {
			return nil, domain.ErrProbeImageSnapshotStorage
		}
		file.writer = nil
	}
	owned.verification = *first
	copies, e := owned.Open(ctx)
	if e != nil {
		return nil, e
	}
	second, e := verifier.VerifyImageContent(ctx, pin, copies)
	if e != nil {
		return nil, e
	}
	if second == nil || *second != *first {
		return nil, domain.ErrProbeImageMetadata
	}
	return owned, nil
}

type imageSnapshotSource struct {
	input io.ReadCloser
	once  sync.Once
}

func (s *imageSnapshotSource) close() { s.once.Do(func() { _ = s.input.Close() }) }

func imageSnapshotNil(value any) bool {
	if value == nil {
		return true
	}
	switch v := reflect.ValueOf(value); v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return v.IsNil()
	default:
		return false
	}
}

func imageSnapshotSources(metadata ports.ProbeImageInputs, layers []io.ReadCloser) ([]*imageSnapshotSource, bool) {
	all := append([]io.ReadCloser{metadata.Index, metadata.Manifest, metadata.Config}, layers...)
	seen := make(map[io.ReadCloser]bool)
	var sources []*imageSnapshotSource
	valid := true
	for _, r := range all {
		if imageSnapshotNil(r) {
			valid = false
			continue
		}
		if reflect.ValueOf(r).Comparable() {
			if seen[r] {
				valid = false
				continue
			}
			seen[r] = true
		}
		sources = append(sources, &imageSnapshotSource{input: r})
	}
	return sources, valid
}

func imageSnapshotBundle(streams []io.ReadCloser) ports.ProbeImageContentInputs {
	return ports.ProbeImageContentInputs{Metadata: ports.ProbeImageInputs{Index: streams[0], Manifest: streams[1], Config: streams[2]}, Layers: append([]io.ReadCloser(nil), streams[3:]...)}
}

type imageSnapshotFile struct {
	name     string
	limit    int64
	size     int64
	identity os.FileInfo
	writer   *os.File
}

type imageSnapshotCopyReader struct {
	source *imageSnapshotSource
	file   *imageSnapshotFile
	owner  *preparedProbeImage
	layer  bool
}

func (r *imageSnapshotCopyReader) Read(b []byte) (int, error) {
	if len(b) == 0 {
		return 0, nil
	}
	if r.file.writer == nil {
		f, err := r.owner.root.OpenFile(r.file.name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			r.owner.copyFailed = true
			return 0, domain.ErrProbeImageSnapshotStorage
		}
		r.file.writer = f
		r.file.identity, err = f.Stat()
		if err != nil {
			r.owner.copyFailed = true
			return 0, domain.ErrProbeImageSnapshotStorage
		}
	}
	n, err := r.source.input.Read(b)
	keep := min(int64(n), r.file.limit-r.file.size)
	if r.layer {
		keep = min(keep, domain.MaxProbeImageContentBytes-r.owner.layerBytes)
	}
	if keep > 0 {
		written, writeErr := r.file.writer.Write(b[:keep])
		r.file.size += int64(written)
		if r.layer {
			r.owner.layerBytes += int64(written)
		}
		if writeErr != nil || int64(written) != keep {
			r.owner.copyFailed = true
			return written, domain.ErrProbeImageSnapshotStorage
		}
	}
	// A refusal may inspect a size+1 sentinel. Do not persist bytes beyond
	// fixed storage budgets; the original verifier still sees the sentinel.
	if err == io.EOF {
		r.source.close()
	}
	return n, err
}
func (r *imageSnapshotCopyReader) Close() error { r.source.close(); return nil }

type preparedProbeImage struct {
	mu           sync.Mutex
	closeMu      sync.Mutex
	dir          string
	identity     os.FileInfo
	root         *os.Root
	files        []*imageSnapshotFile
	verification domain.ProbeImageContentVerification
	layerBytes   int64
	copyFailed   bool
	closed       bool
	removed      bool
	readers      map[*imageSnapshotReader]struct{}
}

func (b *preparedProbeImage) Verification() domain.ProbeImageContentVerification {
	return b.verification
}

func (b *preparedProbeImage) Open(ctx context.Context) (ports.ProbeImageContentInputs, error) {
	if imageSnapshotNil(ctx) {
		return ports.ProbeImageContentInputs{}, domain.ErrProbeImageInput
	}
	if ctx.Err() != nil {
		return ports.ProbeImageContentInputs{}, ctx.Err()
	}
	b.mu.Lock()
	var opened []*imageSnapshotReader
	fail := func(err error) (ports.ProbeImageContentInputs, error) {
		b.mu.Unlock()
		for _, r := range opened {
			_ = r.Close()
		}
		return ports.ProbeImageContentInputs{}, err
	}
	if b.closed {
		return fail(domain.ErrProbeImageSnapshotClosed)
	}
	if len(b.readers) != 0 {
		return fail(domain.ErrProbeImageSnapshotBusy)
	}
	current, err := os.Lstat(b.dir)
	if err != nil || !current.IsDir() || current.Mode().Perm() != 0700 || !os.SameFile(current, b.identity) {
		return fail(domain.ErrProbeImageSnapshotStorage)
	}
	streams := make([]io.ReadCloser, 0, len(b.files))
	for _, entry := range b.files {
		info, e := b.root.Lstat(entry.name)
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || !os.SameFile(info, entry.identity) || info.Size() != entry.size {
			return fail(domain.ErrProbeImageSnapshotStorage)
		}
		f, e := b.root.Open(entry.name)
		if e != nil {
			return fail(domain.ErrProbeImageSnapshotStorage)
		}
		info, e = f.Stat()
		if e != nil || !os.SameFile(info, entry.identity) || info.Size() != entry.size || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 {
			_ = f.Close()
			return fail(domain.ErrProbeImageSnapshotStorage)
		}
		r := &imageSnapshotReader{file: f, owner: b, ctx: ctx, joined: make(chan struct{})}
		b.readers[r] = struct{}{}
		opened = append(opened, r)
		streams = append(streams, r)
		r.stop = context.AfterFunc(ctx, func() {
			r.closeFile()
			b.mu.Lock()
			close(r.joined)
			delete(b.readers, r)
			b.mu.Unlock()
		})
	}
	b.mu.Unlock()
	if ctx.Err() != nil {
		for _, r := range opened {
			_ = r.Close()
		}
		return ports.ProbeImageContentInputs{}, ctx.Err()
	}
	return imageSnapshotBundle(streams), nil
}

func (b *preparedProbeImage) Close(_ context.Context) error {
	b.closeMu.Lock()
	defer b.closeMu.Unlock()
	b.mu.Lock()
	b.closed = true
	readers := make([]*imageSnapshotReader, 0, len(b.readers))
	for r := range b.readers {
		readers = append(readers, r)
	}
	b.mu.Unlock()
	for _, r := range readers {
		_ = r.Close()
	}
	for _, file := range b.files {
		if file.writer != nil {
			_ = file.writer.Close()
			file.writer = nil
		}
	}
	if b.removed {
		return nil
	}
	if b.root != nil {
		defer func() { _ = b.root.Close(); b.root = nil }()
	}
	current, err := os.Lstat(b.dir)
	if err != nil || !current.IsDir() || !os.SameFile(current, b.identity) {
		return domain.ErrProbeImageSnapshotStorage
	}
	if b.root == nil {
		b.root, err = os.OpenRoot(b.dir)
		if err != nil {
			return domain.ErrProbeImageSnapshotStorage
		}
		defer func() { _ = b.root.Close(); b.root = nil }()
	}
	// Refuse unknown contents before removing any known file. Read at most
	// the canonical bounded file count plus one, not an unbounded directory.
	directory, err := b.root.Open(".")
	if err != nil {
		return domain.ErrProbeImageSnapshotStorage
	}
	entries, readErr := directory.ReadDir(len(b.files) + 1)
	closeErr := directory.Close()
	if (readErr != nil && readErr != io.EOF) || closeErr != nil || len(entries) > len(b.files) {
		return domain.ErrProbeImageSnapshotStorage
	}
	known := make(map[string]bool, len(b.files))
	for _, file := range b.files {
		known[file.name] = true
	}
	for _, entry := range entries {
		if !known[entry.Name()] {
			return domain.ErrProbeImageSnapshotStorage
		}
	}
	// Check the complete known graph before removing anything. Missing owned
	// files can be a previous partial cleanup; replacement/unknown files are not.
	for _, file := range b.files {
		info, e := b.root.Lstat(file.name)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil || file.identity == nil || !info.Mode().IsRegular() || !os.SameFile(info, file.identity) {
			return domain.ErrProbeImageSnapshotStorage
		}
	}
	for _, file := range b.files {
		if e := b.root.Remove(file.name); e != nil && !os.IsNotExist(e) {
			return domain.ErrProbeImageSnapshotStorage
		}
	}
	current, err = os.Lstat(b.dir)
	if err != nil || !os.SameFile(current, b.identity) || os.Remove(b.dir) != nil {
		return domain.ErrProbeImageSnapshotStorage
	}
	b.removed = true
	return nil
}

type imageSnapshotReader struct {
	file      *os.File
	owner     *preparedProbeImage
	ctx       context.Context
	stop      func() bool
	joined    chan struct{}
	closeOnce sync.Once
	fileOnce  sync.Once
	err       error
}

func (r *imageSnapshotReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.file.Read(b)
	if err == nil || err == io.EOF {
		return n, err
	}
	if r.ctx.Err() != nil {
		return n, r.ctx.Err()
	}
	if errors.Is(err, os.ErrClosed) {
		return n, domain.ErrProbeImageSnapshotClosed
	}
	return n, domain.ErrProbeImageSnapshotStorage
}
func (r *imageSnapshotReader) Close() error {
	r.closeOnce.Do(func() {
		r.owner.mu.Lock()
		stop := r.stop
		r.owner.mu.Unlock()
		stopped := stop == nil || stop()
		r.closeFile()
		if !stopped {
			<-r.joined
		}
		r.owner.mu.Lock()
		delete(r.owner.readers, r)
		r.owner.mu.Unlock()
	})
	return r.err
}

func (r *imageSnapshotReader) closeFile() {
	r.fileOnce.Do(func() {
		if r.file.Close() != nil {
			r.err = domain.ErrProbeImageSnapshotStorage
		}
	})
}

var _ ports.ProbeImageSnapshotPreparer = (*ProbeImageSnapshotPreparer)(nil)
var _ ports.PreparedProbeImage = (*preparedProbeImage)(nil)
