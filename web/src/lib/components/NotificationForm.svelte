<script lang="ts">
  import {
    notificationsApi,
    type CreateNotificationInput,
    type Notification,
  } from "$lib/api/notifications";
  import {
    notificationTypeConfig,
    notificationTypes,
  } from "$lib/notification-types";
  import { toast } from "svelte-sonner";
  import { Plus, Trash2, X } from "@lucide/svelte";
  import Select from "$lib/components/Select.svelte";
  import { modalFocus } from "$lib/actions/modalFocus";
  import { untrack } from "svelte";
  import type { NotificationTemplate } from "$lib/api/notification-templates";
  import * as m from "$lib/paraglide/messages.js";

  interface Props {
    notification?: Notification;
    templates?: NotificationTemplate[];
    onSaved?: (saved: Notification) => void;
    onClose?: () => void;
  }

  let { notification, templates = [], onSaved, onClose }: Props = $props();
  const initialNotification = untrack(() => notification);

  type DiscordLinkButton = { label: string; url: string };

  function parseDiscordButtons(
    config: Record<string, unknown>,
  ): DiscordLinkButton[] {
    const raw = config.buttons;
    if (!Array.isArray(raw)) return [];
    const buttons: DiscordLinkButton[] = [];
    for (const item of raw) {
      if (!item || typeof item !== "object") continue;
      const row = item as Record<string, unknown>;
      buttons.push({
        label:
          typeof row.label === "string"
            ? row.label
            : typeof row.label_template === "string"
              ? row.label_template
              : "",
        url:
          typeof row.url === "string"
            ? row.url
            : typeof row.url_template === "string"
              ? row.url_template
              : "",
      });
      if (buttons.length >= 5) break;
    }
    return buttons;
  }

  let open = $state(true);
  let loading = $state(false);
  let selectedType = $state(initialNotification?.type || "telegram");
  let selectedTemplateID = $state(
    initialNotification?.template_id
      ? String(initialNotification.template_id)
      : "default",
  );
  let formData = $state({
    name: initialNotification?.name || "",
    config: { ...(initialNotification?.config || {}) } as Record<
      string,
      unknown
    >,
    active: initialNotification?.active ?? true,
    is_default: initialNotification?.is_default ?? false,
    include_ack_url: initialNotification?.include_ack_url ?? false,
  });
  let discordButtons = $state<DiscordLinkButton[]>(
    parseDiscordButtons(
      (initialNotification?.config || {}) as Record<string, unknown>,
    ),
  );

  // Ensure config has defaults for selected type
  $effect(() => {
    const cfg = notificationTypeConfig[selectedType];
    if (cfg) {
      cfg.fields.forEach((f) => {
        if (formData.config[f.key] === undefined && f.default !== undefined) {
          formData.config[f.key] = f.default;
        }
      });
    }
  });

  function updateConfigField(key: string, value: unknown) {
    formData.config = { ...formData.config, [key]: value };
  }

  function handleTypeChange(value: string) {
    selectedType = value;
    selectedTemplateID = "default";
  }

  function getFieldValue(key: string): unknown {
    return formData.config[key] ?? "";
  }

  async function handleSubmit() {
    if (!formData.name.trim()) {
      toast.error(m.monitor_form_name_required());
      return;
    }
    if (!selectedType) {
      toast.error(m.monitor_form_type_required());
      return;
    }

    loading = true;
    try {
      const config = { ...formData.config };
      if (selectedType === "discord") {
        config.buttons = discordButtons
          .filter(
            (button) => button.label.trim() !== "" && button.url.trim() !== "",
          )
          .map((button) => ({
            label: button.label.trim(),
            url: button.url.trim(),
          }));
      } else {
        delete config.buttons;
      }

      const input: CreateNotificationInput = {
        name: formData.name.trim(),
        type: selectedType,
        config,
        active: formData.active,
        is_default: formData.is_default,
        include_ack_url: formData.include_ack_url,
        template_id:
          selectedTemplateID === "default" ? null : Number(selectedTemplateID),
      };

      let saved: Notification;
      if (notification) {
        saved = await notificationsApi.update(notification.id, input);
        toast.success(m.notification_form_updated_toast());
      } else {
        saved = await notificationsApi.create(input);
        toast.success(m.notification_form_created_toast());
      }
      onSaved?.(saved);
      close();
    } catch (err: any) {
      // err is ApiError from client
      toast.error(err?.message || m.monitor_form_save_failed());
    } finally {
      loading = false;
    }
  }

  function close() {
    open = false;
    onClose?.();
  }

  const currentFields = $derived(
    notificationTypeConfig[selectedType]?.fields || [],
  );
  const compatibleTemplates = $derived(
    templates.filter((item) => item.provider === selectedType),
  );
  const supportsTemplates = $derived(
    ["discord", "smtp", "webhook", "line"].includes(selectedType),
  );

  // Shared, token-consistent class strings.
  const inputClass =
    "w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring";
  const primaryBtn =
    "inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60";
  const ghostBtn =
    "inline-flex items-center gap-2 rounded-lg border border-border px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground";
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
      use:modalFocus={{ onClose: close, initialFocus: "#notif-name" }}
      class="relative z-10 max-h-[90dvh] w-full max-w-2xl overflow-y-auto rounded-t-xl border border-border bg-card p-4 shadow-xl sm:rounded-xl sm:p-6"
      role="dialog"
      aria-modal="true"
      aria-labelledby="notification-form-title"
      tabindex="-1"
    >
      <div
        class="flex items-center justify-between border-b border-border pb-4"
      >
        <h2
          id="notification-form-title"
          class="text-lg font-semibold tracking-tight"
        >
          {notification
            ? m.notification_form_edit_title()
            : m.notification_form_add_title()}
        </h2>
        <button
          type="button"
          onclick={close}
          class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
          aria-label={m.btn_close()}
        >
          <X class="h-5 w-5" />
        </button>
      </div>

      <div class="mt-6 space-y-4">
        <div>
          <label for="notif-name" class="text-sm font-medium"
            >{m.notification_form_name_label()}</label
          >
          <input
            id="notif-name"
            type="text"
            bind:value={formData.name}
            class="{inputClass} mt-1"
            placeholder={m.notification_form_name_placeholder()}
          />
        </div>

        <div>
          <label for="notif-type" class="text-sm font-medium"
            >{m.notification_form_provider_type_label()}</label
          >
          <div class="mt-1">
            <Select
              id="notif-type"
              options={notificationTypes.map((t) => ({
                value: t,
                label: notificationTypeConfig[t]?.label || t,
              }))}
              value={selectedType}
              onValueChange={handleTypeChange}
              disabled={!!notification}
              class="w-full disabled:opacity-60"
            />
          </div>
        </div>

        {#if supportsTemplates}
          <div>
            <label for="notif-template" class="text-sm font-medium"
              >{m.notification_form_template_label()}</label
            >
            <div class="mt-1">
              <Select
                id="notif-template"
                options={[
                  {
                    value: "default",
                    label: m.notification_form_template_default(),
                  },
                  ...compatibleTemplates.map((item) => ({
                    value: String(item.id),
                    label: item.name,
                  })),
                ]}
                value={selectedTemplateID}
                onValueChange={(value) => (selectedTemplateID = value)}
                class="w-full"
              />
            </div>
            <p class="mt-1 text-xs text-muted-foreground">
              {compatibleTemplates.length > 0
                ? m.notification_form_template_help()
                : m.notification_form_template_empty()}
            </p>
          </div>
        {/if}

        <div class="space-y-3">
          {#each currentFields as field (field.key)}
            <div>
              <label
                for="notif-{field.key}"
                class="flex items-center gap-1 text-sm font-medium"
              >
                {field.label}
                {#if field.required}<span class="text-danger">*</span>{/if}
              </label>

              {#if field.type === "select" && field.options}
                <div class="mt-1">
                  <Select
                    id="notif-{field.key}"
                    options={field.options.map((opt) => ({
                      value: opt.value,
                      label: opt.label,
                    }))}
                    value={String(getFieldValue(field.key))}
                    onValueChange={(v) => updateConfigField(field.key, v)}
                    class="w-full"
                  />
                </div>
              {:else if field.type === "textarea"}
                <textarea
                  id="notif-{field.key}"
                  value={String(getFieldValue(field.key))}
                  oninput={(e) =>
                    updateConfigField(
                      field.key,
                      (e.target as HTMLTextAreaElement).value,
                    )}
                  class="{inputClass} mt-1"
                  rows="3"
                  placeholder={field.placeholder}></textarea>
              {:else}
                <input
                  id="notif-{field.key}"
                  type={field.type === "password"
                    ? "password"
                    : field.type === "number"
                      ? "number"
                      : "text"}
                  value={String(getFieldValue(field.key))}
                  oninput={(e) => {
                    const val =
                      field.type === "number"
                        ? Number((e.target as HTMLInputElement).value)
                        : (e.target as HTMLInputElement).value;
                    updateConfigField(field.key, val);
                  }}
                  class="{inputClass} mt-1"
                  placeholder={field.placeholder}
                />
              {/if}

              {#if field.help}
                <p class="mt-1 text-xs text-muted-foreground">{field.help}</p>
              {/if}
            </div>
          {/each}
        </div>

        {#if selectedType === "discord"}
          <div class="space-y-3 border-t border-border pt-4">
            <div class="flex items-start justify-between gap-3">
              <div>
                <p class="text-sm font-medium">
                  {m.notification_form_discord_buttons_title()}
                </p>
                <p class="mt-1 text-xs text-muted-foreground">
                  {m.notification_form_discord_buttons_help()}
                </p>
              </div>
              <button
                type="button"
                onclick={() => {
                  if (discordButtons.length >= 5) return;
                  discordButtons = [...discordButtons, { label: "", url: "" }];
                }}
                disabled={discordButtons.length >= 5}
                class="{ghostBtn} shrink-0 disabled:opacity-50"
              >
                <Plus class="h-4 w-4" />
                {m.notification_form_discord_add_button()}
              </button>
            </div>
            {#each discordButtons as button, index (`discord-button-${index}`)}
              <div class="grid gap-2 sm:grid-cols-[1fr_minmax(0,1.5fr)_auto]">
                <input
                  type="text"
                  value={button.label}
                  oninput={(e) => {
                    discordButtons[index].label = (
                      e.target as HTMLInputElement
                    ).value;
                    discordButtons = discordButtons;
                  }}
                  class={inputClass}
                  placeholder={m.notification_form_discord_button_label()}
                  maxlength="80"
                  aria-label={m.notification_form_discord_button_label()}
                />
                <input
                  type="text"
                  value={button.url}
                  oninput={(e) => {
                    discordButtons[index].url = (
                      e.target as HTMLInputElement
                    ).value;
                    discordButtons = discordButtons;
                  }}
                  class={inputClass}
                  placeholder="https://example.com"
                  maxlength="512"
                  aria-label={m.notification_form_discord_button_url()}
                />
                <button
                  type="button"
                  onclick={() => {
                    discordButtons = discordButtons.filter(
                      (_, i) => i !== index,
                    );
                  }}
                  class="grid h-10 w-10 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-danger/10 hover:text-danger"
                  aria-label={m.notification_form_discord_remove_button()}
                >
                  <Trash2 class="h-4 w-4" />
                </button>
              </div>
            {/each}
          </div>
        {/if}

        <div class="flex flex-col gap-2 pt-2">
          <div class="flex items-center gap-2">
            <input
              type="checkbox"
              bind:checked={formData.active}
              id="active"
              class="h-4 w-4 rounded border-border accent-primary"
            />
            <label for="active" class="text-sm text-muted-foreground"
              >{m.notification_form_active_label()}</label
            >
          </div>
          <div class="flex items-start gap-2">
            <input
              type="checkbox"
              bind:checked={formData.is_default}
              id="is-default"
              class="mt-0.5 h-4 w-4 rounded border-border accent-primary"
            />
            <label for="is-default" class="text-sm text-muted-foreground">
              <span class="block">{m.notification_form_default_label()}</span>
              <span class="block text-xs text-faint">
                {m.notification_form_default_help()}
              </span>
            </label>
          </div>
          <div class="flex items-start gap-2">
            <input
              type="checkbox"
              bind:checked={formData.include_ack_url}
              id="include-ack-url"
              class="mt-0.5 h-4 w-4 rounded border-border accent-primary"
            />
            <label for="include-ack-url" class="text-sm text-muted-foreground">
              <span class="block"
                >{m.notification_form_include_ack_label()}</span
              >
              <span class="block text-xs text-faint">
                {m.notification_form_include_ack_help()}
              </span>
            </label>
          </div>
        </div>
      </div>

      <div class="mt-8 flex justify-end gap-3 border-t border-border pt-4">
        <button type="button" onclick={close} class={ghostBtn}
          >{m.btn_cancel()}</button
        >
        <button
          type="button"
          onclick={handleSubmit}
          disabled={loading}
          class={primaryBtn}
        >
          {loading
            ? m.btn_saving()
            : notification
              ? m.btn_update()
              : m.btn_create()}
        </button>
      </div>
    </div>
  </div>
{/if}
