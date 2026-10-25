package services

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const probeBinaryStageReceiveMaxEmptyReads = 100

// probeBinaryStageReceiveSource owns one raw input and exposes a child limited
// to exactly size bytes. The child's EOF is synthetic. Only ConfirmEOF may read
// one outer byte, and only Finish/cancellation closes that outer input after a
// clean full child close.
type probeBinaryStageReceiveSource struct {
	ctx   context.Context
	input io.ReadCloser
	size  int64

	closeOnce sync.Once
	closeErr  error

	joined           chan struct{}
	stop             func() bool
	joinCallbackOnce sync.Once

	childOnce     sync.Once
	childCloseErr error

	mu          sync.Mutex
	remaining   int64
	consumed    int64
	emptyReads  int
	reading     bool
	confirming  bool
	readWait    chan struct{}
	childClosed bool
	failed      bool
	shortEOF    bool
	finished    bool
	confirmed   bool
	confirmErr  error

	child *probeBinaryStageReceiveChild
}

type probeBinaryStageReceiveChild struct {
	src *probeBinaryStageReceiveSource
}

// newProbeBinaryStageReceiveSource owns input on every return, including refusal.
// A typed-nil input or context is never dispatched. Size must be in
// (0, domain.MaxProbeBinarySize].
func newProbeBinaryStageReceiveSource(ctx context.Context, input io.ReadCloser, size int64) (*probeBinaryStageReceiveSource, error) {
	if probeBinaryStageNil(input) {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	if probeBinaryStageNil(ctx) || size <= 0 || size > domain.MaxProbeBinarySize {
		if err := probeBinaryStageReceiveCloseInput(input); err != nil {
			return nil, err
		}
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	src := &probeBinaryStageReceiveSource{
		ctx:       ctx,
		input:     input,
		size:      size,
		remaining: size,
		joined:    make(chan struct{}),
	}
	src.child = &probeBinaryStageReceiveChild{src: src}
	src.stop = context.AfterFunc(ctx, func() {
		defer close(src.joined)
		// closeOuter memoizes its outcome; this callback has no caller to return it to.
		_ = src.closeOuter()
	})
	return src, nil
}

// probeBinaryStageReceiveCloseInput closes a refused input once per call.
// Nil and typed-nil inputs are skipped. A close failure is bare artifact storage.
func probeBinaryStageReceiveCloseInput(input io.ReadCloser) error {
	if probeBinaryStageNil(input) {
		return nil
	}
	if err := input.Close(); err != nil {
		return domain.ErrProbeArtifactStorage
	}
	return nil
}

// Child returns the single stable limited reader. It never consumes byte size+1.
func (s *probeBinaryStageReceiveSource) Child() io.ReadCloser {
	if s == nil {
		return nil
	}
	return s.child
}

// Complete reports full consumption, no terminal non-EOF source failure, no
// active read, and a closed child. A short source EOF is incomplete.
func (s *probeBinaryStageReceiveSource) Complete() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.childClosed && !s.reading && !s.confirming && !s.failed && !s.shortEOF &&
		s.remaining == 0 && s.consumed == s.size
}

// ConfirmEOF requires Complete and a live context, then reads at most one outer
// byte until a real io.EOF. (0, nil) is never EOF. One extra byte or a short
// stream is artifact size; no-progress and other read failures are storage.
func (s *probeBinaryStageReceiveSource) ConfirmEOF() error {
	if s == nil {
		return domain.ErrProbeArtifactStorage
	}
	done, err := s.beginConfirm()
	if done {
		return err
	}
	defer s.endOuterWait()

	var extra [1]byte
	empty := 0
	for {
		if cerr := s.ctx.Err(); cerr != nil {
			return s.finishConfirm(probeBinaryStageReceiveBareContext(cerr))
		}
		n, readErr := s.input.Read(extra[:])
		if n < 0 || n > len(extra) {
			return s.finishConfirm(domain.ErrProbeArtifactStorage)
		}
		if n > 0 {
			return s.finishConfirm(domain.ErrProbeArtifactSize)
		}
		if readErr == nil {
			empty++
			if empty >= probeBinaryStageReceiveMaxEmptyReads {
				return s.finishConfirm(domain.ErrProbeArtifactStorage)
			}
			continue
		}
		if probeBinaryStageReceiveTerminal(readErr, io.EOF) {
			return s.finishConfirm(nil)
		}
		if probeBinaryStageReceiveTerminal(readErr, io.ErrUnexpectedEOF) {
			return s.finishConfirm(domain.ErrProbeArtifactSize)
		}
		if cerr := s.ctx.Err(); cerr != nil {
			return s.finishConfirm(probeBinaryStageReceiveBareContext(cerr))
		}
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			return s.finishConfirm(probeBinaryStageReceiveBareContext(readErr))
		}
		return s.finishConfirm(domain.ErrProbeArtifactStorage)
	}
}

// Finish closes the outer input once and joins an already-running cancellation
// callback. stop(false) is not a join. A second call does not wait for a
// callback that was stopped before it started. A close failure is bare storage.
func (s *probeBinaryStageReceiveSource) Finish() error {
	if s == nil {
		return domain.ErrProbeArtifactStorage
	}
	s.mu.Lock()
	s.finished = true
	wait := s.readWait
	s.mu.Unlock()
	_ = s.closeOuter()
	if wait != nil {
		<-wait
	}
	s.joinCallback()
	return s.closeErr
}

func (c *probeBinaryStageReceiveChild) Read(p []byte) (int, error) {
	if c == nil || c.src == nil {
		return 0, domain.ErrProbeArtifactStorage
	}
	s := c.src
	want, done, n, err := s.beginChildRead(p)
	if done {
		return n, err
	}
	defer s.endOuterWait()

	for {
		if cerr := s.ctx.Err(); cerr != nil {
			return 0, probeBinaryStageReceiveBareContext(cerr)
		}
		readN, readErr := s.input.Read(p[:want])
		if readN < 0 || readN > want {
			s.markFailed()
			return 0, domain.ErrProbeArtifactStorage
		}
		if readN == 0 && readErr == nil {
			if s.noteEmpty() {
				return 0, domain.ErrProbeArtifactStorage
			}
			continue
		}
		s.noteConsumed(readN, readErr)
		if readN > 0 && readErr != nil && !probeBinaryStageReceiveTerminal(readErr, io.EOF) && !probeBinaryStageReceiveTerminal(readErr, io.ErrUnexpectedEOF) {
			if cerr := s.ctx.Err(); cerr != nil {
				return readN, probeBinaryStageReceiveBareContext(cerr)
			}
			if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
				return readN, probeBinaryStageReceiveBareContext(readErr)
			}
			return readN, domain.ErrProbeArtifactStorage
		}
		if readErr == nil {
			return readN, nil
		}
		if probeBinaryStageReceiveTerminal(readErr, io.EOF) {
			return readN, io.EOF
		}
		if probeBinaryStageReceiveTerminal(readErr, io.ErrUnexpectedEOF) {
			if readN > 0 {
				return readN, domain.ErrProbeArtifactSize
			}
			return 0, domain.ErrProbeArtifactSize
		}
		if cerr := s.ctx.Err(); cerr != nil {
			return readN, probeBinaryStageReceiveBareContext(cerr)
		}
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			return readN, probeBinaryStageReceiveBareContext(readErr)
		}
		return 0, domain.ErrProbeArtifactStorage
	}
}

func (c *probeBinaryStageReceiveChild) Close() error {
	if c == nil || c.src == nil {
		return domain.ErrProbeArtifactStorage
	}
	c.src.childOnce.Do(c.src.closeChild)
	return c.src.childCloseErr
}

func (s *probeBinaryStageReceiveSource) closeChild() {
	s.mu.Lock()
	s.childClosed = true
	clean := s.consumed == s.size && s.remaining == 0 && !s.failed && !s.shortEOF && !s.reading && !s.confirming
	wait := s.readWait
	s.mu.Unlock()
	if clean {
		return
	}
	s.childCloseErr = s.closeOuter()
	if wait != nil {
		<-wait
	}
}

func (s *probeBinaryStageReceiveSource) beginChildRead(p []byte) (int, bool, int, error) {
	if cerr := s.ctx.Err(); cerr != nil {
		return 0, true, 0, probeBinaryStageReceiveBareContext(cerr)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.childClosed || s.finished {
		return 0, true, 0, domain.ErrProbeArtifactClosed
	}
	if s.reading || s.confirming {
		s.failed = true
		return 0, true, 0, domain.ErrProbeArtifactStorage
	}
	if s.failed {
		return 0, true, 0, domain.ErrProbeArtifactStorage
	}
	if s.shortEOF || s.remaining == 0 {
		return 0, true, 0, io.EOF
	}
	if len(p) == 0 {
		return 0, true, 0, nil
	}
	want := len(p)
	if int64(want) > s.remaining {
		want = int(s.remaining)
	}
	if want <= 0 {
		return 0, true, 0, io.EOF
	}
	s.reading = true
	s.readWait = make(chan struct{})
	return want, false, 0, nil
}

func (s *probeBinaryStageReceiveSource) beginConfirm() (bool, error) {
	if cerr := s.ctx.Err(); cerr != nil {
		return true, probeBinaryStageReceiveBareContext(cerr)
	}
	if !s.Complete() {
		return true, domain.ErrProbeArtifactSize
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.confirmed || s.confirmErr != nil {
		return true, s.confirmErr
	}
	if s.finished || s.confirming || s.reading {
		return true, domain.ErrProbeArtifactStorage
	}
	s.confirming = true
	s.readWait = make(chan struct{})
	return false, nil
}

func (s *probeBinaryStageReceiveSource) finishConfirm(err error) error {
	s.mu.Lock()
	s.confirmed = err == nil
	s.confirmErr = err
	s.mu.Unlock()
	return err
}

func (s *probeBinaryStageReceiveSource) endOuterWait() {
	s.mu.Lock()
	s.reading = false
	s.confirming = false
	if s.readWait != nil {
		close(s.readWait)
		s.readWait = nil
	}
	s.mu.Unlock()
}

func (s *probeBinaryStageReceiveSource) noteEmpty() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emptyReads++
	if s.emptyReads >= probeBinaryStageReceiveMaxEmptyReads {
		s.failed = true
		return true
	}
	return false
}

func (s *probeBinaryStageReceiveSource) noteConsumed(n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > 0 {
		s.consumed += int64(n)
		s.remaining -= int64(n)
		s.emptyReads = 0
	}
	if err == nil {
		return
	}
	if probeBinaryStageReceiveTerminal(err, io.ErrUnexpectedEOF) {
		// Truncation is incomplete even when the returned count matches size.
		s.shortEOF = true
		return
	}
	if probeBinaryStageReceiveTerminal(err, io.EOF) {
		if s.remaining > 0 {
			s.shortEOF = true
		}
		return
	}
	// A port must not launder a source failure by ignoring Read's error.
	// Context errors from the source are terminal even if our caller is live.
	s.failed = true
}

func (s *probeBinaryStageReceiveSource) markFailed() {
	s.mu.Lock()
	s.failed = true
	s.mu.Unlock()
}

func (s *probeBinaryStageReceiveSource) closeOuter() error {
	s.closeOnce.Do(func() {
		if probeBinaryStageNil(s.input) {
			return
		}
		if err := s.input.Close(); err != nil {
			s.closeErr = domain.ErrProbeArtifactStorage
		}
	})
	return s.closeErr
}

func (s *probeBinaryStageReceiveSource) joinCallback() {
	s.joinCallbackOnce.Do(func() {
		if s.stop == nil {
			return
		}
		if !s.stop() {
			<-s.joined
		}
	})
}

// A single wrapping chain may preserve an EOF status, but a multi-cause
// error containing EOF and an independent failure is not successful completion.
func probeBinaryStageReceiveTerminal(err, terminal error) bool {
	for range 32 {
		if err == nil {
			return false
		}
		if err == terminal {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

func probeBinaryStageReceiveBareContext(err error) error {
	switch {
	case errors.Is(err, context.Canceled):
		return context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	default:
		return domain.ErrProbeArtifactStorage
	}
}
