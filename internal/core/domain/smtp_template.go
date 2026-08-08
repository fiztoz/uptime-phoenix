package domain

import "fmt"

const (
	// SMTPTemplateFormatPlain sends only the required plain-text body.
	SMTPTemplateFormatPlain = "plain"
	// SMTPTemplateFormatHTML adds an HTML multipart alternative.
	SMTPTemplateFormatHTML = "html"
)

// SMTPTemplateConfig contains SMTP-only reusable-template settings.
// BodyTemplate remains the required plain-text body and, in HTML mode, is the
// multipart fallback for clients that cannot render HTML.
type SMTPTemplateConfig struct {
	Format           string
	HTMLBodyTemplate string
}

// DefaultSMTPTemplateConfig returns the legacy-compatible plain-text layout.
func DefaultSMTPTemplateConfig() SMTPTemplateConfig {
	return SMTPTemplateConfig{Format: SMTPTemplateFormatPlain}
}

// ParseSMTPTemplateConfig converts the storage-safe map representation into a
// typed SMTP configuration. Missing configuration defaults to plain text so
// templates created before HTML email support keep their existing behavior.
func ParseSMTPTemplateConfig(values map[string]any) (SMTPTemplateConfig, error) {
	config := DefaultSMTPTemplateConfig()
	if len(values) == 0 {
		return config, nil
	}

	if value, ok := values["format"]; ok {
		format, valid := value.(string)
		if !valid {
			return config, fmt.Errorf("format must be a string")
		}
		config.Format = format
	}
	if value, ok := values["html_body_template"]; ok {
		body, valid := value.(string)
		if !valid {
			return config, fmt.Errorf("html_body_template must be a string")
		}
		config.HTMLBodyTemplate = body
	}

	switch config.Format {
	case SMTPTemplateFormatPlain, SMTPTemplateFormatHTML:
		return config, nil
	default:
		return config, fmt.Errorf("format must be plain or html")
	}
}

// SMTPTemplateConfigMap converts typed SMTP settings into the JSON-safe map
// stored at the repository boundary.
func SMTPTemplateConfigMap(config SMTPTemplateConfig) map[string]any {
	return map[string]any{
		"format":             config.Format,
		"html_body_template": config.HTMLBodyTemplate,
	}
}
