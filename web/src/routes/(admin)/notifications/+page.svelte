<script lang="ts">
  import { notificationsApi, type Notification } from "$lib/api/notifications";
  import { notificationTypeConfig } from "$lib/notification-types";
  import NotificationForm from "$lib/components/NotificationForm.svelte";
  import NotificationTemplateForm from "$lib/components/NotificationTemplateForm.svelte";
  import {
    notificationTemplatesApi,
    type NotificationTemplate,
  } from "$lib/api/notification-templates";
  import Skeleton from "$lib/components/Skeleton.svelte";
  import EmptyState from "$lib/components/EmptyState.svelte";
  import { auth } from "$lib/stores/auth.svelte.ts";
  import {
    Plus,
    Edit2,
    Trash2,
    Send,
    Bell,
    CheckCircle,
    XCircle,
    Braces,
    FileText,
  } from "@lucide/svelte";
  import { confirmAction } from "$lib/stores/confirm.svelte";
  import { toast } from "svelte-sonner";
  import * as m from "$lib/paraglide/messages.js";

  let notifications = $state<Notification[]>([]);
  let templates = $state<NotificationTemplate[]>([]);
  let templateVariables = $state<string[]>([]);
  let loading = $state(true);
  let loadError = $state<string | null>(null);
  let showForm = $state(false);
  let editingNotification = $state<Notification | null>(null);
  let testingId = $state<number | null>(null);
  let showTemplateForm = $state(false);
  let editingTemplate = $state<NotificationTemplate | null>(null);

  const isUserAdmin = $derived(auth.user?.is_admin ?? false);
  const canManage = $derived(
    isUserAdmin || (auth.user?.can_manage_notifications ?? false),
  );

  async function load() {
    loading = true;
    loadError = null;
    try {
      if (canManage) {
        [notifications, templates, templateVariables] = await Promise.all([
          notificationsApi.list(),
          notificationTemplatesApi.list(),
          notificationTemplatesApi.variables(),
        ]);
      } else {
        notifications = await notificationsApi.list();
        templates = [];
        templateVariables = [];
      }
    } catch (error: unknown) {
      loadError =
        error && typeof error === "object" && "message" in error
          ? String((error as { message: string }).message)
          : m.notifications_page_load_failed();
      toast.error(loadError);
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    load();
  });

  function typeLabel(type: string): string {
    return notificationTypeConfig[type]?.label ?? type;
  }

  function handleCreate() {
    editingNotification = null;
    showForm = true;
  }

  function handleCreateTemplate() {
    editingTemplate = null;
    showTemplateForm = true;
  }

  function handleEditTemplate(template: NotificationTemplate) {
    editingTemplate = template;
    showTemplateForm = true;
  }

  async function handleDeleteTemplate(template: NotificationTemplate) {
    const ok = await confirmAction({
      title: m.notification_template_delete_title({ name: template.name }),
      message: m.notification_template_delete_message(),
      confirmLabel: m.notification_template_delete_confirm(),
      destructive: true,
    });
    if (!ok) return;
    try {
      await notificationTemplatesApi.remove(template.id);
      toast.success(m.notification_template_deleted());
      await load();
    } catch (error: unknown) {
      toast.error(
        error && typeof error === "object" && "message" in error
          ? String((error as { message: unknown }).message)
          : m.notification_template_delete_failed(),
      );
    }
  }

  function templateName(id: number | null): string | null {
    if (id === null) return null;
    return (
      templates.find((item) => item.id === id)?.name ??
      m.notification_template_missing()
    );
  }

  function handleEdit(n: Notification) {
    editingNotification = n;
    showForm = true;
  }

  async function handleDelete(n: Notification) {
    const ok = await confirmAction({
      title: m.notifications_page_delete_title({ name: n.name }),
      message: m.notifications_page_delete_message(),
      confirmLabel: m.notifications_page_delete_confirm(),
      destructive: true,
    });
    if (!ok) return;
    try {
      await notificationsApi.remove(n.id);
      toast.success(m.notifications_page_deleted_toast());
      await load();
    } catch {
      toast.error(m.monitors_page_delete_failed());
    }
  }

  async function handleTest(n: Notification) {
    testingId = n.id;
    try {
      await notificationsApi.test(n.id);
      toast.success(m.notifications_page_test_sent({ name: n.name }));
    } catch (err: any) {
      toast.error(err?.message || m.notifications_page_test_failed());
    } finally {
      testingId = null;
    }
  }

  function handleSaved() {
    showForm = false;
    editingNotification = null;
    load();
  }

  function handleTemplateSaved() {
    showTemplateForm = false;
    editingTemplate = null;
    load();
  }
</script>

<svelte:head>
  <title>{m.app_name()} · {m.notifications_title()}</title>
</svelte:head>

<div class="space-y-6">
  <!-- Page heading row -->
  <div
    class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between"
  >
    <div>
      <h1 class="text-2xl font-semibold tracking-tight">
        {m.notifications_title()}
      </h1>
      <p class="mt-1 text-sm text-muted-foreground">
        {m.notifications_page_subtitle()}
      </p>
    </div>
    <div class="flex flex-wrap items-center gap-2">
      {#if canManage}
        <button
          onclick={handleCreateTemplate}
          class="inline-flex items-center gap-2 rounded-lg border border-border bg-card px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <Braces class="h-4 w-4" />
          {m.notification_template_new()}
        </button>
      {/if}
      <button
        onclick={handleCreate}
        disabled={!canManage}
        class="inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50"
      >
        <Plus class="h-4 w-4" />
        {m.notifications_create()}
      </button>
    </div>
  </div>

  {#if loading}
    <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3" role="status">
      <span class="sr-only">{m.notifications_page_loading()}</span>
      {#each Array(3) as _}<div
          class="rounded-xl border border-border bg-card p-5"
        >
          <div class="flex gap-3">
            <Skeleton class="h-10 w-10 rounded-full" />
            <div class="flex-1">
              <Skeleton class="h-4 w-32" /><Skeleton class="mt-2 h-3 w-20" />
            </div>
          </div>
          <Skeleton class="mt-6 h-8 w-full" />
        </div>{/each}
    </div>
  {:else if loadError}
    {#snippet retryAction()}
      <button
        type="button"
        onclick={load}
        class="inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >{m.monitor_group_form_retry()}</button
      >
    {/snippet}
    <EmptyState
      icon={XCircle}
      title={m.notifications_page_load_failed()}
      description={loadError}
      action={retryAction}
    />
  {:else}
    {#if canManage}
      <section class="space-y-3" aria-labelledby="message-templates-title">
        <div class="flex items-center justify-between gap-4">
          <div>
            <h2
              id="message-templates-title"
              class="text-sm font-semibold text-muted-foreground"
            >
              {m.notification_templates_title()}
            </h2>
            <p class="mt-0.5 text-xs text-faint">
              {m.notification_templates_description()}
            </p>
          </div>
          <span class="tnum text-xs text-faint">{templates.length}</span>
        </div>

        <div class="overflow-hidden rounded-xl border border-border bg-card">
          {#if templates.length === 0}
            <div
              class="flex flex-col gap-4 px-5 py-6 sm:flex-row sm:items-center sm:justify-between"
            >
              <div class="flex min-w-0 items-start gap-3">
                <div
                  class="grid h-9 w-9 shrink-0 place-items-center rounded-lg bg-muted text-muted-foreground"
                >
                  <FileText class="h-4 w-4" />
                </div>
                <div class="min-w-0">
                  <p class="text-sm font-medium">
                    {m.notification_templates_empty_title()}
                  </p>
                  <p class="mt-1 text-xs text-muted-foreground">
                    {m.notification_templates_empty_description()}
                  </p>
                </div>
              </div>
              <button
                type="button"
                onclick={handleCreateTemplate}
                class="shrink-0 text-sm font-medium text-primary transition-colors hover:text-primary/80"
              >
                {m.notification_template_create_action()}
              </button>
            </div>
          {:else}
            {#each templates as template (template.id)}
              <div
                class="flex flex-col gap-3 border-b border-border px-4 py-3 last:border-0 sm:flex-row sm:items-center"
              >
                <div class="flex min-w-0 flex-1 items-start gap-3">
                  <div
                    class="grid h-9 w-9 shrink-0 place-items-center rounded-lg bg-primary/10 text-primary"
                  >
                    <Braces class="h-4 w-4" />
                  </div>
                  <div class="min-w-0 flex-1">
                    <div class="flex min-w-0 flex-wrap items-center gap-2">
                      <h3 class="truncate text-sm font-medium">
                        {template.name}
                      </h3>
                      <span
                        class="rounded bg-muted px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground"
                        >{typeLabel(template.provider)}</span
                      >
                      {#if template.provider === "smtp"}
                        <span
                          class="rounded bg-muted px-1.5 py-0.5 text-[11px] font-medium text-muted-foreground"
                        >
                          {template.smtp_config?.format === "html"
                            ? m.notification_template_email_format_html()
                            : m.notification_template_email_format_plain()}
                        </span>
                      {/if}
                    </div>
                    <p
                      class="mt-1 line-clamp-1 break-all font-mono text-xs text-faint"
                    >
                      {template.title_template || template.body_template}
                    </p>
                  </div>
                </div>
                <div class="flex shrink-0 items-center gap-2 pl-12 sm:pl-0">
                  <button
                    type="button"
                    onclick={() => handleEditTemplate(template)}
                    class="inline-flex items-center gap-1 rounded-lg border border-border px-3 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
                  >
                    <Edit2 class="h-3 w-3" />{m.btn_edit()}
                  </button>
                  <button
                    type="button"
                    onclick={() => handleDeleteTemplate(template)}
                    class="inline-flex items-center gap-1 rounded-lg border border-destructive/25 px-3 py-1.5 text-xs font-medium text-destructive transition-colors hover:bg-destructive/10"
                  >
                    <Trash2 class="h-3 w-3" />{m.btn_delete()}
                  </button>
                </div>
              </div>
            {/each}
          {/if}
        </div>
      </section>
    {/if}

    {#if notifications.length === 0}
      {#snippet emptyAction()}{#if canManage}<button
            onclick={handleCreate}
            class="inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
            ><Plus class="h-4 w-4" />{m.notifications_page_add_first()}</button
          >{/if}{/snippet}
      <EmptyState
        icon={Bell}
        title={m.notifications_page_empty_title()}
        description={m.notifications_page_empty_description()}
        action={emptyAction}
      />
    {:else}
      <div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
        {#each notifications as n (n.id)}
          <div
            class="group relative overflow-hidden rounded-xl border border-border bg-card p-5 transition-all duration-200 hover:-translate-y-0.5 hover:border-border/80 hover:shadow-[0_18px_40px_-24px_rgba(0,0,0,0.8)]"
          >
            <div class="flex items-start justify-between gap-3">
              <div class="flex min-w-0 items-center gap-3">
                <div
                  class="grid h-10 w-10 shrink-0 place-items-center rounded-full {n.active
                    ? 'bg-success/10 text-success ring-1 ring-success/20'
                    : 'bg-muted text-muted-foreground'}"
                >
                  {#if n.active}
                    <CheckCircle class="h-5 w-5" />
                  {:else}
                    <XCircle class="h-5 w-5" />
                  {/if}
                </div>
                <div class="min-w-0">
                  <h3 class="truncate font-medium">{n.name}</h3>
                  <p class="truncate text-sm text-muted-foreground">
                    {typeLabel(n.type)}
                  </p>
                  {#if n.template_id !== null}
                    <p
                      class="mt-0.5 flex items-center gap-1 truncate text-xs text-faint"
                    >
                      <Braces class="h-3 w-3 shrink-0" />
                      {templateName(n.template_id)}
                    </p>
                  {/if}
                  {#if n.include_ack_url}
                    <p class="mt-0.5 text-xs text-faint">
                      {m.notifications_ack_link_on()}
                    </p>
                  {/if}
                </div>
              </div>
              <span
                class="inline-flex shrink-0 items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium {n.active
                  ? 'border-success/25 bg-success/10 text-success'
                  : 'border-border bg-muted/40 text-muted-foreground'}"
              >
                <span class="dot {n.active ? 'dot-up' : 'dot-muted'}"></span>
                {n.active
                  ? m.notifications_active()
                  : m.notifications_disabled()}
              </span>
            </div>

            <div
              class="mt-4 flex items-center gap-2 border-t border-border pt-3"
            >
              <button
                onclick={() => handleTest(n)}
                disabled={testingId === n.id || !n.active || !canManage}
                class="inline-flex items-center gap-1 rounded-lg border border-border px-3 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground disabled:opacity-50 disabled:hover:bg-transparent"
                title={m.notifications_page_send_test()}
              >
                <Send class="h-3 w-3" />
                {testingId === n.id
                  ? m.notifications_page_sending()
                  : m.btn_test()}
              </button>
              {#if canManage}
                <button
                  onclick={() => handleEdit(n)}
                  class="inline-flex items-center gap-1 rounded-lg border border-border px-3 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
                  title={m.btn_edit()}
                >
                  <Edit2 class="h-3 w-3" />
                  {m.btn_edit()}
                </button>
                <button
                  onclick={() => handleDelete(n)}
                  class="inline-flex items-center gap-1 rounded-lg border border-destructive/25 px-3 py-1.5 text-xs font-medium text-destructive transition-colors hover:bg-destructive/10"
                  title={m.btn_delete()}
                >
                  <Trash2 class="h-3 w-3" />
                  {m.btn_delete()}
                </button>
              {/if}
            </div>
          </div>
        {/each}
      </div>
    {/if}
  {/if}
</div>

{#if showForm}
  <NotificationForm
    notification={editingNotification ?? undefined}
    {templates}
    onSaved={handleSaved}
    onClose={() => {
      showForm = false;
      editingNotification = null;
    }}
  />
{/if}

{#if showTemplateForm}
  <NotificationTemplateForm
    template={editingTemplate ?? undefined}
    variables={templateVariables}
    onSaved={handleTemplateSaved}
    onClose={() => {
      showTemplateForm = false;
      editingTemplate = null;
    }}
  />
{/if}
