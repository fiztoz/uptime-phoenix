<script lang="ts">
  import type { Heartbeat, Monitor } from "$lib/stores/ws.svelte.js";
  import type { MonitorCondition } from "$lib/api/conditions";
  import {
    cardUsesSignals,
    type DashboardCardBody,
  } from "$lib/dashboard-card";
  import {
    WALLBOARD_CARD_DEFAULT_PX,
    wallboardCardScale,
  } from "$lib/wallboard-split";
  import { sparklinePoints } from "$lib/utils/chart.js";
  import Sparkline from "./charts/Sparkline.svelte";
  import StatusPill from "./StatusPill.svelte";
  import MonitorCardSignals from "./MonitorCardSignals.svelte";
  import { whenVisible } from "$lib/actions/whenVisible";
  import * as m from "$lib/paraglide/messages.js";

  interface Props {
    monitor: Monitor;
    heartbeat?: Heartbeat;
    heartbeatHistory?: Heartbeat[];
    conditions?: MonitorCondition[];
    conditionNow?: number;
    cardBody?: DashboardCardBody;
    sizePx?: number;
    onNeedHistory?: () => void;
  }

  let {
    monitor,
    heartbeat,
    heartbeatHistory = [],
    conditions = [],
    conditionNow = Date.now(),
    cardBody = "response",
    sizePx = WALLBOARD_CARD_DEFAULT_PX,
    onNeedHistory,
  }: Props = $props();

  const sparklineData = $derived(sparklinePoints(heartbeatHistory));
  const isDown = $derived(monitor.status === "down");
  const isPending = $derived(monitor.status === "pending");
  const showSignals = $derived(cardUsesSignals(cardBody, conditions.length));
  const scale = $derived(wallboardCardScale(sizePx));
  const sparkHeight = $derived(Math.round(36 * scale));

  function formatHeartbeatTime(value: string | undefined): string {
    if (!value) return "—";
    const time = new Date(value);
    if (Number.isNaN(time.getTime())) return "—";
    return new Intl.DateTimeFormat(undefined, {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
    }).format(time);
  }
</script>

<article
  use:whenVisible={onNeedHistory}
  class="relative flex flex-col overflow-hidden rounded-xl border bg-card
		{isDown
    ? 'border-danger/45 bg-danger/[0.055]'
    : isPending
      ? 'border-warning/40 bg-warning/[0.045]'
      : 'border-border'}"
  style="--wb: {scale}; min-height: calc(11rem * var(--wb)); padding: calc(1rem * var(--wb));"
>
  <span
    class="absolute inset-y-0 left-0 w-1 {isDown
      ? 'bg-danger'
      : isPending
        ? 'bg-warning'
        : monitor.status === 'up'
          ? 'bg-success'
          : 'bg-muted-foreground/50'}"
  ></span>

  <div class="flex items-start justify-between gap-3 pl-1">
    <div class="min-w-0">
      <h2
        class="truncate font-semibold tracking-tight"
        style="font-size: calc(1rem * var(--wb))"
      >
        {monitor.name}
      </h2>
      <p
        class="mt-1 truncate font-mono text-muted-foreground"
        style="font-size: calc(0.6875rem * var(--wb))"
      >
        <span class="uppercase text-faint">{monitor.type}</span>
        {#if monitor.target}<span class="mx-1 text-faint">·</span
          >{monitor.target}{/if}
      </p>
    </div>
    <StatusPill status={monitor.status} />
  </div>

  {#if showSignals}
    <div class="mt-4 min-w-0">
      <MonitorCardSignals {conditions} now={conditionNow} compact />
    </div>
  {:else}
    <div
      class="mt-4 min-w-0 overflow-hidden rounded-md bg-muted/20 px-1 py-1"
      style="height: calc(2.75rem * var(--wb))"
    >
      {#if sparklineData.length > 0}
        <Sparkline data={sparklineData} width="100%" height={sparkHeight} />
      {:else}
        <div class="grid h-full place-items-center text-[11px] text-faint">
          {m.monitor_card_no_history()}
        </div>
      {/if}
    </div>
  {/if}

  <div
    class="mt-auto grid gap-3 border-t border-border/70 pt-3
		{showSignals ? 'grid-cols-1' : 'grid-cols-2'}"
  >
    {#if !showSignals}
      <div>
        <div class="text-[10px] font-medium uppercase tracking-wider text-faint">
          {m.dashboard_wallboard_response()}
        </div>
        <div
          class="mt-0.5 font-mono font-semibold tabular-nums"
          style="font-size: calc(1.125rem * var(--wb))"
        >
          {#if heartbeat && heartbeat.ping > 0}
            {heartbeat.ping}<span
              class="ml-0.5 text-xs font-normal text-muted-foreground">ms</span
            >
          {:else}
            <span class="text-muted-foreground">—</span>
          {/if}
        </div>
      </div>
    {/if}
    <div class={showSignals ? "" : "text-right"}>
      <div class="text-[10px] font-medium uppercase tracking-wider text-faint">
        {m.dashboard_wallboard_last_check()}
      </div>
      <div class="mt-1 font-mono text-xs tabular-nums text-muted-foreground">
        {formatHeartbeatTime(heartbeat?.time)}
      </div>
    </div>
  </div>

  {#if heartbeat?.msg}
    <p
      class="mt-2 truncate border-t border-border/70 pt-2 text-xs {isDown
        ? 'text-danger'
        : 'text-muted-foreground'}"
    >
      {heartbeat.msg}
    </p>
  {/if}
</article>
