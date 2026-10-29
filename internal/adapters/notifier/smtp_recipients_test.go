package notifier

import (
	"context"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestSMTPRecipientValidation(t *testing.T) {
	for _, tc := range []struct {
		name  string
		to    any
		valid bool
	}{
		{"string", "one@example.test", true},
		{"decoded", []any{"one@example.test", "two@example.test"}, true},
		{"native", []string{"one@example.test", "two@example.test"}, true},
		{"missing", nil, false},
		{"empty string", "", false},
		{"blank", "  ", false},
		{"empty decoded", []any{}, false},
		{"empty native", []string{}, false},
		{"mixed", []any{"one@example.test", 17}, false},
		{"blank member", []string{"one@example.test", " "}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := map[string]any{"host": "127.0.0.1", "port": 1, "from": "ops@example.test", "to": tc.to}
			err := (SMTPSender{}).Validate(cfg)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v error=%v", tc.valid, err)
			}
			if !tc.valid {
				err := (SMTPSender{}).Send(context.Background(), cfg, domain.AlertContext{})
				if err == nil || !strings.Contains(err.Error(), "to") {
					t.Fatalf("invalid recipients must fail before network I/O: %v", err)
				}
			}
		})
	}
}
