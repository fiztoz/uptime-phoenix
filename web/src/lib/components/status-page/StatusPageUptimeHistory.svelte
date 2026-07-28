<script lang="ts">
  import type {
    PublicStatusResponse,
    UptimeHistoryPeriod,
  } from "$lib/api/statuspages";

  type PublicMonitor = PublicStatusResponse["monitors"][number];
  type Granularity = "monthly" | "quarterly";

  interface Props {
    monitors: PublicMonitor[];
    slaTarget?: number | null;
  }

  let { monitors, slaTarget = null }: Props = $props();
  let granularity = $state<Granularity>("monthly");
  let headers = $derived(monitors[0]?.uptime_history?.[granularity] ?? []);

  function periodsFor(monitor: PublicMonitor): UptimeHistoryPeriod[] {
    return monitor.uptime_history?.[granularity] ?? [];
  }

  function uptimeLabel(value: number | null): string {
    return typeof value === "number" && Number.isFinite(value)
      ? `${value.toFixed(3).replace(/0+$/, "").replace(/\.$/, "")}%`
      : "No data";
  }

  function resultClass(value: number | null): string {
    if (typeof value !== "number" || slaTarget == null)
      return "text-muted-foreground";
    return value >= slaTarget ? "text-success" : "text-danger";
  }

  function resultLabel(value: number | null): string {
    if (typeof value !== "number" || slaTarget == null) return "";
    return value >= slaTarget ? "SLA met" : "Below SLA";
  }
</script>

<section class="space-y-5" aria-labelledby="uptime-history-heading">
  <div class="flex flex-col gap-4 sm:flex-row sm:items-end sm:justify-between">
    <div>
      <h2
        id="uptime-history-heading"
        class="text-xl font-semibold tracking-tight"
      >
        Component uptime
      </h2>
      <p class="mt-1 max-w-2xl text-sm text-muted-foreground">
        Calendar periods use UTC. Planned maintenance is excluded from measured
        uptime.
      </p>
    </div>
    <div
      class="inline-flex w-fit rounded-lg border border-border bg-surface p-1"
      aria-label="History period"
    >
      <button
        type="button"
        class="rounded-md px-3 py-1.5 text-sm font-medium transition-colors {granularity ===
        'monthly'
          ? 'bg-card text-foreground shadow-sm'
          : 'text-muted-foreground hover:text-foreground'}"
        aria-pressed={granularity === "monthly"}
        onclick={() => (granularity = "monthly")}
      >
        Monthly
      </button>
      <button
        type="button"
        class="rounded-md px-3 py-1.5 text-sm font-medium transition-colors {granularity ===
        'quarterly'
          ? 'bg-card text-foreground shadow-sm'
          : 'text-muted-foreground hover:text-foreground'}"
        aria-pressed={granularity === "quarterly"}
        onclick={() => (granularity = "quarterly")}
      >
        Quarterly
      </button>
    </div>
  </div>

  {#if slaTarget != null}
    <div
      class="flex items-center justify-between gap-4 rounded-xl border border-primary/25 bg-primary/5 px-4 py-3"
    >
      <div>
        <div
          class="text-xs font-medium uppercase tracking-wider text-muted-foreground"
        >
          Public SLA target
        </div>
        <p class="mt-0.5 text-sm text-foreground">
          Measured periods are evaluated against the configured target.
        </p>
      </div>
      <div class="shrink-0 font-mono text-xl font-semibold text-primary">
        {slaTarget}%
      </div>
    </div>
  {/if}

  {#if monitors.length === 0}
    <div
      class="rounded-xl border border-dashed border-border px-5 py-10 text-center text-sm text-muted-foreground"
    >
      No services are assigned to this status page.
    </div>
  {:else}
    <div class="overflow-x-auto rounded-xl border border-border bg-card">
      <table class="min-w-full border-collapse text-left text-sm">
        <thead>
          <tr class="border-b border-border bg-surface/70">
            <th
              class="sticky left-0 z-10 min-w-48 bg-surface px-4 py-3 font-medium text-foreground"
            >
              Component
            </th>
            {#each headers as period (period.start_date)}
              <th class="min-w-36 px-4 py-3 font-medium text-foreground">
                <span class="block whitespace-nowrap">{period.label}</span>
                {#if !period.complete}
                  <span
                    class="mt-0.5 block text-[11px] font-normal uppercase tracking-wide text-muted-foreground"
                    >To date</span
                  >
                {/if}
              </th>
            {/each}
          </tr>
        </thead>
        <tbody>
          {#each monitors as monitor (monitor.id)}
            <tr class="border-b border-border last:border-0">
              <th
                class="sticky left-0 z-10 bg-card px-4 py-4 font-medium text-foreground"
              >
                <span class="block max-w-48 truncate">{monitor.name}</span>
                <span
                  class="mt-0.5 block text-xs font-normal text-muted-foreground"
                  >{monitor.type}</span
                >
              </th>
              {#each periodsFor(monitor) as period (period.start_date)}
                <td class="px-4 py-4 align-top">
                  <span
                    class="block whitespace-nowrap font-mono font-medium {resultClass(
                      period.uptime_percent,
                    )}"
                  >
                    {uptimeLabel(period.uptime_percent)}
                  </span>
                  {#if resultLabel(period.uptime_percent)}
                    <span
                      class="mt-1 block text-[11px] font-medium uppercase tracking-wide {resultClass(
                        period.uptime_percent,
                      )}"
                    >
                      {resultLabel(period.uptime_percent)}
                    </span>
                  {/if}
                </td>
              {/each}
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</section>
