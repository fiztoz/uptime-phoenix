package ports

import "context"

// EmailMessage is a plain-text + HTML transactional email payload.
// HTMLBody must already be escaped by the caller for any untrusted content.
type EmailMessage struct {
	To       string
	Subject  string
	TextBody string
	HTMLBody string
}

// TransactionalEmailSender delivers ad-hoc email using an existing SMTP
// notification config map (host/port/username/password/from/tls). The
// recipient on the message overrides any "to" field in the config so
// subscription mail goes only to the subscriber.
type TransactionalEmailSender interface {
	Send(ctx context.Context, smtpConfig map[string]any, msg EmailMessage) error
}
