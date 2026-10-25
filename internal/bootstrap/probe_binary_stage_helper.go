package bootstrap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const probeBinaryStageHelperTimeout = 2 * time.Minute
const probeBinaryStageHelperMaxEmptyReads = 100
const probeBinaryStageHelperMaxResponse = 2048

// probeBinaryStageUsage is the only text emitted for a non-empty argument list.
// It is exit 2, stdout stays empty, and the rejected arguments are never echoed.
const probeBinaryStageUsage = "Usage: phoenix-probe-admin stage-binary\nNo flags, paths, or arguments. Stdin is one frame under inherited directory descriptor 3.\nThis is NOT installation, enrollment, cleanup, or host authority.\n"

// probeBinaryStageHelperAllowlist is the only stderr text this command may emit
// for a non-usage failure. Unknown is first so a joined cause cannot be reported
// as a definite refusal.
var probeBinaryStageHelperAllowlist = []error{
	domain.ErrProbeBinaryStageWriteUnknown,
	domain.ErrProbeBinaryStageWriteInput,
	domain.ErrProbeBinaryStageWriteConflict,
	domain.ErrProbeBinaryStageWriteUnavailable,
	domain.ErrProbeBinaryStageWriteUnauthorized,
	domain.ErrProbeArtifactInput,
	domain.ErrProbeArtifactSize,
	domain.ErrProbeArtifactDigest,
	domain.ErrProbeArtifactMetadata,
	domain.ErrProbeArtifactStorage,
	domain.ErrProbeArtifactClosed,
	context.Canceled,
	context.DeadlineExceeded,
}

// RunProbeBinaryStageHelper stages one stdin frame under a borrowed parent.
// It owns a non-typed-nil input on every return and never closes parent.
// Exit 0 is the only acknowledgement of the single JSON line. That line is a
// fresh local stage receipt, not installation, enrollment, cleanup, or authority.
func RunProbeBinaryStageHelper(ctx context.Context, args []string, parent *os.File, input io.ReadCloser, out, stderr io.Writer) int {
	return runProbeBinaryStageHelperEntry(ctx, args, parent, input, out, stderr, nil)
}

// runProbeBinaryStageHelperEntry is RunProbeBinaryStageHelper with an optional
// pre-payload authorization hook. Ownership and exit semantics are unchanged.
func runProbeBinaryStageHelperEntry(ctx context.Context, args []string, parent *os.File, input io.ReadCloser, out, stderr io.Writer, authorize probeBinaryStageHelperAuthorize) int {
	if len(args) != 0 {
		return probeBinaryStageHelperUsage(input, stderr)
	}
	if probeBinaryStageHelperNil(ctx) || probeBinaryStageHelperNil(input) || parent == nil || probeBinaryStageHelperNil(out) || probeBinaryStageHelperNil(stderr) {
		return probeBinaryStageHelperRefuse(input, stderr, domain.ErrProbeBinaryStageWriteInput)
	}
	if err := ctx.Err(); err != nil {
		return probeBinaryStageHelperRefuse(input, stderr, probeBinaryStageHelperContextError(err))
	}
	opCtx, cancel := context.WithTimeout(ctx, probeBinaryStageHelperTimeout)
	defer cancel()
	guard, err := newProbeBinaryStageHelperInput(opCtx, input)
	if err != nil {
		return probeBinaryStageHelperRefuse(input, stderr, err)
	}
	defer func() { _ = guard.Close() }()
	return runProbeBinaryStageHelper(ctx, opCtx, parent, guard, out, stderr, authorize)
}

// RunInheritedProbeBinaryStageHelper duplicates inherited descriptor 3 through
// the main-owned platform helper, then borrows that duplicate. Non-empty args
// are refused before the duplication. The duplicate is closed before any
// successful JSON is flushed. Descriptor 3 and a caller's parent are not closed.
// Run owns input once called; this wrapper does not close it again.
func RunInheritedProbeBinaryStageHelper(ctx context.Context, args []string, input io.ReadCloser, out, stderr io.Writer) int {
	if len(args) != 0 {
		return probeBinaryStageHelperUsage(input, stderr)
	}
	if probeBinaryStageHelperNil(ctx) || probeBinaryStageHelperNil(input) || probeBinaryStageHelperNil(out) || probeBinaryStageHelperNil(stderr) {
		return probeBinaryStageHelperRefuse(input, stderr, domain.ErrProbeBinaryStageWriteInput)
	}
	if err := ctx.Err(); err != nil {
		return probeBinaryStageHelperRefuse(input, stderr, probeBinaryStageHelperContextError(err))
	}
	// inheritedProbeBinaryStageParent is supplied by main's platform files.
	// It must duplicate descriptor 3 and close that duplicate on its own failure.
	parent, err := inheritedProbeBinaryStageParent(ctx)
	if err != nil || parent == nil {
		if parent != nil {
			if cerr := parent.Close(); cerr != nil {
				return probeBinaryStageHelperRefuse(input, stderr, domain.ErrProbeArtifactStorage)
			}
		}
		if cerr := ctx.Err(); cerr != nil {
			return probeBinaryStageHelperRefuse(input, stderr, probeBinaryStageHelperContextError(cerr))
		}
		return probeBinaryStageHelperRefuse(input, stderr, domain.ErrProbeBinaryStageWriteUnavailable)
	}
	ownedInput, err := inheritedProbeBinaryStageInput(ctx, input)
	if err != nil {
		if parent.Close() != nil {
			err = domain.ErrProbeArtifactStorage
		}
		return probeBinaryStageHelperExit(stderr, err)
	}
	var buf, diagnostic bytes.Buffer
	closed := false
	defer func() {
		if !closed {
			_ = parent.Close()
		}
	}()
	code := RunProbeBinaryStageHelper(ctx, nil, parent, ownedInput, &buf, &diagnostic)
	closeErr := parent.Close()
	closed = true
	return finishInheritedProbeBinaryStageHelper(ctx, code, closeErr, buf.Bytes(), diagnostic.String(), out, stderr)
}

// A failed inner call never flushes its buffered receipt. Close acknowledgement
// precedes output, and any diagnostic is reconstructed from fixed sentinels.
func finishInheritedProbeBinaryStageHelper(ctx context.Context, code int, closeErr error, payload []byte, diagnostic string, out, stderr io.Writer) int {
	if closeErr != nil {
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeBinaryStageWriteUnknown)
	}
	if code != 0 {
		for _, safe := range probeBinaryStageHelperAllowlist {
			if diagnostic == safe.Error()+"\n" {
				return probeBinaryStageHelperExit(stderr, safe)
			}
		}
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeBinaryStageWriteUnknown)
	}
	if ctx.Err() != nil {
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeBinaryStageWriteUnknown)
	}
	if err := writeProbeBinaryStageHelperReceipt(out, payload); err != nil || ctx.Err() != nil {
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeBinaryStageWriteUnknown)
	}
	return 0
}

// probeBinaryStageHelperAuthorize runs after the header decodes and before any
// payload byte is consumed or any stage is created. It may read only its own
// fixed frame from the stream. A nil hook is the unchanged unauthorized helper.
type probeBinaryStageHelperAuthorize func(ctx context.Context, intent domain.ProbeBinaryStageIntent, r io.Reader) error

func runProbeBinaryStageHelper(caller, opCtx context.Context, parent *os.File, guard *probeBinaryStageHelperInput, out, stderr io.Writer, authorize probeBinaryStageHelperAuthorize) int {
	writer, err := provisioning.NewLocalBinaryStageWriter(opCtx, parent)
	if err != nil {
		return probeBinaryStageHelperBeforeReceive(guard, stderr, err)
	}
	header, err := readProbeBinaryStageHeader(opCtx, guard)
	if err != nil {
		return probeBinaryStageHelperBeforeReceive(guard, stderr, err)
	}
	intent, err := provisioning.DecodeProbeBinaryStageHeader(header)
	if err != nil {
		return probeBinaryStageHelperBeforeReceive(guard, stderr, err)
	}
	if !intent.Valid() {
		return probeBinaryStageHelperBeforeReceive(guard, stderr, domain.ErrProbeBinaryStageWriteInput)
	}
	if authorize != nil {
		if err := authorize(opCtx, intent, guard); err != nil {
			return probeBinaryStageHelperBeforeReceive(guard, stderr, err)
		}
	}
	service, err := services.NewProbeBinaryStageReceiveService(provisioning.NewProbeBinaryPreparer(""), writer)
	if err != nil {
		return probeBinaryStageHelperBeforeReceive(guard, stderr, err)
	}
	seal, recvErr := service.Receive(opCtx, intent, guard)
	return probeBinaryStageHelperAfterReceive(caller, opCtx, intent, guard, out, stderr, seal, recvErr)
}

func probeBinaryStageHelperBeforeReceive(guard *probeBinaryStageHelperInput, stderr io.Writer, reason error) int {
	closeErr := guard.Close()
	if closeErr != nil {
		return probeBinaryStageHelperExit(stderr, closeErr)
	}
	return probeBinaryStageHelperExit(stderr, probeBinaryStageHelperPreError(reason))
}

func probeBinaryStageHelperAfterReceive(caller, opCtx context.Context, intent domain.ProbeBinaryStageIntent, guard *probeBinaryStageHelperInput, out, stderr io.Writer, seal domain.ProbeBinaryStageSeal, recvErr error) int {
	closeErr := guard.Close()
	callerDead := caller.Err() != nil
	opDead := opCtx.Err() != nil
	exact := recvErr == nil && closeErr == nil && !callerDead && !opDead && seal.Valid() && seal.Intent == intent
	if !exact {
		reason := domain.ErrProbeBinaryStageWriteUnknown
		// Receive guarantees ArtifactStorage is pre-stage. A repeated Close
		// returning that same sticky input error is not a new post-stage fault.
		if !callerDead && seal == (domain.ProbeBinaryStageSeal{}) && recvErr != nil &&
			(closeErr == nil || recvErr == domain.ErrProbeArtifactStorage) {
			reason = probeBinaryStageHelperPublicError(recvErr)
		}
		return probeBinaryStageHelperExit(stderr, reason)
	}
	payload, err := probeBinaryStageHelperReceipt(seal)
	if err != nil || caller.Err() != nil || opCtx.Err() != nil {
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeBinaryStageWriteUnknown)
	}
	if err = writeProbeBinaryStageHelperReceipt(out, payload); err != nil || caller.Err() != nil || opCtx.Err() != nil {
		return probeBinaryStageHelperExit(stderr, domain.ErrProbeBinaryStageWriteUnknown)
	}
	return 0
}

type probeBinaryStageHelperReceiptView struct {
	State         string `json:"state"`
	DirectoryName string `json:"directory_name"`
	MarkerSHA256  string `json:"marker_sha256"`
	PinnedVersion string `json:"pinned_version"`
	VersionSource string `json:"version_source"`
	OS            string `json:"os"`
	Architecture  string `json:"architecture"`
	SHA256        string `json:"sha256"`
	Size          int64  `json:"size"`
}

func probeBinaryStageHelperReceipt(seal domain.ProbeBinaryStageSeal) ([]byte, error) {
	if !seal.Valid() {
		return nil, domain.ErrProbeBinaryStageWriteUnknown
	}
	name := seal.DirectoryName()
	pin := seal.Intent.Identity.Binary
	marker := seal.Intent.MarkerSHA256()
	if !probeBinaryStageHelperRelativeName(name) || marker == "" || !pin.Valid() {
		return nil, domain.ErrProbeBinaryStageWriteUnknown
	}
	body, err := json.Marshal(probeBinaryStageHelperReceiptView{
		State:         "local_binary_staged",
		DirectoryName: name,
		MarkerSHA256:  marker,
		PinnedVersion: pin.Version,
		VersionSource: "operator_pin",
		OS:            "linux",
		Architecture:  pin.Architecture,
		SHA256:        pin.SHA256,
		Size:          pin.Size,
	})
	if err != nil || len(body) == 0 || len(body)+1 > probeBinaryStageHelperMaxResponse {
		return nil, domain.ErrProbeBinaryStageWriteUnknown
	}
	// body is freshly marshalled and owned here; appending needs no size arithmetic.
	return append(body, '\n'), nil
}

func probeBinaryStageHelperRelativeName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

// writeProbeBinaryStageHelperReceipt writes one already-bounded JSON line.
// A short count is a failure even when Write returns a nil error. Oversized
// or multi-line payloads are not written.
func writeProbeBinaryStageHelperReceipt(w io.Writer, payload []byte) error {
	if probeBinaryStageHelperNil(w) || len(payload) == 0 || len(payload) > probeBinaryStageHelperMaxResponse || payload[len(payload)-1] != '\n' || bytes.Count(payload, []byte{'\n'}) != 1 {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	n, err := w.Write(payload)
	if err != nil || n != len(payload) {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	return nil
}

type probeBinaryStageHeaderRead struct {
	ctx   context.Context
	r     io.Reader
	empty int
}

func readProbeBinaryStageHeader(ctx context.Context, r io.Reader) ([]byte, error) {
	if probeBinaryStageHelperNil(ctx) || probeBinaryStageHelperNil(r) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	st := probeBinaryStageHeaderRead{ctx: ctx, r: r}
	prefix := make([]byte, provisioning.ProbeBinaryStagePrefixSize)
	if err := st.exact(prefix); err != nil {
		return nil, err
	}
	// HeaderLength returns the JSON byte count N from the 12-byte prefix,
	// not the total header size. Decode still receives exactly prefix+N.
	n, err := provisioning.ProbeBinaryStageHeaderLength(prefix)
	if err != nil {
		return nil, probeBinaryStageHelperPreError(err)
	}
	if n <= 0 || n > provisioning.MaxProbeBinaryStageHeaderSize {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	header := make([]byte, provisioning.ProbeBinaryStagePrefixSize+n)
	copy(header, prefix)
	if err = st.exact(header[provisioning.ProbeBinaryStagePrefixSize:]); err != nil {
		return nil, err
	}
	return header, nil
}

func (s *probeBinaryStageHeaderRead) exact(dst []byte) error {
	off := 0
	for off < len(dst) {
		if err := s.ctx.Err(); err != nil {
			return probeBinaryStageHelperContextError(err)
		}
		n, err := s.r.Read(dst[off:])
		if n < 0 || n > len(dst)-off {
			return domain.ErrProbeArtifactStorage
		}
		if n == 0 && err == nil {
			s.empty++
			if s.empty >= probeBinaryStageHelperMaxEmptyReads {
				return domain.ErrProbeArtifactStorage
			}
			continue
		}
		if n > 0 {
			s.empty = 0
			off += n
		}
		if err == nil {
			continue
		}
		if cerr := s.ctx.Err(); cerr != nil {
			return probeBinaryStageHelperContextError(cerr)
		}
		if off == len(dst) && probeBinaryStageHelperSingle(err, io.EOF) {
			return nil
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return probeBinaryStageHelperContextError(err)
		}
		if off < len(dst) && (probeBinaryStageHelperSingle(err, io.EOF) || probeBinaryStageHelperSingle(err, io.ErrUnexpectedEOF)) {
			return domain.ErrProbeBinaryStageWriteInput
		}
		return probeBinaryStageHelperPreError(err)
	}
	return nil
}

// probeBinaryStageHelperInput owns one raw stream. Read delegates the caller's
// buffer without copying, limiting, or prefetching, so a later real EOF remains
// visible. Close is idempotent and joins the cancellation callback.
type probeBinaryStageHelperInput struct {
	ctx      context.Context
	raw      io.ReadCloser
	mu       sync.Mutex
	once     sync.Once
	join     sync.Once
	stop     func() bool
	joined   chan struct{}
	wait     chan struct{}
	reading  bool
	closed   bool
	closeErr error
}

func newProbeBinaryStageHelperInput(ctx context.Context, input io.ReadCloser) (*probeBinaryStageHelperInput, error) {
	if probeBinaryStageHelperNil(ctx) || probeBinaryStageHelperNil(input) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	g := &probeBinaryStageHelperInput{ctx: ctx, raw: input, joined: make(chan struct{})}
	g.stop = context.AfterFunc(ctx, func() {
		defer close(g.joined)
		g.closeRaw()
	})
	if g.stop == nil {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	return g, nil
}

func (g *probeBinaryStageHelperInput) Read(p []byte) (int, error) {
	if g == nil || probeBinaryStageHelperNil(g.raw) {
		return 0, domain.ErrProbeArtifactStorage
	}
	if err := g.ctx.Err(); err != nil {
		return 0, probeBinaryStageHelperContextError(err)
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return 0, domain.ErrProbeArtifactClosed
	}
	if g.reading {
		g.mu.Unlock()
		return 0, domain.ErrProbeArtifactStorage
	}
	if len(p) == 0 {
		g.mu.Unlock()
		return 0, nil
	}
	g.reading = true
	g.wait = make(chan struct{})
	g.mu.Unlock()
	defer g.endRead()
	n, err := g.raw.Read(p)
	if n < 0 || n > len(p) {
		return 0, domain.ErrProbeArtifactStorage
	}
	return n, err
}

func (g *probeBinaryStageHelperInput) endRead() {
	g.mu.Lock()
	g.reading = false
	if g.wait != nil {
		close(g.wait)
		g.wait = nil
	}
	g.mu.Unlock()
}

func (g *probeBinaryStageHelperInput) Close() error {
	if g == nil {
		return domain.ErrProbeArtifactStorage
	}
	g.mu.Lock()
	g.closed = true
	wait := g.wait
	g.mu.Unlock()
	g.closeRaw()
	if wait != nil {
		<-wait
	}
	g.joinCallback()
	return g.closeErr
}

func (g *probeBinaryStageHelperInput) closeRaw() {
	g.once.Do(func() {
		if probeBinaryStageHelperNil(g.raw) {
			g.closeErr = domain.ErrProbeArtifactStorage
			return
		}
		if err := g.raw.Close(); err != nil {
			g.closeErr = domain.ErrProbeArtifactStorage
		}
	})
}

func (g *probeBinaryStageHelperInput) joinCallback() {
	g.join.Do(func() {
		if g.stop == nil {
			return
		}
		if !g.stop() {
			<-g.joined
		}
	})
}

func probeBinaryStageHelperUsage(input io.ReadCloser, stderr io.Writer) int {
	if err := probeBinaryStageHelperCloseInput(input); err != nil {
		return probeBinaryStageHelperExit(stderr, err)
	}
	if !probeBinaryStageHelperNil(stderr) {
		_, _ = io.WriteString(stderr, probeBinaryStageUsage)
	}
	return 2
}

func probeBinaryStageHelperRefuse(input io.ReadCloser, stderr io.Writer, reason error) int {
	if err := probeBinaryStageHelperCloseInput(input); err != nil {
		reason = err
	}
	return probeBinaryStageHelperExit(stderr, reason)
}

func probeBinaryStageHelperCloseInput(input io.ReadCloser) error {
	if probeBinaryStageHelperNil(input) {
		return nil
	}
	if err := input.Close(); err != nil {
		return domain.ErrProbeArtifactStorage
	}
	return nil
}

func probeBinaryStageHelperExit(stderr io.Writer, err error) int {
	if !probeBinaryStageHelperNil(stderr) {
		_, _ = fmt.Fprintln(stderr, probeBinaryStageHelperPublicError(err).Error())
	}
	return 1
}

func probeBinaryStageHelperPreError(err error) error {
	if err == nil {
		return domain.ErrProbeBinaryStageWriteInput
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return probeBinaryStageHelperContextError(err)
	}
	for _, safe := range probeBinaryStageHelperAllowlist {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return domain.ErrProbeArtifactStorage
}

func probeBinaryStageHelperPublicError(err error) error {
	for _, safe := range probeBinaryStageHelperAllowlist {
		if errors.Is(err, safe) {
			return safe
		}
	}
	return domain.ErrProbeBinaryStageWriteUnknown
}

func probeBinaryStageHelperContextError(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return domain.ErrProbeBinaryStageWriteInput
	}
}

func probeBinaryStageHelperSingle(err, target error) bool {
	for range 32 {
		if err == nil {
			return false
		}
		if err == target {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

func probeBinaryStageHelperNil(v any) bool {
	if v == nil {
		return true
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return rv.IsNil()
	default:
		return false
	}
}
