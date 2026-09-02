package notifier

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	netmail "net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// fakeSMTPMessage captures one accepted SMTP transaction (MAIL FROM / RCPT TO / DATA).
type fakeSMTPMessage struct {
	From string
	To   []string
	Data string
}

// fakeSMTPServer is a minimal, real-protocol SMTP server used to assert the
// message SMTPSender actually composes and sends, without touching a real
// mail service. It speaks plain (non-TLS, non-AUTH) SMTP, which is all
// gopkg.in/mail.v2 negotiates when StartTLSPolicy is NoStartTLS and no
// username is configured.
type fakeSMTPServer struct {
	ln net.Listener

	rejectMailFrom bool // simulate a provider-side rejection (e.g. bad sender)

	mu       sync.Mutex
	messages []fakeSMTPMessage
}

func newFakeSMTPServer(t *testing.T) *fakeSMTPServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSMTPServer{ln: ln}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeSMTPServer) hostPort(t *testing.T) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(s.ln.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	return host, port
}

func (s *fakeSMTPServer) received() []fakeSMTPMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]fakeSMTPMessage, len(s.messages))
	copy(out, s.messages)
	return out
}

func (s *fakeSMTPServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func extractAngleAddr(line string) string {
	start := strings.Index(line, "<")
	end := strings.Index(line, ">")
	if start == -1 || end == -1 || end < start {
		return strings.TrimSpace(line)
	}
	return line[start+1 : end]
}

func (s *fakeSMTPServer) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	tp := textproto.NewConn(conn)
	if err := tp.PrintfLine("220 fake.smtp ESMTP"); err != nil {
		return
	}

	var from string
	var to []string
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		upper := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(upper, "EHLO") || strings.HasPrefix(upper, "HELO"):
			_ = tp.PrintfLine("250 fake.smtp Hello")
		case strings.HasPrefix(upper, "MAIL FROM:"):
			if s.rejectMailFrom {
				_ = tp.PrintfLine("550 mailbox unavailable")
				continue
			}
			from = extractAngleAddr(line)
			_ = tp.PrintfLine("250 OK")
		case strings.HasPrefix(upper, "RCPT TO:"):
			to = append(to, extractAngleAddr(line))
			_ = tp.PrintfLine("250 OK")
		case upper == "DATA":
			if err := tp.PrintfLine("354 Start mail input; end with <CRLF>.<CRLF>"); err != nil {
				return
			}
			data, err := io.ReadAll(tp.DotReader())
			if err != nil {
				return
			}
			s.mu.Lock()
			s.messages = append(s.messages, fakeSMTPMessage{
				From: from,
				To:   append([]string{}, to...),
				Data: string(data),
			})
			s.mu.Unlock()
			_ = tp.PrintfLine("250 OK: queued")
		case upper == "QUIT":
			_ = tp.PrintfLine("221 Bye")
			return
		default:
			_ = tp.PrintfLine("500 unrecognized command")
		}
	}
}

// decodeQPBody splits a raw RFC 5322 message into header/body and decodes
// the body from quoted-printable, which is gopkg.in/mail.v2's default
// text/plain transfer encoding.
func decodeQPBody(t *testing.T, raw string) string {
	t.Helper()
	sep := "\r\n\r\n"
	idx := strings.Index(raw, sep)
	if idx == -1 {
		sep = "\n\n"
		idx = strings.Index(raw, sep)
	}
	if idx == -1 {
		t.Fatalf("no header/body separator found in message: %q", raw)
	}
	body := raw[idx+len(sep):]
	decoded, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if err != nil {
		t.Fatalf("decode quoted-printable body: %v", err)
	}
	return string(decoded)
}

func decodeMIMEParts(t *testing.T, raw string) map[string]string {
	t.Helper()
	message, err := netmail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("parse MIME message: %v", err)
	}
	mediaType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse MIME content type: %v", err)
	}
	if mediaType != "multipart/alternative" {
		t.Fatalf("message content type = %q; want multipart/alternative", mediaType)
	}

	parts := make(map[string]string)
	reader := multipart.NewReader(message.Body, params["boundary"])
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read MIME part: %v", err)
		}
		partType, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("parse MIME part content type: %v", err)
		}
		var bodyReader io.Reader = part
		if strings.EqualFold(part.Header.Get("Content-Transfer-Encoding"), "quoted-printable") {
			bodyReader = quotedprintable.NewReader(part)
		}
		body, err := io.ReadAll(bodyReader)
		if err != nil {
			t.Fatalf("read MIME %s body: %v", partType, err)
		}
		parts[partType] = string(body)
	}
	return parts
}

func TestSMTPSender_Validate(t *testing.T) {
	s := SMTPSender{}
	valid := map[string]any{"host": "smtp.example.com", "from": "a@example.com", "to": "b@example.com", "port": float64(587)}
	if err := s.Validate(valid); err != nil {
		t.Errorf("valid config failed: %v", err)
	}
	for _, missing := range []string{"host", "from", "to"} {
		cfg := map[string]any{}
		for k, v := range valid {
			if k != missing {
				cfg[k] = v
			}
		}
		if err := s.Validate(cfg); err == nil {
			t.Errorf("missing %s should fail validation", missing)
		}
	}
	cfg := map[string]any{"host": "smtp.example.com", "from": "a@example.com", "to": "b@example.com"}
	if err := s.Validate(cfg); err == nil {
		t.Error("missing port should fail validation")
	}
}

func TestSMTPSender_Send_ComposesMessage(t *testing.T) {
	srv := newFakeSMTPServer(t)
	host, port := srv.hostPort(t)

	s := SMTPSender{}
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "alerts@phoenix.local",
		"to":   "oncall@example.com",
		"tls":  false,
	}
	alert := domain.AlertContext{
		MonitorName:   "api",
		MonitorType:   "http",
		MonitorTarget: "https://api.example.com",
		Status:        domain.StatusDown,
		Message:       "connection refused",
	}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	msgs := srv.received()
	if len(msgs) != 1 {
		t.Fatalf("expected exactly 1 message accepted by the SMTP server, got %d", len(msgs))
	}
	m := msgs[0]

	if m.From != "alerts@phoenix.local" {
		t.Errorf("expected envelope from alerts@phoenix.local, got %q", m.From)
	}
	if len(m.To) != 1 || m.To[0] != "oncall@example.com" {
		t.Errorf("expected envelope to [oncall@example.com], got %v", m.To)
	}
	if !strings.Contains(m.Data, "From: alerts@phoenix.local") {
		t.Errorf("expected From header in message, got:\n%s", m.Data)
	}
	if !strings.Contains(m.Data, "To: oncall@example.com") {
		t.Errorf("expected To header in message, got:\n%s", m.Data)
	}
	if !strings.Contains(m.Data, "Subject: Phoenix Alert: api is DOWN") {
		t.Errorf("expected composed subject header, got:\n%s", m.Data)
	}
	body := decodeQPBody(t, m.Data)
	if !strings.Contains(body, "Monitor: api") || !strings.Contains(body, "Target: https://api.example.com") ||
		!strings.Contains(body, "Status: DOWN") || !strings.Contains(body, "Message: connection refused") {
		t.Errorf("expected composed alert fields in body, got:\n%s", body)
	}
}

func TestSMTPSender_Send_CustomTemplate(t *testing.T) {
	srv := newFakeSMTPServer(t)
	host, port := srv.hostPort(t)
	cfg := map[string]any{
		"host": host, "port": float64(port), "from": "alerts@phoenix.local",
		"to": "oncall@example.com", "tls": false,
	}
	alert := domain.AlertContext{
		MonitorName: "payments", Status: domain.StatusDown,
		TemplateTitle: "Custom: {{ monitor.name }} is {{ status }}",
		TemplateBody:  "Body={{ message }}; target={{ monitor.target }}",
		Message:       "connection refused", MonitorTarget: "https://example.test",
	}
	if err := (SMTPSender{}).Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send custom template: %v", err)
	}
	msgs := srv.received()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d; want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].Data, "Subject: Custom: payments is DOWN") {
		t.Fatalf("custom subject missing: %s", msgs[0].Data)
	}
	body := decodeQPBody(t, msgs[0].Data)
	if !strings.Contains(body, "Body=connection refused; target=https://example.test") {
		t.Fatalf("custom body missing: %s", body)
	}
}

func TestSMTPSender_Send_WhitespaceTemplateSubjectUsesFallback(t *testing.T) {
	srv := newFakeSMTPServer(t)
	host, port := srv.hostPort(t)
	cfg := map[string]any{
		"host": host, "port": float64(port), "from": "alerts@phoenix.local",
		"to": "oncall@example.com", "tls": false,
	}
	alert := domain.AlertContext{
		MonitorName:   "payments",
		Status:        domain.StatusDown,
		TemplateTitle: "   ",
		TemplateBody:  "Plain fallback body",
	}
	if err := (SMTPSender{}).Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send whitespace-subject template: %v", err)
	}

	messages := srv.received()
	if len(messages) != 1 {
		t.Fatalf("messages = %d; want 1", len(messages))
	}
	message, err := netmail.ReadMessage(strings.NewReader(messages[0].Data))
	if err != nil {
		t.Fatalf("parse MIME message: %v", err)
	}
	if got, want := message.Header.Get("Subject"), "Phoenix Alert: payments is DOWN"; got != want {
		t.Fatalf("Subject = %q; want %q", got, want)
	}
}

func TestSMTPSender_Send_HTMLTemplateUsesMultipartAlternative(t *testing.T) {
	srv := newFakeSMTPServer(t)
	host, port := srv.hostPort(t)
	cfg := map[string]any{
		"host": host, "port": float64(port), "from": "alerts@phoenix.local",
		"to": "oncall@example.com", "tls": false,
	}
	alert := domain.AlertContext{
		MonitorName:   `<script>alert("name")</script>`,
		MonitorTarget: "https://example.test/health?a=1&b=2",
		Status:        domain.StatusDown,
		Message:       `<img src=x onerror="alert(1)">`,
		TemplateTitle: "Incident: {{ monitor.name }}",
		TemplateBody:  "Plain {{ monitor.name }}: {{ message }}",
		TemplateConfig: domain.SMTPTemplateConfigMap(domain.SMTPTemplateConfig{
			Format: domain.SMTPTemplateFormatHTML,
			HTMLBodyTemplate: `<h1>{{ monitor.name }}</h1><p>{{ message }}</p>` +
				`<a href="{{ monitor.target }}">Open monitor</a>`,
		}),
	}
	if err := (SMTPSender{}).Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send HTML template: %v", err)
	}

	messages := srv.received()
	if len(messages) != 1 {
		t.Fatalf("messages = %d; want 1", len(messages))
	}
	parts := decodeMIMEParts(t, messages[0].Data)
	plain := parts["text/plain"]
	if !strings.Contains(plain, `Plain <script>alert("name")</script>: <img src=x onerror="alert(1)">`) {
		t.Fatalf("plain fallback missing original text values: %s", plain)
	}
	html := parts["text/html"]
	if strings.Contains(html, "<script>") || strings.Contains(html, "<img") {
		t.Fatalf("HTML alternative contains unescaped alert markup: %s", html)
	}
	for _, want := range []string{
		`&lt;script&gt;alert(&#34;name&#34;)&lt;/script&gt;`,
		`&lt;img src=x onerror=&#34;alert(1)&#34;&gt;`,
		`href="https://example.test/health?a=1&amp;b=2"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML alternative missing %q: %s", want, html)
		}
	}
}

func TestSMTPSender_Send_HTMLTemplateRejectsUnsafeDynamicURL(t *testing.T) {
	srv := newFakeSMTPServer(t)
	host, port := srv.hostPort(t)
	cfg := map[string]any{
		"host": host, "port": float64(port), "from": "alerts@phoenix.local",
		"to": "oncall@example.com", "tls": false,
	}
	alert := domain.AlertContext{
		MonitorName:   "api",
		MonitorTarget: "javascript:alert(document.cookie)",
		TemplateBody:  "Plain fallback",
		TemplateConfig: domain.SMTPTemplateConfigMap(domain.SMTPTemplateConfig{
			Format:           domain.SMTPTemplateFormatHTML,
			HTMLBodyTemplate: `<a href="{{ monitor.target }}">Open monitor</a>`,
		}),
	}
	err := (SMTPSender{}).Send(context.Background(), cfg, alert)
	if err == nil || !strings.Contains(err.Error(), "unsafe value") {
		t.Fatalf("unsafe dynamic URL error = %v; want unsafe value rejection", err)
	}
	if len(srv.received()) != 0 {
		t.Fatal("unsafe HTML template should not be delivered")
	}
}

func TestSMTPSender_Send_MultipleRecipients(t *testing.T) {
	srv := newFakeSMTPServer(t)
	host, port := srv.hostPort(t)

	s := SMTPSender{}
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "alerts@phoenix.local",
		"to":   []any{"a@example.com", "b@example.com"},
		"tls":  false,
	}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusUp}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	msgs := srv.received()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if len(msgs[0].To) != 2 {
		t.Fatalf("expected 2 envelope recipients, got %v", msgs[0].To)
	}
	want := map[string]bool{"a@example.com": true, "b@example.com": true}
	for _, to := range msgs[0].To {
		if !want[to] {
			t.Errorf("unexpected recipient %q", to)
		}
		delete(want, to)
	}
	if len(want) != 0 {
		t.Errorf("missing recipients: %v", want)
	}
}

func TestSMTPSender_Send_CertificateExpiry(t *testing.T) {
	srv := newFakeSMTPServer(t)
	host, port := srv.hostPort(t)

	s := SMTPSender{}
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "alerts@phoenix.local",
		"to":   "oncall@example.com",
		"tls":  false,
	}
	alert := domain.AlertContext{
		MonitorName:       "shop",
		EventKind:         domain.AlertEventCertificateExpiry,
		CertDaysRemaining: 7,
	}
	if err := s.Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}

	msgs := srv.received()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if !strings.Contains(msgs[0].Data, "Subject: Phoenix Alert: Certificate expiring: shop") {
		t.Errorf("expected cert-expiry subject, got:\n%s", msgs[0].Data)
	}
	body := decodeQPBody(t, msgs[0].Data)
	if !strings.Contains(body, "Event: certificate_expiry") || !strings.Contains(body, "7 day") {
		t.Errorf("expected cert-expiry body fields, got:\n%s", body)
	}
}

func TestSMTPSender_Send_DialError(t *testing.T) {
	// Bind and immediately close a listener so the port is refusing
	// connections: a fast, deterministic transport failure.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	_ = ln.Close()

	s := SMTPSender{}
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "alerts@phoenix.local",
		"to":   "oncall@example.com",
		"tls":  false,
	}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown}
	err = s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected dial error")
	}
	if !strings.Contains(err.Error(), "smtp: sending email") {
		t.Errorf("expected wrapped dial error, got %v", err)
	}
}

func TestSMTPSender_Send_ProviderRejection(t *testing.T) {
	srv := newFakeSMTPServer(t)
	srv.rejectMailFrom = true
	host, port := srv.hostPort(t)

	s := SMTPSender{}
	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "alerts@phoenix.local",
		"to":   "oncall@example.com",
		"tls":  false,
	}
	alert := domain.AlertContext{MonitorName: "api", Status: domain.StatusDown}
	err := s.Send(context.Background(), cfg, alert)
	if err == nil {
		t.Fatal("expected error when server rejects MAIL FROM")
	}
	if !strings.Contains(err.Error(), "550") {
		t.Errorf("expected server rejection envelope (550) surfaced in error, got %v", err)
	}
	if len(srv.received()) != 0 {
		t.Errorf("expected no message accepted when MAIL FROM is rejected, got %d", len(srv.received()))
	}
}

func TestSMTPSender_Send_EmptyTargetOmitted(t *testing.T) {
	srv := newFakeSMTPServer(t)
	host, port := srv.hostPort(t)

	cfg := map[string]any{
		"host": host,
		"port": float64(port),
		"from": "alerts@phoenix.local",
		"to":   "oncall@example.com",
		"tls":  false,
	}
	alert := domain.AlertContext{
		MonitorName: "api",
		MonitorType: "http",
		Status:      domain.StatusDown,
		Message:     "connection refused",
	}
	if err := (SMTPSender{}).Send(context.Background(), cfg, alert); err != nil {
		t.Fatalf("send failed: %v", err)
	}
	msgs := srv.received()
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	body := decodeQPBody(t, msgs[0].Data)
	if strings.Contains(body, "Target:") {
		t.Errorf("empty target must not render a Target line, got:\n%s", body)
	}
}
