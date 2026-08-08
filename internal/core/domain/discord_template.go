package domain

import "fmt"

// DiscordStatusColors defines the embed accent used for each Phoenix event state.
type DiscordStatusColors struct {
	Up          string
	Down        string
	Pending     string
	Maintenance string
	Certificate string
}

// DiscordEmbedFieldTemplate is one configurable field in a Discord embed.
type DiscordEmbedFieldTemplate struct {
	NameTemplate  string
	ValueTemplate string
	Inline        bool
}

// DiscordTemplateConfig contains the Discord-only portions of a reusable
// notification template. Title and description remain on NotificationTemplate
// because SMTP, Webhook, and LINE share those fields.
type DiscordTemplateConfig struct {
	TitleURLTemplate string
	FooterTemplate   string
	ShowTimestamp    bool
	Colors           DiscordStatusColors
	Fields           []DiscordEmbedFieldTemplate
}

// DefaultDiscordTemplateConfig returns the structured layout used by new and
// legacy Discord templates when no Discord-specific configuration is stored.
func DefaultDiscordTemplateConfig() DiscordTemplateConfig {
	return DiscordTemplateConfig{
		ShowTimestamp: true,
		Colors: DiscordStatusColors{
			Up:          "#00FF00",
			Down:        "#FF0000",
			Pending:     "#FFA500",
			Maintenance: "#808080",
			Certificate: "#FFA500",
		},
		Fields: []DiscordEmbedFieldTemplate{
			{NameTemplate: "Name", ValueTemplate: "{{ alert.name }}", Inline: true},
			{NameTemplate: "Type", ValueTemplate: "{{ alert.type }}", Inline: true},
			{NameTemplate: "Target", ValueTemplate: "{{ alert.target }}"},
			{NameTemplate: "Group condition", ValueTemplate: "{{ group.condition }}"},
		},
	}
}

// ParseDiscordTemplateConfig converts the storage-safe map representation into
// a typed Discord configuration. Missing keys inherit stable defaults so
// templates created before structured embeds continue to render correctly.
func ParseDiscordTemplateConfig(values map[string]any) (DiscordTemplateConfig, error) {
	config := DefaultDiscordTemplateConfig()
	if len(values) == 0 {
		return config, nil
	}

	if value, ok := values["title_url_template"]; ok {
		text, ok := value.(string)
		if !ok {
			return config, fmt.Errorf("title_url_template must be a string")
		}
		config.TitleURLTemplate = text
	}
	if value, ok := values["footer_template"]; ok {
		text, ok := value.(string)
		if !ok {
			return config, fmt.Errorf("footer_template must be a string")
		}
		config.FooterTemplate = text
	}
	if value, ok := values["show_timestamp"]; ok {
		show, ok := value.(bool)
		if !ok {
			return config, fmt.Errorf("show_timestamp must be a boolean")
		}
		config.ShowTimestamp = show
	}

	if value, ok := values["colors"]; ok {
		colors, ok := value.(map[string]any)
		if !ok {
			return config, fmt.Errorf("colors must be an object")
		}
		for key, target := range map[string]*string{
			"up": &config.Colors.Up, "down": &config.Colors.Down,
			"pending": &config.Colors.Pending, "maintenance": &config.Colors.Maintenance,
			"certificate": &config.Colors.Certificate,
		} {
			if raw, exists := colors[key]; exists {
				text, valid := raw.(string)
				if !valid {
					return config, fmt.Errorf("colors.%s must be a string", key)
				}
				*target = text
			}
		}
	}

	if value, ok := values["fields"]; ok {
		items, ok := value.([]any)
		if !ok {
			return config, fmt.Errorf("fields must be an array")
		}
		config.Fields = make([]DiscordEmbedFieldTemplate, 0, len(items))
		for i, item := range items {
			field, ok := item.(map[string]any)
			if !ok {
				return config, fmt.Errorf("fields[%d] must be an object", i)
			}
			name, nameOK := field["name_template"].(string)
			valueTemplate, valueOK := field["value_template"].(string)
			if !nameOK || !valueOK {
				return config, fmt.Errorf("fields[%d] name_template and value_template must be strings", i)
			}
			inline := false
			if raw, exists := field["inline"]; exists {
				var valid bool
				inline, valid = raw.(bool)
				if !valid {
					return config, fmt.Errorf("fields[%d].inline must be a boolean", i)
				}
			}
			config.Fields = append(config.Fields, DiscordEmbedFieldTemplate{
				NameTemplate: name, ValueTemplate: valueTemplate, Inline: inline,
			})
		}
	}

	return config, nil
}

// DiscordTemplateConfigMap converts typed Discord settings into the JSON-safe
// map stored at the repository boundary.
func DiscordTemplateConfigMap(config DiscordTemplateConfig) map[string]any {
	fields := make([]any, len(config.Fields))
	for i, field := range config.Fields {
		fields[i] = map[string]any{
			"name_template":  field.NameTemplate,
			"value_template": field.ValueTemplate,
			"inline":         field.Inline,
		}
	}
	return map[string]any{
		"title_url_template": config.TitleURLTemplate,
		"footer_template":    config.FooterTemplate,
		"show_timestamp":     config.ShowTimestamp,
		"colors": map[string]any{
			"up": config.Colors.Up, "down": config.Colors.Down,
			"pending": config.Colors.Pending, "maintenance": config.Colors.Maintenance,
			"certificate": config.Colors.Certificate,
		},
		"fields": fields,
	}
}
