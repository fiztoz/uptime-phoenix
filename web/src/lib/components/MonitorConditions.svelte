<script lang="ts">
  import type { MonitorCondition } from "$lib/api/conditions";
  import ConditionChip from "./ConditionChip.svelte";
  import { Database, Clock3 } from "@lucide/svelte";
  import * as m from "$lib/paraglide/messages.js";

  interface Props {
    conditions: MonitorCondition[];
    now?: number;
  }

  let { conditions, now = Date.now() }: Props = $props();

  function formatMeasurement(condition: MonitorCondition): string {
    if (condition.used == null || condition.limit == null) return "—";
    if (condition.unit === "bytes") {
      const gib = 1024 ** 3;
      return m.condition_used_of_limit({
        used: (condition.used / gib).toFixed(1),
        limit: (condition.limit / gib).toFixed(1),
        unit: "GiB",
      });
    }
    return m.condition_used_of_limit({
      used: condition.used.toLocaleString(),
      limit: condition.limit.toLocaleString(),
      unit: condition.unit === "connections" ? "" : condition.unit,
    });
  }

  function observedLabel(condition: MonitorCondition): string {
    const observed = new Date(condition.observed_at);
    return Number.isNaN(observed.getTime()) ? "—" : observed.toLocaleString();
  }
</script>

{#if conditions.length > 0}
  <section class="overflow-hidden rounded-xl border border-border bg-card">
    <div class="flex items-start gap-3 border-b border-border px-5 py-4">
      <Database class="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" />
      <div>
        <h2 class="text-sm font-semibold tracking-tight">
          {m.monitor_conditions_heading()}
        </h2>
        <p class="mt-1 text-sm text-muted-foreground">
          {m.monitor_conditions_description()}
        </p>
      </div>
    </div>
    <div class="divide-y divide-border">
      {#each conditions as condition (`${condition.monitor_id}:${condition.kind}`)}
        <div
          class="grid gap-3 px-5 py-4 lg:grid-cols-[minmax(0,1fr)_auto] lg:items-center"
        >
          <div class="min-w-0">
            <div class="flex flex-wrap items-center gap-2">
              <ConditionChip {condition} {now} />
              <span class="text-sm font-medium tabular-nums">
                {formatMeasurement(condition)}
              </span>
            </div>
            <p class="mt-2 text-sm text-muted-foreground">
              {condition.message}
            </p>
            {#if condition.source}
              <p
                class="mt-1 truncate font-mono text-[11px] text-faint"
                title={condition.source}
              >
                {m.condition_source({ source: condition.source })}
              </p>
            {/if}
          </div>
          <div
            class="flex items-center gap-1.5 text-xs text-muted-foreground lg:justify-end"
          >
            <Clock3 class="h-3.5 w-3.5" />
            {m.condition_observed_at({ time: observedLabel(condition) })}
          </div>
        </div>
      {/each}
    </div>
  </section>
{/if}
