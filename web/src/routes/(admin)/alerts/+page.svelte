<script lang="ts">
  import { BellRing, Check, Search, X } from "@lucide/svelte";
  import {
    alertsApi,
    type Alert,
    type AlertListParams,
    type AlertStatus,
  } from "$lib/api/alerts";
  import { realtime } from "$lib/stores/ws.svelte.js";
  import { toast } from "svelte-sonner";
  import Skeleton from "$lib/components/Skeleton.svelte";
  import EmptyState from "$lib/components/EmptyState.svelte";
  import * as m from "$lib/paraglide/messages.js";

  /** Server-side status scope for GET /api/alerts. */
  type StatusFilter = "open" | AlertStatus | "all";

  const STATUS_FILTERS: StatusFilter[] = [
    "open",
    "firing",
    "acked",
    "resolved",
    "all",
  ];

  let alerts = $state<Alert[]>([]);
  let loading = $state(true);
  let loadError = $state<string | null>(null);
  let statusFilter = $state<StatusFilter>("open");
  let searchQuery = $state("");
  let acking = $state<Record<number, boolean>>({});
  let selectedIds = $state<number[]>([]);
  let bulkAcking = $state(false);

  let query = $derived(searchQuery.trim().toLowerCase());

  let visibleAlerts = $derived.by(() => {
    // Re-evaluate when the monitor catalog arrives so name search is accurate.
    void realtime.monitors;
    if (!query) return alerts;
    return alerts.filter((alert) => {
      const name = monitorName(alert.monitor_id).toLowerCase();
      const message = (alert.message ?? "").toLowerCase();
      return name.includes(query) || message.includes(query);
    });
  });

  let visibleFiringIds = $derived(
    visibleAlerts
      .filter((alert) => alert.status === "firing")
      .map((alert) => alert.id),
  );
  let selectedCount = $derived(
    selectedIds.filter((id) => visibleFiringIds.includes(id)).length,
  );
  let allVisibleFiringSelected = $derived(
    visibleFiringIds.length > 0 &&
      visibleFiringIds.every((id) => selectedIds.includes(id)),
  );
  let someVisibleFiringSelected = $derived(
    !allVisibleFiringSelected &&
      visibleFiringIds.some((id) => selectedIds.includes(id)),
  );
  let individualAcking = $derived(Object.values(acking).some(Boolean));
  let busy = $derived(bulkAcking || individualAcking);
  let hasActiveClientFilter = $derived(query.length > 0);
  let filterActive = $derived(statusFilter !== "open" || hasActiveClientFilter);

  function listParams(filter: StatusFilter): AlertListParams {
    const base: AlertListParams = { limit: 100 };
    switch (filter) {
      case "open":
        return { ...base, open: true };
      case "firing":
      case "acked":
      case "resolved":
        return { ...base, status: filter };
      case "all":
      default:
        return base;
    }
  }

  function matchesStatusFilter(alert: Alert, filter: StatusFilter): boolean {
    switch (filter) {
      case "open":
        return alert.status === "firing" || alert.status === "acked";
      case "firing":
      case "acked":
      case "resolved":
        return alert.status === filter;
      case "all":
        return true;
    }
  }

  function applyAcknowledged(updated: Alert) {
    if (!matchesStatusFilter(updated, statusFilter)) {
      alerts = alerts.filter((a) => a.id !== updated.id);
    } else {
      alerts = alerts.map((a) => (a.id === updated.id ? updated : a));
    }
    selectedIds = selectedIds.filter((id) => id !== updated.id);
  }

  function statusFilterLabel(filter: StatusFilter): string {
    switch (filter) {
      case "open":
        return m.alerts_filter_open();
      case "firing":
        return m.alerts_status_firing();
      case "acked":
        return m.alerts_status_acked();
      case "resolved":
        return m.alerts_status_resolved();
      case "all":
        return m.alerts_filter_all();
    }
  }

  async function load() {
    loading = true;
    loadError = null;
    try {
      const loaded = await alertsApi.list(listParams(statusFilter));
      const firingIds = new Set(
        loaded
          .filter((alert) => alert.status === "firing")
          .map((alert) => alert.id),
      );
      alerts = loaded;
      // Drop selections that are no longer firing or no longer in the list.
      selectedIds = selectedIds.filter((id) => firingIds.has(id));
    } catch (e: unknown) {
      const message =
        e instanceof Error ? e.message : m.alerts_page_load_failed();
      const msg =
        typeof e === "object" &&
        e &&
        "message" in e &&
        typeof (e as { message: unknown }).message === "string"
          ? (e as { message: string }).message
          : message;
      loadError = msg;
      toast.error(msg);
    } finally {
      loading = false;
    }
  }

  $effect(() => {
    // Reload when server-side status scope changes (also runs on mount).
    void statusFilter;
    void load();
  });

  function setStatusFilter(next: StatusFilter) {
    if (next === statusFilter || busy) return;
    statusFilter = next;
    // Fresh scope should not carry stale multi-select across lists.
    selectedIds = [];
  }

  function clearSearch() {
    searchQuery = "";
  }

  function clearFilters() {
    searchQuery = "";
    if (statusFilter !== "open") {
      setStatusFilter("open");
    }
  }

  function monitorName(id: number): string {
    const mon = realtime.monitors.find((x) => x.id === id);
    return mon?.name ?? m.alerts_page_monitor_fallback({ id });
  }

  function statusClass(status: string): string {
    if (status === "firing") return "bg-danger/10 text-danger border-danger/25";
    if (status === "acked")
      return "bg-warning/10 text-warning border-warning/25";
    return "bg-muted/40 text-muted-foreground border-border";
  }

  function escalationClass(status: string): string {
    if (status === "pending")
      return "bg-warning/10 text-warning border-warning/25";
    return "bg-muted/40 text-muted-foreground border-border";
  }

  function escalationLabel(esc: NonNullable<Alert["escalation"]>): string {
    if (esc.status === "pending")
      return m.alerts_escalation_pending({ step: esc.next_step });
    if (esc.status === "canceled") return m.alerts_escalation_canceled();
    return m.alerts_escalation_done();
  }

  function statusLabel(status: string): string {
    if (status === "firing") return m.alerts_status_firing();
    if (status === "acked") return m.alerts_status_acked();
    return m.alerts_status_resolved();
  }

  async function ack(alert: Alert) {
    if (alert.status !== "firing" || bulkAcking) return;
    acking[alert.id] = true;
    try {
      const updated = await alertsApi.acknowledge(alert.id);
      applyAcknowledged(updated);
      toast.success(m.alerts_page_acked_toast());
    } catch (e: unknown) {
      const msg =
        typeof e === "object" &&
        e &&
        "message" in e &&
        typeof (e as { message: unknown }).message === "string"
          ? (e as { message: string }).message
          : m.alerts_page_ack_failed();
      toast.error(msg);
    } finally {
      acking[alert.id] = false;
    }
  }

  function toggleAlert(id: number) {
    if (bulkAcking || acking[id]) return;
    selectedIds = selectedIds.includes(id)
      ? selectedIds.filter((selectedID) => selectedID !== id)
      : [...selectedIds, id];
  }

  function toggleAllVisibleFiring() {
    if (busy) return;
    const visible = new Set(visibleFiringIds);
    selectedIds = allVisibleFiringSelected
      ? selectedIds.filter((id) => !visible.has(id))
      : [...new Set([...selectedIds, ...visibleFiringIds])];
  }

  async function acknowledgeSelected() {
    // Ack only firing rows still visible under the current search, so the
    // bulk action matches what the operator can see.
    const ids = selectedIds.filter((id) => visibleFiringIds.includes(id));
    if (ids.length === 0 || busy) return;

    bulkAcking = true;
    try {
      const results = await Promise.allSettled(
        ids.map((id) => alertsApi.acknowledge(id)),
      );
      const acknowledged = results.flatMap((result) =>
        result.status === "fulfilled" ? [result.value] : [],
      );
      const failed = results.length - acknowledged.length;

      for (const updated of acknowledged) {
        applyAcknowledged(updated);
      }

      if (failed === 0) {
        toast.success(
          m.alerts_bulk_acked_toast({ count: acknowledged.length }),
        );
      } else {
        toast.error(
          m.alerts_bulk_partial_toast({
            succeeded: acknowledged.length,
            failed,
          }),
        );
      }
    } finally {
      bulkAcking = false;
    }
  }

  function resultSummary(): string {
    if (hasActiveClientFilter) {
      return m.alerts_result_count_filtered({
        shown: visibleAlerts.length,
        total: alerts.length,
      });
    }
    return m.alerts_result_count({ count: alerts.length });
  }
</script>

<svelte:head>
  <title>{m.app_name()} · {m.alerts_title()}</title>
</svelte:head>

<div class="space-y-6">
  <div class="flex flex-wrap items-end justify-between gap-3">
    <div class="min-w-0">
      <h1 class="text-2xl font-semibold tracking-tight">{m.alerts_title()}</h1>
      <p class="mt-1 text-sm text-muted-foreground">
        {m.alerts_page_subtitle()}
      </p>
    </div>
    {#if !loading && !loadError && alerts.length > 0}
      <p class="tnum text-sm text-muted-foreground" aria-live="polite">
        {resultSummary()}
      </p>
    {/if}
  </div>

  <!-- Filter bar -->
  <div class="flex flex-col gap-3 sm:flex-row sm:items-center">
    <div class="relative min-w-0 flex-1">
      <Search
        class="pointer-events-none absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
      />
      <input
        type="text"
        bind:value={searchQuery}
        placeholder={m.alerts_search_placeholder()}
        aria-label={m.alerts_search_placeholder()}
        autocomplete="off"
        class="h-9 w-full rounded-lg border border-border bg-surface py-2 pl-8 pr-9 text-sm text-foreground placeholder:text-faint transition-colors hover:border-border/80 focus:outline-none focus:ring-2 focus:ring-ring"
      />
      {#if searchQuery}
        <button
          type="button"
          onclick={clearSearch}
          class="absolute right-1.5 top-1/2 grid h-6 w-6 -translate-y-1/2 place-items-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          aria-label={m.alerts_search_clear()}
        >
          <X class="h-3.5 w-3.5" />
        </button>
      {/if}
    </div>
    <div
      class="inline-flex flex-wrap rounded-lg border border-border p-0.5 text-sm"
      role="group"
      aria-label={m.alerts_filter_status_aria()}
    >
      {#each STATUS_FILTERS as filter (filter)}
        <button
          type="button"
          class="rounded-md px-2.5 py-1.5 transition-colors sm:px-3 {statusFilter ===
          filter
            ? 'bg-accent font-medium text-foreground'
            : 'text-muted-foreground hover:text-foreground'} disabled:cursor-not-allowed disabled:opacity-50"
          aria-pressed={statusFilter === filter}
          disabled={busy && statusFilter !== filter}
          onclick={() => setStatusFilter(filter)}
        >
          {statusFilterLabel(filter)}
        </button>
      {/each}
    </div>
  </div>

  {#if loading}
    <div class="rounded-xl border border-border bg-card" role="status">
      <span class="sr-only">{m.alerts_page_loading()}</span>
      {#each Array(3) as _, i}
        <div class="px-5 py-4 {i < 2 ? 'border-b border-border' : ''}">
          <Skeleton class="h-4 w-44" /><Skeleton class="mt-3 h-3 w-2/3" />
        </div>
      {/each}
    </div>
  {:else if loadError}
    {#snippet retryAction()}
      <button
        type="button"
        onclick={load}
        class="inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {m.monitor_group_form_retry()}
      </button>
    {/snippet}
    <EmptyState
      icon={BellRing}
      title={m.alerts_page_load_failed()}
      description={loadError}
      action={retryAction}
    />
  {:else if alerts.length === 0}
    {#if statusFilter !== "open" && statusFilter !== "all"}
      {#snippet resetStatusAction()}
        <button
          type="button"
          onclick={() => setStatusFilter("open")}
          class="inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          {m.alerts_empty_show_open()}
        </button>
      {/snippet}
      <EmptyState
        icon={BellRing}
        title={m.alerts_empty_status_title({
          status: statusFilterLabel(statusFilter),
        })}
        description={m.alerts_empty_status_description({
          status: statusFilterLabel(statusFilter),
        })}
        action={resetStatusAction}
      />
    {:else}
      <EmptyState
        icon={BellRing}
        title={m.alerts_page_empty_title()}
        description={m.alerts_page_empty_description()}
      />
    {/if}
  {:else if visibleAlerts.length === 0}
    {#snippet clearSearchAction()}
      <button
        type="button"
        onclick={clearSearch}
        class="inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {m.alerts_search_clear()}
      </button>
    {/snippet}
    <EmptyState
      icon={Search}
      title={m.alerts_empty_search_title()}
      description={m.alerts_empty_search_description({
        query: searchQuery.trim(),
      })}
      action={clearSearchAction}
    />
  {:else}
    <div class="overflow-hidden rounded-xl border border-border bg-card">
      {#if visibleFiringIds.length > 0 || selectedCount > 0}
        <div
          class="flex flex-wrap items-center justify-between gap-3 border-b border-border bg-muted/20 px-5 py-3"
        >
          <label
            class="flex min-h-8 cursor-pointer items-center gap-2.5 text-sm font-medium"
          >
            <input
              type="checkbox"
              checked={allVisibleFiringSelected}
              indeterminate={someVisibleFiringSelected}
              disabled={busy || visibleFiringIds.length === 0}
              onchange={toggleAllVisibleFiring}
              class="h-4 w-4 rounded border-border accent-primary disabled:cursor-not-allowed disabled:opacity-50"
              aria-label={m.alerts_select_all_firing()}
            />
            <span class="text-foreground">{m.alerts_select_all_firing()}</span>
            {#if selectedCount > 0}
              <span class="tnum text-xs text-muted-foreground">
                {m.alerts_selected_count({ count: selectedCount })}
              </span>
            {/if}
          </label>
          <div class="flex flex-wrap items-center gap-2">
            {#if filterActive && selectedCount === 0}
              <button
                type="button"
                onclick={clearFilters}
                class="inline-flex min-h-8 items-center gap-1 rounded-lg border border-border bg-background px-2.5 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
              >
                {m.alerts_filters_reset()}
              </button>
            {/if}
            <button
              type="button"
              class="inline-flex min-h-8 items-center gap-1.5 rounded-lg bg-primary px-3 py-1.5 text-xs font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-50"
              disabled={selectedCount === 0 || busy}
              onclick={acknowledgeSelected}
            >
              <Check class="size-3.5" />
              {bulkAcking
                ? m.alerts_bulk_acking({ count: selectedCount })
                : m.alerts_bulk_ack()}
            </button>
          </div>
        </div>
      {/if}
      {#each visibleAlerts as alert, i (alert.id)}
        <div
          data-testid="alert-row"
          class="px-5 py-4 transition-colors {selectedIds.includes(alert.id)
            ? 'bg-primary/5'
            : 'hover:bg-muted/15'} {i !== visibleAlerts.length - 1
            ? 'border-b border-border'
            : ''}"
        >
          <div class="flex flex-wrap items-start gap-3">
            <div class="flex h-8 w-4 shrink-0 items-center">
              {#if alert.status === "firing"}
                <input
                  type="checkbox"
                  checked={selectedIds.includes(alert.id)}
                  disabled={bulkAcking || acking[alert.id]}
                  onchange={() => toggleAlert(alert.id)}
                  aria-label={m.alerts_select_alert({
                    monitor: monitorName(alert.monitor_id),
                  })}
                  class="h-4 w-4 rounded border-border accent-primary disabled:cursor-not-allowed disabled:opacity-50"
                />
              {/if}
            </div>
            <div class="min-w-0 flex-1">
              <div class="flex flex-wrap items-center gap-2">
                <h3 class="font-medium tracking-tight">
                  <a
                    class="hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring rounded-sm"
                    href="/monitors/{alert.monitor_id}"
                  >
                    {monitorName(alert.monitor_id)}
                  </a>
                </h3>
                <span
                  class="inline-flex rounded-full border px-2.5 py-0.5 text-xs font-medium {statusClass(
                    alert.status,
                  )}"
                >
                  {statusLabel(alert.status)}
                </span>
                {#if alert.escalation}
                  <span
                    data-testid="alert-escalation-badge"
                    data-escalation-status={alert.escalation.status}
                    class="inline-flex rounded-full border px-2.5 py-0.5 text-xs font-medium {escalationClass(
                      alert.escalation.status,
                    )}"
                  >
                    {escalationLabel(alert.escalation)}
                  </span>
                {/if}
              </div>
              <p class="mt-1 text-sm text-muted-foreground line-clamp-2">
                {alert.message || "—"}
              </p>
              <p class="mt-2 text-xs text-muted-foreground">
                {m.alerts_page_fired_at({
                  time: new Date(alert.fired_at).toLocaleString(),
                })}
                {#if alert.acked_at}
                  · {m.alerts_page_acked_at({
                    time: new Date(alert.acked_at).toLocaleString(),
                  })}
                {/if}
                {#if alert.resolved_at}
                  · {m.alerts_page_resolved_at({
                    time: new Date(alert.resolved_at).toLocaleString(),
                  })}
                {/if}
              </p>
            </div>
            {#if alert.status === "firing"}
              <div class="ml-auto flex shrink-0 items-center">
                <button
                  type="button"
                  class="inline-flex min-h-8 items-center gap-1.5 rounded-lg border border-border bg-background px-3 py-1.5 text-xs font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50"
                  disabled={acking[alert.id] || bulkAcking}
                  onclick={() => ack(alert)}
                >
                  <Check class="size-3.5" />
                  {acking[alert.id] ? m.alerts_acking() : m.alerts_ack()}
                </button>
              </div>
            {/if}
          </div>
        </div>
      {/each}
    </div>
  {/if}
</div>
