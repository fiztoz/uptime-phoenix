<script lang="ts">
  /**
   * Sample-message preview for one notification channel. Renders the attached
   * template when one is selected; otherwise approximates the provider's
   * built-in layout. This is not a live send — Test still fires a real message.
   */
  import DiscordEmbedPreview from "$lib/components/DiscordEmbedPreview.svelte";
  import EmailPreview from "$lib/components/EmailPreview.svelte";
  import {
    DEFAULT_DISCORD_COLORS,
    builtinStatusEmoji,
    previewEntityName,
    previewSampleValue,
    renderNotificationPreview,
    safePreviewURL,
    type PreviewScope,
    type PreviewStatus,
  } from "$lib/notification-preview";
  import type { Notification } from "$lib/api/notifications";
  import type {
    DiscordTemplateConfig,
    NotificationTemplate,
    SMTPTemplateConfig,
  } from "$lib/api/notification-templates";
  import { Braces } from "@lucide/svelte";
  import * as m from "$lib/paraglide/messages.js";

  interface Props {
    notification: Notification;
    template: NotificationTemplate | null;
  }

  let { notification, template }: Props = $props();

  type EmailPreviewView = "desktop" | "mobile" | "plain";

  let previewScope = $state<PreviewScope>("monitor");
  let previewStatus = $state<PreviewStatus>("DOWN");
  let emailPreviewView = $state<EmailPreviewView>("desktop");

  const provider = $derived(notification.type);
  const usesTemplate = $derived(
    template !== null && template.provider === provider,
  );

  const discordConfig = $derived<DiscordTemplateConfig>(
    usesTemplate && template?.discord_config
      ? template.discord_config
      : {
          title_url_template: "",
          footer_template: "",
          show_timestamp: true,
          colors: { ...DEFAULT_DISCORD_COLORS },
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
          ],
          buttons: [],
        },
  );

  const smtpConfig = $derived<SMTPTemplateConfig>(
    usesTemplate && template?.smtp_config
      ? template.smtp_config
      : { format: "plain", html_body_template: "" },
  );

  function render(source: string, escapeForHTML = false): string {
    return renderNotificationPreview(
      source,
      previewScope,
      previewStatus,
      escapeForHTML,
    );
  }

  function str(variable: string): string {
    const value = previewSampleValue(previewScope, previewStatus, variable);
    return value === undefined || value === null ? "" : String(value);
  }

  const sampleName = $derived(previewEntityName(previewScope));
  const sampleMessage = $derived(str("message"));
  const sampleOutput = $derived(str("check_output"));
  const sampleAck = $derived(
    notification.include_ack_url ? str("ack_url") : "",
  );

  const previewTitle = $derived.by(() => {
    if (usesTemplate && template) {
      return render(template.title_template);
    }
    if (provider === "smtp") {
      return `Phoenix Alert: ${sampleName} is ${previewStatus}`;
    }
    if (provider === "discord") {
      return `${sampleName} is ${previewStatus}`;
    }
    return "";
  });

  const previewBody = $derived.by(() => {
    if (usesTemplate && template) {
      return render(template.body_template);
    }
    if (provider === "smtp") {
      const targetLine = str("alert.target")
        ? `Target: ${str("alert.target")}\n`
        : "";
      const duration = str("duration");
      return `Monitor: ${sampleName}\nType: ${str("alert.type")}\n${targetLine}Status: ${previewStatus}\nMessage: ${sampleMessage}\nTime: ${str("timestamp")}\nDuration: ${duration}\n\n${sampleOutput}`;
    }
    if (provider === "discord") {
      return [sampleMessage, sampleOutput].filter(Boolean).join("\n");
    }
    if (provider === "webhook") {
      return JSON.stringify(
        {
          title: `${sampleName} is ${previewStatus}`,
          message: sampleMessage,
          event_kind: "status_change",
          timestamp: str("timestamp"),
          monitor: {
            name: sampleName,
            type: str("alert.type"),
            target: str("alert.target"),
            id: previewSampleValue(previewScope, previewStatus, "alert.id"),
          },
          check_output: sampleOutput,
        },
        null,
        2,
      );
    }
    const emoji = builtinStatusEmoji(previewStatus);
    const lines = [
      `${emoji} ${sampleName} is ${previewStatus}`,
      sampleMessage,
      sampleOutput,
    ];
    if (sampleAck) lines.push(sampleAck);
    return lines.filter(Boolean).join("\n");
  });

  const previewHTMLBody = $derived(
    usesTemplate && smtpConfig.html_body_template
      ? render(smtpConfig.html_body_template, true)
      : "",
  );

  const previewTitleURL = $derived(
    provider === "discord"
      ? safePreviewURL(render(discordConfig.title_url_template))
      : "",
  );
  const previewFooter = $derived(
    provider === "discord" ? render(discordConfig.footer_template) : "",
  );
  const previewFields = $derived(
    provider === "discord"
      ? discordConfig.fields
          .map((field) => ({
            ...field,
            name: render(field.name_template).trim(),
            value: render(field.value_template).trim(),
          }))
          .filter((field) => field.name !== "" && field.value !== "")
      : [],
  );
  const previewColor = $derived(
    discordConfig.colors[
      previewStatus.toLowerCase() as keyof typeof discordConfig.colors
    ] ?? DEFAULT_DISCORD_COLORS.down,
  );

  const previewButtons = $derived.by(() => {
    if (provider !== "discord") return [];
    const buttons: { label: string; url: string }[] = [];
    if (sampleAck) {
      buttons.push({ label: "Acknowledge", url: sampleAck });
    }
    if (usesTemplate) {
      for (const button of discordConfig.buttons) {
        const label = render(button.label_template).trim();
        const url = safePreviewURL(render(button.url_template));
        if (label && url) buttons.push({ label, url });
      }
    }
    const raw = notification.config?.buttons;
    if (Array.isArray(raw)) {
      for (const item of raw) {
        if (!item || typeof item !== "object") continue;
        const row = item as Record<string, unknown>;
        const label =
          typeof row.label === "string"
            ? render(row.label).trim()
            : typeof row.label_template === "string"
              ? render(row.label_template).trim()
              : "";
        const urlSource =
          typeof row.url === "string"
            ? row.url
            : typeof row.url_template === "string"
              ? row.url_template
              : "";
        const url = safePreviewURL(render(urlSource));
        if (label && url) buttons.push({ label, url });
      }
    }
    const seen = new Set<string>();
    return buttons.filter((button) => {
      if (seen.has(button.url)) return false;
      seen.add(button.url);
      return true;
    });
  });
</script>

<section
  class="rounded-xl border border-border bg-card p-5"
  data-testid="notification-preview"
>
  <div
    class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between"
  >
    <div class="min-w-0">
      <div class="flex items-center gap-2">
        <Braces class="h-4 w-4 text-primary" />
        <h2 class="text-sm font-semibold tracking-tight">
          {m.notification_preview_title()}
        </h2>
      </div>
      <p class="mt-0.5 text-xs text-muted-foreground">
        {m.notification_preview_description()}
      </p>
      <p class="mt-1 text-xs text-faint">
        {#if usesTemplate && template}
          {m.notification_preview_template_layout({ name: template.name })}
        {:else if notification.template_id !== null && !usesTemplate}
          {m.notification_preview_template_missing()}
        {:else}
          {m.notification_preview_default_layout()}
        {/if}
      </p>
    </div>
    <div class="flex flex-col gap-2 sm:items-end">
      <div class="grid grid-cols-2 rounded-lg bg-muted p-1">
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
      <label class="sr-only" for="notification-preview-status"
        >{m.notification_template_preview_status()}</label
      >
      <select
        id="notification-preview-status"
        bind:value={previewStatus}
        class="min-h-11 rounded-lg border border-border bg-surface px-3 py-2 text-xs focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring sm:min-h-0"
      >
        <option value="DOWN">DOWN</option>
        <option value="UP">UP</option>
        <option value="PENDING">PENDING</option>
        <option value="MAINTENANCE">MAINTENANCE</option>
      </select>
    </div>
  </div>

  {#if provider === "smtp" && smtpConfig.format === "html"}
    <div
      class="mt-3 grid grid-cols-3 rounded-lg bg-muted p-1"
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

  <div class="mt-4">
    {#if provider === "discord"}
      <DiscordEmbedPreview
        title={previewTitle}
        description={previewBody}
        titleUrl={previewTitleURL}
        fields={previewFields}
        buttons={previewButtons}
        footer={previewFooter}
        showTimestamp={discordConfig.show_timestamp}
        color={previewColor}
      />
    {:else if provider === "smtp"}
      <EmailPreview
        subject={previewTitle}
        htmlBody={previewHTMLBody}
        plainBody={previewBody}
        view={smtpConfig.format === "plain" ? "plain" : emailPreviewView}
        alertName={sampleName}
        status={previewStatus}
      />
    {:else}
      <pre
        class="max-h-80 overflow-auto whitespace-pre-wrap break-words rounded-lg border border-border bg-muted/30 p-4 font-mono text-xs leading-6 text-muted-foreground">{previewBody ||
          "—"}</pre>
    {/if}
  </div>
</section>
