package notifier

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/mail.v2"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// SMTPSender implements NotificationSender for SMTP email.
type SMTPSender struct{}

func init() { Register(SMTPSender{}) }

func (SMTPSender) Type() string { return "smtp" }

func (SMTPSender) Validate(config map[string]any) error {
	required := []string{"host", "from", "to"}
	for _, k := range required {
		if v, ok := config[k].(string); !ok || v == "" {
			return fmt.Errorf("%s is required", k)
		}
	}
	if _, ok := config["port"].(float64); !ok {
		if _, ok2 := config["port"].(int); !ok2 {
			return fmt.Errorf("port is required (number)")
		}
	}
	return nil
}

func (SMTPSender) Send(ctx context.Context, config map[string]any, alert domain.AlertContext) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	templateConfig, err := domain.ParseSMTPTemplateConfig(alert.TemplateConfig)
	if err != nil {
		return fmt.Errorf("smtp: invalid template configuration: %w", err)
	}
	host, _ := config["host"].(string)
	port := 587
	if p, ok := config["port"].(float64); ok {
		port = int(p)
	} else if p, ok := config["port"].(int); ok {
		port = p
	}
	username, _ := config["username"].(string)
	password, _ := config["password"].(string)
	from, _ := config["from"].(string)
	toIface := config["to"]
	var toAddrs []string
	switch v := toIface.(type) {
	case string:
		toAddrs = []string{v}
	case []any:
		for _, t := range v {
			if s, ok := t.(string); ok {
				toAddrs = append(toAddrs, s)
			}
		}
	case []string:
		toAddrs = v
	default:
		return fmt.Errorf("to must be string or array of strings")
	}
	useTLS := true
	if tls, ok := config["tls"].(bool); ok {
		useTLS = tls
	}

	var subject, body string
	targetLine := ""
	if alert.MonitorTarget != "" {
		targetLine = fmt.Sprintf("Target: %s\n", alert.MonitorTarget)
	}
	if isProbeConnection(alert) {
		subject = alertTitleWithPrefix("Phoenix Alert:", alert)
		body = alertBody(alert) + "\nTime: " + time.Now().UTC().Format(time.RFC3339)
	} else if isAuxiliaryAlert(alert) {
		subject = alertTitleWithPrefix("Phoenix Alert:", alert)
		body = fmt.Sprintf("Monitor: %s\nType: %s\n%sEvent: %s\n%s\nTime: %s\n",
			alert.MonitorName, alert.MonitorType, targetLine, alert.EventKind, alertBody(alert),
			time.Now().Format(time.RFC3339))
	} else {
		subject = fmt.Sprintf("Phoenix Alert: %s is %s", alert.MonitorName, alert.Status)
		body = fmt.Sprintf("Monitor: %s\nType: %s\n%sStatus: %s\nMessage: %s\nTime: %s\nDuration: %s\n\n%s",
			alert.MonitorName, alert.MonitorType, targetLine, alert.Status, alert.Message,
			time.Now().Format(time.RFC3339), alert.Duration, alert.CheckOutput)
	}
	renderedAt := time.Now().UTC()
	customSubject, customBody, custom, err := renderCustomLayoutAt(alert, renderedAt)
	if err != nil {
		return fmt.Errorf("smtp: %w", err)
	}
	if custom {
		if strings.TrimSpace(customSubject) != "" {
			subject = customSubject
		}
		body = customBody
	}
	// Header values must remain a single line even when a variable contains
	// untrusted monitor/check text.
	subject = strings.NewReplacer("\r", " ", "\n", " ").Replace(subject)
	if len([]rune(subject)) > 998 {
		return fmt.Errorf("smtp: rendered template title exceeds 998 characters")
	}

	m := mail.NewMessage()
	m.SetHeader("From", from)
	m.SetHeader("To", toAddrs...)
	m.SetHeader("Subject", subject)
	m.SetBody("text/plain", body)
	if templateConfig.Format == domain.SMTPTemplateFormatHTML {
		if strings.TrimSpace(templateConfig.HTMLBodyTemplate) == "" {
			return fmt.Errorf("smtp: HTML body template is required for html format")
		}
		htmlBody, err := domain.RenderNotificationHTMLTemplate(
			templateConfig.HTMLBodyTemplate,
			alert,
			renderedAt,
		)
		if err != nil {
			return fmt.Errorf("smtp: render HTML body: %w", err)
		}
		m.AddAlternative("text/html", htmlBody)
	}

	// Keep mail.v2's envelope/MIME formatting, but own the socket lifetime. Its
	// Dialer waits for the greeting before installing a deadline, ignores ctx,
	// and can recursively reconnect; durable delivery owns retries instead.
	sendCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	transport := smtpTransportConfig{host: host, port: port, username: username, password: password, useTLS: useTLS}
	err = mail.Send(mail.SendFunc(func(from string, to []string, message io.WriterTo) error {
		return transport.send(sendCtx, from, to, message)
	}), m)
	if err != nil {
		if sendCtx.Err() != nil {
			err = sendCtx.Err()
		}
		return fmt.Errorf("smtp: sending email: %w", err)
	}
	return nil
}

type smtpTransportConfig struct {
	host, username, password string
	port                     int
	useTLS                   bool
}

func (c smtpTransportConfig) send(ctx context.Context, from string, to []string, message io.WriterTo) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(c.host, strconv.Itoa(c.port)))
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	tlsConfig := &tls.Config{ServerName: c.host, MinVersion: tls.VersionTLS12}
	transport := conn
	if c.port == 465 {
		secure := tls.Client(conn, tlsConfig)
		if err := secure.HandshakeContext(ctx); err != nil {
			return err
		}
		transport = secure
	}
	client, err := smtp.NewClient(transport, c.host)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if c.port != 465 && c.useTLS {
		if supported, _ := client.Extension("STARTTLS"); !supported {
			return errors.New("smtp: mandatory STARTTLS unavailable")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if c.username != "" {
		if ok, mechanisms := client.Extension("AUTH"); ok {
			auth := smtp.PlainAuth("", c.username, c.password, c.host)
			advertised := strings.Fields(mechanisms)
			if slices.Contains(advertised, "CRAM-MD5") {
				auth = smtp.CRAMMD5Auth(c.username, c.password)
			} else if slices.Contains(advertised, "LOGIN") && !slices.Contains(advertised, "PLAIN") {
				auth = smtpLoginAuth{host: c.host, username: c.username, password: c.password}
			}
			if err := client.Auth(auth); err != nil {
				return err
			}
		}
	}
	if err := client.Mail(from); err != nil {
		return err
	}
	for _, recipient := range to {
		if err := client.Rcpt(recipient); err != nil {
			return err
		}
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := message.WriteTo(writer); err != nil {
		// Closing the socket aborts this transaction. Do not send a final DATA
		// terminator for a partially serialized message or retry implicitly.
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}
	// DATA acknowledgement is the provider result; QUIT is bounded cleanup.
	_ = client.Quit()
	return nil
}

// Preserve mail.v2's advertised LOGIN-only server compatibility.
type smtpLoginAuth struct{ host, username, password string }

func (a smtpLoginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if server.Name != a.host || !server.TLS && !slices.Contains(server.Auth, "LOGIN") {
		return "", nil, errors.New("smtp: LOGIN server identity mismatch")
	}
	return "LOGIN", nil, nil
}

func (a smtpLoginAuth) Next(challenge []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch string(challenge) {
	case "Username:":
		return []byte(a.username), nil
	case "Password:":
		return []byte(a.password), nil
	default:
		return nil, errors.New("smtp: unsupported LOGIN challenge")
	}
}
