<script lang="ts">
  import { page } from "$app/stores";
  import {
    realtime,
    type Heartbeat as WsHeartbeat,
  } from "$lib/stores/ws.svelte.js";
  import { heartbeatsApi, type Heartbeat } from "$lib/api/heartbeats.js";
  import { statsApi, type MonitorStats } from "$lib/api/stats.js";
  import { conditionsApi, type MonitorCondition } from "$lib/api/conditions";
  import { notificationsApi, type MonitorNotification } from "$lib/api/notifications";
  import { tagsApi, type MonitorTag, type Tag } from "$lib/api/tags";
  import StatusPill from "$lib/components/StatusPill.svelte";
  import MetricCard from "$lib/components/MetricCard.svelte";
  import ResponseTimeChart, {
    type ChartPayload,
  } from "$lib/components/ResponseTimeChart.svelte";
  import StatusHistoryTable from "$lib/components/StatusHistoryTable.svelte";
  import RecentCheckBar from "$lib/components/RecentCheckBar.svelte";
  import MonitorConditions from "$lib/components/MonitorConditions.svelte";
  import MonitorForm from "$lib/components/MonitorForm.svelte";
  import EmptyState from "$lib/components/EmptyState.svelte";
  import Skeleton from "$lib/components/Skeleton.svelte";
  import { monitorsApi } from "$lib/api/monitors";
  import type { Monitor } from "$lib/stores/ws.svelte.ts";
  import {
    acceptLiveHeartbeat,
    deriveStatusHint,
    latestObservedTime,
    monitorDataSource,
    monitorFromApi,
    resolveDisplayedMonitor,
    shouldStopPostClearPolling,
  } from "$lib/monitor-detail-state";
  import { confirmAction } from "$lib/stores/confirm.svelte";
  import { toast } from "svelte-sonner";
  import {
    ArrowLeft,
    Bell,
    Tag as TagIcon,
    Activity,
    Pause,
    Play,
    Pencil,
    Copy,
    Trash2,
    ExternalLink,
    BadgeCheck,
    ClipboardCopy,
    AlertTriangle,
  } from "@lucide/svelte";
  import { goto } from "$app/navigation";
  import { untrack } from "svelte";
  import * as m from "$lib/paraglide/messages.js";
  import Select from "$lib/components/Select.svelte";

  let monitorId = $derived(Number($page.params.id));
  let fetchedMonitor = $state<Monitor | null>(null);
  let monitor = $derived.by(() => {
    const base = resolveDisplayedMonitor(
      realtime.monitors.find((mo) => mo.id === monitorId) ?? null,
      fetchedMonitor,
    );
    if (!base) return null;
    const latestTimeline = timelineHeartbeats[timelineHeartbeats.length - 1];
    const hint = deriveStatusHint(
      statusHistory[0] ?? latestTimeline,
      realtime.heartbeats.get(monitorId),
      stats,
    );
    if (!hint || base.active === false) return base;
    return hint !== base.status ? { ...base, status: hint } : base;
  });

  let chartData = $state<ChartPayload | null>(null);
  let timelineHeartbeats = $state<Heartbeat[]>([]);
  let statusHistory = $state<Heartbeat[]>([]);
  let stats = $state<MonitorStats | null>(null);
  let conditionClock = $state(Date.now());
  let chartHours = $state(24);
  let chartLoading = $state(false);
  let chartRequestGeneration = 0;
  let lastKnownStatus = $state<string | null>(null);
  let lastProcessedHeartbeatTime = $state<string | null>(null);

  let assignedNotifications = $state<MonitorNotification[]>([]);
  let allNotifications = $state<any[]>([]);
  let assignedTags = $state<MonitorTag[]>([]);
  let allTags = $state<Tag[]>([]);
  let loading = $state(true);
  let loadError = $state<string | null>(null);
  let selectedNotifToAdd = $state("");
  let selectedTagToAdd = $state("");
  let tagValueInput = $state("");
  let showEditForm = $state(false);
  let clearingHistory = $state(false);
  let actionLoading = $state(false);
  let historyClearedAt = $state<string | null>(null);
  let postClearPollGeneration = 0;
  let detailsGeneration = 0;
  let dataSource = $derived(
    monitorDataSource(monitorId, realtime.monitors, fetchedMonitor),
  );
  /** Source at first successful load — unchanged when WS hydrates later. */
  let initialDataSource = $state<"api" | "ws" | null>(null);

  let monitorConditions = $derived.by((): MonitorCondition[] => {
    void realtime.conditionSeq;
    return [...realtime.conditions.values()]
      .filter((condition) => condition.monitor_id === monitorId)
      .sort((a, b) => a.kind.localeCompare(b.kind));
  });

  $effect(() => {
    const timer = setInterval(() => (conditionClock = Date.now()), 30_000);
    return () => clearInterval(timer);
  });

  $effect(() => {
    const id = monitorId;
    if (!id) return;
    // loadDetails reads conditionSeq (via beginConditionSnapshot) and then
    // applyConditionSnapshot increments it. That pair must stay untracked or
    // this effect refetches in a loop until the API returns 429.
    untrack(() => {
      stopPostClearPolling();
      initialDataSource = null;
      void loadDetails();
    });
  });

  function stopPostClearPolling() {
    postClearPollGeneration += 1;
    historyClearedAt = null;
  }

  async function runPostClearPoll(gen: number, attempt: number) {
    if (gen !== postClearPollGeneration || !historyClearedAt) return;
    if (attempt >= 8) {
      stopPostClearPolling();
      return;
    }
    await refreshObservabilityAfterClear();
    if (gen !== postClearPollGeneration || !historyClearedAt) return;
    if (
      shouldStopPostClearPolling(
        timelineHeartbeats.length,
        statusHistory.length,
      )
    ) {
      stopPostClearPolling();
      return;
    }
    setTimeout(() => void runPostClearPoll(gen, attempt + 1), 1000);
  }

  function startPostClearPolling() {
    const gen = ++postClearPollGeneration;
    void runPostClearPoll(gen, 0);
  }

  function liveHeartbeatEntry(live: WsHeartbeat): Heartbeat {
    return {
      id: -Date.now(),
      monitor_id: monitorId,
      status: live.status as Heartbeat["status"],
      ping: live.ping,
      message: live.msg ?? "",
      time: live.time,
      important: true,
    };
  }

  // Merge live WS heartbeats into chart + status history.
  $effect(() => {
    // Read the map reference so Svelte tracks WS heartbeat updates.
    const hbMap = realtime.heartbeats;
    const live = hbMap.get(monitorId);
    if (
      !live ||
      !acceptLiveHeartbeat({
        clearedAt: historyClearedAt,
        hbTime: live.time,
        lastProcessedTime: lastProcessedHeartbeatTime,
      })
    ) {
      return;
    }

    lastProcessedHeartbeatTime = live.time;
    const newStatus = live.status;
    const entry = liveHeartbeatEntry(live);

    if (lastKnownStatus !== null && lastKnownStatus !== newStatus) {
      // Status transition — prepend a new history row.
      if (
        !statusHistory.some(
          (h) => h.time === entry.time && h.status === entry.status,
        )
      ) {
        statusHistory = [entry, ...statusHistory].slice(0, 100);
      }
    } else if (
      statusHistory.length > 0 &&
      statusHistory[0].status === newStatus
    ) {
      // Same status — refresh the leading row with the latest check.
      statusHistory = [
        {
          ...statusHistory[0],
          ping: live.ping,
          message: live.msg ?? statusHistory[0].message,
          time: live.time,
        },
        ...statusHistory.slice(1),
      ];
    } else if (statusHistory.length === 0) {
      statusHistory = [entry];
    }

    lastKnownStatus = newStatus;

    const timelineEntry: Heartbeat = {
      id: 0,
      monitor_id: monitorId,
      status: live.status as Heartbeat["status"],
      ping: live.ping,
      message: live.msg ?? "",
      time: live.time,
      important: false,
    };
    if (timelineHeartbeats.length === 0) {
      timelineHeartbeats = [timelineEntry];
    } else {
      const latestTimeline = timelineHeartbeats[timelineHeartbeats.length - 1];
      const liveTime = new Date(live.time).getTime();
      const timelineTime = latestTimeline
        ? new Date(latestTimeline.time).getTime()
        : 0;
      if (liveTime > timelineTime) {
        timelineHeartbeats = [...timelineHeartbeats, timelineEntry].slice(-60);
      }
    }

    if (
      historyClearedAt &&
      shouldStopPostClearPolling(
        timelineHeartbeats.length,
        statusHistory.length,
      )
    ) {
      stopPostClearPolling();
    }

    if (stats) {
      stats = { ...stats, current_ping_ms: live.ping };
    }
  });

  async function refreshObservabilityAfterClear() {
    const [history, timeline, statsData] = await Promise.all([
      heartbeatsApi
        .listOptions(monitorId, {
          hours: 720,
          limit: 100,
          order: "desc",
          important: true,
        })
        .catch(() => []),
      heartbeatsApi
        .listOptions(monitorId, { hours: 24, limit: 60, order: "asc" })
        .catch(() => []),
      statsApi.get(monitorId).catch(() => null),
    ]);
    // Do not clobber WS-merged rows with stale empty API responses right after clear.
    if (history.length > 0) statusHistory = history;
    if (timeline.length > 0) timelineHeartbeats = timeline;
    if (statsData) stats = statsData;
    if (history[0]) {
      lastKnownStatus = history[0].status;
    }
    const hint = deriveStatusHint(
      history[0],
      realtime.heartbeats.get(monitorId),
      statsData,
    );
    if (fetchedMonitor && hint) {
      fetchedMonitor = monitorFromApi(fetchedMonitor, hint);
    }
    if (shouldStopPostClearPolling(timeline.length, history.length)) {
      stopPostClearPolling();
    }
  }

  async function loadChartData(hours: number) {
    chartHours = hours;
    const generation = ++chartRequestGeneration;
    chartLoading = true;
    const nextChart = await heartbeatsApi
      .chart(monitorId, hours)
      .catch(() => null);
    if (generation !== chartRequestGeneration) return;
    chartData = nextChart;
    chartLoading = false;
  }

  async function loadDetails() {
    const generation = ++detailsGeneration;
    loading = true;
    loadError = null;
    try {
      const snapshotAt = realtime.beginConditionSnapshot();
      const [monitorData, statsData, history, chart, timeline, conditions] =
        await Promise.all([
          monitorsApi.get(monitorId),
          statsApi.get(monitorId).catch(() => null),
          heartbeatsApi
            .listOptions(monitorId, {
              hours: 720,
              limit: 100,
              order: "desc",
              important: true,
            })
            .catch(() => []),
          heartbeatsApi.chart(monitorId, chartHours).catch(() => null),
          heartbeatsApi
            .listOptions(monitorId, { hours: 24, limit: 60, order: "asc" })
            .catch(() => []),
          conditionsApi.list(monitorId).catch(() => null),
        ]);
      if (generation !== detailsGeneration) return;

      stats = statsData;
      statusHistory = history;
      chartData = chart;
      timelineHeartbeats = timeline;
      realtime.applyConditionSnapshot(conditions, snapshotAt, monitorId);

      const latestHistory = history[0];
      const liveHb = realtime.heartbeats.get(monitorId);
      const statusHint = deriveStatusHint(latestHistory, liveHb, statsData);

      if (monitorData) {
        fetchedMonitor = monitorFromApi(monitorData, statusHint);
        if (initialDataSource === null) {
          initialDataSource = realtime.monitors.some((m) => m.id === monitorId)
            ? "ws"
            : "api";
        }
      }

      // Seed status tracker; skip re-processing the current WS heartbeat.
      lastKnownStatus =
        latestHistory?.status ?? monitor?.status ?? liveHb?.status ?? null;
      lastProcessedHeartbeatTime = liveHb?.time ?? null;

      assignedNotifications = await notificationsApi.listForMonitor(monitorId);
      if (generation !== detailsGeneration) return;
      allNotifications = await notificationsApi.list();
      if (generation !== detailsGeneration) return;

      try {
        assignedTags = await tagsApi.listForMonitor(monitorId);
      } catch {
        assignedTags = [];
      }
      try {
        allTags = await tagsApi.list();
      } catch {
        allTags = [];
      }
    } catch (e: any) {
      if (generation !== detailsGeneration) return;
      const message = e?.message || m.monitor_detail_page_load_failed();
      loadError = message;
      toast.error(message);
    } finally {
      if (generation === detailsGeneration) {
        loading = false;
      }
    }
  }

  async function handleChartRangeChange(hours: number) {
    await loadChartData(hours);
  }

  async function assignNotification() {
    if (!selectedNotifToAdd) return;
    try {
      await notificationsApi.assignToMonitor(
        monitorId,
        Number(selectedNotifToAdd),
      );
      toast.success(m.monitor_detail_page_notification_assigned());
      selectedNotifToAdd = "";
      await loadDetails();
    } catch (e: any) {
      toast.error(e?.message || m.monitor_detail_page_assign_failed());
    }
  }

  async function assignTag() {
    if (!selectedTagToAdd) return;
    try {
      await tagsApi.assignToMonitor(
        monitorId,
        Number(selectedTagToAdd),
        tagValueInput.trim() || undefined,
      );
      toast.success(m.monitor_detail_page_tag_assigned());
      selectedTagToAdd = "";
      tagValueInput = "";
      await loadDetails();
    } catch (e: any) {
      toast.error(e?.message || m.monitor_detail_page_assign_tag_failed());
    }
  }

  async function unassignTag(tagId: number) {
    const tag = tagMeta(tagId);
    const ok = await confirmAction({
      title: tag
        ? m.monitor_detail_page_remove_tag_named({ name: tag.name })
        : m.monitor_detail_page_remove_tag_generic(),
      message: m.monitor_detail_page_remove_tag_message(),
      confirmLabel: m.monitor_detail_page_remove_tag_confirm(),
      destructive: true,
    });
    if (!ok) return;
    try {
      await tagsApi.unassignFromMonitor(monitorId, tagId);
      toast.success(m.monitor_detail_page_tag_removed());
      await loadDetails();
    } catch (e: any) {
      toast.error(e?.message || m.monitor_detail_page_remove_tag_failed());
    }
  }

  function tagMeta(tagId: number): Tag | undefined {
    return (
      allTags.find((t) => t.id === tagId) ??
      assignedTags.find((mt) => mt.tag_id === tagId)?.tag
    );
  }

  async function unassignNotification(nid: number) {
    const name = assignedNotifications.find((n) => n.id === nid)?.name;
    const ok = await confirmAction({
      title: name
        ? m.monitor_detail_page_stop_sending_named({ name })
        : m.monitor_detail_page_stop_sending_generic(),
      message: m.monitor_detail_page_unassign_message(),
      confirmLabel: m.monitor_detail_page_unassign_confirm(),
      destructive: true,
    });
    if (!ok) return;
    try {
      await notificationsApi.unassignFromMonitor(monitorId, nid);
      toast.success(m.monitor_detail_page_notification_unassigned());
      await loadDetails();
    } catch (e: any) {
      toast.error(e?.message || m.monitor_detail_page_unassign_failed());
    }
  }

  async function toggleIncludeTarget(n: MonitorNotification) {
    const next = !n.include_target;
    // Optimistic flip so the checkbox responds immediately; reload reconciles.
    n.include_target = next;
    try {
      await notificationsApi.setMonitorIncludeTarget(monitorId, n.id, next);
    } catch (e: any) {
      n.include_target = !next;
      toast.error(e?.message || m.monitor_detail_page_assign_failed());
    }
  }

  function goBack() {
    goto("/monitors");
  }

  const targetUrl = $derived(() => {
    if (!monitor) return "";
    // Prefer the authoritative config value over the denormalized `target`
    // field. `target` is a WS-only convenience field computed by the server
    // from config — after an optimistic edit patch it can be stale because
    // the HTTP API response does not include `target`.
    const cfg = monitor.config ?? {};
    if (typeof cfg.url === "string") return cfg.url;
    if (typeof cfg.hostname === "string") {
      const port = cfg.port != null ? `:${cfg.port}` : "";
      return `${cfg.hostname}${port}`;
    }
    return "";
  });

  const isPaused = $derived(
    monitor?.status === "paused" || monitor?.active === false,
  );

  async function togglePause() {
    if (!monitor) return;
    actionLoading = true;
    try {
      const updated = isPaused
        ? await monitorsApi.resume(monitorId)
        : await monitorsApi.pause(monitorId);
      const patched = monitorFromApi(
        updated,
        updated.active === false
          ? "paused"
          : monitor.status === "paused"
            ? "pending"
            : monitor.status,
      );
      fetchedMonitor = patched;
      realtime.patchMonitor(patched);
      toast.success(
        isPaused
          ? m.monitor_detail_page_resumed_toast()
          : m.monitor_detail_page_paused_toast(),
      );
    } catch (e: any) {
      toast.error(e?.message || m.monitor_detail_page_action_failed());
    } finally {
      actionLoading = false;
    }
  }

  async function handleClone() {
    if (!monitor) return;
    actionLoading = true;
    try {
      const cloned = await monitorsApi.clone(monitorId);
      toast.success(m.monitor_detail_page_cloned_toast({ name: cloned.name }));
      goto(`/monitors/${cloned.id}`);
    } catch (e: any) {
      toast.error(e?.message || m.monitor_detail_page_clone_failed());
    } finally {
      actionLoading = false;
    }
  }

  async function handleDelete() {
    if (!monitor) return;
    const ok = await confirmAction({
      title: m.monitors_page_delete_title({ name: monitor.name }),
      message: m.monitors_page_delete_message(),
      confirmLabel: m.monitors_page_delete_confirm(),
      destructive: true,
    });
    if (!ok) return;
    actionLoading = true;
    try {
      await monitorsApi.remove(monitorId);
      toast.success(m.monitors_page_deleted_toast());
      goto("/monitors");
    } catch (e: any) {
      toast.error(e?.message || m.monitors_page_delete_failed());
    } finally {
      actionLoading = false;
    }
  }

  async function handleClearHistory() {
    clearingHistory = true;
    try {
      const anchor =
        latestObservedTime([...statusHistory, ...timelineHeartbeats]) ??
        "1970-01-01T00:00:00.000Z";
      lastKnownStatus = null;
      lastProcessedHeartbeatTime = anchor;
      await heartbeatsApi.clear(monitorId);
      historyClearedAt = anchor;
      statusHistory = [];
      chartData = { buckets: [], downtime_intervals: [] };
      timelineHeartbeats = [];
      stats = stats
        ? {
            ...stats,
            current_ping_ms: 0,
            avg_ping_24h: 0,
            uptime_24h: null,
            uptime_30d: stats.uptime_30d,
          }
        : null;
      startPostClearPolling();
      toast.success(m.monitor_detail_page_history_cleared());
    } catch (e: any) {
      stopPostClearPolling();
      toast.error(e?.message || m.monitor_detail_page_clear_failed());
    } finally {
      clearingHistory = false;
    }
  }

  const currentPing = $derived(() => {
    const live = realtime.heartbeats.get(monitorId);
    if (live?.ping && live.ping > 0) return live.ping;
    return stats?.current_ping_ms ?? 0;
  });

  const avgPing24h = $derived(
    stats?.avg_ping_24h != null ? Math.round(stats.avg_ping_24h) : null,
  );

  const uptime24h = $derived(
    stats?.uptime_24h != null ? `${stats.uptime_24h.toFixed(2)}%` : "—",
  );

  const uptime30d = $derived(
    stats?.uptime_30d != null ? `${stats.uptime_30d.toFixed(2)}%` : "—",
  );

  const certExpiry = $derived(() => {
    if (!stats?.cert_expiry_date) return null;
    const date = new Date(stats.cert_expiry_date);
    const days = stats.cert_days_left ?? 0;
    return {
      date: date.toISOString().slice(0, 10),
      days,
    };
  });

  const showCert = $derived(
    stats?.cert_expiry_date != null && stats?.cert_days_left != null,
  );

  // --- Status badge (embeddable shields.io-style SVGs, public endpoints) ---
  const badgeOrigin = $derived($page.url.origin);
  const statusBadgeUrl = $derived(
    `${badgeOrigin}/api/badge/${monitorId}/status.svg`,
  );
  const uptimeBadgeUrl = $derived(
    `${badgeOrigin}/api/badge/${monitorId}/uptime.svg?duration=24h`,
  );
  const pingBadgeUrl = $derived(
    `${badgeOrigin}/api/badge/${monitorId}/ping.svg`,
  );
  const badgeMarkdownSnippet = $derived(
    `[![${monitor?.name ?? "Status"}](${statusBadgeUrl})](${badgeOrigin}) [![Uptime](${uptimeBadgeUrl})](${badgeOrigin})`,
  );

  async function copyToClipboard(text: string, what: string) {
    try {
      await navigator.clipboard.writeText(text);
      toast.success(m.monitor_detail_page_copied_toast({ what }));
    } catch {
      toast.error(m.monitor_detail_page_copy_failed());
    }
  }
</script>

<svelte:head>
  <title
    >{m.app_name()} · {monitor?.name ??
      m.monitor_detail_page_title_fallback()}</title
  >
</svelte:head>

<div class="max-w-6xl space-y-6">
  <button
    onclick={goBack}
    class="inline-flex items-center gap-2 text-sm text-muted-foreground transition-colors hover:text-foreground"
  >
    <ArrowLeft class="h-4 w-4" />
    {m.monitor_detail_page_back_to_monitors()}
  </button>

  {#snippet retryLoadAction()}
    <button
      type="button"
      onclick={loadDetails}
      class="inline-flex items-center gap-2 rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >{m.monitor_group_form_retry()}</button
    >
  {/snippet}

  {#if loading && !monitor}
    <div class="space-y-6" role="status">
      <span class="sr-only">{m.monitor_detail_page_loading()}</span>
      <div class="flex items-start justify-between gap-4">
        <div class="space-y-3">
          <Skeleton class="h-8 w-64 max-w-full" />
          <Skeleton class="h-4 w-48 max-w-full" />
        </div>
        <Skeleton class="h-9 w-28" />
      </div>
      <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {#each Array(4) as _}
          <div class="rounded-xl border border-border bg-card p-5">
            <Skeleton class="h-3 w-24" />
            <Skeleton class="mt-4 h-8 w-20" />
          </div>
        {/each}
      </div>
      <div class="rounded-xl border border-border bg-card p-5">
        <Skeleton class="h-5 w-40" />
        <Skeleton class="mt-6 h-64 w-full" />
      </div>
    </div>
  {:else if loadError}
    <EmptyState
      icon={AlertTriangle}
      title={m.monitor_detail_page_load_failed()}
      description={loadError}
      action={retryLoadAction}
    />
  {:else if !monitor}
    {#snippet notFoundAction()}
      <button
        type="button"
        onclick={goBack}
        class="inline-flex items-center gap-2 rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent"
      >
        <ArrowLeft class="h-4 w-4" />
        {m.monitor_detail_page_back_to_monitors_lower()}
      </button>
    {/snippet}
    <EmptyState
      icon={Activity}
      title={m.monitor_detail_page_not_found_title()}
      description={m.monitor_detail_page_not_found_description()}
      action={notFoundAction}
    />
  {:else}
    <!-- Header + actions -->
    <div
      data-testid="monitor-detail-root"
      data-monitor-source={dataSource}
      data-initial-monitor-source={initialDataSource ?? "loading"}
      class="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between"
    >
      <div class="min-w-0 flex-1">
        <h1 class="truncate text-2xl font-semibold tracking-tight">
          {monitor.name}
        </h1>
        <p class="mt-1 text-sm text-muted-foreground">
          <span class="font-mono text-xs">{monitor.type}</span>
          <span class="mx-1 text-faint">·</span>
          {m.monitor_detail_page_every_seconds({
            seconds: monitor.interval || 60,
          })}
        </p>
        {#if monitor.effective_owner || monitor.owner}
          <p class="mt-1 text-sm text-muted-foreground">
            {m.monitor_detail_page_owner({
              owner: monitor.effective_owner || monitor.owner || "",
            })}
            {#if monitor.inherit_group_owner}
              <span class="text-faint">
                · {m.monitor_detail_page_owner_inherited()}</span
              >
            {/if}
          </p>
        {/if}
        {#if targetUrl()}
          {@const url = targetUrl()}
          {@const href =
            url.startsWith("http://") || url.startsWith("https://")
              ? url
              : monitor.type === "http" || monitor.type === "websocket"
                ? `https://${url}`
                : null}
          {#if href}
            <a
              {href}
              target="_blank"
              rel="noopener noreferrer"
              class="mt-2 inline-flex items-center gap-1.5 text-sm text-primary transition-colors hover:text-primary/80"
            >
              <span class="break-all font-mono text-xs">{url}</span>
              <ExternalLink class="h-3.5 w-3.5 shrink-0" />
            </a>
          {:else}
            <p class="mt-2 font-mono text-xs text-muted-foreground">{url}</p>
          {/if}
        {/if}
      </div>
      <div
        class="flex flex-wrap items-center gap-2"
        data-testid="monitor-toolbar"
      >
        <div data-testid="monitor-status-pill">
          <StatusPill status={monitor.status} />
        </div>
        <button
          type="button"
          data-testid="monitor-toolbar-pause"
          onclick={togglePause}
          disabled={actionLoading}
          class="inline-flex items-center gap-1.5 rounded-lg border border-border px-3 py-1.5 text-sm transition-colors hover:bg-accent disabled:opacity-50"
          title={isPaused
            ? m.monitor_detail_page_resume()
            : m.monitor_detail_page_pause()}
        >
          {#if isPaused}<Play class="h-4 w-4" />{:else}<Pause
              class="h-4 w-4"
            />{/if}
          {isPaused
            ? m.monitor_detail_page_resume()
            : m.monitor_detail_page_pause()}
        </button>
        <button
          type="button"
          onclick={() => (showEditForm = true)}
          class="inline-flex items-center gap-1.5 rounded-lg border border-border px-3 py-1.5 text-sm transition-colors hover:bg-accent"
        >
          <Pencil class="h-4 w-4" />
          {m.btn_edit()}
        </button>
        <button
          type="button"
          onclick={handleClone}
          disabled={actionLoading}
          class="inline-flex items-center gap-1.5 rounded-lg border border-border px-3 py-1.5 text-sm transition-colors hover:bg-accent disabled:opacity-50"
        >
          <Copy class="h-4 w-4" />
          {m.monitor_detail_page_clone()}
        </button>
        <button
          type="button"
          onclick={handleDelete}
          disabled={actionLoading}
          class="inline-flex items-center gap-1.5 rounded-lg border border-danger/30 px-3 py-1.5 text-sm text-danger transition-colors hover:bg-danger/10 disabled:opacity-50"
        >
          <Trash2 class="h-4 w-4" />
          {m.btn_delete()}
        </button>
      </div>
    </div>

    <RecentCheckBar heartbeats={timelineHeartbeats} />

    <!-- Metrics row (Uptime Kuma style) -->
    <div
      class="grid gap-3 sm:grid-cols-2 {showCert
        ? 'lg:grid-cols-5'
        : 'lg:grid-cols-4'}"
    >
      <MetricCard
        label={m.monitor_detail_page_metric_response_current()}
        value={currentPing() > 0 ? currentPing() : "—"}
        unit={currentPing() > 0 ? "ms" : undefined}
        highlight
      />
      <MetricCard
        label={m.monitor_detail_page_metric_avg_response()}
        value={avgPing24h != null ? avgPing24h : "—"}
        unit={avgPing24h != null ? "ms" : undefined}
      />
      <MetricCard
        label={m.monitor_detail_page_metric_uptime_24h()}
        value={uptime24h}
      />
      <MetricCard
        label={m.monitor_detail_page_metric_uptime_30d()}
        value={uptime30d}
      />
      {#if showCert}
        <MetricCard
          label={m.monitor_detail_page_metric_cert_exp()}
          value={certExpiry() ? certExpiry()!.date : "—"}
          unit={certExpiry()
            ? m.monitor_detail_page_days_unit({ days: certExpiry()!.days })
            : undefined}
        />
      {/if}
    </div>

    <MonitorConditions conditions={monitorConditions} now={conditionClock} />

    <!-- Response time chart -->
    <ResponseTimeChart
      chart={chartData}
      selectedHours={chartHours}
      onRangeChange={handleChartRangeChange}
      loading={chartLoading}
    />

    <!-- Status history table -->
    <StatusHistoryTable
      heartbeats={statusHistory}
      onClear={handleClearHistory}
      clearing={clearingHistory}
      monitorName={monitor?.name}
    />

    <!-- Config + Tags/Notifications -->
    <div class="grid gap-4 md:grid-cols-2">
      <div class="rounded-xl border border-border bg-card p-5">
        <h3
          class="flex items-center gap-2 text-sm font-semibold tracking-tight"
        >
          <Activity class="h-4 w-4 text-muted-foreground" />
          {m.monitor_detail_config()}
        </h3>
        <div class="mt-4 space-y-2 text-sm">
          <div class="flex items-center justify-between gap-4">
            <span class="shrink-0 text-muted-foreground"
              >{m.monitors_target()}</span
            >
            <span class="break-all text-right font-mono text-xs"
              >{targetUrl() ||
                JSON.stringify(monitor.config).slice(0, 60)}</span
            >
          </div>
          <div
            class="flex items-center justify-between border-t border-border pt-2"
          >
            <span class="text-muted-foreground"
              >{m.monitor_detail_page_created_label()}</span
            >
            <span class="tnum"
              >{new Date(monitor.created_at).toLocaleDateString()}</span
            >
          </div>
        </div>
      </div>

      <!-- Assigned Tags -->
      <div class="rounded-xl border border-border bg-card p-5">
        <div
          class="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between"
        >
          <h3
            class="flex items-center gap-2 text-sm font-semibold tracking-tight"
          >
            <TagIcon class="h-4 w-4 text-muted-foreground" />
            {m.monitor_detail_tags()}
          </h3>
          <div class="flex flex-wrap gap-2">
            <div class="flex-1 sm:w-auto">
              <Select
                options={[
                  { value: "", label: m.monitor_detail_page_select_tag() },
                  ...allTags
                    .filter((t) => !assignedTags.some((a) => a.tag_id === t.id))
                    .map((t) => ({ value: String(t.id), label: t.name })),
                ]}
                value={selectedTagToAdd}
                ariaLabel={m.monitor_detail_page_select_tag()}
                onValueChange={(v) => (selectedTagToAdd = v)}
                class="w-full"
              />
            </div>
            <input
              type="text"
              bind:value={tagValueInput}
              placeholder={m.monitor_detail_page_optional_value()}
              class="w-full rounded-lg border border-border bg-surface px-3 py-1.5 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring sm:w-32"
            />
            <button
              type="button"
              onclick={assignTag}
              disabled={!selectedTagToAdd}
              class="inline-flex items-center gap-1 rounded-lg bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 disabled:opacity-50"
              >{m.btn_assign()}</button
            >
          </div>
        </div>
        {#if assignedTags.length === 0}
          <p class="mt-4 text-sm text-muted-foreground">
            {m.monitor_detail_no_tags()}
          </p>
        {:else}
          <div class="mt-4 flex flex-wrap gap-2">
            {#each assignedTags as mt (mt.tag_id)}
              {@const meta = tagMeta(mt.tag_id)}
              {@const color = meta?.color ?? "#666666"}
              <span
                class="inline-flex items-center gap-2 rounded px-1.5 py-0.5 text-[11px] font-medium"
                style="background-color: {color}22; color: {color}; border: 1px solid {color}44;"
              >
                {meta?.name ??
                  m.monitor_detail_page_tag_fallback({ id: mt.tag_id })}
                {#if mt.value}
                  <span class="opacity-80">= {mt.value}</span>
                {/if}
                <button
                  type="button"
                  class="opacity-70 transition-opacity hover:opacity-100"
                  onclick={() => unassignTag(mt.tag_id)}
                  aria-label={m.monitor_detail_page_remove_tag_confirm()}
                  >×</button
                >
              </span>
            {/each}
          </div>
        {/if}
      </div>
    </div>

    <!-- Status badge -->
    <div class="rounded-xl border border-border bg-card p-5">
      <h3 class="flex items-center gap-2 text-sm font-semibold tracking-tight">
        <BadgeCheck class="h-4 w-4 text-muted-foreground" />
        {m.monitor_detail_page_status_badge_heading()}
      </h3>
      <p class="mt-1 text-sm text-muted-foreground">
        {m.monitor_detail_page_status_badge_description()}
      </p>

      <div class="mt-4 flex flex-wrap items-center gap-4">
        <img
          src={statusBadgeUrl}
          alt={m.monitor_detail_page_badge_alt_status({ name: monitor.name })}
          class="h-5"
        />
        <img
          src={uptimeBadgeUrl}
          alt={m.monitor_detail_page_badge_alt_uptime({ name: monitor.name })}
          class="h-5"
        />
        <img
          src={pingBadgeUrl}
          alt={m.monitor_detail_page_badge_alt_ping({ name: monitor.name })}
          class="h-5"
        />
      </div>

      <div class="mt-4 space-y-2">
        {#each [{ label: m.monitor_detail_page_badge_status(), url: statusBadgeUrl }, { label: m.monitor_detail_page_badge_uptime_24h(), url: uptimeBadgeUrl }, { label: m.monitor_detail_page_badge_ping(), url: pingBadgeUrl }] as badge (badge.label)}
          <div class="flex items-center gap-2">
            <span class="w-24 shrink-0 text-xs text-muted-foreground"
              >{badge.label}</span
            >
            <code
              class="min-w-0 flex-1 truncate rounded-lg border border-border bg-surface px-3 py-1.5 font-mono text-xs"
              >{badge.url}</code
            >
            <button
              type="button"
              onclick={() =>
                copyToClipboard(
                  badge.url,
                  m.monitor_detail_page_badge_url_label({ label: badge.label }),
                )}
              class="inline-flex shrink-0 items-center gap-1.5 rounded-lg border border-border px-2.5 py-1.5 text-xs transition-colors hover:bg-accent"
            >
              <Copy class="h-3.5 w-3.5" />
              {m.monitor_detail_page_copy()}
            </button>
          </div>
        {/each}

        <div class="flex items-start gap-2 border-t border-border pt-3">
          <span class="w-24 shrink-0 pt-1.5 text-xs text-muted-foreground"
            >{m.monitor_detail_page_markdown_label()}</span
          >
          <code
            class="min-w-0 flex-1 whitespace-pre-wrap break-all rounded-lg border border-border bg-surface px-3 py-1.5 font-mono text-xs"
            >{badgeMarkdownSnippet}</code
          >
          <button
            type="button"
            onclick={() =>
              copyToClipboard(
                badgeMarkdownSnippet,
                m.monitor_detail_page_markdown_snippet(),
              )}
            class="inline-flex shrink-0 items-center gap-1.5 rounded-lg border border-border px-2.5 py-1.5 text-xs transition-colors hover:bg-accent"
          >
            <ClipboardCopy class="h-3.5 w-3.5" />
            {m.monitor_detail_page_copy()}
          </button>
        </div>
      </div>
    </div>

    <!-- Assigned Notifications -->
    <div class="rounded-xl border border-border bg-card p-5">
      <div
        class="flex flex-col gap-2 sm:flex-row sm:items-center sm:justify-between"
      >
        <h3
          class="flex items-center gap-2 text-sm font-semibold tracking-tight"
        >
          <Bell class="h-4 w-4 text-muted-foreground" />
          {m.monitor_detail_notifications()}
        </h3>
        <div class="flex flex-wrap gap-2">
          <div class="flex-1 sm:w-auto">
            <Select
              options={[
                { value: "", label: m.monitor_detail_page_select_to_assign() },
                ...allNotifications
                  .filter(
                    (n) => !assignedNotifications.some((a) => a.id === n.id),
                  )
                  .map((n) => ({
                    value: String(n.id),
                    label: `${n.name} (${n.type})`,
                  })),
              ]}
              value={selectedNotifToAdd}
              ariaLabel={m.monitor_detail_page_select_to_assign()}
              onValueChange={(v) => (selectedNotifToAdd = v)}
              class="w-full"
            />
          </div>
          <button
            onclick={assignNotification}
            disabled={!selectedNotifToAdd}
            class="inline-flex items-center gap-1 rounded-lg bg-primary px-3 py-1.5 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 disabled:opacity-50"
            >{m.btn_assign()}</button
          >
        </div>
      </div>

      {#if assignedNotifications.length === 0}
        <p class="mt-4 text-sm text-muted-foreground">
          {m.monitor_detail_no_notifications()}
        </p>
      {:else}
        <div class="mt-4 overflow-hidden rounded-lg border border-border">
          {#each assignedNotifications as n, i (n.id)}
            <div
              class="flex items-center justify-between px-4 py-3 text-sm transition-colors hover:bg-accent/40 {i !==
              assignedNotifications.length - 1
                ? 'border-b border-border'
                : ''}"
            >
              <div>
                <div class="font-medium">{n.name}</div>
                <div class="text-xs text-muted-foreground">{n.type}</div>
              </div>
              <div class="flex items-center gap-3">
                <label class="flex items-center gap-1.5 text-xs text-muted-foreground">
                  <input
                    type="checkbox"
                    checked={n.include_target}
                    onchange={() => toggleIncludeTarget(n)}
                    class="h-4 w-4 rounded border-border accent-primary"
                  />
                  {m.monitor_detail_include_target()}
                </label>
                <button
                  onclick={() => unassignNotification(n.id)}
                  class="text-xs text-danger transition-colors hover:text-danger/80"
                  >{m.btn_remove()}</button
                >
              </div>
            </div>
          {/each}
        </div>
      {/if}
    </div>
  {/if}
</div>

{#if showEditForm && monitor}
  <MonitorForm
    {monitor}
    onSaved={() => {
      showEditForm = false;
      loadDetails();
    }}
    onClose={() => (showEditForm = false)}
  />
{/if}
