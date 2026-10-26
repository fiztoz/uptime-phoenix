package services

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const receiveSourcePrivate = "receive-source-private-/tmp/secret"

type receivePanicCloser struct{}

func (*receivePanicCloser) Read([]byte) (int, error) { panic("typed-nil read") }
func (*receivePanicCloser) Close() error             { panic("typed-nil close") }

type receivePanicContext struct{ context.Context }

func (*receivePanicContext) Deadline() (time.Time, bool) { panic("typed-nil deadline") }
func (*receivePanicContext) Done() <-chan struct{}       { panic("typed-nil done") }
func (*receivePanicContext) Err() error                  { panic("typed-nil err") }
func (*receivePanicContext) Value(any) any               { panic("typed-nil value") }

type receiveCountCloser struct {
	mu     sync.Mutex
	reads  int
	closes int
	data   []byte
	off    int
	err    error
	readFn func(call int, p []byte) (int, error)
	block  chan struct{}
}

func (c *receiveCountCloser) Read(p []byte) (int, error) {
	c.mu.Lock()
	c.reads++
	call := c.reads
	fn := c.readFn
	c.mu.Unlock()
	if c.block != nil && call == 1 {
		<-c.block
	}
	if fn != nil {
		return fn(call, p)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.off >= len(c.data) {
		if c.err != nil {
			return 0, c.err
		}
		return 0, io.EOF
	}
	n := copy(p, c.data[c.off:])
	c.off += n
	return n, nil
}

func (c *receiveCountCloser) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closes++
	return c.err
}

func (c *receiveCountCloser) snapshot() (reads, closes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reads, c.closes
}

func mustReceiveSource(t *testing.T, ctx context.Context, input io.ReadCloser, size int64) *probeBinaryStageReceiveSource {
	t.Helper()
	src, err := newProbeBinaryStageReceiveSource(ctx, input, size)
	if err != nil || src == nil {
		t.Fatalf("constructor: src=%v err=%v", src, err)
	}
	return src
}

func readAllChild(t *testing.T, child io.Reader, size int) []byte {
	t.Helper()
	buf := make([]byte, size+8)
	var got []byte
	for len(got) < size {
		n, err := child.Read(buf)
		if n > 0 {
			got = append(got, buf[:n]...)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("child read: %v", err)
		}
		if n == 0 {
			t.Fatal("child made no progress")
		}
	}
	return got
}

func TestProbeBinaryStageReceiveSourceRefusesBeforeContextMethods(t *testing.T) {
	var typedInput io.ReadCloser = (*receivePanicCloser)(nil)
	var typedCtx context.Context = (*receivePanicContext)(nil)
	if src, err := newProbeBinaryStageReceiveSource(context.Background(), typedInput, 1); src != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatalf("typed-nil input: src=%v err=%v", src, err)
	}
	if src, err := newProbeBinaryStageReceiveSource(typedCtx, typedInput, 0); src != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatalf("typed-nil pair: src=%v err=%v", src, err)
	}

	input := &receiveCountCloser{data: []byte{1}}
	var bareNilCtx context.Context
	if src, err := newProbeBinaryStageReceiveSource(bareNilCtx, input, 1); src != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatalf("nil context: src=%v err=%v", src, err)
	}
	if _, closes := input.snapshot(); closes != 1 {
		t.Fatalf("nil context closes=%d", closes)
	}
	if src, err := newProbeBinaryStageReceiveSource(typedCtx, input, 1); src != nil || err != domain.ErrProbeBinaryStageWriteInput {
		t.Fatalf("typed-nil context: src=%v err=%v", src, err)
	}
	if _, closes := input.snapshot(); closes != 2 {
		t.Fatalf("typed-nil context closes=%d", closes)
	}
}

func TestProbeBinaryStageReceiveSourceRefusalOwnsInput(t *testing.T) {
	cases := []int64{0, -1, domain.MaxProbeBinarySize + 1}
	for _, size := range cases {
		input := &receiveCountCloser{}
		src, err := newProbeBinaryStageReceiveSource(context.Background(), input, size)
		if src != nil || err != domain.ErrProbeBinaryStageWriteInput {
			t.Fatalf("size %d: src=%v err=%v", size, src, err)
		}
		if _, closes := input.snapshot(); closes != 1 {
			t.Fatalf("size %d closes=%d", size, closes)
		}
	}
	input := &receiveCountCloser{err: errors.New(receiveSourcePrivate)}
	src, err := newProbeBinaryStageReceiveSource(context.Background(), input, 0)
	if src != nil || err != domain.ErrProbeArtifactStorage || strings.Contains(err.Error(), receiveSourcePrivate) {
		t.Fatalf("close failure: src=%v err=%v", src, err)
	}
}

func TestProbeBinaryStageReceiveCloseInputOncePerCall(t *testing.T) {
	if err := probeBinaryStageReceiveCloseInput(nil); err != nil {
		t.Fatalf("nil: %v", err)
	}
	var typed io.ReadCloser = (*receivePanicCloser)(nil)
	if err := probeBinaryStageReceiveCloseInput(typed); err != nil {
		t.Fatalf("typed nil: %v", err)
	}
	input := &receiveCountCloser{}
	if err := probeBinaryStageReceiveCloseInput(input); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := probeBinaryStageReceiveCloseInput(input); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if _, closes := input.snapshot(); closes != 2 {
		t.Fatalf("closes=%d", closes)
	}
	secret := &receiveCountCloser{err: fmt.Errorf("wrap: %w", errors.New(receiveSourcePrivate))}
	err := probeBinaryStageReceiveCloseInput(secret)
	if err != domain.ErrProbeArtifactStorage || strings.Contains(err.Error(), receiveSourcePrivate) {
		t.Fatalf("sanitized=%v", err)
	}
}

func TestProbeBinaryStageReceiveChildExactThenOuterEOF(t *testing.T) {
	payload := []byte("abc")
	input := &receiveCountCloser{data: append(append([]byte{}, payload...), 'Z')}
	src := mustReceiveSource(t, context.Background(), input, int64(len(payload)))
	child := src.Child()
	if child == nil || src.Child() != child {
		t.Fatal("child is not stable")
	}
	got := readAllChild(t, child, len(payload))
	if string(got) != string(payload) {
		t.Fatalf("payload=%q", got)
	}
	n, err := child.Read(make([]byte, 4))
	if n != 0 || err != io.EOF {
		t.Fatalf("synthetic eof n=%d err=%v", n, err)
	}
	if reads, _ := input.snapshot(); reads != 1 {
		t.Fatalf("child consumed outer reads=%d", reads)
	}
	if src.Complete() {
		t.Fatal("complete before child close")
	}
	if err := src.ConfirmEOF(); err != domain.ErrProbeArtifactSize {
		t.Fatalf("confirm before close: %v", err)
	}
	if reads, closes := input.snapshot(); reads != 1 || closes != 0 {
		t.Fatalf("early confirm reads=%d closes=%d", reads, closes)
	}
	if err := child.Close(); err != nil {
		t.Fatalf("clean close: %v", err)
	}
	if _, closes := input.snapshot(); closes != 0 {
		t.Fatalf("clean close closed outer")
	}
	if !src.Complete() {
		t.Fatal("expected complete")
	}
	if err := src.ConfirmEOF(); err != domain.ErrProbeArtifactSize {
		t.Fatalf("extra byte: %v", err)
	}
	if err := src.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if _, closes := input.snapshot(); closes != 1 {
		t.Fatalf("finish closes=%d", closes)
	}
}

func TestProbeBinaryStageReceiveStickyPayloadEOFStillConfirmed(t *testing.T) {
	input := &receiveCountCloser{readFn: func(call int, p []byte) (int, error) {
		if call == 1 {
			if len(p) == 0 {
				return 0, nil
			}
			p[0] = 'x'
			return 1, io.EOF
		}
		if call > 2 {
			return 0, errors.New("confirm reread")
		}
		return 0, io.EOF
	}}
	src := mustReceiveSource(t, context.Background(), input, 1)
	n, err := src.Child().Read(make([]byte, 8))
	if n != 1 || err != io.EOF {
		t.Fatalf("payload n=%d err=%v", n, err)
	}
	if err := src.Child().Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := src.ConfirmEOF(); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if err := src.ConfirmEOF(); err != nil {
		t.Fatalf("second confirm: %v", err)
	}
	if reads, _ := input.snapshot(); reads != 2 {
		t.Fatalf("reads=%d", reads)
	}
	if err := src.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestProbeBinaryStageReceiveShortAndFailedCloseOuter(t *testing.T) {
	short := &receiveCountCloser{readFn: func(int, []byte) (int, error) {
		return 1, fmt.Errorf("%s: %w", receiveSourcePrivate, io.EOF)
	}}
	src := mustReceiveSource(t, context.Background(), short, 4)
	n, err := src.Child().Read(make([]byte, 4))
	if n != 1 || err != io.EOF {
		t.Fatalf("short n=%d err=%v", n, err)
	}
	if src.Complete() {
		t.Fatal("short complete")
	}
	if err := src.Child().Close(); err != nil {
		t.Fatalf("short close: %v", err)
	}
	if _, closes := short.snapshot(); closes != 1 {
		t.Fatalf("short closes=%d", closes)
	}
	if err := src.ConfirmEOF(); err != domain.ErrProbeArtifactSize {
		t.Fatalf("short confirm: %v", err)
	}
	if err := src.Child().Close(); err != nil {
		t.Fatalf("second short close: %v", err)
	}
	if _, closes := short.snapshot(); closes != 1 {
		t.Fatalf("second short closes=%d", closes)
	}

	failed := &receiveCountCloser{readFn: func(_ int, p []byte) (int, error) {
		if len(p) == 0 {
			return 0, nil
		}
		p[0] = 9
		return 1, errors.New(receiveSourcePrivate)
	}}
	src = mustReceiveSource(t, context.Background(), failed, 1)
	n, err = src.Child().Read(make([]byte, 1))
	if n != 1 || err != domain.ErrProbeArtifactStorage || strings.Contains(err.Error(), receiveSourcePrivate) {
		t.Fatalf("failed read n=%d err=%v", n, err)
	}
	n, err = src.Child().Read(make([]byte, 1))
	if n != 0 || err != domain.ErrProbeArtifactStorage {
		t.Fatalf("retry n=%d err=%v", n, err)
	}
	if reads, _ := failed.snapshot(); reads != 1 {
		t.Fatalf("retry dispatched reads=%d", reads)
	}
	if err := src.Child().Close(); err != nil {
		t.Fatalf("failed close: %v", err)
	}
	if _, closes := failed.snapshot(); closes != 1 {
		t.Fatalf("failed closes=%d", closes)
	}
}

func TestProbeBinaryStageReceiveInvalidCountsAndEmptyReads(t *testing.T) {
	invalid := &receiveCountCloser{readFn: func(_ int, p []byte) (int, error) {
		return len(p) + 3, nil
	}}
	src := mustReceiveSource(t, context.Background(), invalid, 4)
	n, err := src.Child().Read(make([]byte, 2))
	if n != 0 || err != domain.ErrProbeArtifactStorage {
		t.Fatalf("oversize n=%d err=%v", n, err)
	}
	negative := &receiveCountCloser{readFn: func(int, []byte) (int, error) { return -1, nil }}
	src = mustReceiveSource(t, context.Background(), negative, 1)
	n, err = src.Child().Read(make([]byte, 1))
	if n != 0 || err != domain.ErrProbeArtifactStorage {
		t.Fatalf("negative n=%d err=%v", n, err)
	}

	var calls int
	stalled := &receiveCountCloser{readFn: func(int, []byte) (int, error) {
		calls++
		return 0, nil
	}}
	src = mustReceiveSource(t, context.Background(), stalled, 2)
	n, err = src.Child().Read(make([]byte, 2))
	if n != 0 || err != domain.ErrProbeArtifactStorage || calls != probeBinaryStageReceiveMaxEmptyReads {
		t.Fatalf("empty n=%d err=%v calls=%d", n, err, calls)
	}

	calls = 0
	progress := &receiveCountCloser{readFn: func(_ int, p []byte) (int, error) {
		calls++
		if calls <= probeBinaryStageReceiveMaxEmptyReads-1 {
			return 0, nil
		}
		if len(p) == 0 {
			return 0, nil
		}
		p[0] = 1
		return 1, nil
	}}
	src = mustReceiveSource(t, context.Background(), progress, 1)
	n, err = src.Child().Read(make([]byte, 1))
	if n != 1 || err != nil {
		t.Fatalf("progress n=%d err=%v", n, err)
	}
}

func TestProbeBinaryStageReceiveUnexpectedEOFIsSize(t *testing.T) {
	input := &receiveCountCloser{readFn: func(int, []byte) (int, error) {
		return 0, fmt.Errorf("%s: %w", receiveSourcePrivate, io.ErrUnexpectedEOF)
	}}
	src := mustReceiveSource(t, context.Background(), input, 2)
	n, err := src.Child().Read(make([]byte, 2))
	if n != 0 || err != domain.ErrProbeArtifactSize || strings.Contains(err.Error(), receiveSourcePrivate) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if err := src.Child().Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, closes := input.snapshot(); closes != 1 {
		t.Fatal("unexpected eof did not close outer")
	}
}

func TestProbeBinaryStageReceiveConfirmClassifiesOuter(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		var calls int
		input := &receiveCountCloser{readFn: func(int, []byte) (int, error) {
			calls++
			return 0, nil
		}}
		src := mustReceiveSource(t, context.Background(), input, 1)
		if n, err := src.Child().Read(make([]byte, 1)); n != 0 || err != domain.ErrProbeArtifactStorage || calls != probeBinaryStageReceiveMaxEmptyReads {
			t.Fatalf("child empty n=%d err=%v calls=%d", n, err, calls)
		}
		if err := src.Finish(); err != nil {
			t.Fatal(err)
		}
		input = &receiveCountCloser{data: []byte{1}}
		src = mustReceiveSource(t, context.Background(), input, 1)
		if n, err := src.Child().Read(make([]byte, 1)); n != 1 || err != nil {
			t.Fatalf("payload n=%d err=%v", n, err)
		}
		if err := src.Child().Close(); err != nil {
			t.Fatal(err)
		}
		var confirmCalls int
		input.readFn = func(int, []byte) (int, error) {
			confirmCalls++
			return 0, nil
		}
		err := src.ConfirmEOF()
		if err != domain.ErrProbeArtifactStorage || confirmCalls != probeBinaryStageReceiveMaxEmptyReads {
			t.Fatalf("empty confirm err=%v calls=%d", err, confirmCalls)
		}
		if err := src.Finish(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("failure", func(t *testing.T) {
		input := &receiveCountCloser{data: []byte{7}}
		src := mustReceiveSource(t, context.Background(), input, 1)
		if n, err := src.Child().Read(make([]byte, 1)); n != 1 || err != nil {
			t.Fatalf("direct n=%d err=%v", n, err)
		}
		if err := src.Child().Close(); err != nil {
			t.Fatal(err)
		}
		input.readFn = func(int, []byte) (int, error) {
			return 0, errors.New(receiveSourcePrivate)
		}
		err := src.ConfirmEOF()
		if err != domain.ErrProbeArtifactStorage || strings.Contains(err.Error(), receiveSourcePrivate) {
			t.Fatalf("confirm err=%v", err)
		}
		if err := src.Finish(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("truncated", func(t *testing.T) {
		input := &receiveCountCloser{data: []byte{7}}
		src := mustReceiveSource(t, context.Background(), input, 1)
		if n, err := src.Child().Read(make([]byte, 1)); n != 1 || err != nil {
			t.Fatalf("n=%d err=%v", n, err)
		}
		if err := src.Child().Close(); err != nil {
			t.Fatal(err)
		}
		input.readFn = func(int, []byte) (int, error) { return 0, io.ErrUnexpectedEOF }
		if err := src.ConfirmEOF(); err != domain.ErrProbeArtifactSize {
			t.Fatalf("truncation: %v", err)
		}
		if err := src.Finish(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestProbeBinaryStageReceiveConcurrentChildReadFailsClosed(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var calls int
	var mu sync.Mutex
	input := &receiveCountCloser{readFn: func(_ int, p []byte) (int, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		once.Do(func() { close(entered) })
		<-release
		p[0] = 1
		return 1, nil
	}}
	src := mustReceiveSource(t, context.Background(), input, 1)
	go func() { _, _ = src.Child().Read(make([]byte, 1)) }()
	<-entered
	n, err := src.Child().Read(make([]byte, 1))
	if n != 0 || err != domain.ErrProbeArtifactStorage {
		t.Fatalf("concurrent n=%d err=%v", n, err)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("dispatched reads=%d", got)
	}
	close(release)
	if err := src.Child().Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := src.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestProbeBinaryStageReceiveActiveCloseJoinsRead(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseRead)
	entered := make(chan struct{})
	closed := make(chan struct{})
	var closeOnce sync.Once
	blocking := &receiveBlockingCloser{release: release, entered: entered, closed: closed, closeOnce: &closeOnce}
	src := mustReceiveSource(t, context.Background(), blocking, 1)
	readDone := make(chan error, 1)
	go func() {
		_, err := src.Child().Read(make([]byte, 1))
		readDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("child read did not start")
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- src.Child().Close() }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("active close did not close outer")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("close returned before join: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	releaseRead()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not join read")
	}
	select {
	case _ = <-readDone:
	case <-time.After(2 * time.Second):
		t.Fatal("read did not finish")
	}
	n, err := src.Child().Read(make([]byte, 1))
	if n != 0 || err != domain.ErrProbeArtifactClosed {
		t.Fatalf("after close n=%d err=%v", n, err)
	}
	if err := src.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

type receiveBlockingCloser struct {
	release   chan struct{}
	entered   chan struct{}
	closed    chan struct{}
	closeOnce *sync.Once
	closes    int
	mu        sync.Mutex
}

func (c *receiveBlockingCloser) Read([]byte) (int, error) {
	close(c.entered)
	<-c.release
	return 0, io.EOF
}

func (c *receiveBlockingCloser) Close() error {
	c.mu.Lock()
	c.closes++
	c.mu.Unlock()
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func TestProbeBinaryStageReceiveFinishJoinsRunningCallback(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	input := &receiveCallbackCloser{release: release, started: started}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	src := mustReceiveSource(t, ctx, input, 1)
	cancel()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("callback did not start")
	}
	finishDone := make(chan error, 1)
	go func() { finishDone <- src.Finish() }()
	select {
	case err := <-finishDone:
		t.Fatalf("Finish returned before running callback: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case err := <-finishDone:
		if err != nil {
			t.Fatalf("finish: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Finish did not join running callback")
	}
	select {
	case <-src.joined:
	default:
		t.Fatal("callback channel not joined")
	}
	if err := src.Finish(); err != nil {
		t.Fatalf("second finish: %v", err)
	}
}

func TestProbeBinaryStageReceiveFinishDoesNotWaitForStoppedCallback(t *testing.T) {
	input := &receiveCountCloser{data: []byte{1}}
	src := mustReceiveSource(t, context.Background(), input, 1)
	done := make(chan error, 1)
	go func() { done <- src.Finish() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("finish: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Finish waited for a never-started callback")
	}
	done = make(chan error, 1)
	go func() { done <- src.Finish() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("second: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second Finish waited for a stopped never-started callback")
	}
	if _, closes := input.snapshot(); closes != 1 {
		t.Fatalf("closes=%d", closes)
	}
}

type receiveCallbackCloser struct {
	release chan struct{}
	started chan struct{}
	once    sync.Once
	err     error
}

func (c *receiveCallbackCloser) Read([]byte) (int, error) { return 0, io.EOF }

func (c *receiveCallbackCloser) Close() error {
	c.once.Do(func() { close(c.started) })
	<-c.release
	if c.err != nil {
		return c.err
	}
	return nil
}

func TestProbeBinaryStageReceiveFinishCloseErrorIsBareStorage(t *testing.T) {
	input := &receiveCallbackCloser{release: make(chan struct{}), started: make(chan struct{}), err: errors.New(receiveSourcePrivate)}
	close(input.release)
	src := mustReceiveSource(t, context.Background(), input, 1)
	err := src.Finish()
	if err != domain.ErrProbeArtifactStorage || strings.Contains(err.Error(), receiveSourcePrivate) {
		t.Fatalf("finish err=%v", err)
	}
	if again := src.Finish(); again != domain.ErrProbeArtifactStorage {
		t.Fatalf("second=%v", again)
	}
}

func TestProbeBinaryStageReceiveContextErrorIsBare(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	input := &receiveCountCloser{readFn: func(int, []byte) (int, error) {
		return 0, fmt.Errorf("%s: %w", receiveSourcePrivate, context.Canceled)
	}}
	src := mustReceiveSource(t, ctx, input, 1)
	cancel()
	n, err := src.Child().Read(make([]byte, 1))
	if n != 0 || err != context.Canceled || strings.Contains(err.Error(), receiveSourcePrivate) {
		t.Fatalf("canceled n=%d err=%v", n, err)
	}
	deadline, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	input = &receiveCountCloser{data: []byte{1}}
	src = mustReceiveSource(t, deadline, input, 1)
	if err := src.ConfirmEOF(); err != context.DeadlineExceeded {
		t.Fatalf("confirm deadline: %v", err)
	}
	if reads, _ := input.snapshot(); reads != 0 {
		t.Fatalf("confirm read on dead context reads=%d", reads)
	}
	if err := src.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestProbeBinaryStageReceiveMaxSizeConstructsWithoutBuffer(t *testing.T) {
	input := &receiveCountCloser{}
	src := mustReceiveSource(t, context.Background(), input, domain.MaxProbeBinarySize)
	if src.Child() == nil || src.Complete() {
		t.Fatal("max-size child state")
	}
	if err := src.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestProbeBinaryStageReceiveReadsAfterCloseDoNotTouchInput(t *testing.T) {
	input := &receiveCountCloser{data: []byte{1, 2}}
	src := mustReceiveSource(t, context.Background(), input, 2)
	if err := src.Child().Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	n, err := src.Child().Read(make([]byte, 2))
	if n != 0 || err != domain.ErrProbeArtifactClosed {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if reads, closes := input.snapshot(); reads != 0 || closes != 1 {
		t.Fatalf("reads=%d closes=%d", reads, closes)
	}
	if err := src.Finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
}
