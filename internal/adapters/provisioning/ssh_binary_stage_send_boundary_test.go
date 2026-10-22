package provisioning

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestSSHBinaryStageSenderFixedCommandLiteral(t *testing.T) {
	if SSHProbeBinaryStageCommand != "/usr/local/bin/phoenix-probe-admin stage-binary-host" {
		t.Fatal("fixed command differs from the independently stated contract")
	}
}

func TestSSHBinaryStageSenderTypedNilOpenReader(t *testing.T) {
	pin := sendTPin([]byte("x"))
	f := serveStageSend(t, sendTConfig{})
	var reader *sendTReader
	binary := sendTBinary(pin, func(context.Context) (io.ReadCloser, error) { return reader, nil })
	defer func() {
		if recovered := recover(); recovered != nil {
			t.Errorf("typed-nil Open reader panicked: %T", recovered)
		}
	}()
	ack, err := f.sender(t).Send(t.Context(), f.intent(pin), binary)
	if ack != (SSHBinaryStageAck{}) || err != domain.ErrProbeArtifactStorage {
		t.Fatalf("typed-nil reader returned ack=%+v error=%v", ack, err)
	}
	if binary.opens.Load() != 1 || binary.snapshotCloses.Load() != 0 || f.connCalls.Load() != 0 {
		t.Fatalf("unexpected ownership or I/O: opens=%d snapshot closes=%d connections=%d", binary.opens.Load(), binary.snapshotCloses.Load(), f.connCalls.Load())
	}
}

func TestSSHBinaryStageSenderPreExecCleanupFailure(t *testing.T) {
	data := []byte("source close must remain observable")
	reader := sendTCloseFailReader(data)
	binary := sendTBinary(sendTPin(data), func(context.Context) (io.ReadCloser, error) { return reader, nil })
	f := serveStageSend(t, sendTConfig{})
	// The unchanged endpoint policy denies loopback when no CIDR grants it.
	sender, err := NewSSHBinaryStageSender(t.Context(), f.clientKey, f.hostFP(), f.clientFP(), probe.EndpointPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	ack, err := sender.Send(t.Context(), f.intent(binary.Pin()), binary)
	if ack != (SSHBinaryStageAck{}) || err != domain.ErrProbeArtifactStorage {
		t.Fatalf("pre-exec reader Close failure was lost: ack=%+v error=%v", ack, err)
	}
	if reader.closes.Load() != 1 || f.execCalls.Load() != 0 || f.connCalls.Load() != 0 || binary.snapshotCloses.Load() != 0 {
		t.Fatalf("unexpected ownership or exec: closes=%d exec=%d connections=%d snapshot closes=%d", reader.closes.Load(), f.execCalls.Load(), f.connCalls.Load(), binary.snapshotCloses.Load())
	}
}

func TestSSHBinaryStageSenderPayloadEOFIsCompletion(t *testing.T) {
	data := []byte("one read can contain both the final bytes and EOF")
	var reads atomic.Int32
	reader := &sendTReader{readFn: func(p []byte) (int, error) {
		if reads.Add(1) != 1 {
			return 0, io.ErrClosedPipe
		}
		return copy(p, data), io.EOF
	}}
	binary := sendTBinary(sendTPin(data), func(context.Context) (io.ReadCloser, error) { return reader, nil })
	f := serveStageSend(t, sendTConfig{stdout: sendTReceiptFor(binary.Pin())})
	intent := f.intent(binary.Pin())
	ack, err := f.sender(t).Send(t.Context(), intent, binary)
	if ack != (SSHBinaryStageAck{Intent: intent}) || err != nil {
		t.Fatalf("full payload plus real EOF was not completion: ack=%+v error=%v reads=%d", ack, err, reads.Load())
	}
	if reads.Load() != 1 || reader.closes.Load() != 1 || binary.snapshotCloses.Load() != 0 {
		t.Fatalf("unexpected reader lifetime: reads=%d closes=%d snapshot closes=%d", reads.Load(), reader.closes.Load(), binary.snapshotCloses.Load())
	}
}

type sendBoundaryStdin struct {
	io.WriteCloser
	closes      atomic.Int32
	beforeClose func()
}

func (w *sendBoundaryStdin) Close() error {
	w.closes.Add(1)
	if w.beforeClose != nil {
		w.beforeClose()
	}
	return w.WriteCloser.Close()
}

// These two focused tests observe the sender's own completion call directly.
// A server-side channel EOF cannot distinguish channel EOF from disconnect.
func TestSSHBinaryStageSenderKnownCancellationBeforeCompletion(t *testing.T) {
	data := []byte("cancel while closing the source")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	reader := sendTScripted(data, nil, io.EOF)
	reader.closeFn = func() error { cancel(); return nil }
	binary := sendTBinary(sendTPin(data), func(context.Context) (io.ReadCloser, error) { return reader, nil })
	f := serveStageSend(t, sendTConfig{stdout: sendTReceiptFor(binary.Pin())})
	call, stdin := sendBoundaryCall(t, ctx, f, binary)
	defer call.join()
	if err := call.execute(); err == nil {
		t.Error("known cancellation did not stop completion")
	}
	if stdin.closes.Load() != 0 || call.eofSent {
		t.Fatalf("normal completion attempted after known cancellation: closes=%d eofSent=%t", stdin.closes.Load(), call.eofSent)
	}
}

func TestSSHBinaryStageSenderKnownCancellationBeforeExec(t *testing.T) {
	data := []byte("do not start after cancellation is known")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	binary := sendTBytesBinary(data)
	f := serveStageSend(t, sendTConfig{})
	call, _ := sendBoundaryCall(t, ctx, f, binary)
	defer call.join()
	cancel()
	if err := call.execute(); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-exec cancellation was not preserved: %v", err)
	}
	if call.attempted || f.execCalls.Load() != 0 {
		t.Fatalf("crossed exec boundary after known cancellation: attempted=%t exec=%d", call.attempted, f.execCalls.Load())
	}
}

func sendBoundaryCall(t *testing.T, ctx context.Context, f *sendTFixture, binary *sendTFakeBinary) (*stageSendCall, *sendBoundaryStdin) {
	t.Helper()
	intent := f.intent(binary.Pin())
	header, err := EncodeProbeBinaryStageHeader(intent)
	if err != nil {
		t.Fatal(err)
	}
	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	t.Cleanup(cancel)
	call := &stageSendCall{ctx: ctx, tctx: tctx, intent: intent, pin: binary.Pin(), header: header, src: &stageSendSource{}, cancelJoined: make(chan struct{})}
	if err := call.openSource(binary); err != nil {
		call.join()
		t.Fatal(err)
	}
	if err := call.connect(f.sender(t).insp); err != nil {
		call.join()
		t.Fatal(err)
	}
	stdin := &sendBoundaryStdin{WriteCloser: call.stdin}
	call.stdin = stdin
	return call, stdin
}

type sendBoundaryWriter struct {
	calls int
	fn    func([]byte) (int, error)
}

func (w *sendBoundaryWriter) Write(p []byte) (int, error) {
	w.calls++
	return w.fn(p)
}

func TestSSHBinaryStageSenderShortWriteFailsClosed(t *testing.T) {
	writer := &sendBoundaryWriter{fn: func(p []byte) (int, error) { return len(p) - 1, nil }}
	if err := stageSendWriteAll(writer, []byte("frame")); err == nil {
		t.Fatal("short write was accepted")
	}
	if writer.calls != 1 {
		t.Fatalf("short nil-error write was retried %d times", writer.calls)
	}
}

func TestSSHBinaryStageSenderSourceCloseBeforeCompletion(t *testing.T) {
	data := []byte("source Close completes before wire EOF")
	var closed, early atomic.Bool
	reader := sendTScripted(data, nil, io.EOF)
	reader.closeFn = func() error { closed.Store(true); return nil }
	binary := sendTBinary(sendTPin(data), func(context.Context) (io.ReadCloser, error) { return reader, nil })
	f := serveStageSend(t, sendTConfig{stdout: sendTReceiptFor(binary.Pin())})
	call, stdin := sendBoundaryCall(t, t.Context(), f, binary)
	defer func() { _ = call.join() }()
	stdin.beforeClose = func() { early.Store(!closed.Load()) }
	if err := call.execute(); err != nil {
		t.Fatal(err)
	}
	if early.Load() || !closed.Load() || reader.closes.Load() != 1 {
		t.Fatalf("wire completion preceded source Close: early=%t closed=%t closes=%d", early.Load(), closed.Load(), reader.closes.Load())
	}
}

func TestSSHBinaryStageSenderOutputBudgetAborts(t *testing.T) {
	var aborted atomic.Int32
	writer := &stageSendBounded{max: 4, abort: func() { aborted.Add(1) }}
	if n, err := writer.Write([]byte("12345")); n != 0 || err == nil {
		t.Fatalf("over-budget write accepted: %d %v", n, err)
	}
	if n, err := writer.Write([]byte("1")); n != 0 || err == nil {
		t.Fatalf("failed stream resumed: %d %v", n, err)
	}
	if !writer.exceeded() || len(writer.snapshot()) != 0 || aborted.Load() != 1 {
		t.Fatal("output budget did not abort once without retaining excess bytes")
	}
}

type sendBoundaryCancelConn struct {
	net.Conn
	cancel context.CancelFunc
	once   sync.Once
}

func (c *sendBoundaryCancelConn) Close() error {
	c.once.Do(c.cancel)
	return c.Conn.Close()
}

func TestSSHBinaryStageSenderCancellationDuringLocalJoin(t *testing.T) {
	binary := sendTBytesBinary([]byte("receipt and normal exit precede local cleanup"))
	f := serveStageSend(t, sendTConfig{stdout: sendTReceiptFor(binary.Pin()), holdAfterSession: true})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sender := f.sender(t)
	dial := sender.insp.host.dial
	// Explicit boundary model: keep the real SSH connection open after exit
	// and revoke context when the sender's local cleanup closes it.
	sender.insp.host.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &sendBoundaryCancelConn{Conn: conn, cancel: cancel}, nil
	}
	ack, err := sender.Send(ctx, f.intent(binary.Pin()), binary)
	if ack != (SSHBinaryStageAck{}) || err != ErrSSHBinaryStageUnknown || ctx.Err() != context.Canceled {
		t.Fatalf("local-join revocation acknowledged: ack=%+v error=%v", ack, err)
	}
}

func TestSSHBinaryStageSenderCallbackTailJoin(t *testing.T) {
	entered, tail, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	var release sync.Once
	releaseTail := func() { release.Do(func() { close(tail) }) }
	call := &stageSendCall{src: &stageSendSource{}, cancelJoined: tail, stop: func() bool { close(entered); return false }}
	go func() { done <- call.join() }()
	t.Cleanup(func() {
		releaseTail()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("synthetic callback-tail model did not join")
		}
	})
	sendTWaitSignal(t, entered, "stop(false) callback-tail boundary")
	select {
	case <-done:
		t.Fatal("local join returned before the running callback tail ended")
	case <-time.After(50 * time.Millisecond):
	}
	releaseTail()
	// Cleanup consumes and joins the completed result even on assertion failure.
}

func TestSSHBinaryStageSenderInvalidReadCounts(t *testing.T) {
	for _, count := range []int{-1, 2} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			reader := &sendTReader{readFn: func([]byte) (int, error) { return count, nil }}
			binary := sendTBinary(sendTPin([]byte("x")), func(context.Context) (io.ReadCloser, error) { return reader, nil })
			f := serveStageSend(t, sendTConfig{})
			ack, err := f.sender(t).Send(t.Context(), f.intent(binary.Pin()), binary)
			if ack != (SSHBinaryStageAck{}) || err != ErrSSHBinaryStageUnknown || reader.closes.Load() != 1 {
				t.Fatalf("invalid count accepted or reader leaked: ack=%+v error=%v closes=%d", ack, err, reader.closes.Load())
			}
		})
	}
}
