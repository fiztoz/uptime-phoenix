<script lang="ts">
  import { page } from "$app/stores";
  import { goto } from "$app/navigation";
  import { notificationsApi, type Notification } from "$lib/api/notifications";
  import {
    notificationTemplatesApi,
    type NotificationTemplate,
  } from "$lib/api/notification-templates";
  import { notificationTypeConfig } from "$lib/notification-types";
  import NotificationForm from "$lib/components/NotificationForm.svelte";
  import NotificationAssignments from "$lib/components/NotificationAssignments.svelte";
  import NotificationPreview from "$lib/components/NotificationPreview.svelte";
  import EmptyState from "$lib/components/EmptyState.svelte";
  import Skeleton from "$lib/components/Skeleton.svelte";
  import { auth } from "$lib/stores/auth.svelte.ts";
  import { confirmAction } from "$lib/stores/confirm.svelte";
  import { toast } from "svelte-sonner";
  import {
    ArrowLeft,
    Bell,
    Braces,
    CheckCircle,
    Edit2,
    Send,
    Trash2,
    XCircle,
  } from "@lucide/svelte";
  import * as m from "$lib/paraglide/messages.js";

  let notificationId = $derived(Number($page.params.id));
  let notification = $state<Notification | null>(null);
  let templates = $state<NotificationTemplate[]>([]);
  let loading = $state(true);
  let loadError = $state<string | null>(null);
  let notFound = $state(false);
  let showForm = $state(false);
  let testing = $state(false);

  const isUserAdmin = $derived(auth.user?.is_admin ?? false);
  const canManage = $derived(
    isUserAdmin || (auth.user?.can_manage_notifications ?? false),
  );

  function typeLabel(type: string): string {
    return notificationTypeConfig[type]?.label ?? type;
  }

  function templateName(id: number | null): string | null {
    if (id === null) return null;
    return (
      templates.find((item) => item.id === id)?.name ??
      m.notification_template_missing()
    );
  }

  const attachedTemplate = $derived.by(() => {
    const current = notification;
    if (current === null || current.template_id === null) return null;
    return templates.find((item) => item.id === current.template_id) ?? null;
  });

  async function load() {
    loading = true;
    loadError = null;
    notFound = false;
    try {
      const id = notificationId;
      const [n, tpls] = await Promise.all([
        notificationsApi.get(id),
        canManage
          ? notificationTemplatesApi.list()
          : Promise.resolve([] as NotificationTemplate[]),
      ]);
      if (id !== notificationId) return;
      notification = n;
      templates = tpls;
    } catch (error: unknown) {
      const message =
        error && typeof error === "object" && "message" in error
          ? String((error as { message: string }).message)
          : m.notification_detail_load_failed();
      if (/not found/i.test(message)) {
        notFound = true;
        notification = null;
      } else {
        loadError = message;
        toast.error(message);
      }
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    void notificationId;
    void load();
  });

  async function handleTest() {
    if (!notification) return;
    testing = true;
    try {
      await notificationsApi.test(notification.id);
      toast.success(
        m.notifications_page_test_sent({ name: notification.name }),
      );
    } catch (err: unknown) {
      toast.error(
        err && typeof err === "object" && "message" in err
          ? String((err as { message: string }).message)
          : m.notifications_page_test_failed(),
      );
    } finally {
      testing = false;
    }
  }

  async function handleDelete() {
    if (!notification) return;
    const ok = await confirmAction({
      title: m.notifications_page_delete_title({ name: notification.name }),
      message: m.notifications_page_delete_message(),
      confirmLabel: m.notifications_page_delete_confirm(),
      destructive: true,
    });
    if (!ok) return;
    try {
      await notificationsApi.remove(notification.id);
      toast.success(m.notifications_page_deleted_toast());
      await goto("/notifications");
    } catch {
      toast.error(m.monitors_page_delete_failed());
    }
  }

  function handleSaved(saved: Notification) {
    showForm = false;
    notification = saved;
  }

  const ghostBtn =
    "inline-flex items-center gap-2 rounded-lg border border-border px-3 py-1.5 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50";
</script>

<svelte:head>
  <title>{m.app_name()} · {notification?.name ?? m.notifications_title()}</title
  >
</svelte:head>

<div class="space-y-6">
  <button
    type="button"
    onclick={() => goto("/notifications")}
    class="inline-flex items-center gap-2 text-sm text-muted-foreground transition-colors hover:text-foreground"
  >
    <ArrowLeft class="h-4 w-4" />
    {m.notification_detail_back()}
  </button>

  {#snippet retryAction()}
    <button type="button" onclick={load} class={ghostBtn}
      >{m.monitor_group_form_retry()}</button
    >
  {/snippet}

  {#if loading}
    <div class="space-y-5" role="status">
      <span class="sr-only">{m.notifications_page_loading()}</span>
      <div class="flex items-start justify-between gap-4">
        <div class="space-y-3">
          <Skeleton class="h-8 w-64 max-w-full" />
          <Skeleton class="h-4 w-40" />
        </div>
        <Skeleton class="h-9 w-28" />
      </div>
      <div class="rounded-xl border border-border bg-card p-5">
        <Skeleton class="h-5 w-40" />
        <Skeleton class="mt-5 h-24 w-full" />
      </div>
    </div>
  {:else if loadError}
    <EmptyState
      icon={XCircle}
      title={m.notification_detail_load_failed()}
      description={loadError}
      action={retryAction}
    />
  {:else if notFound || !notification}
    <EmptyState
      icon={Bell}
      title={m.notification_detail_not_found_title()}
      description={m.notification_detail_not_found_description()}
    />
  {:else}
    <div
      class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between"
    >
      <div class="flex min-w-0 items-start gap-3">
        <div
          class="grid h-10 w-10 shrink-0 place-items-center rounded-full {notification.active
            ? 'bg-success/10 text-success ring-1 ring-success/20'
            : 'bg-muted text-muted-foreground'}"
        >
          {#if notification.active}
            <CheckCircle class="h-5 w-5" />
          {:else}
            <XCircle class="h-5 w-5" />
          {/if}
        </div>
        <div class="min-w-0">
          <h1 class="truncate text-2xl font-semibold tracking-tight">
            {notification.name}
          </h1>
          <p class="mt-1 text-sm text-muted-foreground">
            {typeLabel(notification.type)}
          </p>
          <div class="mt-2 flex flex-wrap items-center gap-2">
            <span
              class="inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium {notification.active
                ? 'border-success/25 bg-success/10 text-success'
                : 'border-border bg-muted/40 text-muted-foreground'}"
            >
              <span class="dot {notification.active ? 'dot-up' : 'dot-muted'}"
              ></span>
              {notification.active
                ? m.notifications_active()
                : m.notifications_disabled()}
            </span>
            {#if notification.is_default}
              <span
                class="rounded-full border border-border bg-muted/40 px-2.5 py-0.5 text-xs font-medium text-muted-foreground"
              >
                {m.notification_detail_default_badge()}
              </span>
            {/if}
            {#if notification.include_ack_url}
              <span class="text-xs text-faint"
                >{m.notifications_ack_link_on()}</span
              >
            {/if}
            {#if notification.template_id !== null}
              <span class="inline-flex items-center gap-1 text-xs text-faint">
                <Braces class="h-3 w-3 shrink-0" />
                {templateName(notification.template_id)}
              </span>
            {/if}
          </div>
          {#if notification.is_default}
            <p class="mt-2 max-w-2xl text-xs text-muted-foreground">
              {m.notification_detail_default_help()}
            </p>
          {/if}
        </div>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        <button
          type="button"
          onclick={handleTest}
          disabled={testing || !notification.active || !canManage}
          class={ghostBtn}
          title={m.notifications_page_send_test()}
        >
          <Send class="h-4 w-4" />
          {testing ? m.notifications_page_sending() : m.btn_test()}
        </button>
        {#if canManage}
          <button
            type="button"
            onclick={() => (showForm = true)}
            class={ghostBtn}
          >
            <Edit2 class="h-4 w-4" />
            {m.notification_detail_edit_settings()}
          </button>
          <button
            type="button"
            onclick={handleDelete}
            class="inline-flex items-center gap-2 rounded-lg border border-destructive/25 px-3 py-1.5 text-sm font-medium text-destructive transition-colors hover:bg-destructive/10 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            <Trash2 class="h-4 w-4" />
            {m.btn_delete()}
          </button>
        {/if}
      </div>
    </div>

    <NotificationPreview {notification} template={attachedTemplate} />
    <NotificationAssignments notificationId={notification.id} {canManage} />
  {/if}
</div>

{#if showForm && notification}
  <NotificationForm
    {notification}
    {templates}
    onSaved={handleSaved}
    onClose={() => {
      showForm = false;
    }}
  />
{/if}
