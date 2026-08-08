<script lang="ts">
  import { tick, untrack } from "svelte";
  import { ArrowDown, ArrowUp, Braces, Plus, Trash2, X } from "@lucide/svelte";
  import { toast } from "svelte-sonner";
  import DiscordEmbedPreview from "$lib/components/DiscordEmbedPreview.svelte";
  import EmailPreview from "$lib/components/EmailPreview.svelte";
  import Select from "$lib/components/Select.svelte";
  import { modalFocus } from "$lib/actions/modalFocus";
  import { escapeEmailHTML } from "$lib/utils/email-preview";
  import {
    notificationTemplatesApi,
    type DiscordEmbedFieldTemplate,
    type DiscordStatusColors,
    type DiscordTemplateConfig,
    type NotificationTemplate,
    type SMTPTemplateConfig,
    type TemplateProvider,
  } from "$lib/api/notification-templates";
  import * as m from "$lib/paraglide/messages.js";

  interface Props {
    template?: NotificationTemplate;
    variables: string[];
    onSaved?: () => void;
    onClose?: () => void;
  }

  type PreviewScope = "monitor" | "group";
  type PreviewStatus = "UP" | "DOWN" | "PENDING" | "MAINTENANCE";
  type EmailPreviewView = "desktop" | "mobile" | "plain";
  type SMTPEditorPane = "html" | "plain";
  type ActiveTarget =
    | { kind: "title" | "body" | "smtp_html_body" | "title_url" | "footer" }
    | { kind: "field_name" | "field_value"; index: number };

  const providerOptions: Array<{ value: TemplateProvider; label: string }> = [
    { value: "discord", label: "Discord" },
    { value: "smtp", label: "SMTP Email" },
    { value: "webhook", label: "Webhook" },
    { value: "line", label: "LINE" },
  ];

  const defaultLayouts: Record<
    TemplateProvider,
    { title: string; body: string }
  > = {
    discord: {
      title: "{{ status.emoji }} {{ alert.name }} is {{ status }}",
      body: "{{ message }}\n{{ check_output }}",
    },
    smtp: {
      title: "Phoenix Alert: {{ alert.name }} is {{ status }}",
      body: "Name: {{ alert.name }}\nScope: {{ alert.scope }}\nType: {{ alert.type }}\nStatus: {{ status }}\nPrevious status: {{ previous_status }}\n\n{{ message }}\n{{ check_output }}\n\n{{ ack_url }}",
    },
    webhook: {
      title: "",
      body: '{\n  "event_kind": {{ json.event_kind }},\n  "scope": {{ json.alert.scope }},\n  "entity": {\n    "id": {{ json.alert.id }},\n    "name": {{ json.alert.name }},\n    "type": {{ json.alert.type }},\n    "target": {{ json.alert.target }}\n  },\n  "status": {{ json.status }},\n  "previous_status": {{ json.previous_status }},\n  "message": {{ json.message }},\n  "check_output": {{ json.check_output }},\n  "timestamp": {{ json.timestamp }}\n}',
    },
    line: {
      title: "",
      body: "{{ status.emoji }} {{ alert.name }} is {{ status }}\n{{ message }}\nTarget: {{ alert.target }}\n{{ ack_url }}",
    },
  };

  const defaultDiscordConfig: DiscordTemplateConfig = {
    title_url_template: "{{ alert.target }}",
    footer_template: "Phoenix • {{ alert.scope }}",
    show_timestamp: true,
    colors: {
      up: "#00FF00",
      down: "#FF0000",
      pending: "#FFA500",
      maintenance: "#808080",
      certificate: "#FFA500",
    },
    fields: [
      {
        name_template: "Name",
        value_template: "{{ alert.name }}",
        inline: true,
      },
      {
        name_template: "Type",
        value_template: "{{ alert.type }}",
        inline: true,
      },
      {
        name_template: "Target",
        value_template: "{{ alert.target }}",
        inline: false,
      },
      {
        name_template: "Group condition",
        value_template: "{{ group.condition }}",
        inline: true,
      },
      {
        name_template: "Threshold",
        value_template: "{{ group.threshold_display }}",
        inline: true,
      },
    ],
  };

  const defaultSMTPHTML = `<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="width:100%;background:#f3f4f6;padding:24px 12px;">
  <tr>
    <td align="center">
      <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="width:100%;max-width:600px;background:#ffffff;border:1px solid #e5e7eb;border-radius:12px;overflow:hidden;font-family:Arial,Helvetica,sans-serif;color:#1f2937;">
        <tr>
          <td style="background:#171717;padding:24px 28px;">
            <p style="margin:0 0 8px;color:#fb923c;font-size:12px;font-weight:700;letter-spacing:1.2px;">PHOENIX MONITORING</p>
            <h1 style="margin:0;color:#ffffff;font-size:24px;line-height:1.3;">{{ status.emoji }} {{ alert.name }} is {{ status }}</h1>
          </td>
        </tr>
        <tr>
          <td style="padding:28px;">
            <p style="margin:0 0 20px;font-size:16px;line-height:1.6;">{{ message }}</p>
            <table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="width:100%;border-collapse:collapse;margin-bottom:20px;">
              <tr><td style="padding:10px 0;border-bottom:1px solid #e5e7eb;color:#6b7280;font-size:13px;">Name</td><td align="right" style="padding:10px 0;border-bottom:1px solid #e5e7eb;font-size:13px;font-weight:600;">{{ alert.name }}</td></tr>
              <tr><td style="padding:10px 0;border-bottom:1px solid #e5e7eb;color:#6b7280;font-size:13px;">Scope</td><td align="right" style="padding:10px 0;border-bottom:1px solid #e5e7eb;font-size:13px;font-weight:600;">{{ alert.scope }}</td></tr>
              <tr><td style="padding:10px 0;border-bottom:1px solid #e5e7eb;color:#6b7280;font-size:13px;">Type</td><td align="right" style="padding:10px 0;border-bottom:1px solid #e5e7eb;font-size:13px;font-weight:600;">{{ alert.type }}</td></tr>
              <tr><td style="padding:10px 0;color:#6b7280;font-size:13px;">Status</td><td align="right" style="padding:10px 0;font-size:13px;font-weight:700;">{{ status }}</td></tr>
            </table>
            <div style="margin:0 0 22px;padding:14px 16px;border-radius:8px;background:#f3f4f6;color:#374151;font-family:Menlo,Consolas,monospace;font-size:12px;line-height:1.6;white-space:pre-wrap;">{{ check_output }}</div>
          </td>
        </tr>
        <tr>
          <td style="padding:16px 28px;background:#f9fafb;border-top:1px solid #e5e7eb;color:#6b7280;font-size:12px;line-height:1.5;">Sent by Phoenix at {{ timestamp }}</td>
        </tr>
      </table>
    </td>
  </tr>
</table>`;

  const defaultSMTPConfig: SMTPTemplateConfig = {
    format: "html",
    html_body_template: defaultSMTPHTML,
  };

  function cloneDiscordConfig(
    config: DiscordTemplateConfig | undefined,
  ): DiscordTemplateConfig {
    const source = config ?? defaultDiscordConfig;
    return {
      ...source,
      colors: { ...source.colors },
      fields: source.fields.map((field) => ({ ...field })),
    };
  }

  function cloneSMTPConfig(
    config: SMTPTemplateConfig | undefined,
    existing: boolean,
  ): SMTPTemplateConfig {
    if (!config && existing) return { format: "plain", html_body_template: "" };
    const source = config ?? defaultSMTPConfig;
    return { ...source };
  }

  let { template, variables, onSaved, onClose }: Props = $props();
  const initialTemplate = untrack(() => template);
  const initialProvider = initialTemplate?.provider ?? "discord";
  const initialSMTPConfig = cloneSMTPConfig(
    initialTemplate?.smtp_config,
    Boolean(initialTemplate),
  );

  let open = $state(true);
  let saving = $state(false);
  let provider = $state<TemplateProvider>(initialProvider);
  let name = $state(initialTemplate?.name ?? "");
  let titleTemplate = $state(
    initialTemplate?.title_template ?? defaultLayouts[initialProvider].title,
  );
  let bodyTemplate = $state(
    initialTemplate?.body_template ?? defaultLayouts[initialProvider].body,
  );
  let discordConfig = $state(
    cloneDiscordConfig(initialTemplate?.discord_config),
  );
  let smtpConfig = $state(initialSMTPConfig);
  let previewScope = $state<PreviewScope>("monitor");
  let previewStatus = $state<PreviewStatus>("DOWN");
  let emailPreviewView = $state<EmailPreviewView>(
    initialSMTPConfig.format === "html" ? "desktop" : "plain",
  );
  let smtpEditorPane = $state<SMTPEditorPane>(
    initialSMTPConfig.format === "html" ? "html" : "plain",
  );
  let activeTarget = $state<ActiveTarget>({ kind: "body" });
  let activeInput = $state<HTMLInputElement | HTMLTextAreaElement>();
  let jsonSafe = $state(initialProvider === "webhook");

  const hasTitle = $derived(provider === "discord" || provider === "smtp");
  const titleLimit = $derived(provider === "discord" ? 256 : 998);
  const bodyLimit = $derived(
    provider === "discord" ? 4096 : provider === "line" ? 5000 : 65536,
  );
  const previewTitle = $derived(renderPreview(titleTemplate));
  const previewBody = $derived(renderPreview(bodyTemplate));
  const previewHTMLBody = $derived(
    renderPreview(smtpConfig.html_body_template, true),
  );
  const previewAlertName = $derived(String(sampleValue("alert.name")));
  const previewFooter = $derived(renderPreview(discordConfig.footer_template));
  const renderedTitleURL = $derived(
    renderPreview(discordConfig.title_url_template),
  );
  const previewTitleURL = $derived(safePreviewURL(renderedTitleURL));
  const previewFields = $derived(
    discordConfig.fields
      .map((field) => ({
        ...field,
        name: renderPreview(field.name_template).trim(),
        value: renderPreview(field.value_template).trim(),
      }))
      .filter((field) => field.name && field.value),
  );
  const previewColor = $derived(
    discordConfig.colors[
      previewStatus.toLowerCase() as keyof DiscordStatusColors
    ],
  );
  const webhookJSONValid = $derived(
    provider !== "webhook" || isValidJSON(previewBody),
  );
  const variableSections = $derived([
    {
      label: m.notification_template_variables_alert(),
      items: variables.filter(
        (variable) =>
          !variable.startsWith("monitor.") &&
          !variable.startsWith("group.") &&
          !variable.startsWith("certificate."),
      ),
    },
    {
      label: m.notification_template_variables_monitor(),
      items: variables.filter((variable) => variable.startsWith("monitor.")),
    },
    {
      label: m.notification_template_variables_group(),
      items: variables.filter((variable) => variable.startsWith("group.")),
    },
    {
      label: m.notification_template_variables_certificate(),
      items: variables.filter((variable) =>
        variable.startsWith("certificate."),
      ),
    },
  ]);

  const inputClass =
    "w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring";
  const primaryBtn =
    "inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60";
  const ghostBtn =
    "inline-flex items-center gap-2 rounded-lg border border-border px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground";

  const colorOptions: Array<{
    key: keyof DiscordStatusColors;
    label: () => string;
  }> = [
    { key: "up", label: m.notification_template_discord_color_up },
    { key: "down", label: m.notification_template_discord_color_down },
    { key: "pending", label: m.notification_template_discord_color_pending },
    {
      key: "maintenance",
      label: m.notification_template_discord_color_maintenance,
    },
    {
      key: "certificate",
      label: m.notification_template_discord_color_certificate,
    },
  ];

  function handleProviderChange(value: string) {
    provider = value as TemplateProvider;
    jsonSafe = provider === "webhook";
    if (!initialTemplate) {
      titleTemplate = defaultLayouts[provider].title;
      bodyTemplate = defaultLayouts[provider].body;
      discordConfig = cloneDiscordConfig(undefined);
      smtpConfig = cloneSMTPConfig(undefined, false);
      smtpEditorPane = provider === "smtp" ? "html" : "plain";
      emailPreviewView = provider === "smtp" ? "desktop" : "plain";
    }
  }

  function setSMTPFormat(format: SMTPTemplateConfig["format"]) {
    smtpConfig.format = format;
    smtpEditorPane = format === "html" ? "html" : "plain";
    emailPreviewView = format === "html" ? "desktop" : "plain";
    activeTarget =
      format === "html" ? { kind: "smtp_html_body" } : { kind: "body" };
    activeInput = undefined;
  }

  function setSMTPEditorPane(pane: SMTPEditorPane) {
    smtpEditorPane = pane;
    activeTarget =
      pane === "html" ? { kind: "smtp_html_body" } : { kind: "body" };
    activeInput = undefined;
  }

  function sampleValue(variable: string): unknown {
    const isGroup = previewScope === "group";
    const isUp = previewStatus === "UP";
    const statusEmoji: Record<PreviewStatus, string> = {
      UP: "✅",
      DOWN: "❌",
      PENDING: "⚠️",
      MAINTENANCE: "🛠️",
    };
    const entityName = isGroup ? "Platform Services" : "Payments API";
    const entityType = isGroup ? "group" : "http";
    const entityTarget = isGroup ? "" : "https://api.example.com/health";
    const values: Record<string, unknown> = {
      "alert.scope": previewScope,
      "alert.id": isGroup ? 7 : 42,
      "alert.name": entityName,
      "alert.type": entityType,
      "alert.target": entityTarget,
      "monitor.id": isGroup ? 0 : 42,
      "monitor.name": entityName,
      "monitor.type": entityType,
      "monitor.target": entityTarget,
      "monitor.description": isGroup
        ? ""
        : "Public checkout and payment authorization API",
      "monitor.owner": isGroup ? "" : "Payments on-call",
      "group.id": isGroup ? 7 : 0,
      "group.name": isGroup ? "Platform Services" : "",
      "group.description": isGroup
        ? "Customer-facing platform dependencies"
        : "",
      "group.owner": isGroup ? "Platform SRE" : "",
      "group.condition": isGroup ? "threshold" : "",
      "group.threshold": isGroup ? 2 : 0,
      "group.threshold_is_percent": false,
      "group.threshold_display": isGroup ? "2" : "",
      status: previewStatus,
      "status.emoji": statusEmoji[previewStatus],
      previous_status: isUp ? "DOWN" : "UP",
      message: isGroup
        ? `Group "${entityName}" is ${previewStatus}`
        : `${entityName} is ${previewStatus}`,
      check_output: isGroup
        ? isUp
          ? "All child monitors are UP"
          : "2 child monitors are DOWN"
        : isUp
          ? "200 OK • 184 ms"
          : "Request failed with status code 504",
      duration: !isGroup && isUp ? "3m12s" : "",
      started_at:
        !isGroup && (previewStatus === "DOWN" || previewStatus === "UP")
          ? "2026-08-08T02:01:00Z"
          : "",
      "started_at.unix":
        !isGroup && (previewStatus === "DOWN" || previewStatus === "UP")
          ? 1786154460
          : undefined,
      timestamp: "2026-08-08T02:04:12Z",
      "timestamp.unix": 1786154652,
      event_kind: "status_change",
      ack_url:
        !isGroup && previewStatus === "DOWN"
          ? "https://status.example.com/ack/example"
          : "",
      tags: isGroup ? {} : { team: "payments", region: "ap-southeast-1" },
      "certificate.threshold": 7,
      "certificate.days_remaining": 6,
      "certificate.issuer": "Example Trust Services",
      "certificate.not_after": "2026-08-14T00:00:00Z",
    };
    return values[variable];
  }

  function renderPreview(source: string, escapeForHTML = false): string {
    return source.replace(
      /\{\{\s*([a-z][a-z0-9_.]*)\s*\}\}/g,
      (_match, placeholder: string) => {
        const asJSON = placeholder.startsWith("json.");
        const variable = asJSON ? placeholder.slice(5) : placeholder;
        const value = sampleValue(variable);
        if (asJSON) {
          const renderedJSON = JSON.stringify(value ?? null);
          return escapeForHTML ? escapeEmailHTML(renderedJSON) : renderedJSON;
        }
        let rendered: string;
        if (value === undefined || value === null) {
          rendered = "";
        } else if (
          variable === "tags" &&
          typeof value === "object" &&
          value !== null
        ) {
          rendered = Object.entries(value as Record<string, unknown>)
            .map(([key, item]) => `${key}=${String(item)}`)
            .join(", ");
        } else {
          rendered = String(value);
        }
        return escapeForHTML ? escapeEmailHTML(rendered) : rendered;
      },
    );
  }

  function safePreviewURL(value: string): string {
    try {
      const url = new URL(value);
      return url.protocol === "http:" || url.protocol === "https:"
        ? url.toString()
        : "";
    } catch {
      return "";
    }
  }

  function isValidJSON(value: string): boolean {
    try {
      JSON.parse(value);
      return true;
    } catch {
      return false;
    }
  }

  function validateSource(source: string): string | null {
    const allowed = new Set(variables);
    const matches = source.matchAll(/\{\{\s*([a-z][a-z0-9_.]*)\s*\}\}/g);
    for (const match of matches) {
      const raw = match[1];
      const variable = raw.startsWith("json.") ? raw.slice(5) : raw;
      if (!allowed.has(variable))
        return m.notification_template_unknown_variable({ variable: raw });
    }
    const withoutPlaceholders = source.replace(
      /\{\{\s*([a-z][a-z0-9_.]*)\s*\}\}/g,
      "",
    );
    if (withoutPlaceholders.includes("{{"))
      return m.notification_template_malformed_variable();
    return null;
  }

  function activate(event: FocusEvent, target: ActiveTarget) {
    activeInput = event.currentTarget as HTMLInputElement | HTMLTextAreaElement;
    activeTarget = target;
  }

  function activeValue(): string {
    switch (activeTarget.kind) {
      case "title":
        return titleTemplate;
      case "body":
        return bodyTemplate;
      case "smtp_html_body":
        return smtpConfig.html_body_template;
      case "title_url":
        return discordConfig.title_url_template;
      case "footer":
        return discordConfig.footer_template;
      case "field_name":
        return discordConfig.fields[activeTarget.index]?.name_template ?? "";
      case "field_value":
        return discordConfig.fields[activeTarget.index]?.value_template ?? "";
    }
  }

  function setActiveValue(value: string) {
    switch (activeTarget.kind) {
      case "title":
        titleTemplate = value;
        break;
      case "body":
        bodyTemplate = value;
        break;
      case "smtp_html_body":
        smtpConfig.html_body_template = value;
        break;
      case "title_url":
        discordConfig.title_url_template = value;
        break;
      case "footer":
        discordConfig.footer_template = value;
        break;
      case "field_name":
        if (discordConfig.fields[activeTarget.index])
          discordConfig.fields[activeTarget.index].name_template = value;
        break;
      case "field_value":
        if (discordConfig.fields[activeTarget.index])
          discordConfig.fields[activeTarget.index].value_template = value;
        break;
    }
  }

  async function insertVariable(variable: string) {
    const placeholder = `{{ ${jsonSafe ? `json.${variable}` : variable} }}`;
    const current = activeValue();
    const start = activeInput?.selectionStart ?? current.length;
    const end = activeInput?.selectionEnd ?? start;
    setActiveValue(current.slice(0, start) + placeholder + current.slice(end));
    await tick();
    activeInput?.focus();
    activeInput?.setSelectionRange(
      start + placeholder.length,
      start + placeholder.length,
    );
  }

  function addField() {
    if (discordConfig.fields.length >= 25) return;
    discordConfig.fields.push({
      name_template: "Label",
      value_template: "{{ alert.name }}",
      inline: false,
    });
  }

  function removeField(index: number) {
    discordConfig.fields.splice(index, 1);
    activeTarget = { kind: "body" };
    activeInput = undefined;
  }

  function moveField(index: number, direction: -1 | 1) {
    const target = index + direction;
    if (target < 0 || target >= discordConfig.fields.length) return;
    const fields = [...discordConfig.fields];
    [fields[index], fields[target]] = [fields[target], fields[index]];
    discordConfig.fields = fields;
  }

  function errorMessage(error: unknown): string {
    if (error && typeof error === "object" && "message" in error)
      return String((error as { message: unknown }).message);
    return m.monitor_form_save_failed();
  }

  async function handleSubmit() {
    if (!name.trim()) {
      toast.error(m.monitor_form_name_required());
      return;
    }
    if (provider !== "discord" && !bodyTemplate.trim()) {
      toast.error(m.notification_template_body_required());
      return;
    }
    if (
      provider === "discord" &&
      !titleTemplate.trim() &&
      !bodyTemplate.trim() &&
      discordConfig.fields.length === 0
    ) {
      toast.error(m.notification_template_discord_content_required());
      return;
    }
    if (
      provider === "smtp" &&
      smtpConfig.format === "html" &&
      !smtpConfig.html_body_template.trim()
    ) {
      toast.error(m.notification_template_email_html_required());
      smtpEditorPane = "html";
      return;
    }
    const sources = [titleTemplate, bodyTemplate];
    if (provider === "discord") {
      sources.push(
        discordConfig.title_url_template,
        discordConfig.footer_template,
      );
      for (const field of discordConfig.fields)
        sources.push(field.name_template, field.value_template);
    }
    if (provider === "smtp" && smtpConfig.format === "html")
      sources.push(smtpConfig.html_body_template);
    for (const source of sources) {
      const sourceError = validateSource(source);
      if (sourceError) {
        toast.error(sourceError);
        return;
      }
    }
    if (
      provider === "discord" &&
      Object.values(discordConfig.colors).some(
        (color) => !/^#[0-9a-fA-F]{6}$/.test(color),
      )
    ) {
      toast.error(m.notification_template_discord_color_invalid());
      return;
    }
    if (!webhookJSONValid) {
      toast.error(m.notification_template_invalid_json());
      return;
    }

    saving = true;
    try {
      const input = {
        name: name.trim(),
        title_template: hasTitle ? titleTemplate : "",
        body_template: bodyTemplate,
        discord_config:
          provider === "discord"
            ? cloneDiscordConfig(discordConfig)
            : undefined,
        smtp_config:
          provider === "smtp"
            ? smtpConfig.format === "plain"
              ? { format: "plain" as const, html_body_template: "" }
              : cloneSMTPConfig(smtpConfig, false)
            : undefined,
      };
      if (initialTemplate) {
        await notificationTemplatesApi.update(initialTemplate.id, input);
        toast.success(m.notification_template_updated());
      } else {
        await notificationTemplatesApi.create({ ...input, provider });
        toast.success(m.notification_template_created());
      }
      onSaved?.();
      close();
    } catch (error: unknown) {
      toast.error(errorMessage(error));
    } finally {
      saving = false;
    }
  }

  function close() {
    open = false;
    onClose?.();
  }
</script>

{#if open}
  <div
    class="fixed inset-0 z-50 flex items-end justify-center bg-black/60 p-0 backdrop-blur-sm sm:items-center sm:p-4"
  >
    <button
      type="button"
      tabindex="-1"
      class="absolute inset-0 cursor-default"
      onclick={close}
      aria-label={m.btn_close()}
    ></button>
    <div
      use:modalFocus={{ onClose: close, initialFocus: "#template-name" }}
      class="relative z-10 max-h-[94dvh] w-full max-w-7xl overflow-y-auto rounded-t-xl border border-border bg-card p-4 shadow-xl sm:rounded-xl sm:p-6"
      role="dialog"
      aria-modal="true"
      aria-labelledby="notification-template-form-title"
      aria-describedby="notification-template-form-description"
      tabindex="-1"
    >
      <div
        class="flex items-start justify-between gap-4 border-b border-border pb-4"
      >
        <div class="min-w-0">
          <h2
            id="notification-template-form-title"
            class="text-lg font-semibold tracking-tight"
          >
            {initialTemplate
              ? m.notification_template_edit_title()
              : m.notification_template_add_title()}
          </h2>
          <p
            id="notification-template-form-description"
            class="mt-1 max-w-2xl text-sm text-muted-foreground"
          >
            {m.notification_template_form_description()}
          </p>
        </div>
        <button
          type="button"
          onclick={close}
          class="grid h-11 w-11 shrink-0 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground sm:h-8 sm:w-8"
          aria-label={m.btn_close()}
        >
          <X class="h-5 w-5" />
        </button>
      </div>

      <div
        class="mt-6 grid gap-6 {provider === 'smtp'
          ? 'xl:grid-cols-2'
          : 'xl:grid-cols-[minmax(0,3fr)_minmax(22rem,2fr)]'}"
      >
        <div class="min-w-0 space-y-6">
          <div class="grid gap-4 sm:grid-cols-2">
            <div>
              <label for="template-name" class="text-sm font-medium"
                >{m.notification_template_name_label()}</label
              >
              <input
                id="template-name"
                bind:value={name}
                maxlength="255"
                class="{inputClass} mt-1"
                placeholder={m.notification_template_name_placeholder()}
              />
            </div>
            <div>
              <label for="template-provider" class="text-sm font-medium"
                >{m.notification_form_provider_type_label()}</label
              >
              <div class="mt-1">
                <Select
                  id="template-provider"
                  options={providerOptions}
                  value={provider}
                  onValueChange={handleProviderChange}
                  disabled={Boolean(initialTemplate)}
                  class="w-full"
                />
              </div>
            </div>
          </div>

          {#if hasTitle}
            <div>
              <div class="flex items-center justify-between gap-3">
                <label for="template-title" class="text-sm font-medium"
                  >{provider === "smtp"
                    ? m.notification_template_subject_label()
                    : m.notification_template_title_label()}</label
                >
                <span class="text-xs text-faint"
                  >{titleTemplate.length}/{titleLimit}</span
                >
              </div>
              <textarea
                id="template-title"
                bind:value={titleTemplate}
                onfocus={(event) => activate(event, { kind: "title" })}
                maxlength={titleLimit}
                rows="2"
                class="{inputClass} mt-1 resize-y font-mono text-xs leading-relaxed"
              ></textarea>
            </div>
          {/if}

          {#if provider === "smtp"}
            <section class="space-y-4" aria-labelledby="smtp-format-title">
              <div>
                <h3 id="smtp-format-title" class="text-sm font-medium">
                  {m.notification_template_email_format()}
                </h3>
                <p class="mt-1 text-xs leading-5 text-muted-foreground">
                  {m.notification_template_email_format_help()}
                </p>
              </div>
              <div
                class="grid grid-cols-2 rounded-lg bg-muted p-1"
                aria-label={m.notification_template_email_format()}
              >
                <button
                  type="button"
                  aria-pressed={smtpConfig.format === "plain"}
                  onclick={() => setSMTPFormat("plain")}
                  class="min-h-11 rounded-md px-3 py-2 text-sm font-medium transition-colors sm:min-h-0 {smtpConfig.format ===
                  'plain'
                    ? 'bg-card text-foreground shadow-sm'
                    : 'text-muted-foreground hover:text-foreground'}"
                >
                  {m.notification_template_email_format_plain()}
                </button>
                <button
                  type="button"
                  aria-pressed={smtpConfig.format === "html"}
                  onclick={() => setSMTPFormat("html")}
                  class="min-h-11 rounded-md px-3 py-2 text-sm font-medium transition-colors sm:min-h-0 {smtpConfig.format ===
                  'html'
                    ? 'bg-card text-foreground shadow-sm'
                    : 'text-muted-foreground hover:text-foreground'}"
                >
                  {m.notification_template_email_format_html()}
                </button>
              </div>

              {#if smtpConfig.format === "html"}
                <div
                  class="flex flex-wrap items-center gap-2 border-b border-border pb-3"
                  aria-label={m.notification_template_email_editor_label()}
                >
                  <button
                    type="button"
                    aria-pressed={smtpEditorPane === "html"}
                    onclick={() => setSMTPEditorPane("html")}
                    class="min-h-11 rounded-lg px-3 py-2 text-sm font-medium transition-colors sm:min-h-0 {smtpEditorPane ===
                    'html'
                      ? 'bg-primary/10 text-primary'
                      : 'text-muted-foreground hover:bg-accent hover:text-foreground'}"
                  >
                    {m.notification_template_email_html_source()}
                  </button>
                  <button
                    type="button"
                    aria-pressed={smtpEditorPane === "plain"}
                    onclick={() => setSMTPEditorPane("plain")}
                    class="min-h-11 rounded-lg px-3 py-2 text-sm font-medium transition-colors sm:min-h-0 {smtpEditorPane ===
                    'plain'
                      ? 'bg-primary/10 text-primary'
                      : 'text-muted-foreground hover:bg-accent hover:text-foreground'}"
                  >
                    {m.notification_template_email_plain_fallback()}
                  </button>
                </div>
              {/if}

              {#if smtpConfig.format === "html" && smtpEditorPane === "html"}
                <div>
                  <div class="flex items-center justify-between gap-3">
                    <label for="template-html-body" class="text-sm font-medium"
                      >{m.notification_template_email_html_source()}</label
                    >
                    <span class="text-xs text-faint"
                      >{smtpConfig.html_body_template.length}/{bodyLimit}</span
                    >
                  </div>
                  <textarea
                    id="template-html-body"
                    bind:value={smtpConfig.html_body_template}
                    onfocus={(event) =>
                      activate(event, { kind: "smtp_html_body" })}
                    maxlength={bodyLimit}
                    rows="18"
                    class="{inputClass} mt-1 resize-y font-mono text-xs leading-relaxed"
                    spellcheck="false"
                  ></textarea>
                  <p class="mt-1.5 text-xs leading-5 text-muted-foreground">
                    {m.notification_template_email_html_help()}
                  </p>
                </div>
              {:else}
                <div>
                  <div class="flex items-center justify-between gap-3">
                    <label for="template-body" class="text-sm font-medium"
                      >{smtpConfig.format === "html"
                        ? m.notification_template_email_plain_fallback()
                        : m.notification_template_email_plain_body()}</label
                    >
                    <span class="text-xs text-faint"
                      >{bodyTemplate.length}/{bodyLimit}</span
                    >
                  </div>
                  <textarea
                    id="template-body"
                    bind:value={bodyTemplate}
                    onfocus={(event) => activate(event, { kind: "body" })}
                    maxlength={bodyLimit}
                    rows="14"
                    class="{inputClass} mt-1 resize-y font-mono text-xs leading-relaxed"
                    spellcheck="false"
                  ></textarea>
                  {#if smtpConfig.format === "html"}<p
                      class="mt-1.5 text-xs leading-5 text-muted-foreground"
                    >
                      {m.notification_template_email_plain_fallback_help()}
                    </p>{/if}
                </div>
              {/if}
            </section>
          {:else}
            <div>
              <div class="flex items-center justify-between gap-3">
                <label for="template-body" class="text-sm font-medium"
                  >{provider === "webhook"
                    ? m.notification_template_payload_label()
                    : m.notification_template_body_label()}</label
                >
                <span class="text-xs text-faint"
                  >{bodyTemplate.length}/{bodyLimit}</span
                >
              </div>
              <textarea
                id="template-body"
                bind:value={bodyTemplate}
                onfocus={(event) => activate(event, { kind: "body" })}
                maxlength={bodyLimit}
                rows={provider === "discord" ? 6 : 12}
                class="{inputClass} mt-1 resize-y font-mono text-xs leading-relaxed"
                spellcheck="false"
              ></textarea>
              {#if provider === "webhook"}
                <p
                  class="mt-1.5 text-xs {webhookJSONValid
                    ? 'text-muted-foreground'
                    : 'text-danger'}"
                >
                  {webhookJSONValid
                    ? m.notification_template_json_valid()
                    : m.notification_template_invalid_json()}
                </p>
              {/if}
            </div>
          {/if}

          {#if provider === "discord"}
            <section class="space-y-4 border-t border-border pt-6">
              <div class="grid gap-4 sm:grid-cols-2">
                <div>
                  <label for="discord-title-url" class="text-sm font-medium"
                    >{m.notification_template_discord_title_url()}</label
                  >
                  <input
                    id="discord-title-url"
                    bind:value={discordConfig.title_url_template}
                    onfocus={(event) => activate(event, { kind: "title_url" })}
                    maxlength="2048"
                    class="{inputClass} mt-1 font-mono text-xs"
                    placeholder={"https://status.example.com/monitors/{{ monitor.id }}"}
                  />
                  <p class="mt-1.5 text-xs text-muted-foreground">
                    {m.notification_template_discord_title_url_help()}
                  </p>
                </div>
                <div>
                  <label for="discord-footer" class="text-sm font-medium"
                    >{m.notification_template_discord_footer()}</label
                  >
                  <input
                    id="discord-footer"
                    bind:value={discordConfig.footer_template}
                    onfocus={(event) => activate(event, { kind: "footer" })}
                    maxlength="2048"
                    class="{inputClass} mt-1 font-mono text-xs"
                  />
                  <label
                    class="mt-2 inline-flex items-center gap-2 text-xs text-muted-foreground"
                  >
                    <input
                      type="checkbox"
                      bind:checked={discordConfig.show_timestamp}
                      class="h-4 w-4 rounded border-border accent-primary"
                    />
                    {m.notification_template_discord_timestamp()}
                  </label>
                </div>
              </div>

              <div>
                <h3 class="text-sm font-semibold">
                  {m.notification_template_discord_colors_title()}
                </h3>
                <p class="mt-0.5 text-xs text-muted-foreground">
                  {m.notification_template_discord_colors_help()}
                </p>
                <div class="mt-3 grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
                  {#each colorOptions as option (option.key)}
                    <label
                      class="flex items-center gap-2 rounded-lg border border-border bg-surface px-3 py-2"
                    >
                      <input
                        type="color"
                        bind:value={discordConfig.colors[option.key]}
                        class="h-7 w-7 shrink-0 cursor-pointer rounded border-0 bg-transparent p-0"
                        aria-label={option.label()}
                      />
                      <span class="min-w-0 flex-1 text-xs font-medium"
                        >{option.label()}</span
                      >
                      <input
                        type="text"
                        bind:value={discordConfig.colors[option.key]}
                        maxlength="7"
                        class="w-20 bg-transparent text-right font-mono text-[11px] text-muted-foreground outline-none"
                        aria-label={`${option.label()} hex color`}
                      />
                    </label>
                  {/each}
                </div>
              </div>
            </section>

            <section class="space-y-3 border-t border-border pt-6">
              <div
                class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between"
              >
                <div>
                  <h3 class="text-sm font-semibold">
                    {m.notification_template_discord_fields_title()}
                  </h3>
                  <p
                    class="mt-0.5 max-w-2xl text-xs leading-5 text-muted-foreground"
                  >
                    {m.notification_template_discord_fields_help()}
                  </p>
                </div>
                <button
                  type="button"
                  onclick={addField}
                  disabled={discordConfig.fields.length >= 25}
                  class="{ghostBtn} shrink-0 disabled:opacity-50"
                >
                  <Plus class="h-4 w-4" />
                  {m.notification_template_discord_add_field()}
                </button>
              </div>
              {#if discordConfig.fields.length > 0}
                <div
                  class="overflow-hidden rounded-xl border border-border bg-surface/30"
                >
                  {#each discordConfig.fields as field, index (`discord-field-${index}`)}
                    <div
                      class="grid gap-3 border-b border-border p-4 last:border-0 sm:grid-cols-[minmax(0,1fr)_minmax(0,2fr)_auto] sm:items-start"
                    >
                      <div>
                        <label
                          for={`discord-field-name-${index}`}
                          class="text-xs font-medium text-muted-foreground"
                          >{m.notification_template_discord_field_name()}</label
                        >
                        <input
                          id={`discord-field-name-${index}`}
                          bind:value={field.name_template}
                          onfocus={(event) =>
                            activate(event, { kind: "field_name", index })}
                          maxlength="256"
                          class="{inputClass} mt-1 font-mono text-xs"
                        />
                      </div>
                      <div>
                        <label
                          for={`discord-field-value-${index}`}
                          class="text-xs font-medium text-muted-foreground"
                          >{m.notification_template_discord_field_value()}</label
                        >
                        <textarea
                          id={`discord-field-value-${index}`}
                          bind:value={field.value_template}
                          onfocus={(event) =>
                            activate(event, { kind: "field_value", index })}
                          maxlength="1024"
                          rows="2"
                          class="{inputClass} mt-1 resize-y font-mono text-xs leading-relaxed"
                        ></textarea>
                        <label
                          class="mt-2 inline-flex items-center gap-2 text-xs text-muted-foreground"
                        >
                          <input
                            type="checkbox"
                            bind:checked={field.inline}
                            class="h-4 w-4 rounded border-border accent-primary"
                          />
                          {m.notification_template_discord_field_inline()}
                        </label>
                      </div>
                      <div class="flex items-center justify-end gap-1 sm:pt-5">
                        <button
                          type="button"
                          onclick={() => moveField(index, -1)}
                          disabled={index === 0}
                          class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:opacity-30"
                          aria-label={m.notification_template_discord_move_up()}
                        >
                          <ArrowUp class="h-4 w-4" />
                        </button>
                        <button
                          type="button"
                          onclick={() => moveField(index, 1)}
                          disabled={index === discordConfig.fields.length - 1}
                          class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:opacity-30"
                          aria-label={m.notification_template_discord_move_down()}
                        >
                          <ArrowDown class="h-4 w-4" />
                        </button>
                        <button
                          type="button"
                          onclick={() => removeField(index)}
                          class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-danger/10 hover:text-danger"
                          aria-label={m.notification_template_discord_remove_field()}
                        >
                          <Trash2 class="h-4 w-4" />
                        </button>
                      </div>
                    </div>
                  {/each}
                </div>
              {/if}
            </section>
          {/if}

          <div class="rounded-xl border border-border bg-surface/50 p-4">
            <div
              class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between"
            >
              <div>
                <h3 class="text-sm font-semibold">
                  {m.notification_template_variables_title()}
                </h3>
                <p class="mt-0.5 text-xs text-muted-foreground">
                  {m.notification_template_variables_help()}
                </p>
              </div>
              {#if provider === "webhook"}
                <label
                  class="inline-flex items-center gap-2 text-xs text-muted-foreground"
                >
                  <input
                    type="checkbox"
                    bind:checked={jsonSafe}
                    class="h-4 w-4 rounded border-border accent-primary"
                  />
                  {m.notification_template_json_safe()}
                </label>
              {/if}
            </div>
            <div class="mt-4 max-h-52 space-y-4 overflow-y-auto pr-1">
              {#each variableSections as section (section.label)}
                {#if section.items.length > 0}
                  <div>
                    <p class="text-xs font-medium text-faint">
                      {section.label}
                    </p>
                    <div class="mt-2 flex flex-wrap gap-1.5">
                      {#each section.items as variable (variable)}
                        <button
                          type="button"
                          onclick={() => insertVariable(variable)}
                          class="min-h-11 rounded border border-border bg-card px-2 py-1 font-mono text-[11px] text-muted-foreground transition-colors hover:border-primary/30 hover:text-foreground sm:min-h-0"
                          title={m.notification_template_insert_variable({
                            variable,
                          })}
                        >
                          {`{{ ${jsonSafe ? `json.${variable}` : variable} }}`}
                        </button>
                      {/each}
                    </div>
                  </div>
                {/if}
              {/each}
            </div>
          </div>
        </div>

        <aside class="min-w-0 xl:sticky xl:top-0 xl:self-start">
          <div
            class="overflow-hidden rounded-xl border border-border bg-background"
          >
            <div class="border-b border-border px-4 py-3">
              <div class="flex items-center gap-2">
                <Braces class="h-4 w-4 text-primary" />
                <h3 class="text-sm font-semibold">
                  {m.notification_template_preview_title()}
                </h3>
                <span
                  class="ml-auto rounded bg-muted px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground"
                  >{providerOptions.find((item) => item.value === provider)
                    ?.label}</span
                >
              </div>
              <div class="mt-3 flex flex-col gap-2 sm:flex-row sm:items-center">
                <div class="grid flex-1 grid-cols-2 rounded-lg bg-muted p-1">
                  <button
                    type="button"
                    aria-pressed={previewScope === "monitor"}
                    onclick={() => (previewScope = "monitor")}
                    class="min-h-11 rounded-md px-2 py-1.5 text-xs font-medium transition-colors sm:min-h-0 {previewScope ===
                    'monitor'
                      ? 'bg-card text-foreground shadow-sm'
                      : 'text-muted-foreground hover:text-foreground'}"
                  >
                    {m.notification_template_preview_monitor()}
                  </button>
                  <button
                    type="button"
                    aria-pressed={previewScope === "group"}
                    onclick={() => (previewScope = "group")}
                    class="min-h-11 rounded-md px-2 py-1.5 text-xs font-medium transition-colors sm:min-h-0 {previewScope ===
                    'group'
                      ? 'bg-card text-foreground shadow-sm'
                      : 'text-muted-foreground hover:text-foreground'}"
                  >
                    {m.notification_template_preview_group()}
                  </button>
                </div>
                <label class="sr-only" for="template-preview-status"
                  >{m.notification_template_preview_status()}</label
                >
                <select
                  id="template-preview-status"
                  bind:value={previewStatus}
                  class="min-h-11 rounded-lg border border-border bg-surface px-3 py-2 text-xs focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring sm:min-h-0"
                >
                  <option value="DOWN">DOWN</option>
                  <option value="UP">UP</option>
                  <option value="PENDING">PENDING</option>
                  <option value="MAINTENANCE">MAINTENANCE</option>
                </select>
              </div>
              {#if provider === "smtp" && smtpConfig.format === "html"}
                <div
                  class="mt-2 grid grid-cols-3 rounded-lg bg-muted p-1"
                  aria-label={m.notification_template_email_preview_view()}
                >
                  <button
                    type="button"
                    aria-pressed={emailPreviewView === "desktop"}
                    onclick={() => (emailPreviewView = "desktop")}
                    class="min-h-11 rounded-md px-2 py-1.5 text-xs font-medium transition-colors sm:min-h-0 {emailPreviewView ===
                    'desktop'
                      ? 'bg-card text-foreground shadow-sm'
                      : 'text-muted-foreground hover:text-foreground'}"
                    >{m.notification_template_email_preview_desktop()}</button
                  >
                  <button
                    type="button"
                    aria-pressed={emailPreviewView === "mobile"}
                    onclick={() => (emailPreviewView = "mobile")}
                    class="min-h-11 rounded-md px-2 py-1.5 text-xs font-medium transition-colors sm:min-h-0 {emailPreviewView ===
                    'mobile'
                      ? 'bg-card text-foreground shadow-sm'
                      : 'text-muted-foreground hover:text-foreground'}"
                    >{m.notification_template_email_preview_mobile()}</button
                  >
                  <button
                    type="button"
                    aria-pressed={emailPreviewView === "plain"}
                    onclick={() => (emailPreviewView = "plain")}
                    class="min-h-11 rounded-md px-2 py-1.5 text-xs font-medium transition-colors sm:min-h-0 {emailPreviewView ===
                    'plain'
                      ? 'bg-card text-foreground shadow-sm'
                      : 'text-muted-foreground hover:text-foreground'}"
                    >{m.notification_template_email_preview_plain()}</button
                  >
                </div>
              {/if}
            </div>

            {#if provider === "discord"}
              <div class="p-3">
                <DiscordEmbedPreview
                  title={previewTitle}
                  description={previewBody}
                  titleUrl={previewTitleURL}
                  fields={previewFields}
                  footer={previewFooter}
                  showTimestamp={discordConfig.show_timestamp}
                  color={previewColor}
                />
              </div>
            {:else if provider === "smtp"}
              <div class="p-3">
                <EmailPreview
                  subject={previewTitle}
                  htmlBody={previewHTMLBody}
                  plainBody={previewBody}
                  view={smtpConfig.format === "plain"
                    ? "plain"
                    : emailPreviewView}
                  alertName={previewAlertName}
                  status={previewStatus}
                />
              </div>
            {:else}
              <div class="space-y-4 p-4">
                {#if hasTitle}
                  <div>
                    <p
                      class="text-[11px] font-semibold uppercase tracking-wider text-faint"
                    >
                      {m.notification_template_subject_label()}
                    </p>
                    <p class="mt-1 break-words text-sm font-semibold">
                      {previewTitle || "—"}
                    </p>
                  </div>
                {/if}
                <div>
                  <p
                    class="text-[11px] font-semibold uppercase tracking-wider text-faint"
                  >
                    {m.notification_template_rendered_message()}
                  </p>
                  <pre
                    class="mt-2 max-h-80 overflow-auto whitespace-pre-wrap break-words font-sans text-sm leading-6 text-muted-foreground">{previewBody ||
                      "—"}</pre>
                </div>
              </div>
            {/if}
          </div>
        </aside>
      </div>

      <div class="mt-6 flex justify-end gap-3 border-t border-border pt-4">
        <button
          type="button"
          onclick={close}
          class="{ghostBtn} min-h-11 sm:min-h-0">{m.btn_cancel()}</button
        >
        <button
          type="button"
          onclick={handleSubmit}
          disabled={saving}
          class="{primaryBtn} min-h-11 sm:min-h-0"
        >
          {saving
            ? m.btn_saving()
            : initialTemplate
              ? m.btn_update()
              : m.btn_create()}
        </button>
      </div>
    </div>
  </div>
{/if}
