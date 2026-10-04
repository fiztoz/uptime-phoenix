package notifier

import (
	"context"
	"errors"
	"io"
	"net"
	"net/smtp"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestSMTPSenderCancellationBeforeGreeting(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			accepted <- conn
		}
	}()
	config := map[string]any{"host": "127.0.0.1", "port": listener.Addr().(*net.TCPAddr).Port, "tls": false, "from": "from@example.test", "to": "to@example.test"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- (SMTPSender{}).Send(ctx, config, domain.AlertContext{}) }()
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("SMTP connection never started")
	}
	defer func() { _ = conn.Close() }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("SMTP lost cancellation cause", err)
		}
	case <-time.After(300 * time.Millisecond):
		_ = conn.Close()
		<-done
		t.Fatal("SMTP blocked waiting for greeting after cancellation")
	}
}

func TestSMTPSenderCanceledContextDoesNotConnect(t *testing.T) {
	server := newFakeSMTPServer(t)
	host, port := server.hostPort(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := (SMTPSender{}).Send(ctx, map[string]any{"host": host, "port": port, "tls": false, "from": "from@example.test", "to": "to@example.test"}, domain.AlertContext{})
	if !errors.Is(err, context.Canceled) || len(server.received()) != 0 {
		t.Fatal("canceled SMTP operation reached provider", err)
	}
}

func TestSMTPSenderCancellationDuringDataReceipt(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	received, peerDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(peerDone)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		peer := textproto.NewConn(conn)
		_ = peer.PrintfLine("220 test SMTP")
		for {
			line, err := peer.ReadLine()
			if err != nil {
				return
			}
			if line == "DATA" {
				_ = peer.PrintfLine("354 send message")
				if _, err := io.Copy(io.Discard, peer.DotReader()); err != nil {
					return
				}
				close(received)
				_, _ = peer.ReadLine() // Deliberately withhold the acceptance receipt.
				return
			}
			_ = peer.PrintfLine("250 test")
		}
	}()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- (SMTPSender{}).Send(ctx, map[string]any{"host": "127.0.0.1", "port": listener.Addr().(*net.TCPAddr).Port, "tls": false, "from": "from@example.test", "to": "to@example.test"}, domain.AlertContext{})
	}()
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("SMTP DATA never arrived")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("missing uncertain delivery cancellation", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("SMTP DATA receipt wait ignored cancellation")
	}
	<-peerDone
}

func TestSMTPSenderRequiresSTARTTLSWithoutPlaintextFallback(t *testing.T) {
	server := newFakeSMTPServer(t)
	host, port := server.hostPort(t)
	err := (SMTPSender{}).Send(t.Context(), map[string]any{"host": host, "port": port, "from": "from@example.test", "to": "to@example.test"}, domain.AlertContext{})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") || len(server.received()) != 0 {
		t.Fatal("mandatory TLS silently downgraded", err)
	}
}

func TestSMTPLoginAuthenticationCompatibility(t *testing.T) {
	auth := smtpLoginAuth{host: "mail.example", username: "user", password: "test-password"}
	if mechanism, _, err := auth.Start(&smtp.ServerInfo{Name: "mail.example", Auth: []string{"LOGIN"}}); err != nil || mechanism != "LOGIN" {
		t.Fatal("advertised LOGIN rejected", err)
	}
	if _, _, err := auth.Start(&smtp.ServerInfo{Name: "foreign.example", TLS: true}); err == nil {
		t.Fatal("wrong server identity accepted")
	}
	if _, _, err := auth.Start(&smtp.ServerInfo{Name: "mail.example"}); err == nil {
		t.Fatal("unadvertised plaintext authentication accepted")
	}
	for challenge, want := range map[string]string{"Username:": "user", "Password:": "test-password"} {
		got, err := auth.Next([]byte(challenge), true)
		if err != nil || string(got) != want {
			t.Fatal("LOGIN response mismatch", challenge)
		}
	}
	if _, err := auth.Next([]byte("untrusted server content"), true); err == nil || strings.Contains(err.Error(), "untrusted") {
		t.Fatal("unknown challenge accepted or exposed")
	}
}
