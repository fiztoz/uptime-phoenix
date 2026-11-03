<!--
  @component
  Recent-check timeline bar (Uptime Kuma style) with per-check hover tooltips.
  Freezes the bar while hovered so live WS updates do not shift segments under the cursor.
-->
<script lang="ts">
  import { timeFormat } from "d3-time-format";
  import type { Heartbeat } from "#lib/api/heartbeats.js";
  import { uniqueKeys } from "#lib/utils/chart.js";
  import * as m from "#lib/paraglide/messages.js";
  import LoadErrorNotice from "#lib/components/LoadErrorNotice.svelte";
  import { timelineLayout } from "#lib/recent-checks.js";

  interface Props {
    heartbeats: Heartbeat[];
    maxChecks?: number;
    overall?: boolean;
    /** Set when the last load failed: shown with a Retry, never as "no checks". */
    loadError?: string | null;
    onRetry?: () => void;
  }

  let {
    heartbeats,
    maxChecks = 60,
    overall = false,
    loadError = null,
    onRetry,
  }: Props = $props();

  const formatTime = timeFormat("%Y-%m-%d %H:%M:%S");

  const liveSegments = $derived.by(() => {
    return [...heartbeats]
      .sort((a, b) => new Date(a.time).getTime() - new Date(b.time).getTime())
      .slice(-maxChecks);
  });

  let isHovering = $state(false);
  let frozenSegments = $state<Heartbeat[]>([]);
  let hoveredIndex = $state<number | null>(null);
  let barEl = $state<HTMLDivElement | null>(null);
  let tooltipX = $state(0);

  const segments = $derived(isHovering ? frozenSegments : liveSegments);
  const segmentKeys = $derived(uniqueKeys(segments, (hb) => `${hb.id}-${hb.time}`));

  /** Requested window of the strip (the page asks for the last 24h). */
  const WINDOW_HOURS = 24;
  let nowMs = $state(Date.now());
  // The newest overall interval runs until now, so its width grows; checks
  // are laid out by count and need no clock.
  $effect(() => {
    if (!overall) return;
    const timer = setInterval(() => (nowMs = Date.now()), 30_000);
    return () => clearInterval(timer);
  });
  const layout = $derived(
    timelineLayout(segments, {
      overall,
      nowMs,
      windowHours: WINDOW_HOURS,
      truncated: heartbeats.length >= maxChecks,
    }),
  );

  function color(status: Heartbeat["status"]): string {
    if (status === "up") return "bg-success";
    if (status === "down") return "bg-danger";
    if (status === "pending") return "bg-warning";
    return "bg-muted";
  }

  function tooltipLines(
    hb: Heartbeat,
    index: number,
  ): {
    status: string;
    ping: string;
    time: string;
    message?: string;
  } {
    // An overall interval runs from its start to the next one (the newest
    // until now); a check is a single instant.
    const next = segments[index + 1];
    const time = overall
      ? `${formatTime(new Date(hb.time))} – ${formatTime(next ? new Date(next.time) : new Date(nowMs))}`
      : formatTime(new Date(hb.time));
    return {
      status: hb.status.toUpperCase(),
      ping: hb.ping > 0 ? `${hb.ping} ms` : "—",
      time,
      message: hb.message || undefined,
    };
  }

  function handleBarEnter() {
    frozenSegments = liveSegments;
    isHovering = true;
  }

  function handleBarLeave() {
    isHovering = false;
    hoveredIndex = null;
    frozenSegments = [];
  }

  function handleSegmentEnter(index: number, event: MouseEvent | FocusEvent) {
    hoveredIndex = index;
    const target = event.currentTarget as HTMLDivElement;
    const bar = barEl;
    if (!bar) return;
    const barRect = bar.getBoundingClientRect();
    const segRect = target.getBoundingClientRect();
    tooltipX = segRect.left - barRect.left + segRect.width / 2;
  }

  function handleSegmentLeave() {
    hoveredIndex = null;
  }

  function handleSegmentFocus(index: number, event: FocusEvent) {
    if (!isHovering) handleBarEnter();
    handleSegmentEnter(index, event);
  }

  function handleSegmentBlur() {
    queueMicrotask(() => {
      if (!barEl?.contains(document.activeElement)) handleBarLeave();
    });
  }

  const hoveredBeat = $derived(
    hoveredIndex != null ? (segments[hoveredIndex] ?? null) : null,
  );
</script>

<div class="rounded-xl border border-border bg-card p-5">
  <h3 class="text-sm font-semibold tracking-tight text-muted-foreground">
    {overall ? m.monitor_detail_overall_timeline() : "Recent Checks"}
  </h3>
  {#if overall}
    <p class="mt-2 text-xs text-muted-foreground">
      {m.monitor_detail_overall_timeline_help()}
    </p>
  {/if}

  {#if loadError}
    <LoadErrorNotice message={loadError} {onRetry} class="mt-3" />
  {/if}
  {#if segments.length === 0}
    {#if !loadError}
      <p class="mt-4 text-sm text-muted-foreground">No checks recorded yet.</p>
    {/if}
  {:else}
    <div class="relative mt-3">
      <div
        bind:this={barEl}
        class="flex h-8 w-full items-stretch gap-[2px]"
        role="group"
        aria-label="Recent check timeline"
        data-testid="recent-check-timeline"
        data-hover-paused={isHovering}
        onmouseenter={handleBarEnter}
        onmouseleave={handleBarLeave}
      >
        {#if layout.lead > 0}
          <!-- Before the first interval: the monitor did not exist yet. -->
          <div
            style:flex-grow={layout.lead}
            class="h-full min-w-0 shrink basis-0 rounded-[2px] border border-dashed border-border"
            role="img"
            aria-label={m.monitor_detail_not_monitored()}
            title={m.monitor_detail_not_monitored()}
            data-testid="recent-check-not-monitored"
          ></div>
        {/if}
        {#each segments as hb, index (segmentKeys[index])}
          {@const lines = tooltipLines(hb, index)}
          <button
            type="button"
            style:flex-grow={layout.weights[index]}
            class="h-full {overall
              ? 'min-w-[2px]'
              : 'min-w-0'} shrink basis-0 rounded-[2px] {color(
              hb.status,
            )} opacity-85 outline-none transition-all hover:opacity-100 hover:brightness-110 focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background {hoveredIndex ===
            index
              ? 'ring-1 ring-foreground/40'
              : ''}"
            onmouseenter={(e) => handleSegmentEnter(index, e)}
            onmouseleave={handleSegmentLeave}
            onfocus={(e) => handleSegmentFocus(index, e)}
            onblur={handleSegmentBlur}
            aria-label={`${lines.status}, ${lines.ping}, ${lines.time}`}
          ></button>
        {/each}
      </div>

      {#if hoveredBeat}
        {@const lines = tooltipLines(hoveredBeat, hoveredIndex ?? 0)}
        <div
          class="pointer-events-none absolute bottom-full z-20 mb-2 -translate-x-1/2 rounded-md border border-border bg-elevated px-2.5 py-1.5 text-xs shadow-lg"
          style:left="{tooltipX}px"
          data-testid="recent-check-tooltip"
        >
          <div
            class="font-semibold {hoveredBeat.status === 'up'
              ? 'text-success'
              : hoveredBeat.status === 'down'
                ? 'text-danger'
                : 'text-warning'}"
          >
            {lines.status} · {lines.ping}
          </div>
          <div class="mt-0.5 font-mono text-muted-foreground">{lines.time}</div>
          {#if lines.message}
            <div class="mt-0.5 max-w-[220px] truncate text-muted-foreground">
              {lines.message}
            </div>
          {/if}
        </div>
      {/if}
    </div>
    <p class="mt-2 text-xs text-muted-foreground">
      {overall
        ? m.monitor_detail_health_intervals({ count: segments.length })
        : `${segments.length} checks`} · hover for details{isHovering
        ? " · live updates paused"
        : ""}
    </p>
  {/if}
</div>
