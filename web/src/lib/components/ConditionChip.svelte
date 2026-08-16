<script lang="ts">
  import {
    CircleAlert,
    CircleCheck,
    CircleX,
    Clock3,
    Database,
    Users,
  } from "@lucide/svelte";
  import {
    displayedConditionState,
    type MonitorCondition,
  } from "$lib/api/conditions";
  import * as m from "$lib/paraglide/messages.js";

  interface Props {
    condition: MonitorCondition;
    now?: number;
    compact?: boolean;
  }

  let { condition, now = Date.now(), compact = false }: Props = $props();

  const state = $derived(displayedConditionState(condition, now));
  const label = $derived(
    condition.kind === "session_pool"
      ? m.condition_session_pool()
      : m.condition_storage(),
  );
  const stateLabel = $derived.by(() => {
    switch (state) {
      case "warning":
        return m.condition_state_warning();
      case "error":
        return m.condition_state_error();
      case "stale":
        return m.condition_state_stale();
      default:
        return m.condition_state_ok();
    }
  });
  const value = $derived(
    condition.percent == null
      ? stateLabel
      : m.condition_percent_value({ percent: condition.percent.toFixed(1) }),
  );
  const tone = $derived.by(() => {
    switch (state) {
      case "warning":
        return "border-warning/25 bg-warning/10 text-warning";
      case "error":
        return "border-danger/25 bg-danger/10 text-danger";
      case "stale":
        return "border-border bg-muted/50 text-muted-foreground";
      default:
        return "border-success/25 bg-success/10 text-success";
    }
  });
</script>

<span
  class="inline-flex min-w-0 items-center gap-1.5 rounded-full border px-2 py-1 text-[11px] font-medium leading-none {tone}"
  title={condition.message || `${label}: ${stateLabel}`}
>
  {#if state === "warning"}
    <CircleAlert class="h-3 w-3 shrink-0" />
  {:else if state === "error"}
    <CircleX class="h-3 w-3 shrink-0" />
  {:else if state === "stale"}
    <Clock3 class="h-3 w-3 shrink-0" />
  {:else if condition.kind === "session_pool"}
    <Users class="h-3 w-3 shrink-0" />
  {:else if condition.kind === "storage"}
    <Database class="h-3 w-3 shrink-0" />
  {:else}
    <CircleCheck class="h-3 w-3 shrink-0" />
  {/if}
  <span class="truncate">{label}</span>
  {#if !compact || state !== "ok"}
    <span class="shrink-0 opacity-80">· {value}</span>
  {/if}
</span>
