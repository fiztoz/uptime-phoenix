import { api } from "./client";

export type TemplateProvider = "discord" | "smtp" | "webhook" | "line";

export interface DiscordStatusColors {
  up: string;
  down: string;
  pending: string;
  maintenance: string;
  certificate: string;
}

export interface DiscordEmbedFieldTemplate {
  name_template: string;
  value_template: string;
  inline: boolean;
}

export interface DiscordButtonTemplate {
  label_template: string;
  url_template: string;
}

export interface DiscordTemplateConfig {
  title_url_template: string;
  footer_template: string;
  show_timestamp: boolean;
  colors: DiscordStatusColors;
  fields: DiscordEmbedFieldTemplate[];
  buttons: DiscordButtonTemplate[];
}

export interface SMTPTemplateConfig {
  format: "plain" | "html";
  html_body_template: string;
}

export interface NotificationTemplate {
  id: number;
  name: string;
  provider: TemplateProvider;
  title_template: string;
  body_template: string;
  discord_config?: DiscordTemplateConfig;
  smtp_config?: SMTPTemplateConfig;
  created_at: string;
  updated_at: string;
}

export interface CreateNotificationTemplateInput {
  name: string;
  provider: TemplateProvider;
  title_template: string;
  body_template: string;
  discord_config?: DiscordTemplateConfig;
  smtp_config?: SMTPTemplateConfig;
}

export interface UpdateNotificationTemplateInput {
  name: string;
  title_template: string;
  body_template: string;
  discord_config?: DiscordTemplateConfig;
  smtp_config?: SMTPTemplateConfig;
}

export const notificationTemplatesApi = {
  async list(): Promise<NotificationTemplate[]> {
    return api.get<NotificationTemplate[]>("/notification-templates");
  },

  async variables(): Promise<string[]> {
    const result = await api.get<{ variables: string[] }>(
      "/notification-templates/variables",
    );
    return result.variables;
  },

  async create(
    input: CreateNotificationTemplateInput,
  ): Promise<NotificationTemplate> {
    return api.post<NotificationTemplate>("/notification-templates", input);
  },

  async update(
    id: number,
    input: UpdateNotificationTemplateInput,
  ): Promise<NotificationTemplate> {
    return api.put<NotificationTemplate>(
      `/notification-templates/${id}`,
      input,
    );
  },

  async remove(id: number): Promise<void> {
    return api.del(`/notification-templates/${id}`);
  },
};
