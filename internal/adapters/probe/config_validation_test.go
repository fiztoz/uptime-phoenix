package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func localValidationFixture(t *testing.T) ConfigSnapshot {
	t.Helper()
	d := localDefinitionFixture()
	d.Notifications[0].Config = map[string]any{"host": "mail.test", "port": 587, "from": "ops@example.test", "to": "oncall@example.test"}
	doc, err := (LocalConfigEncoder{}).EncodeLocal(d)
	if err != nil {
		t.Fatal(err)
	}
	s, err := DecodeLocalConfigSnapshot(doc)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func localValidationDocument(t *testing.T, s ConfigSnapshot) []byte {
	t.Helper()
	doc, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func validationTarget(s ConfigSnapshot) domain.ProbeConfigTarget {
	return domain.ProbeConfigTarget{HubID: s.HubID, ProbeID: s.ProbeID}
}

func TestLocalConfigValidationCompleteAndEmpty(t *testing.T) {
	v := NewLocalConfigValidator(checker.Get, notifier.Get)
	s := localValidationFixture(t)
	doc := localValidationDocument(t, s)
	original := bytes.Clone(doc)
	if err := v.ValidateLocal(context.Background(), doc, validationTarget(s)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(doc, original) {
		t.Fatal("validation changed immutable bytes")
	}
	s.Assignments, s.NotificationChannels, s.NotificationTemplates = []ConfigAssignment{}, []ConfigChannel{}, []ConfigTemplate{}
	s.ProxyBindings, s.MaintenanceWindows, s.EscalationPolicies = []ConfigProxy{}, []ConfigMaintenance{}, []ConfigEscalation{}
	if err := v.ValidateLocal(context.Background(), localValidationDocument(t, s), validationTarget(s)); err != nil {
		t.Fatal(err)
	}
}

func TestLocalConfigValidationInstalledCheckers(t *testing.T) {
	configs := map[string]string{
		"http":      `{"url":"https://example.test"}`,
		"tcp":       `{"hostname":"example.test","port":443}`,
		"ping":      `{"hostname":"example.test"}`,
		"dns":       `{"hostname":"example.test"}`,
		"websocket": `{"url":"wss://example.test"}`,
		"push":      `{}`,
		"docker":    `{"container":"phoenix","docker_daemon":"unix:///var/run/docker.sock"}`,
		"mqtt":      `{"broker":"mqtt://example.test:1883"}`,
		"rabbitmq":  `{"url":"amqp://example.test:5672"}`,
		"grpc":      `{"url":"example.test:50051"}`,
		"snmp":      `{"hostname":"example.test","oid":"1.3.6.1.2.1.1.1.0"}`,
		"database":  `{"engine":"postgres","dsn":"postgres://user:secret@example.test/db"}`,
		"s3":        `{"bucket":"health_bucket","access_key":"fixture-access","secret_key":"fixture-secret"}`,
	}
	for kind, config := range configs {
		t.Run(kind, func(t *testing.T) {
			s := localValidationFixture(t)
			a := &s.Assignments[0]
			a.Monitor.Type, a.Monitor.Config = kind, json.RawMessage(config)
			a.RequiredCapabilities = []string{"checker." + kind + ".v1"}
			v := NewLocalConfigValidator(checker.Get, notifier.Get)
			if err := v.ValidateLocal(context.Background(), localValidationDocument(t, s), validationTarget(s)); err != nil {
				t.Fatal(err)
			}
			if kind == "push" {
				return // No push credential belongs in an execution snapshot.
			}
			a.Monitor.Config = json.RawMessage(`{}`)
			if err := v.ValidateLocal(context.Background(), localValidationDocument(t, s), validationTarget(s)); !errors.Is(err, domain.ErrValidation) {
				t.Fatal("accepted incomplete checker settings")
			}
		})
	}
}

func TestLocalConfigValidationInstalledNotifiers(t *testing.T) {
	configs := map[string]string{
		"telegram":   `{"bot_token":"fixture-token","chat_id":"123"}`,
		"discord":    `{"webhook_url":"https://example.test/hook"}`,
		"slack":      `{"webhook_url":"https://example.test/hook"}`,
		"smtp":       `{"host":"mail.test","port":587,"from":"ops@example.test","to":"oncall@example.test"}`,
		"webhook":    `{"url":"https://example.test/hook"}`,
		"teams":      `{"webhook_url":"https://example.test/hook"}`,
		"mattermost": `{"webhook_url":"https://example.test/hook"}`,
		"gotify":     `{"server_url":"https://example.test","app_token":"fixture-token"}`,
		"bark":       `{"server_url":"https://example.test","device_key":"fixture-key"}`,
		"feishu":     `{"webhook_url":"https://example.test/hook"}`,
		"line":       `{"channel_access_token":"fixture-token","user_id":"U123"}`,
	}
	for kind, config := range configs {
		t.Run(kind, func(t *testing.T) {
			s := localValidationFixture(t)
			c := &s.NotificationChannels[1] // Disabled escalation dependency.
			c.Type, c.Config = kind, json.RawMessage(config)
			v := NewLocalConfigValidator(checker.Get, notifier.Get)
			if err := v.ValidateLocal(context.Background(), localValidationDocument(t, s), validationTarget(s)); err != nil {
				t.Fatal(err)
			}
			c.Config = json.RawMessage(`{}`)
			if err := v.ValidateLocal(context.Background(), localValidationDocument(t, s), validationTarget(s)); !errors.Is(err, domain.ErrValidation) {
				t.Fatal("accepted incomplete disabled channel")
			}
		})
	}
}

func TestLocalConfigValidationRejectsInvalidSemantics(t *testing.T) {
	for name, change := range map[string]func(*ConfigSnapshot){
		"paused checker": func(s *ConfigSnapshot) {
			s.Assignments[0].Active = false
			s.Assignments[0].Monitor.Config = json.RawMessage(`{"url":"https://fixture-secret-value@bad%"}`)
		},
		"unsupported capability": func(s *ConfigSnapshot) {
			s.Assignments[0].RequiredCapabilities = append(s.Assignments[0].RequiredCapabilities, "runtime.fixture-secret-value.v1")
		},
		"unsupported version": func(s *ConfigSnapshot) {
			s.Assignments[0].RequiredCapabilities = append(s.Assignments[0].RequiredCapabilities, "checker.http.v2")
		},
		"push secret": func(s *ConfigSnapshot) {
			s.Assignments[1].Monitor.Config = json.RawMessage(`{"push_token":"fixture-secret-value"}`)
		},
		"template placeholder": func(s *ConfigSnapshot) { s.NotificationTemplates[0].BodyTemplate = "{{fixture.secret.value}}" },
		"template structured settings": func(s *ConfigSnapshot) {
			s.NotificationTemplates[0].Config = json.RawMessage(`{"secret":"fixture-secret-value"}`)
		},
		"disabled bad timezone": func(s *ConfigSnapshot) {
			s.MaintenanceWindows[0].Active = false
			s.MaintenanceWindows[0].Timezone = "fixture-secret-value"
		},
		"host-local timezone":   func(s *ConfigSnapshot) { s.MaintenanceWindows[0].Timezone = "Local" },
		"proxy URL as host":     func(s *ConfigSnapshot) { s.ProxyBindings[0].Host = "https://fixture-secret-value:password@proxy.test" },
		"proxy host whitespace": func(s *ConfigSnapshot) { s.ProxyBindings[0].Host = "proxy.test\nfixture-secret-value" },
		"proxy host with port":  func(s *ConfigSnapshot) { s.ProxyBindings[0].Host = "proxy.test:8080" },
		"local watchdog":        func(s *ConfigSnapshot) { s.Watchdog.Enabled = true },
		"missing graph link":    func(s *ConfigSnapshot) { s.NotificationChannels = s.NotificationChannels[1:] },
		"bad cron": func(s *ConfigSnapshot) {
			w := &s.MaintenanceWindows[0]
			w.Strategy, w.StartDate, w.EndDate, w.CronExpr, w.Duration = "cron", nil, nil, "fixture-secret-value", 5
		},
		"impossible cron": func(s *ConfigSnapshot) {
			w := &s.MaintenanceWindows[0]
			w.Strategy, w.StartDate, w.EndDate, w.CronExpr, w.Duration = "cron", nil, nil, "0 0 30 2 *", 5
		},
		"host-local cron timezone": func(s *ConfigSnapshot) {
			w := &s.MaintenanceWindows[0]
			w.Strategy, w.StartDate, w.EndDate, w.CronExpr, w.Duration = "cron", nil, nil, "CRON_TZ=Local 0 2 * * *", 5
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := localValidationFixture(t)
			change(&s)
			err := NewLocalConfigValidator(checker.Get, notifier.Get).ValidateLocal(context.Background(), localValidationDocument(t, s), validationTarget(s))
			if !errors.Is(err, domain.ErrValidation) || strings.Contains(err.Error(), "fixture-secret-value") || strings.Contains(err.Error(), "fixture.secret.value") {
				t.Fatal("invalid config succeeded or leaked extension diagnostics")
			}
		})
	}
}

func TestLocalConfigValidationStructuredTemplates(t *testing.T) {
	for _, tc := range []struct {
		provider, channel, valid, invalid string
	}{
		{"smtp", `{"host":"mail.test","port":587,"from":"ops@example.test","to":"oncall@example.test"}`,
			`{"format":"html","html_body_template":"<p>{{monitor.name}}</p>"}`,
			`{"format":"html","html_body_template":"<p>{{fixture.secret}}</p>"}`},
		{"discord", `{"webhook_url":"https://example.test/hook"}`,
			`{"footer_template":"{{monitor.name}}"}`, `{"footer_template":"{{fixture.secret}}"}`},
		{"line", `{"channel_access_token":"fixture-token","user_id":"U123"}`, `{}`, `{"extra":"fixture-secret"}`},
		{"webhook", `{"url":"https://example.test/hook"}`, `{}`, `{"extra":"fixture-secret"}`},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			s := localValidationFixture(t)
			s.NotificationChannels[0].Type, s.NotificationChannels[0].Config = tc.provider, json.RawMessage(tc.channel)
			s.NotificationTemplates[0].Provider, s.NotificationTemplates[0].Config = tc.provider, json.RawMessage(tc.valid)
			v := NewLocalConfigValidator(checker.Get, notifier.Get)
			if err := v.ValidateLocal(context.Background(), localValidationDocument(t, s), validationTarget(s)); err != nil {
				t.Fatal(err)
			}
			s.NotificationTemplates[0].Config = json.RawMessage(tc.invalid)
			if err := v.ValidateLocal(context.Background(), localValidationDocument(t, s), validationTarget(s)); !errors.Is(err, domain.ErrValidation) || strings.Contains(err.Error(), "fixture") {
				t.Fatal("invalid structured template succeeded or leaked")
			}
		})
	}
}

func TestLocalConfigValidationScheduleAndProxyVariants(t *testing.T) {
	for _, expr := range []string{"0 2 * * *", "@daily", "@every 5m", "CRON_TZ=Asia/Bangkok 0 2 * * *", "0 0 29 2 *"} {
		s := localValidationFixture(t)
		w := &s.MaintenanceWindows[0]
		w.Strategy, w.StartDate, w.EndDate, w.CronExpr, w.Duration, w.Timezone = "cron", nil, nil, expr, 5, "Asia/Bangkok"
		if err := NewLocalConfigValidator(checker.Get, notifier.Get).ValidateLocal(context.Background(), localValidationDocument(t, s), validationTarget(s)); err != nil {
			t.Fatalf("valid schedule %q: %v", expr, err)
		}
	}
	for _, protocol := range []string{"http", "https", "socks5"} {
		for _, host := range []string{"proxy.test", "127.0.0.1", "::1", "fe80::1%en0"} {
			s := localValidationFixture(t)
			s.ProxyBindings[0].Protocol, s.ProxyBindings[0].Host = protocol, host
			if err := NewLocalConfigValidator(checker.Get, notifier.Get).ValidateLocal(context.Background(), localValidationDocument(t, s), validationTarget(s)); err != nil {
				t.Fatalf("valid proxy %s/%s: %v", protocol, host, err)
			}
		}
	}
}

type validationChecker struct {
	kind     string
	validate func(map[string]any) error
	checks   int
}

func (c *validationChecker) Type() string                         { return c.kind }
func (c *validationChecker) Validate(config map[string]any) error { return c.validate(config) }
func (c *validationChecker) Check(context.Context, map[string]any) (ports.CheckResult, error) {
	c.checks++
	return ports.CheckResult{}, nil
}

type validationSender struct {
	sends int
}

func (*validationSender) Type() string                  { return "smtp" }
func (*validationSender) Validate(map[string]any) error { return nil }
func (s *validationSender) Send(context.Context, map[string]any, domain.AlertContext) error {
	s.sends++
	return nil
}

func TestLocalConfigValidationEffectiveSettingsAndNoIO(t *testing.T) {
	s := localValidationFixture(t)
	s.Assignments[0].Monitor.TLSIgnore = true
	s.Assignments[0].Monitor.Config = json.RawMessage(`{"url":"https://example.test","timeout":-1,"tls_ignore":false,"accepted_statuscodes":["wrong"]}`)
	calls := 0
	c := &validationChecker{kind: "http", validate: func(cfg map[string]any) error {
		calls++
		if cfg["timeout"] != 1.25 || cfg["tls_ignore"] != true || !reflect.DeepEqual(cfg["accepted_statuscodes"], []any{"200-299"}) {
			t.Fatal("validator did not receive effective scheduler settings")
		}
		cfg["url"] = "changed-by-validator"
		return nil
	}}
	n := &validationSender{}
	v := NewLocalConfigValidator(func(kind string) (ports.Checker, bool) {
		if kind == "http" {
			return c, true
		}
		return checker.Get(kind)
	}, func(kind string) (ports.NotificationSender, bool) {
		if kind == "smtp" {
			return n, true
		}
		return notifier.Get(kind)
	})
	doc := localValidationDocument(t, s)
	original := bytes.Clone(doc)
	if err := v.ValidateLocal(context.Background(), doc, validationTarget(s)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || c.checks != 0 || n.sends != 0 || !bytes.Equal(doc, original) {
		t.Fatal("validation performed I/O or changed source bytes")
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.validate = func(map[string]any) error { cancel(); return errors.New("fixture-secret-value") }
	if err := v.ValidateLocal(ctx, doc, validationTarget(s)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation during validation was lost")
	}
}

func TestLocalConfigValidationFailsClosed(t *testing.T) {
	s := localValidationFixture(t)
	doc, target := localValidationDocument(t, s), validationTarget(s)
	for name, v := range map[string]*LocalConfigValidator{
		"nil":                 nil,
		"missing lookups":     NewLocalConfigValidator(nil, nil),
		"checker unavailable": NewLocalConfigValidator(func(string) (ports.Checker, bool) { return nil, false }, notifier.Get),
		"sender unavailable":  NewLocalConfigValidator(checker.Get, func(string) (ports.NotificationSender, bool) { return nil, false }),
		"checker mismatch":    NewLocalConfigValidator(func(string) (ports.Checker, bool) { return checker.Get("tcp") }, notifier.Get),
	} {
		t.Run(name, func(t *testing.T) {
			if err := v.ValidateLocal(context.Background(), doc, target); !errors.Is(err, domain.ErrValidation) {
				t.Fatal("incomplete runtime accepted")
			}
		})
	}
	v := NewLocalConfigValidator(checker.Get, notifier.Get)
	for _, bad := range []domain.ProbeConfigTarget{{HubID: target.HubID, ProbeID: target.HubID}, {HubID: "22222222-2222-4222-8222-222222222222", ProbeID: "local"}} {
		if err := v.ValidateLocal(context.Background(), doc, bad); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("wrong target accepted")
		}
	}
	if err := v.ValidateLocal(context.Background(), []byte(`{"fixture-secret-value":true}`), target); !errors.Is(err, domain.ErrValidation) || strings.Contains(err.Error(), "fixture-secret-value") {
		t.Fatal("malformed document leaked")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := v.ValidateLocal(ctx, doc, target); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored cancellation")
	}
}
