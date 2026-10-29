package probe

import (
	"bytes"
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
)

func TestLocalConfigPushCredentialDoesNotBlockOtherMonitors(t *testing.T) {
	d := localDefinitionFixture()
	// Monitor creation stores the credential in both the domain field and config.
	push := d.Assignments[0].Monitor
	push.Active = true
	push.Config = map[string]any{"push_token": push.PushToken, "custom": "preserved"}
	d.Notifications[0].Config = map[string]any{"host": "mail.test", "port": 587, "from": "ops@example.test", "to": "oncall@example.test"}
	doc, err := (LocalConfigEncoder{}).EncodeLocal(d)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(doc, []byte("push_token")) || bytes.Contains(doc, []byte(push.PushToken)) {
		t.Error("push credential leaked into execution snapshot")
	}
	if err := NewLocalConfigValidator(checker.Get, notifier.Get).ValidateLocal(context.Background(), doc, d.Target); err != nil {
		t.Fatalf("push monitor blocks complete local snapshot: %v", err)
	}
	if push.Config["push_token"] != push.PushToken {
		t.Error("encoder mutated hub-owned push credential")
	}
}

func TestLocalConfigSMTPRecipientArray(t *testing.T) {
	for name, to := range map[string]any{
		"decoded": []any{"one@example.test", "two@example.test"},
		"native":  []string{"one@example.test", "two@example.test"},
	} {
		t.Run(name, func(t *testing.T) {
			d := localDefinitionFixture()
			d.Notifications[0].Config = map[string]any{"host": "mail.test", "port": 587, "from": "ops@example.test", "to": to}
			doc, err := (LocalConfigEncoder{}).EncodeLocal(d)
			if err != nil {
				t.Fatal(err)
			}
			if err := NewLocalConfigValidator(checker.Get, notifier.Get).ValidateLocal(context.Background(), doc, d.Target); err != nil {
				t.Fatalf("valid SMTP array blocks complete local snapshot: %v", err)
			}
		})
	}
}
