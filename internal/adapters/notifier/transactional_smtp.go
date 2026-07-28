package notifier

import (
	"context"
	"fmt"
	"html"
	"time"

	"gopkg.in/mail.v2"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// TransactionalSMTP delivers ad-hoc email using an SMTP notification config.
// It is intentionally separate from the registered SMTPSender alert adapter
// so alert formatting can evolve without affecting subscription mail.
type TransactionalSMTP struct{}

// NewTransactionalSMTP returns a ports.TransactionalEmailSender.
func NewTransactionalSMTP() *TransactionalSMTP {
	return &TransactionalSMTP{}
}

// Send delivers msg to msg.To using smtpConfig (host/port/username/password/from/tls).
// The config "to" field is ignored — the message recipient always wins.
func (TransactionalSMTP) Send(ctx context.Context, smtpConfig map[string]any, msg ports.EmailMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if msg.To == "" {
		return fmt.Errorf("transactional smtp: recipient is required")
	}
	if msg.Subject == "" {
		return fmt.Errorf("transactional smtp: subject is required")
	}

	host, _ := smtpConfig["host"].(string)
	if host == "" {
		return fmt.Errorf("transactional smtp: host is required")
	}
	from, _ := smtpConfig["from"].(string)
	if from == "" {
		return fmt.Errorf("transactional smtp: from is required")
	}

	port := 587
	if p, ok := smtpConfig["port"].(float64); ok {
		port = int(p)
	} else if p, ok := smtpConfig["port"].(int); ok {
		port = p
	} else if p, ok := smtpConfig["port"].(int64); ok {
		port = int(p)
	}

	username, _ := smtpConfig["username"].(string)
	password, _ := smtpConfig["password"].(string)
	useTLS := true
	if tls, ok := smtpConfig["tls"].(bool); ok {
		useTLS = tls
	}

	textBody := msg.TextBody
	htmlBody := msg.HTMLBody
	if htmlBody == "" && textBody != "" {
		// Safe fallback: escape plain text into a minimal HTML body.
		htmlBody = "<pre>" + html.EscapeString(textBody) + "</pre>"
	}

	m := mail.NewMessage()
	m.SetHeader("From", from)
	m.SetHeader("To", msg.To)
	m.SetHeader("Subject", msg.Subject)
	if textBody != "" {
		m.SetBody("text/plain", textBody)
		if htmlBody != "" {
			m.AddAlternative("text/html", htmlBody)
		}
	} else if htmlBody != "" {
		m.SetBody("text/html", htmlBody)
	} else {
		return fmt.Errorf("transactional smtp: body is required")
	}

	d := mail.NewDialer(host, port, username, password)
	d.Timeout = 10 * time.Second
	if useTLS {
		d.StartTLSPolicy = mail.MandatoryStartTLS
	} else {
		d.StartTLSPolicy = mail.NoStartTLS
	}

	if err := d.DialAndSend(m); err != nil {
		return fmt.Errorf("transactional smtp: sending email: %w", err)
	}
	return nil
}

var _ ports.TransactionalEmailSender = (*TransactionalSMTP)(nil)
