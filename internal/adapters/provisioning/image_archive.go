package provisioning

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"os"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	probeImageArchivePrefix   = "phoenix-probe-archive-"
	probeImageArchiveFile     = "archive.tar"
	probeImageOCILayoutName   = "oci-layout"
	probeImageOCILayoutJSON   = `{"imageLayoutVersion":"1.0.0"}`
	probeImageLayoutIndexName = "index.json"
)

// errProbeImageArchiveWrite is private. Callers only observe fixed sentinels.
var errProbeImageArchiveWrite = errors.New("probe image archive write")

// ProbeImageArchivePacker writes one private uncompressed selected-layout tar.
// baseDir is trusted local staging; an empty value selects the OS temp directory.
// It accepts no caller path and no arbitrary archive input.
type ProbeImageArchivePacker struct {
	baseDir string
	limit   int64
	// beforeSeal and writeFault are same-package test seams. Production leaves
	// both nil. They must not leak diagnostics or change the published contract.
	beforeSeal func(*os.File) error
	writeFault func() bool
}

// NewProbeImageArchivePacker uses private temporary storage under baseDir.
func NewProbeImageArchivePacker(baseDir string) *ProbeImageArchivePacker {
	return &ProbeImageArchivePacker{baseDir: baseDir, limit: domain.MaxProbeImageArchiveBytes}
}

type archivePackState struct{ writeFailed bool }

type probeLayoutPlatform struct {
	Architecture string `json:"architecture"`
	OS           string `json:"os"`
}

type probeLayoutManifest struct {
	MediaType string              `json:"mediaType"`
	Digest    string              `json:"digest"`
	Size      int64               `json:"size"`
	Platform  probeLayoutPlatform `json:"platform"`
}

type probeLayoutIndex struct {
	SchemaVersion int                   `json:"schemaVersion"`
	MediaType     string                `json:"mediaType"`
	Manifests     []probeLayoutManifest `json:"manifests"`
}

// PackImageArchive borrows image for one bundle and never closes it. Original
// index trust stays on the content verifier; the tar root index is a distinct
// single-target document. No daemon, load, registry or installation is involved.
func (p *ProbeImageArchivePacker) PackImageArchive(ctx context.Context, pin domain.ProbeImagePin, image ports.PreparedProbeImage) (prepared ports.PreparedProbeImageArchive, err error) {
	var sources []*imageSnapshotSource
	var stop func() bool
	var owned *preparedProbeImageArchive
	state := &archivePackState{}
	joined := make(chan struct{})
	closeSources := func() {
		for _, source := range sources {
			source.close()
		}
	}
	defer func() {
		closeSources()
		if stop != nil && !stop() {
			<-joined
		}
		if state.writeFailed {
			prepared, err = nil, domain.ErrProbeImageArchiveStorage
		}
		storageCause := errors.Is(err, domain.ErrProbeImageArchiveStorage) || errors.Is(err, domain.ErrProbeImageSnapshotStorage)
		if !storageCause && !imageSnapshotNil(ctx) && ctx.Err() != nil {
			prepared, err = nil, ctx.Err()
		}
		if prepared == nil && owned != nil && owned.Close(context.Background()) != nil {
			err = domain.ErrProbeImageArchiveStorage
		}
	}()
	if p == nil || imageSnapshotNil(ctx) || imageSnapshotNil(image) || !pin.Valid() {
		return nil, domain.ErrProbeImageInput
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	entry := image.Verification()
	if !entry.Valid() || entry.Selection.Pin != pin {
		return nil, domain.ErrProbeImageMetadata
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	bundle, openErr := image.Open(ctx)
	layers := append([]io.ReadCloser(nil), bundle.Layers...)
	var streamsOK bool
	sources, streamsOK = imageSnapshotSources(bundle.Metadata, layers)
	stop = context.AfterFunc(ctx, func() { defer close(joined); closeSources() })
	if openErr != nil {
		return nil, bareProbeImageErr(openErr)
	}
	if !streamsOK || len(sources) != 3+len(layers) {
		return nil, domain.ErrProbeImageInput
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if len(layers) != entry.Selection.LayerCount {
		return nil, domain.ErrProbeImageInput
	}
	indexBytes, err := readProbeImageBlob(ctx, sources[0].input, pin.IndexSize, pin.IndexSHA256)
	if err != nil {
		return nil, bareProbeImageErr(err)
	}
	selected, err := selectProbeImage(ctx, indexBytes, pin.Architecture)
	if err != nil {
		return nil, bareProbeImageErr(err)
	}
	if entry.Selection.ManifestSHA256 != selected.sha {
		return nil, domain.ErrProbeImageMetadata
	}
	manifestBytes, err := readProbeImageBlob(ctx, sources[1].input, selected.size, selected.sha)
	if err != nil {
		return nil, bareProbeImageErr(err)
	}
	configDesc, layerDescs, err := probeImageManifest(ctx, manifestBytes, selected.media)
	if err != nil {
		return nil, bareProbeImageErr(err)
	}
	configBytes, err := readProbeImageBlob(ctx, sources[2].input, configDesc.size, configDesc.sha)
	if err != nil {
		return nil, bareProbeImageErr(err)
	}
	if err = validateProbeImageConfig(ctx, configBytes, pin, len(layerDescs)); err != nil {
		return nil, bareProbeImageErr(err)
	}
	if entry.Selection.ConfigSHA256 != configDesc.sha || entry.Selection.LayerCount != len(layerDescs) || len(layers) != len(layerDescs) {
		return nil, domain.ErrProbeImageMetadata
	}
	var declared int64
	blobs := make(map[string]int64, 2+len(layerDescs))
	if err = noteArchiveBlob(blobs, selected.sha, int64(len(manifestBytes))); err != nil {
		return nil, err
	}
	if err = noteArchiveBlob(blobs, configDesc.sha, int64(len(configBytes))); err != nil {
		return nil, err
	}
	for _, desc := range layerDescs {
		if _, ok := probeImageLayerGzip(desc.media); !ok {
			return nil, domain.ErrProbeImageContentEncoding
		}
		if desc.size > domain.MaxProbeImageLayerBytes || desc.size > domain.MaxProbeImageContentBytes-declared {
			return nil, domain.ErrProbeImageContentLimit
		}
		declared += desc.size
		if err = noteArchiveBlob(blobs, desc.sha, desc.size); err != nil {
			return nil, err
		}
	}
	layout, err := probeImageLayoutIndex(pin, selected)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(p.baseDir, probeImageArchivePrefix)
	if err != nil {
		return nil, domain.ErrProbeImageArchiveStorage
	}
	identity, err := os.Lstat(dir)
	if err != nil {
		_ = os.Remove(dir)
		return nil, domain.ErrProbeImageArchiveStorage
	}
	file := &imageSnapshotFile{name: probeImageArchiveFile, limit: p.archiveLimit()}
	owned = &preparedProbeImageArchive{inner: &preparedProbeImage{
		dir: dir, identity: identity, files: []*imageSnapshotFile{file},
		readers: make(map[*imageSnapshotReader]struct{}),
	}}
	owned.inner.root, err = os.OpenRoot(dir)
	if err != nil {
		return nil, domain.ErrProbeImageArchiveStorage
	}
	raw, err := owned.inner.root.OpenFile(file.name, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, domain.ErrProbeImageArchiveStorage
	}
	file.writer = raw
	file.identity, err = raw.Stat()
	if err != nil {
		return nil, domain.ErrProbeImageArchiveStorage
	}
	sink := &archiveFileWriter{file: raw, hash: sha256.New(), limit: p.archiveLimit(), fault: p.writeFault, state: state}
	tw := tar.NewWriter(sink)
	if err = writeArchiveRegular(tw, probeImageOCILayoutName, []byte(probeImageOCILayoutJSON)); err != nil {
		state.writeFailed = true
		return nil, domain.ErrProbeImageArchiveStorage
	}
	if err = writeArchiveRegular(tw, probeImageLayoutIndexName, layout); err != nil {
		state.writeFailed = true
		return nil, domain.ErrProbeImageArchiveStorage
	}
	written := make(map[string]bool, len(blobs))
	if err = writeArchiveBlob(tw, written, selected.sha, manifestBytes); err != nil {
		state.writeFailed = true
		return nil, domain.ErrProbeImageArchiveStorage
	}
	if err = writeArchiveBlob(tw, written, configDesc.sha, configBytes); err != nil {
		state.writeFailed = true
		return nil, domain.ErrProbeImageArchiveStorage
	}
	replay := make([]io.ReadCloser, len(layerDescs))
	for i, desc := range layerDescs {
		name, nameErr := probeImageBlobPath(desc.sha)
		if nameErr != nil {
			return nil, nameErr
		}
		replay[i] = &archiveLayerReader{
			source: sources[3+i], tw: tw, name: name, size: desc.size,
			skip: written[desc.sha], state: state,
		}
		written[desc.sha] = true
	}
	verified, verifyErr := NewProbeImageContentVerifier().VerifyImageContent(ctx, pin, ports.ProbeImageContentInputs{
		Metadata: ports.ProbeImageInputs{
			Index:    io.NopCloser(bytes.NewReader(indexBytes)),
			Manifest: io.NopCloser(bytes.NewReader(manifestBytes)),
			Config:   io.NopCloser(bytes.NewReader(configBytes)),
		},
		Layers: replay,
	})
	var tarErr error
	if !state.writeFailed {
		tarErr = tw.Close()
	}
	if state.writeFailed || sink.failed {
		state.writeFailed = true
		return nil, domain.ErrProbeImageArchiveStorage
	}
	if verifyErr != nil {
		return nil, bareProbeImageErr(verifyErr)
	}
	if tarErr != nil || verified == nil || *verified != entry {
		if tarErr != nil {
			return nil, domain.ErrProbeImageArchiveStorage
		}
		return nil, domain.ErrProbeImageMetadata
	}
	if p.beforeSeal != nil && p.beforeSeal(raw) != nil {
		return nil, domain.ErrProbeImageArchiveStorage
	}
	info, err := raw.Stat()
	if err != nil || !os.SameFile(info, file.identity) {
		return nil, domain.ErrProbeImageArchiveStorage
	}
	if raw.Sync() != nil || raw.Chmod(0400) != nil {
		return nil, domain.ErrProbeImageArchiveStorage
	}
	info, err = raw.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || !os.SameFile(info, file.identity) {
		return nil, domain.ErrProbeImageArchiveStorage
	}
	file.identity = info
	file.size = sink.n
	if raw.Close() != nil {
		return nil, domain.ErrProbeImageArchiveStorage
	}
	file.writer = nil
	digest := hex.EncodeToString(sink.hash.Sum(nil))
	if err = readbackProbeImageArchive(ctx, owned.inner.root, file, digest); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(layout)
	descriptor := domain.ProbeImageArchiveDescriptor{
		Content:           entry,
		LayoutIndexSHA256: hex.EncodeToString(sum[:]),
		LayoutIndexSize:   int64(len(layout)),
		ArchiveSHA256:     digest,
		ArchiveSize:       file.size,
	}
	if !descriptor.Valid() || descriptor.Content != entry || descriptor.LayoutIndexSHA256 == "" {
		return nil, domain.ErrProbeImageArchiveIntegrity
	}
	owned.descriptor = descriptor
	return owned, nil
}

func (p *ProbeImageArchivePacker) archiveLimit() int64 {
	if p == nil || p.limit <= 0 || p.limit > domain.MaxProbeImageArchiveBytes {
		return domain.MaxProbeImageArchiveBytes
	}
	return p.limit
}

func noteArchiveBlob(seen map[string]int64, sha string, size int64) error {
	if len(sha) != 64 || size <= 0 {
		return domain.ErrProbeImageMetadata
	}
	if prev, ok := seen[sha]; ok && prev != size {
		return domain.ErrProbeImageMetadata
	}
	seen[sha] = size
	return nil
}

func probeImageBlobPath(sha string) (string, error) {
	if _, err := hex.DecodeString(sha); err != nil || len(sha) != 64 {
		return "", domain.ErrProbeImageMetadata
	}
	return "blobs/sha256/" + sha, nil
}

func probeImageLayoutIndex(pin domain.ProbeImagePin, manifest probeImageDescriptor) ([]byte, error) {
	if manifest.media == "" || manifest.size <= 0 || manifest.size > domain.MaxProbeImageMetadataSize {
		return nil, domain.ErrProbeImageMetadata
	}
	encoded, err := json.Marshal(probeLayoutIndex{
		SchemaVersion: 2,
		MediaType:     probeOCIIndex,
		Manifests: []probeLayoutManifest{{
			MediaType: manifest.media,
			Digest:    "sha256:" + manifest.sha,
			Size:      manifest.size,
			Platform:  probeLayoutPlatform{Architecture: pin.Architecture, OS: "linux"},
		}},
	})
	if err != nil || int64(len(encoded)) == 0 || int64(len(encoded)) > domain.MaxProbeImageMetadataSize {
		return nil, domain.ErrProbeImageMetadata
	}
	return encoded, nil
}

func archiveHeader(name string, size int64) *tar.Header {
	return &tar.Header{
		Name:     name,
		Mode:     0o444,
		Size:     size,
		ModTime:  time.Unix(0, 0).UTC(),
		Typeflag: tar.TypeReg,
		Format:   tar.FormatUSTAR,
	}
}

func writeArchiveRegular(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(archiveHeader(name, int64(len(data)))); err != nil {
		return errProbeImageArchiveWrite
	}
	if _, err := tw.Write(data); err != nil {
		return errProbeImageArchiveWrite
	}
	return nil
}

func writeArchiveBlob(tw *tar.Writer, written map[string]bool, sha string, data []byte) error {
	if written[sha] {
		return nil
	}
	name, err := probeImageBlobPath(sha)
	if err != nil {
		return err
	}
	if err = writeArchiveRegular(tw, name, data); err != nil {
		return err
	}
	written[sha] = true
	return nil
}

type archiveFileWriter struct {
	file   *os.File
	hash   hash.Hash
	n      int64
	limit  int64
	fault  func() bool
	state  *archivePackState
	failed bool
}

func (w *archiveFileWriter) Write(p []byte) (int, error) {
	if w.failed || w.state.writeFailed {
		w.failed = true
		w.state.writeFailed = true
		return 0, errProbeImageArchiveWrite
	}
	if len(p) == 0 {
		return 0, nil
	}
	fault := w.fault != nil && w.fault()
	if fault || int64(len(p)) > w.limit-w.n {
		w.failed = true
		w.state.writeFailed = true
		return 0, errProbeImageArchiveWrite
	}
	n, err := w.file.Write(p)
	if n > 0 {
		_, _ = w.hash.Write(p[:n])
		w.n += int64(n)
	}
	if err != nil || n != len(p) {
		w.failed = true
		w.state.writeFailed = true
		return n, errProbeImageArchiveWrite
	}
	return n, nil
}

type archiveLayerReader struct {
	source  *imageSnapshotSource
	tw      *tar.Writer
	name    string
	size    int64
	written int64
	skip    bool
	started bool
	eof     bool
	state   *archivePackState
}

func (r *archiveLayerReader) Read(p []byte) (int, error) {
	if r.eof {
		return 0, io.EOF
	}
	n, err := r.source.input.Read(p)
	if n > 0 && !r.skip && !r.state.writeFailed {
		keep := int64(n)
		if room := r.size - r.written; keep > room {
			keep = room
		}
		if keep < 0 {
			keep = 0
		}
		if keep > 0 {
			if !r.started {
				if headerErr := r.tw.WriteHeader(archiveHeader(r.name, r.size)); headerErr != nil {
					r.state.writeFailed = true
					return n, errProbeImageArchiveWrite
				}
				r.started = true
			}
			if _, writeErr := r.tw.Write(p[:keep]); writeErr != nil {
				r.state.writeFailed = true
				return n, errProbeImageArchiveWrite
			}
			r.written += keep
		}
	}
	if err == io.EOF {
		r.eof = true
		r.source.close()
	}
	return n, err
}

func (r *archiveLayerReader) Close() error {
	r.source.close()
	return nil
}

func readbackProbeImageArchive(ctx context.Context, root *os.Root, file *imageSnapshotFile, digest string) error {
	opened, err := root.Open(file.name)
	if err != nil {
		return domain.ErrProbeImageArchiveStorage
	}
	defer func() { _ = opened.Close() }()
	info, err := opened.Stat()
	if err != nil || file.identity == nil || !os.SameFile(info, file.identity) || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 {
		return domain.ErrProbeImageArchiveStorage
	}
	if info.Size() != file.size {
		return domain.ErrProbeImageArchiveIntegrity
	}
	sum := sha256.New()
	n, err := io.Copy(sum, io.LimitReader(probeImageContextReader{ctx: ctx, r: opened}, file.size+1))
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return domain.ErrProbeImageArchiveStorage
	}
	if n != file.size || hex.EncodeToString(sum.Sum(nil)) != digest {
		return domain.ErrProbeImageArchiveIntegrity
	}
	return nil
}

func bareProbeImageErr(err error) error {
	if err == nil {
		return err
	}
	for _, safe := range []error{
		domain.ErrProbeImageInput,
		domain.ErrProbeImageSize,
		domain.ErrProbeImageDigest,
		domain.ErrProbeImageMetadata,
		domain.ErrProbeImageUnavailable,
		domain.ErrProbeImageContentLimit,
		domain.ErrProbeImageContentEncoding,
		domain.ErrProbeImageContentDiffID,
		domain.ErrProbeImageSnapshotStorage,
		domain.ErrProbeImageSnapshotClosed,
		domain.ErrProbeImageSnapshotBusy,
		domain.ErrProbeImageArchiveStorage,
		domain.ErrProbeImageArchiveClosed,
		domain.ErrProbeImageArchiveBusy,
		domain.ErrProbeImageArchiveIntegrity,
		context.Canceled,
		context.DeadlineExceeded,
	} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return domain.ErrProbeImageUnavailable
}

func translateArchiveSnapshotErr(err error) error {
	switch {
	case err == nil || err == io.EOF:
		return err
	case errors.Is(err, domain.ErrProbeImageSnapshotStorage):
		return domain.ErrProbeImageArchiveStorage
	case errors.Is(err, domain.ErrProbeImageSnapshotClosed):
		return domain.ErrProbeImageArchiveClosed
	case errors.Is(err, domain.ErrProbeImageSnapshotBusy):
		return domain.ErrProbeImageArchiveBusy
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return domain.ErrProbeImageArchiveStorage
	}
}

type preparedProbeImageArchive struct {
	inner      *preparedProbeImage
	descriptor domain.ProbeImageArchiveDescriptor
}

func (a *preparedProbeImageArchive) Descriptor() domain.ProbeImageArchiveDescriptor {
	return a.descriptor
}

// Open registers one context-bound snapshot reader and translates its storage
// errors. preparedProbeImage.Open is intentionally not used: that helper bundles
// at least three streams and cannot represent this single archive file.
func (a *preparedProbeImageArchive) Open(ctx context.Context) (io.ReadCloser, error) {
	if a == nil || a.inner == nil || imageSnapshotNil(ctx) {
		return nil, domain.ErrProbeImageInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	reader, err := a.openRegistered(ctx)
	if err != nil {
		return nil, translateArchiveSnapshotErr(err)
	}
	if err = ctx.Err(); err != nil {
		_ = reader.Close()
		return nil, err
	}
	return &archiveBoundReader{imageSnapshotReader: reader}, nil
}

func (a *preparedProbeImageArchive) openRegistered(ctx context.Context) (*imageSnapshotReader, error) {
	b := a.inner
	b.mu.Lock()
	fail := func(err error) (*imageSnapshotReader, error) {
		b.mu.Unlock()
		return nil, err
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
	if b.root == nil || len(b.files) != 1 {
		return fail(domain.ErrProbeImageSnapshotStorage)
	}
	entry := b.files[0]
	info, err := b.root.Lstat(entry.name)
	if err != nil || entry.identity == nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 || !os.SameFile(info, entry.identity) || info.Size() != entry.size {
		return fail(domain.ErrProbeImageSnapshotStorage)
	}
	opened, err := b.root.Open(entry.name)
	if err != nil {
		return fail(domain.ErrProbeImageSnapshotStorage)
	}
	info, err = opened.Stat()
	if err != nil || !os.SameFile(info, entry.identity) || info.Size() != entry.size || !info.Mode().IsRegular() || info.Mode().Perm() != 0400 {
		b.mu.Unlock()
		_ = opened.Close()
		return nil, domain.ErrProbeImageSnapshotStorage
	}
	reader := &imageSnapshotReader{file: opened, owner: b, ctx: ctx, joined: make(chan struct{})}
	b.readers[reader] = struct{}{}
	reader.stop = context.AfterFunc(ctx, func() {
		reader.closeFile()
		b.mu.Lock()
		close(reader.joined)
		delete(b.readers, reader)
		b.mu.Unlock()
	})
	b.mu.Unlock()
	return reader, nil
}

func (a *preparedProbeImageArchive) Close(ctx context.Context) error {
	if a == nil || a.inner == nil {
		return domain.ErrProbeImageArchiveStorage
	}
	return translateArchiveSnapshotErr(a.inner.Close(ctx))
}

type archiveBoundReader struct{ *imageSnapshotReader }

func (r *archiveBoundReader) Read(p []byte) (int, error) {
	n, err := r.imageSnapshotReader.Read(p)
	if err == io.EOF {
		return n, io.EOF
	}
	return n, translateArchiveSnapshotErr(err)
}

func (r *archiveBoundReader) Close() error {
	return translateArchiveSnapshotErr(r.imageSnapshotReader.Close())
}

var _ ports.ProbeImageArchivePacker = (*ProbeImageArchivePacker)(nil)
var _ ports.PreparedProbeImageArchive = (*preparedProbeImageArchive)(nil)
