<script lang="ts">
  import {
    conditionUsagePercent,
    displayedConditionState,
    type MonitorCondition,
  } from "$lib/api/conditions";
  import * as m from "$lib/paraglide/messages.js";

  interface Props {
    conditions: MonitorCondition[];
    now?: number;
    compact?: boolean;
  }

  let { conditions, now = Date.now(), compact = false }: Props = $props();

  function labelFor(condition: MonitorCondition): string {
    return condition.kind === "session_pool"
      ? m.condition_session_pool()
      : condition.kind === "storage"
        ? m.condition_storage()
        : condition.resource || condition.kind;
  }

  function stateLabel(condition: MonitorCondition): string {
    switch (displayedConditionState(condition, now)) {
      case "warning":
        return m.condition_state_warning();
      case "error":
        return m.condition_state_error();
      case "stale":
        return m.condition_state_stale();
      default:
        return m.condition_state_ok();
    }
  }

  function usageLabel(condition: MonitorCondition): string {
    if (condition.used == null || condition.limit == null) {
      return stateLabel(condition);
    }
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

  function fillClass(condition: MonitorCondition): string {
    switch (displayedConditionState(condition, now)) {
      case "warning":
        return "bg-warning";
      case "error":
        return "bg-danger";
      case "stale":
        return "bg-muted-foreground/50";
      default:
        return "bg-success";
    }
  }

  function percentLabel(condition: MonitorCondition): string {
    const percent = conditionUsagePercent(condition);
    return percent == null
      ? stateLabel(condition)
      : m.condition_percent_value({ percent: percent.toFixed(1) });
  }
</script>

<div
  class={conditions.length > 1
    ? "grid grid-cols-2 gap-3"
    : "grid grid-cols-1"}
>
  {#each conditions as condition (`${condition.monitor_id}:${condition.kind}`)}
    {@const percent = conditionUsagePercent(condition)}
    <div class="min-w-0" title={condition.message || `${labelFor(condition)}: ${stateLabel(condition)}`}>
      <div class="flex items-baseline justify-between gap-2">
        <span class="eyebrow truncate">{labelFor(condition)}</span>
        <span class="shrink-0 text-sm font-semibold tabular-nums">
          {percentLabel(condition)}
        </span>
      </div>
      <div class="mt-1.5 h-1.5 overflow-hidden rounded-full bg-muted">
        {#if percent != null}
          <div
            class="h-full rounded-full {fillClass(condition)}"
            style="width: {percent}%"
          ></div>
        {:else}
          <div class="h-full w-full rounded-full bg-muted-foreground/25"></div>
        {/if}
      </div>
      {#if !compact}
        <p class="mt-1 truncate text-[11px] text-muted-foreground">
          {usageLabel(condition)}
        </p>
      {/if}
    </div>
  {/each}
</div>
