<script lang="ts">
  import { onMount, untrack } from "svelte";
  import { realtime } from "$lib/stores/ws.svelte.js";
  import {
    regionalApi,
    type MonitorHealthView,
    type RegionalHeartbeat,
  } from "$lib/api/regional";
  import { errorCode, errorMessage } from "$lib/api/probes";
  import ProbeStatus from "./ProbeStatus.svelte";
  import * as m from "$lib/paraglide/messages.js";
  let {
    monitorId,
    onSelect,
  }: { monitorId: number; onSelect?: (probeId: string) => void } = $props();
  let health = $state<MonitorHealthView | null>(null);
  let histories = $state<Record<string, RegionalHeartbeat[]>>({});
  let error = $state("");
  let unavailable = $state(false);
  let loading = $state(true);
  let active = true;
  let inFlight = false;
  let heartbeatVersion = 0;
  const time = (value: string | null) =>
    value ? new Date(value).toLocaleString() : m.probes_unreported();
  async function load(withHistory = true) {
    if (inFlight) return;
    inFlight = true;
    try {
      const next = await regionalApi.health(monitorId);
      if (!active) return;
      health = next;
      error = "";
      if (!withHistory) return;
      const historyVersion = heartbeatVersion;
      const rows = await Promise.all(
        next.regions.map(
          async (region) =>
            [
              region.probe_id,
              await regionalApi
                .history(monitorId, region.probe_id, {
                  hours: 24,
                  limit: 30,
                  order: "desc",
                })
                .catch(() => []),
            ] as const,
        ),
      );
      if (active) {
        histories = Object.fromEntries(
          rows.map(([id, snapshot]) => {
            if (heartbeatVersion === historyVersion) return [id, snapshot];
            const merged = new Map(snapshot.map((beat) => [beat.id, beat]));
            for (const beat of histories[id] ?? []) merged.set(beat.id, beat);
            return [
              id,
              [...merged.values()]
                .sort(
                  (a, b) =>
                    Date.parse(b.time) - Date.parse(a.time) || b.id - a.id,
                )
                .slice(0, 30),
            ];
          }),
        );
      }
    } catch (e) {
      if (active) {
        unavailable = errorCode(e) === "probes_disabled";
        if (!unavailable) error = errorMessage(e, m.probes_load_failed());
      }
    } finally {
      inFlight = false;
      loading = false;
    }
  }
  let refreshTimer: ReturnType<typeof setTimeout> | undefined;
  let seenEpoch = 0;
  function schedule() {
    if (refreshTimer) return;
    refreshTimer = setTimeout(() => {
      refreshTimer = undefined;
      void load(false);
    }, 500);
  }
  $effect(() => {
    const epoch = realtime.connectionEpoch;
    untrack(() => {
      if (seenEpoch && epoch !== seenEpoch) void load();
      seenEpoch = epoch;
    });
  });
  onMount(() => {
    active = true;
    void load();
    const offHealth = realtime.on("monitor.health", (payload) => {
      if (
        typeof payload === "object" &&
        payload !== null &&
        "monitor_id" in payload &&
        payload.monitor_id === monitorId
      )
        schedule();
    });
    const offStatus = realtime.on("monitor.probe.status", (payload) => {
      if (
        typeof payload === "object" &&
        payload !== null &&
        "monitor_id" in payload &&
        payload.monitor_id === monitorId
      )
        schedule();
    });
    const offBeat = realtime.on("monitor.probe.heartbeat", (payload) => {
      if (typeof payload !== "object" || payload === null) return;
      const row = payload as Record<string, unknown>;
      if (
        row.monitor_id !== monitorId ||
        typeof row.probe_id !== "string" ||
        typeof row.id !== "number" ||
        typeof row.time !== "string"
      )
        return;
      const beat: RegionalHeartbeat = {
        id: row.id,
        monitor_id: monitorId,
        probe_id: row.probe_id,
        status: row.status as RegionalHeartbeat["status"],
        ping: typeof row.ping === "number" ? row.ping : 0,
        message: typeof row.message === "string" ? row.message : "",
        time: row.time,
        important: row.important === true,
        received_at:
          typeof row.received_at === "string" ? row.received_at : row.time,
        assignment_generation: String(row.assignment_generation ?? "0"),
        config_revision: String(row.config_revision ?? "0"),
      };
      heartbeatVersion += 1;
      histories = {
        ...histories,
        [beat.probe_id]: [
          beat,
          ...(histories[beat.probe_id] ?? []).filter(
            (existing) => existing.id !== beat.id,
          ),
        ].slice(0, 30),
      };
    });
    const interval = setInterval(() => {
      if (!document.hidden) void load(false);
    }, 30000);
    return () => {
      active = false;
      clearInterval(interval);
      clearTimeout(refreshTimer);
      offHealth();
      offStatus();
      offBeat();
    };
  });
</script>

{#if !unavailable}
  <section
    class="space-y-4 rounded-xl border border-border bg-card p-5"
    aria-label={m.probes_health()}
    data-testid="regional-health"
  >
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h2 class="text-sm font-semibold">{m.probes_health()}</h2>
      <button
        type="button"
        onclick={() => load()}
        class="rounded-lg border border-border px-3 py-2 text-xs hover:bg-accent"
        >{m.probes_refresh()}</button
      >
    </div>
    {#if error}<p role="alert" class="text-sm text-danger">{error}</p>{/if}
    {#if loading}<p
        role="status"
        class="animate-pulse text-sm text-muted-foreground"
      >
        {m.probes_loading()}
      </p>{:else if health}
      <div class="flex flex-wrap items-center gap-4">
        <ProbeStatus status={health.status} />
        <p class="text-sm">
          <span class="text-muted-foreground">{m.probes_coverage()}</span>
          <span class="font-mono"
            >{health.coverage_percent === null
              ? m.probes_unreported()
              : `${health.coverage_percent.toFixed(1)}%`}</span
          >
        </p>
        <p class="text-sm">
          <span class="text-muted-foreground">{m.probes_uptime()}</span>
          <span class="font-mono"
            >{health.uptime_percent === null
              ? m.probes_unreported()
              : `${health.uptime_percent.toFixed(1)}%`}</span
          >
        </p>
      </div>
      <p class="text-xs text-muted-foreground">{m.probes_unknown_help()}</p>
      <div class="divide-y divide-border border-t border-border">
        {#each health.regions as region (region.probe_id)}<article
            class="space-y-3 py-4 last:pb-0"
            data-testid="regional-health-row"
          >
            <div class="flex flex-wrap items-start justify-between gap-2">
              <button
                type="button"
                class="text-left hover:text-primary"
                onclick={() => onSelect?.(region.probe_id)}
                ><span class="block text-sm font-medium"
                  >{region.name || region.probe_id}</span
                ><span class="text-xs text-muted-foreground"
                  >{region.location}</span
                ></button
              ><ProbeStatus status={region.status} />
            </div>
            <div class="flex flex-wrap gap-3 text-xs">
              <span
                >{m.probes_connection()}
                <ProbeStatus status={region.connection_status} /></span
              ><span
                >{m.probes_config()}
                <ProbeStatus status={region.config_sync_status} /></span
              >
            </div>
            <div aria-label={m.probes_history()} class="flex gap-1">
              {#each [...(histories[region.probe_id] ?? [])].reverse() as beat (beat.id)}<span
                  class="h-6 min-w-1 flex-1 rounded-sm {beat.status === 'up'
                    ? 'bg-success'
                    : beat.status === 'down'
                      ? 'bg-danger'
                      : beat.status === 'pending'
                        ? 'bg-warning'
                        : 'bg-muted'}"
                  title={`${time(beat.time)} · ${beat.status}`}
                  ><span class="sr-only">{time(beat.time)}: {beat.status}</span
                  ></span
                >{:else}<span class="text-xs text-muted-foreground"
                  >{m.probes_no_history()}</span
                >{/each}
            </div>
            <dl class="space-y-1 text-xs text-muted-foreground">
              <div class="flex flex-wrap justify-between gap-2">
                <dt>{m.probes_observed()}</dt>
                <dd>{time(region.observed_at)}</dd>
              </div>
              <div class="flex flex-wrap justify-between gap-2">
                <dt>{m.probes_received()}</dt>
                <dd>{time(region.received_at)}</dd>
              </div>
              <div class="flex flex-wrap justify-between gap-2">
                <dt>{m.probes_fresh_until()}</dt>
                <dd>{time(region.fresh_until)}</dd>
              </div>
            </dl>
          </article>{/each}
      </div>
    {/if}
  </section>
{/if}
