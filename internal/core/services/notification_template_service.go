package services

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var discordColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

const (
	maxNotificationTemplateName  = 255
	maxNotificationTemplateTitle = 1000
	maxNotificationTemplateBody  = 64 * 1024
)

// NotificationTemplateService manages reusable provider-specific message layouts.
type NotificationTemplateService struct {
	repo ports.NotificationTemplateRepository
}

// NewNotificationTemplateService creates a notification-template service.
func NewNotificationTemplateService(repo ports.NotificationTemplateRepository) *NotificationTemplateService {
	return &NotificationTemplateService{repo: repo}
}

// Create validates and persists a notification template.
func (s *NotificationTemplateService) Create(ctx context.Context, template *domain.NotificationTemplate) error {
	if err := validateNotificationTemplate(template); err != nil {
		return err
	}
	return s.repo.Create(ctx, template)
}

// GetByID retrieves a notification template by ID.
func (s *NotificationTemplateService) GetByID(ctx context.Context, id int64) (*domain.NotificationTemplate, error) {
	return s.repo.GetByID(ctx, id)
}

// List retrieves all notification templates in the install.
func (s *NotificationTemplateService) List(ctx context.Context) ([]*domain.NotificationTemplate, error) {
	return s.repo.List(ctx)
}

// Update validates and persists a notification template.
func (s *NotificationTemplateService) Update(ctx context.Context, template *domain.NotificationTemplate) error {
	if err := validateNotificationTemplate(template); err != nil {
		return err
	}
	return s.repo.Update(ctx, template)
}

// Delete removes a notification template. Notifications that selected it fall
// back to their provider's built-in layout through the database foreign key.
func (s *NotificationTemplateService) Delete(ctx context.Context, id int64) error {
	return s.repo.Delete(ctx, id)
}

func validateNotificationTemplate(template *domain.NotificationTemplate) error {
	if template == nil {
		return fmt.Errorf("notification template: %w: template is required", domain.ErrValidation)
	}
	template.Name = strings.TrimSpace(template.Name)
	template.Provider = strings.TrimSpace(template.Provider)
	if template.Name == "" {
		return fmt.Errorf("notification template: %w: name is required", domain.ErrValidation)
	}
	if len(template.Name) > maxNotificationTemplateName {
		return fmt.Errorf("notification template: %w: name is too long", domain.ErrValidation)
	}
	if !domain.NotificationTemplateProviderSupported(template.Provider) {
		return fmt.Errorf("notification template: %w: provider must be discord, smtp, webhook, or line", domain.ErrValidation)
	}
	if len(template.TitleTemplate) > maxNotificationTemplateTitle {
		return fmt.Errorf("notification template: %w: title template is too long", domain.ErrValidation)
	}
	if template.Provider != "discord" && strings.TrimSpace(template.BodyTemplate) == "" {
		return fmt.Errorf("notification template: %w: body template is required", domain.ErrValidation)
	}
	if len(template.BodyTemplate) > maxNotificationTemplateBody {
		return fmt.Errorf("notification template: %w: body template is too long", domain.ErrValidation)
	}
	switch template.Provider {
	case "discord":
		if utf8.RuneCountInString(template.TitleTemplate) > 256 {
			return fmt.Errorf("notification template: %w: Discord title exceeds 256 characters", domain.ErrValidation)
		}
		if utf8.RuneCountInString(template.BodyTemplate) > 4096 {
			return fmt.Errorf("notification template: %w: Discord body exceeds 4096 characters", domain.ErrValidation)
		}
		if err := validateDiscordTemplateConfig(template); err != nil {
			return err
		}
	case "smtp":
		if utf8.RuneCountInString(template.TitleTemplate) > 998 {
			return fmt.Errorf("notification template: %w: SMTP subject exceeds 998 characters", domain.ErrValidation)
		}
		if err := validateSMTPTemplateConfig(template); err != nil {
			return err
		}
	case "line":
		if utf8.RuneCountInString(template.BodyTemplate) > 5000 {
			return fmt.Errorf("notification template: %w: LINE body exceeds 5000 characters", domain.ErrValidation)
		}
	}
	if template.Provider != "discord" && template.Provider != "smtp" && len(template.Config) > 0 {
		return fmt.Errorf("notification template: %w: structured settings are only supported by Discord and SMTP", domain.ErrValidation)
	}
	if err := domain.ValidateNotificationTemplateText(template.TitleTemplate); err != nil {
		return fmt.Errorf("notification template: %w: invalid title template: %v", domain.ErrValidation, err)
	}
	if err := domain.ValidateNotificationTemplateText(template.BodyTemplate); err != nil {
		return fmt.Errorf("notification template: %w: invalid body template: %v", domain.ErrValidation, err)
	}
	return nil
}

func validateSMTPTemplateConfig(template *domain.NotificationTemplate) error {
	config, err := domain.ParseSMTPTemplateConfig(template.Config)
	if err != nil {
		return fmt.Errorf("notification template: %w: invalid SMTP configuration: %v", domain.ErrValidation, err)
	}
	if config.Format == domain.SMTPTemplateFormatPlain {
		if strings.TrimSpace(config.HTMLBodyTemplate) != "" {
			return fmt.Errorf("notification template: %w: SMTP HTML body requires html format", domain.ErrValidation)
		}
		return nil
	}
	if strings.TrimSpace(config.HTMLBodyTemplate) == "" {
		return fmt.Errorf("notification template: %w: SMTP HTML body is required for html format", domain.ErrValidation)
	}
	if len(config.HTMLBodyTemplate) > maxNotificationTemplateBody {
		return fmt.Errorf("notification template: %w: SMTP HTML body template is too long", domain.ErrValidation)
	}
	if _, err := domain.RenderNotificationHTMLTemplate(config.HTMLBodyTemplate, domain.AlertContext{}, time.Unix(0, 0).UTC()); err != nil {
		return fmt.Errorf("notification template: %w: invalid SMTP HTML body template: %v", domain.ErrValidation, err)
	}
	return nil
}

func validateDiscordTemplateConfig(template *domain.NotificationTemplate) error {
	config, err := domain.ParseDiscordTemplateConfig(template.Config)
	if err != nil {
		return fmt.Errorf("notification template: %w: invalid Discord configuration: %v", domain.ErrValidation, err)
	}
	colors := map[string]string{
		"up": config.Colors.Up, "down": config.Colors.Down, "pending": config.Colors.Pending,
		"maintenance": config.Colors.Maintenance, "certificate": config.Colors.Certificate,
	}
	for name, color := range colors {
		if !discordColorPattern.MatchString(color) {
			return fmt.Errorf("notification template: %w: Discord %s color must be a six-digit hex color", domain.ErrValidation, name)
		}
	}
	if len(config.Fields) > 25 {
		return fmt.Errorf("notification template: %w: Discord embeds support at most 25 fields", domain.ErrValidation)
	}
	if utf8.RuneCountInString(config.TitleURLTemplate) > 2048 {
		return fmt.Errorf("notification template: %w: Discord title URL exceeds 2048 characters", domain.ErrValidation)
	}
	if utf8.RuneCountInString(config.FooterTemplate) > 2048 {
		return fmt.Errorf("notification template: %w: Discord footer exceeds 2048 characters", domain.ErrValidation)
	}
	for label, source := range map[string]string{
		"title URL": config.TitleURLTemplate,
		"footer":    config.FooterTemplate,
	} {
		if err := domain.ValidateNotificationTemplateText(source); err != nil {
			return fmt.Errorf("notification template: %w: invalid Discord %s template: %v", domain.ErrValidation, label, err)
		}
	}
	for i, field := range config.Fields {
		if strings.TrimSpace(field.NameTemplate) == "" || strings.TrimSpace(field.ValueTemplate) == "" {
			return fmt.Errorf("notification template: %w: Discord field %d requires a name and value", domain.ErrValidation, i+1)
		}
		if utf8.RuneCountInString(field.NameTemplate) > 256 {
			return fmt.Errorf("notification template: %w: Discord field %d name exceeds 256 characters", domain.ErrValidation, i+1)
		}
		if utf8.RuneCountInString(field.ValueTemplate) > 1024 {
			return fmt.Errorf("notification template: %w: Discord field %d value exceeds 1024 characters", domain.ErrValidation, i+1)
		}
		if err := domain.ValidateNotificationTemplateText(field.NameTemplate); err != nil {
			return fmt.Errorf("notification template: %w: invalid Discord field %d name: %v", domain.ErrValidation, i+1, err)
		}
		if err := domain.ValidateNotificationTemplateText(field.ValueTemplate); err != nil {
			return fmt.Errorf("notification template: %w: invalid Discord field %d value: %v", domain.ErrValidation, i+1, err)
		}
	}
	if strings.TrimSpace(template.TitleTemplate) == "" && strings.TrimSpace(template.BodyTemplate) == "" && len(config.Fields) == 0 {
		return fmt.Errorf("notification template: %w: Discord embed requires a title, body, or field", domain.ErrValidation)
	}
	return nil
}
