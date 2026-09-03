<script lang="ts">
  /**
   * Reverse assignment panel for one notification: lists monitors and
   * folders attached to it, and (for capability holders) allows
   * assign/unassign plus the per-monitor include_target flag.
   *
   * A folder attachment alerts on the folder's own derived status. It is
   * not a shortcut for attaching every monitor inside the folder.
   */
  import {
    notificationsApi,
    type NotificationAssignments,
    type NotificationGroupAssignment,
    type NotificationMonitorAssignment,
  } from "$lib/api/notifications";
  import { monitorsApi } from "$lib/api/monitors";
  import {
    monitorGroupsApi,
    type MonitorGroupView,
  } from "$lib/api/monitorGroups";
  import type { Monitor } from "$lib/stores/ws.svelte.ts";
  import MonitorPicker from "$lib/components/MonitorPicker.svelte";
  import Select from "$lib/components/Select.svelte";
  import { Folder, Link2Off, Loader2 } from "@lucide/svelte";
  import { toast } from "svelte-sonner";
  import * as m from "$lib/paraglide/messages.js";

  interface Props {
    notificationId: number;
    canManage: boolean;
  }

  let { notificationId, canManage }: Props = $props();

  let loading = $state(true);
  let loadError = $state<string | null>(null);
  let assignments = $state<NotificationAssignments>({
    monitors: [],
    groups: [],
  });

  let allMonitors = $state<Monitor[]>([]);
  let allGroups = $state<MonitorGroupView[]>([]);
  let pickersLoading = $state(false);
  let pickersError = $state<string | null>(null);

  let groupPick = $state("");
  let busyKey = $state<string | null>(null);

  const assignedMonitorIds = $derived(
    new Set(assignments.monitors.map((x) => x.id)),
  );
  const assignedGroupIds = $derived(
    new Set(assignments.groups.map((x) => x.id)),
  );

  const groupOptions = $derived(
    allGroups
      .filter((g) => !assignedGroupIds.has(g.id))
      .map((g) => ({ value: String(g.id), label: g.name })),
  );

  async function loadAssignments() {
    loading = true;
    loadError = null;
    try {
      assignments = await notificationsApi.listAssignments(notificationId);
    } catch (error: unknown) {
      loadError =
        error && typeof error === "object" && "message" in error
          ? String((error as { message: string }).message)
          : m.notification_assignments_load_failed();
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    const id = notificationId;
    const manage = canManage;
    let cancelled = false;
    loading = true;
    loadError = null;
    void notificationsApi
      .listAssignments(id)
      .then((data) => {
        if (cancelled) return;
        assignments = data;
      })
      .catch((error: unknown) => {
        if (cancelled) return;
        loadError =
          error && typeof error === "object" && "message" in error
            ? String((error as { message: string }).message)
            : m.notification_assignments_load_failed();
      })
      .finally(() => {
        if (!cancelled) loading = false;
      });
    if (manage) {
      pickersLoading = true;
      pickersError = null;
      void Promise.all([monitorsApi.list(), monitorGroupsApi.list()])
        .then(([mons, grps]) => {
          if (cancelled) return;
          allMonitors = mons;
          allGroups = grps;
        })
        .catch((error: unknown) => {
          if (cancelled) return;
          pickersError =
            error && typeof error === "object" && "message" in error
              ? String((error as { message: string }).message)
              : m.notification_assignments_load_failed();
        })
        .finally(() => {
          if (!cancelled) pickersLoading = false;
        });
    }
    return () => {
      cancelled = true;
    };
  });

  async function unassignMonitor(ref: NotificationMonitorAssignment) {
    const key = `m:${ref.id}`;
    busyKey = key;
    try {
      await notificationsApi.unassignFromMonitor(ref.id, notificationId);
      assignments = {
        ...assignments,
        monitors: assignments.monitors.filter((x) => x.id !== ref.id),
      };
      toast.success(m.notification_assignments_unassigned_toast());
    } catch {
      toast.error(m.notification_assignments_unassign_failed());
    } finally {
      busyKey = null;
    }
  }

  async function unassignGroup(ref: NotificationGroupAssignment) {
    const key = `g:${ref.id}`;
    busyKey = key;
    try {
      await notificationsApi.detachFromGroup(notificationId, ref.id);
      assignments = {
        ...assignments,
        groups: assignments.groups.filter((x) => x.id !== ref.id),
      };
      toast.success(m.notification_assignments_unassigned_toast());
    } catch {
      toast.error(m.notification_assignments_unassign_failed());
    } finally {
      busyKey = null;
    }
  }

  async function assignMonitor(monitor: Monitor) {
    if (assignedMonitorIds.has(monitor.id)) return;
    busyKey = `am:${monitor.id}`;
    try {
      await notificationsApi.assignToMonitor(monitor.id, notificationId);
      assignments = {
        ...assignments,
        monitors: [
          ...assignments.monitors,
          { id: monitor.id, name: monitor.name, include_target: true },
        ].sort((a, b) => a.name.localeCompare(b.name) || a.id - b.id),
      };
      toast.success(m.notification_assignments_assigned_toast());
    } catch {
      toast.error(m.notification_assignments_assign_failed());
    } finally {
      busyKey = null;
    }
  }

  async function assignGroup(value: string) {
    const id = Number(value);
    if (!id || assignedGroupIds.has(id)) return;
    const grp = allGroups.find((x) => x.id === id);
    busyKey = `ag:${id}`;
    try {
      await notificationsApi.attachToGroup(notificationId, id);
      if (grp) {
        assignments = {
          ...assignments,
          groups: [...assignments.groups, { id, name: grp.name }].sort(
            (a, b) => a.name.localeCompare(b.name) || a.id - b.id,
          ),
        };
      }
      groupPick = "";
      toast.success(m.notification_assignments_assigned_toast());
    } catch {
      toast.error(m.notification_assignments_assign_failed());
    } finally {
      busyKey = null;
    }
  }

  async function toggleIncludeTarget(ref: NotificationMonitorAssignment) {
    const next = !ref.include_target;
    busyKey = `it:${ref.id}`;
    assignments = {
      ...assignments,
      monitors: assignments.monitors.map((x) =>
        x.id === ref.id ? { ...x, include_target: next } : x,
      ),
    };
    try {
      await notificationsApi.setMonitorIncludeTarget(
        ref.id,
        notificationId,
        next,
      );
    } catch {
      assignments = {
        ...assignments,
        monitors: assignments.monitors.map((x) =>
          x.id === ref.id ? { ...x, include_target: !next } : x,
        ),
      };
      toast.error(m.notification_assignments_assign_failed());
    } finally {
      busyKey = null;
    }
  }
</script>

<div
  class="rounded-xl border border-border bg-card p-5"
  data-testid="notification-assignments"
>
  <div
    class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between"
  >
    <div class="min-w-0">
      <h2 class="text-sm font-semibold tracking-tight">
        {m.notification_assignments_title()}
      </h2>
      <p class="mt-0.5 text-xs text-muted-foreground">
        {m.notification_assignments_description()}
      </p>
    </div>
    {#if canManage}
      <div class="w-full sm:w-72">
        <p class="mb-1 text-[11px] font-medium text-muted-foreground">
          {m.notification_assignments_assign_monitor()}
        </p>
        {#if pickersLoading}
          <p
            class="inline-flex items-center gap-2 text-xs text-muted-foreground"
          >
            <Loader2 class="h-3.5 w-3.5 animate-spin" />
            {m.notification_assignments_loading()}
          </p>
        {:else if pickersError}
          <p class="text-xs text-destructive">{pickersError}</p>
        {:else}
          <MonitorPicker
            monitors={allMonitors}
            exclude={assignedMonitorIds}
            placeholder={m.notification_assignments_assign_placeholder_monitor()}
            emptyText={m.notification_assignments_none_available_monitors()}
            onSelect={assignMonitor}
          />
        {/if}
      </div>
    {/if}
  </div>

  <div class="mt-4 space-y-4">
    {#if loading}
      <p
        class="inline-flex items-center gap-2 text-sm text-muted-foreground"
        role="status"
      >
        <Loader2 class="h-4 w-4 animate-spin" />
        {m.notification_assignments_loading()}
      </p>
    {:else if loadError}
      <div class="space-y-2">
        <p class="text-sm text-destructive">{loadError}</p>
        <button
          type="button"
          onclick={loadAssignments}
          class="rounded-md border border-border px-2 py-1 text-xs font-medium transition-colors hover:bg-accent"
        >
          {m.monitor_group_form_retry()}
        </button>
      </div>
    {:else}
      {#if assignments.monitors.length === 0 && assignments.groups.length === 0}
        <p class="text-sm text-muted-foreground">
          {m.notification_assignments_empty()}
        </p>
      {:else}
        {#if assignments.monitors.length > 0}
          <div>
            <p
              class="mb-1.5 text-[11px] font-medium uppercase tracking-wide text-muted-foreground"
            >
              {m.notification_assignments_monitors()}
            </p>
            <ul class="overflow-hidden rounded-lg border border-border">
              {#each assignments.monitors as mon, i (mon.id)}
                <li
                  class="flex flex-col gap-2 px-4 py-3 text-sm sm:flex-row sm:items-center sm:justify-between {i !==
                  assignments.monitors.length - 1
                    ? 'border-b border-border'
                    : ''}"
                  data-testid="notification-assigned-monitor"
                >
                  <a
                    href="/monitors/{mon.id}"
                    class="min-w-0 truncate font-medium text-foreground underline-offset-2 hover:underline"
                  >
                    {mon.name}
                  </a>
                  <div class="flex shrink-0 items-center gap-3">
                    {#if canManage}
                      <label
                        class="flex items-center gap-1.5 text-xs text-muted-foreground"
                      >
                        <input
                          type="checkbox"
                          checked={mon.include_target}
                          disabled={busyKey === `it:${mon.id}`}
                          onchange={() => toggleIncludeTarget(mon)}
                          class="h-4 w-4 rounded border-border accent-primary"
                        />
                        {m.notification_assignments_include_target()}
                      </label>
                      <button
                        type="button"
                        disabled={busyKey === `m:${mon.id}`}
                        onclick={() => unassignMonitor(mon)}
                        class="inline-flex items-center gap-1 text-xs text-danger transition-colors hover:text-danger/80 disabled:opacity-50"
                        title={m.notification_assignments_unassign()}
                      >
                        <Link2Off class="h-3 w-3" />
                        {m.notification_assignments_unassign()}
                      </button>
                    {/if}
                  </div>
                </li>
              {/each}
            </ul>
          </div>
        {/if}

        {#if assignments.groups.length > 0}
          <div>
            <p
              class="mb-1.5 text-[11px] font-medium uppercase tracking-wide text-muted-foreground"
            >
              {m.notification_assignments_groups()}
            </p>
            <p class="mb-2 text-xs text-muted-foreground">
              {m.notification_assignments_groups_help()}
            </p>
            <ul class="overflow-hidden rounded-lg border border-border">
              {#each assignments.groups as grp, i (grp.id)}
                <li
                  class="flex items-center justify-between gap-2 px-4 py-3 text-sm {i !==
                  assignments.groups.length - 1
                    ? 'border-b border-border'
                    : ''}"
                  data-testid="notification-assigned-group"
                >
                  <a
                    href="/monitors"
                    class="inline-flex min-w-0 items-center gap-1.5 truncate font-medium text-foreground underline-offset-2 hover:underline"
                  >
                    <Folder class="h-3.5 w-3.5 shrink-0 text-primary/80" />
                    {grp.name}
                  </a>
                  {#if canManage}
                    <button
                      type="button"
                      disabled={busyKey === `g:${grp.id}`}
                      onclick={() => unassignGroup(grp)}
                      class="inline-flex shrink-0 items-center gap-1 text-xs text-danger transition-colors hover:text-danger/80 disabled:opacity-50"
                      title={m.notification_assignments_unassign()}
                    >
                      <Link2Off class="h-3 w-3" />
                      {m.notification_assignments_unassign()}
                    </button>
                  {/if}
                </li>
              {/each}
            </ul>
          </div>
        {/if}
      {/if}

      {#if canManage}
        <div class="space-y-1 border-t border-border/60 pt-4">
          <label
            class="text-[11px] font-medium text-muted-foreground"
            for={`notif-assign-grp-${notificationId}`}
          >
            {m.notification_assignments_assign_group()}
          </label>
          {#if pickersLoading}
            <p
              class="inline-flex items-center gap-2 text-xs text-muted-foreground"
            >
              <Loader2 class="h-3.5 w-3.5 animate-spin" />
              {m.notification_assignments_loading()}
            </p>
          {:else if pickersError}
            <p class="text-xs text-destructive">{pickersError}</p>
          {:else if groupOptions.length === 0}
            <p class="text-xs text-muted-foreground">
              {m.notification_assignments_none_available_groups()}
            </p>
          {:else}
            <Select
              id={`notif-assign-grp-${notificationId}`}
              size="sm"
              options={groupOptions}
              value={groupPick}
              placeholder={m.notification_assignments_assign_placeholder_group()}
              disabled={busyKey !== null}
              onValueChange={(v) => {
                groupPick = v;
                assignGroup(v);
              }}
            />
          {/if}
        </div>
      {/if}
    {/if}
  </div>
</div>
