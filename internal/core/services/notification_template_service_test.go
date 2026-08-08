package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type templateServiceRepo struct {
	next  int64
	items map[int64]*domain.NotificationTemplate
}

func newTemplateServiceRepo() *templateServiceRepo {
	return &templateServiceRepo{items: make(map[int64]*domain.NotificationTemplate)}
}

func (r *templateServiceRepo) Create(_ context.Context, template *domain.NotificationTemplate) error {
	r.next++
	template.ID = r.next
	template.CreatedAt = time.Now().UTC()
	template.UpdatedAt = template.CreatedAt
	copy := *template
	r.items[template.ID] = &copy
	return nil
}

func (r *templateServiceRepo) GetByID(_ context.Context, id int64) (*domain.NotificationTemplate, error) {
	template, ok := r.items[id]
	if !ok {
		return nil, ports.ErrNotFound
	}
	copy := *template
	return &copy, nil
}

func (r *templateServiceRepo) List(_ context.Context) ([]*domain.NotificationTemplate, error) {
	out := make([]*domain.NotificationTemplate, 0, len(r.items))
	for _, template := range r.items {
		copy := *template
		out = append(out, &copy)
	}
	return out, nil
}

func (r *templateServiceRepo) Update(_ context.Context, template *domain.NotificationTemplate) error {
	if _, ok := r.items[template.ID]; !ok {
		return ports.ErrNotFound
	}
	copy := *template
	r.items[template.ID] = &copy
	return nil
}

func (r *templateServiceRepo) Delete(_ context.Context, id int64) error {
	if _, ok := r.items[id]; !ok {
		return ports.ErrNotFound
	}
	delete(r.items, id)
	return nil
}

func TestNotificationTemplateService_CreateValidatesProviderAndVariables(t *testing.T) {
	svc := NewNotificationTemplateService(newTemplateServiceRepo())
	ctx := context.Background()

	valid := &domain.NotificationTemplate{
		Name: "Discord on-call", Provider: "discord",
		TitleTemplate: "{{ status.emoji }} {{ alert.name }} is {{ status }}",
		BodyTemplate:  "{{ message }}",
		Config: domain.DiscordTemplateConfigMap(domain.DiscordTemplateConfig{
			Colors: domain.DefaultDiscordTemplateConfig().Colors,
			Fields: []domain.DiscordEmbedFieldTemplate{
				{NameTemplate: "Condition", ValueTemplate: "{{ group.condition }}"},
			},
		}),
	}
	if err := svc.Create(ctx, valid); err != nil {
		t.Fatalf("Create(valid): %v", err)
	}
	if valid.ID == 0 {
		t.Fatal("Create(valid) did not persist the template")
	}

	for _, invalid := range []*domain.NotificationTemplate{
		{Name: "Slack", Provider: "slack", BodyTemplate: "{{ message }}"},
		{Name: "Unknown variable", Provider: "line", BodyTemplate: "{{ uptime }}"},
		{Name: "Missing body", Provider: "smtp"},
		{Name: "Bad Discord color", Provider: "discord", TitleTemplate: "Alert", Config: map[string]any{"colors": map[string]any{"down": "red"}}},
		{Name: "Bad Discord field", Provider: "discord", TitleTemplate: "Alert", Config: map[string]any{"fields": []any{map[string]any{"name_template": "Name", "value_template": "{{ missing }}"}}}},
	} {
		if err := svc.Create(ctx, invalid); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("Create(%q) error = %v; want ErrValidation", invalid.Name, err)
		}
	}
}

func TestNotificationTemplateService_ValidatesSMTPHTMLConfiguration(t *testing.T) {
	svc := NewNotificationTemplateService(newTemplateServiceRepo())
	ctx := context.Background()

	valid := &domain.NotificationTemplate{
		Name:          "SMTP rich incident",
		Provider:      "smtp",
		TitleTemplate: "{{ status.emoji }} {{ alert.name }} is {{ status }}",
		BodyTemplate:  "{{ alert.name }} is {{ status }}\n{{ message }}",
		Config: domain.SMTPTemplateConfigMap(domain.SMTPTemplateConfig{
			Format:           domain.SMTPTemplateFormatHTML,
			HTMLBodyTemplate: `<h1>{{ alert.name }}</h1><p>{{ message }}</p><a href="{{ ack_url }}">Acknowledge</a>`,
		}),
	}
	if err := svc.Create(ctx, valid); err != nil {
		t.Fatalf("Create(valid HTML SMTP): %v", err)
	}

	for _, invalid := range []*domain.NotificationTemplate{
		{
			Name: "Missing HTML", Provider: "smtp", BodyTemplate: "plain fallback",
			Config: domain.SMTPTemplateConfigMap(domain.SMTPTemplateConfig{Format: domain.SMTPTemplateFormatHTML}),
		},
		{
			Name: "HTML in plain mode", Provider: "smtp", BodyTemplate: "plain fallback",
			Config: domain.SMTPTemplateConfigMap(domain.SMTPTemplateConfig{
				Format: domain.SMTPTemplateFormatPlain, HTMLBodyTemplate: "<p>unused</p>",
			}),
		},
		{
			Name: "Unknown format", Provider: "smtp", BodyTemplate: "plain fallback",
			Config: map[string]any{"format": "markdown"},
		},
		{
			Name: "Unknown HTML variable", Provider: "smtp", BodyTemplate: "plain fallback",
			Config: domain.SMTPTemplateConfigMap(domain.SMTPTemplateConfig{
				Format: domain.SMTPTemplateFormatHTML, HTMLBodyTemplate: "<p>{{ uptime }}</p>",
			}),
		},
	} {
		if err := svc.Create(ctx, invalid); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("Create(%q) error = %v; want ErrValidation", invalid.Name, err)
		}
	}

	legacyPlain := &domain.NotificationTemplate{
		Name: "Legacy plain", Provider: "smtp", TitleTemplate: "Alert", BodyTemplate: "{{ message }}",
	}
	if err := svc.Create(ctx, legacyPlain); err != nil {
		t.Fatalf("Create(legacy plain SMTP): %v", err)
	}
}

func TestNotificationService_ResolvesMatchingTemplateForDispatch(t *testing.T) {
	repo := newTemplateServiceRepo()
	templateSvc := NewNotificationTemplateService(repo)
	template := &domain.NotificationTemplate{Name: "LINE", Provider: "line", BodyTemplate: "{{ monitor.name }}: {{ status }}"}
	if err := templateSvc.Create(context.Background(), template); err != nil {
		t.Fatalf("create template: %v", err)
	}

	notificationSvc := NewNotificationService(nil, nil)
	notificationSvc.SetTemplateRepository(repo)
	notification := &domain.Notification{Type: "line", TemplateID: &template.ID}
	alert, err := notificationSvc.alertForNotification(context.Background(), notification, domain.AlertContext{MonitorName: "API", Status: domain.StatusDown})
	if err != nil {
		t.Fatalf("alertForNotification: %v", err)
	}
	if alert.TemplateBody != template.BodyTemplate {
		t.Fatalf("resolved body = %q; want %q", alert.TemplateBody, template.BodyTemplate)
	}
	if len(alert.TemplateConfig) != len(template.Config) {
		t.Fatalf("resolved config = %#v; want %#v", alert.TemplateConfig, template.Config)
	}

	notification.Type = "discord"
	if err := notificationSvc.validateTemplateAssignment(context.Background(), notification); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("mismatched provider error = %v; want ErrValidation", err)
	}
}
