package notifier

import (
	"context"
	"fmt"
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
	if isCertificateExpiry(alert) {
		subject = alertTitleWithPrefix("Phoenix Alert:", alert)
		body = fmt.Sprintf("Monitor: %s\nType: %s\nTarget: %s\nEvent: certificate_expiry\n%s\nTime: %s\n",
			alert.MonitorName, alert.MonitorType, alert.MonitorTarget, alertBody(alert),
			time.Now().Format(time.RFC3339))
	} else {
		subject = fmt.Sprintf("Phoenix Alert: %s is %s", alert.MonitorName, alert.Status)
		body = fmt.Sprintf("Monitor: %s\nType: %s\nTarget: %s\nStatus: %s\nMessage: %s\nTime: %s\nDuration: %s\n\n%s",
			alert.MonitorName, alert.MonitorType, alert.MonitorTarget, alert.Status, alert.Message,
			time.Now().Format(time.RFC3339), alert.Duration, alert.CheckOutput)
	}

	m := mail.NewMessage()
	m.SetHeader("From", from)
	m.SetHeader("To", toAddrs...)
	m.SetHeader("Subject", subject)
	m.SetBody("text/plain", body)

	d := mail.NewDialer(host, port, username, password)
	d.Timeout = 10 * time.Second
	if useTLS {
		d.StartTLSPolicy = mail.MandatoryStartTLS
	} else {
		d.StartTLSPolicy = mail.NoStartTLS
	}

	// Use context? mail.v2 dialer doesn't take ctx directly, but we timeout via dialer.
	// For simplicity, send with timeout via goroutine + ctx (but to keep simple, rely on dialer timeout)
	if err := d.DialAndSend(m); err != nil {
		return fmt.Errorf("smtp: sending email: %w", err)
	}
	return nil
}
