package services

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const bootstrapSecret = "hunter2-very-secret-password"

type fakeHostBootstrapper struct {
	mu       sync.Mutex
	calls    int
	seenPass []byte
	seenKey  []byte
	result   domain.ProbeHostBootstrapResult
	err      error
	block    chan struct{}
	entered  chan struct{}
	deadline time.Duration
}

func (f *fakeHostBootstrapper) Bootstrap(ctx context.Context, req domain.ProbeHostBootstrapRequest) (domain.ProbeHostBootstrapResult, error) {
	f.mu.Lock()
	f.calls++
	f.seenPass = bytes.Clone(req.Password)
	f.seenKey = bytes.Clone(req.PrivateKey)
	if d, ok := ctx.Deadline(); ok {
		f.deadline = time.Until(d)
	}
	block, entered := f.block, f.entered
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return domain.ProbeHostBootstrapResult{}, ctx.Err()
		}
	}
	return f.result, f.err
}

func okBootstrapResult() domain.ProbeHostBootstrapResult {
	return domain.ProbeHostBootstrapResult{Helper: "installed", StageAuthority: "installed", AuthorizedKey: "added", Verified: true}
}

func bootstrapRequest() domain.ProbeHostBootstrapRequest {
	return domain.ProbeHostBootstrapRequest{
		Host:               domain.ProbeSSHHost{Address: "203.0.113.10:22", User: "ops"},
		Password:           []byte(bootstrapSecret),
		HostKeyFingerprint: "SHA256:" + strings.Repeat("A", 43),
		PrivateKey:         []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nfake\n-----END OPENSSH PRIVATE KEY-----\n"),
		Helper:             domain.ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: strings.Repeat("a", 64), Size: 10},
		RequestedBy:        7,
	}
}

func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

func TestProbeHostBootstrapServiceSuccessClearsSecretsAndLogsNothingSecret(t *testing.T) {
	logs := captureSlog(t)
	fake := &fakeHostBootstrapper{result: okBootstrapResult()}
	svc, err := NewProbeHostBootstrapService(fake, 2)
	if err != nil {
		t.Fatal(err)
	}
	req := bootstrapRequest()
	result, err := svc.Bootstrap(t.Context(), req)
	if err != nil || result != okBootstrapResult() {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if string(fake.seenPass) != bootstrapSecret {
		t.Fatal("bootstrapper did not receive the password")
	}
	if !bytes.Equal(req.Password, make([]byte, len(req.Password))) || !bytes.Equal(req.PrivateKey, make([]byte, len(req.PrivateKey))) {
		t.Fatal("password or key buffer was not cleared")
	}
	if fake.deadline <= 0 || fake.deadline > probeHostBootstrapTTL {
		t.Fatalf("bootstrap not bounded: %v", fake.deadline)
	}
	out := logs.String()
	for _, secret := range []string{bootstrapSecret, "PRIVATE KEY", "203.0.113.10", "SHA256:"} {
		if strings.Contains(out, secret) {
			t.Fatalf("log leaks %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, `"outcome":"verified"`) || !strings.Contains(out, `"requested_by":7`) {
		t.Fatalf("audit line missing: %s", out)
	}
}

func TestProbeHostBootstrapServiceErrorsAreFixedAndRedacted(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want func(error) bool
	}{
		{"refusal", &domain.ProbeHostBootstrapError{Code: domain.ProbeHostBootstrapKeyClash}, func(e error) bool {
			var r *domain.ProbeHostBootstrapError
			return errors.As(e, &r) && r.Code == domain.ProbeHostBootstrapKeyClash
		}},
		{"unknown code", &domain.ProbeHostBootstrapError{Code: "password=" + bootstrapSecret}, func(e error) bool {
			var r *domain.ProbeHostBootstrapError
			return errors.As(e, &r) && r.Code == domain.ProbeHostBootstrapFailed
		}},
		{"raw adapter error", errors.New("ssh: handshake failed for " + bootstrapSecret), func(e error) bool {
			var r *domain.ProbeHostBootstrapError
			return errors.As(e, &r) && r.Code == domain.ProbeHostBootstrapFailed
		}},
		{"artifact", domain.ErrProbeInstallArtifact, func(e error) bool { return errors.Is(e, domain.ErrProbeInstallArtifact) }},
		{"key", domain.ErrProbeSSHInput, func(e error) bool { return errors.Is(e, domain.ErrProbeSSHInput) }},
		{"deadline", context.DeadlineExceeded, func(e error) bool { return errors.Is(e, context.DeadlineExceeded) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureSlog(t)
			fake := &fakeHostBootstrapper{err: tc.err}
			svc, _ := NewProbeHostBootstrapService(fake, 1)
			req := bootstrapRequest()
			result, err := svc.Bootstrap(t.Context(), req)
			if !tc.want(err) || result != (domain.ProbeHostBootstrapResult{}) {
				t.Fatalf("err=%v result=%+v", err, result)
			}
			if strings.Contains(err.Error(), bootstrapSecret) || strings.Contains(logs.String(), bootstrapSecret) {
				t.Fatal("password leaked into an error or log")
			}
			if !bytes.Equal(req.Password, make([]byte, len(req.Password))) {
				t.Fatal("password not cleared after failure")
			}
		})
	}
}

func TestProbeHostBootstrapServiceRejectsUnverifiedSuccess(t *testing.T) {
	for name, result := range map[string]domain.ProbeHostBootstrapResult{
		"unverified":    {Helper: "installed", StageAuthority: "installed", AuthorizedKey: "added"},
		"unknown state": {Helper: "maybe", StageAuthority: "installed", AuthorizedKey: "added", Verified: true},
		"zero":          {},
	} {
		t.Run(name, func(t *testing.T) {
			svc, _ := NewProbeHostBootstrapService(&fakeHostBootstrapper{result: result}, 1)
			got, err := svc.Bootstrap(t.Context(), bootstrapRequest())
			var r *domain.ProbeHostBootstrapError
			if !errors.As(err, &r) || r.Code != domain.ProbeHostBootstrapFailed || got != (domain.ProbeHostBootstrapResult{}) {
				t.Fatalf("got=%+v err=%v", got, err)
			}
		})
	}
}

func TestProbeHostBootstrapServiceValidation(t *testing.T) {
	fake := &fakeHostBootstrapper{result: okBootstrapResult()}
	svc, _ := NewProbeHostBootstrapService(fake, 1)
	for name, mutate := range map[string]func(*domain.ProbeHostBootstrapRequest){
		"no actor":         func(r *domain.ProbeHostBootstrapRequest) { r.RequestedBy = 0 },
		"no password":      func(r *domain.ProbeHostBootstrapRequest) { r.Password = nil },
		"newline password": func(r *domain.ProbeHostBootstrapRequest) { r.Password = []byte("a\nb") },
		"nul password":     func(r *domain.ProbeHostBootstrapRequest) { r.Password = []byte("a\x00b") },
		"long password":    func(r *domain.ProbeHostBootstrapRequest) { r.Password = bytes.Repeat([]byte("a"), 1025) },
		"no key":           func(r *domain.ProbeHostBootstrapRequest) { r.PrivateKey = nil },
		"bad fingerprint":  func(r *domain.ProbeHostBootstrapRequest) { r.HostKeyFingerprint = "MD5:aa" },
		"bad host":         func(r *domain.ProbeHostBootstrapRequest) { r.Host.Address = "no-port" },
		"bad helper pin":   func(r *domain.ProbeHostBootstrapRequest) { r.Helper.SHA256 = "nothex" },
	} {
		t.Run(name, func(t *testing.T) {
			req := bootstrapRequest()
			mutate(&req)
			if _, err := svc.Bootstrap(t.Context(), req); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("err=%v", err)
			}
			if len(req.Password) > 0 && !bytes.Equal(req.Password, make([]byte, len(req.Password))) {
				t.Fatal("password not cleared after a validation refusal")
			}
		})
	}
	if fake.calls != 0 {
		t.Fatal("invalid request reached the bootstrapper")
	}
	if _, err := NewProbeHostBootstrapService(nil, 1); err == nil {
		t.Fatal("nil bootstrapper accepted")
	}
	if _, err := NewProbeHostBootstrapService(fake, 5); err == nil {
		t.Fatal("capacity above 4 accepted")
	}
}

func TestProbeHostBootstrapServiceBusyAndClose(t *testing.T) {
	fake := &fakeHostBootstrapper{result: okBootstrapResult(), block: make(chan struct{}), entered: make(chan struct{}, 1)}
	svc, _ := NewProbeHostBootstrapService(fake, 1)
	done := make(chan error, 1)
	go func() {
		_, err := svc.Bootstrap(context.Background(), bootstrapRequest())
		done <- err
	}()
	<-fake.entered
	if _, err := svc.Bootstrap(t.Context(), bootstrapRequest()); !errors.Is(err, domain.ErrProbeHostBootstrapBusy) {
		t.Fatalf("second bootstrap err=%v, want busy", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- svc.Close(context.Background()) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := svc.Bootstrap(t.Context(), bootstrapRequest()); errors.Is(err, domain.ErrProbeHostBootstrapClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("service never refused after Close")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while a bootstrap was in flight", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(fake.block)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
}
