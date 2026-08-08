package sqlite

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestNotificationTemplateRepository_RoundTripAndDeleteFallback(t *testing.T) {
	repo := setupTestDB(t)
	ctx := context.Background()
	user := &domain.User{
		Username: "template-owner", PasswordHash: "hashed", Active: true, Timezone: "UTC",
	}
	if err := repo.UserRepo.Create(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}

	template := &domain.NotificationTemplate{
		UserID: user.ID, Name: "Discord incident", Provider: "discord",
		TitleTemplate: "{{ alert.name }} is {{ status }}",
		BodyTemplate:  "{{ message }}",
		Config: domain.DiscordTemplateConfigMap(domain.DiscordTemplateConfig{
			TitleURLTemplate: "{{ alert.target }}",
			ShowTimestamp:    true,
			Colors:           domain.DefaultDiscordTemplateConfig().Colors,
			Fields: []domain.DiscordEmbedFieldTemplate{
				{NameTemplate: "Name", ValueTemplate: "{{ alert.name }}", Inline: true},
			},
		}),
	}
	if err := repo.NotificationTemplateRepo.Create(ctx, template); err != nil {
		t.Fatalf("create template: %v", err)
	}
	if template.ID == 0 {
		t.Fatal("template ID was not populated")
	}

	notification := &domain.Notification{
		UserID: user.ID, Name: "Ops Discord", Type: "discord", Active: true,
		TemplateID: &template.ID, Config: map[string]any{"webhook_url": "https://example.test/hook"},
	}
	if err := repo.NotificationRepo.Create(ctx, notification); err != nil {
		t.Fatalf("create notification: %v", err)
	}
	got, err := repo.NotificationRepo.GetByID(ctx, notification.ID)
	if err != nil {
		t.Fatalf("get notification: %v", err)
	}
	if got.TemplateID == nil || *got.TemplateID != template.ID {
		t.Fatalf("notification template_id = %v; want %d", got.TemplateID, template.ID)
	}

	template.Name = "Webhook JSON v2"
	if err := repo.NotificationTemplateRepo.Update(ctx, template); err != nil {
		t.Fatalf("update template: %v", err)
	}
	all, err := repo.NotificationTemplateRepo.List(ctx)
	if err != nil || len(all) != 1 || all[0].Name != template.Name {
		t.Fatalf("list templates = %+v, %v; want updated template", all, err)
	}
	config, err := domain.ParseDiscordTemplateConfig(all[0].Config)
	if err != nil || config.TitleURLTemplate != "{{ alert.target }}" || len(config.Fields) != 1 {
		t.Fatalf("persisted Discord config = %+v, %v", config, err)
	}

	if err := repo.NotificationTemplateRepo.Delete(ctx, template.ID); err != nil {
		t.Fatalf("delete template: %v", err)
	}
	got, err = repo.NotificationRepo.GetByID(ctx, notification.ID)
	if err != nil {
		t.Fatalf("get notification after template delete: %v", err)
	}
	if got.TemplateID != nil {
		t.Fatalf("template delete left notification.template_id = %v; want nil fallback", *got.TemplateID)
	}
}
