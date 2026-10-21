package services

import (
	"context"
	"errors"
	"io"
	"testing"
)

func TestProbeBinaryStageReceiveTerminalSourceErrorsStayFailed(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
	}{
		{"mixed_eof", errors.Join(io.EOF, errors.New("private-read-failure"))},
		{"source_canceled", context.Canceled},
		{"source_deadline", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := &receiveCountCloser{readFn: func(_ int, p []byte) (int, error) { p[0] = 1; return 1, tc.cause }}
			src := mustReceiveSource(t, t.Context(), input, 1)
			defer func() { _ = src.Finish() }()
			n, _ := src.Child().Read(make([]byte, 1))
			if n != 1 {
				t.Fatal("payload differential did not consume byte")
			}
			if err := src.Child().Close(); err != nil {
				t.Fatal(err)
			}
			if src.Complete() {
				t.Fatal("terminal source error accepted as complete payload")
			}
		})
	}
}

func TestProbeBinaryStageReceiveOverlapRevokesCompletion(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	input := &receiveCountCloser{readFn: func(_ int, p []byte) (int, error) { close(entered); <-release; p[0] = 1; return 1, nil }}
	src := mustReceiveSource(t, t.Context(), input, 1)
	defer func() { _ = src.Finish() }()
	done := make(chan struct{})
	go func() { _, _ = src.Child().Read(make([]byte, 1)); close(done) }()
	<-entered
	_, err := src.Child().Read(make([]byte, 1))
	if err == nil {
		close(release)
		<-done
		t.Fatal("overlap accepted")
	}
	close(release)
	<-done
	_ = src.Child().Close()
	if src.Complete() {
		t.Fatal("overlap refusal was forgotten before staging")
	}
}
