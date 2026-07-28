<!--
  @component
  Status history table (Uptime Kuma style) for important/status-change heartbeats.
-->
<script lang="ts">
  import { timeFormat } from "d3-time-format";
  import type { Heartbeat } from "$lib/api/heartbeats.js";
  import { confirmAction } from "$lib/stores/confirm.svelte";
  import { Trash2 } from "@lucide/svelte";

  interface Props {
    heartbeats: Heartbeat[];
    onClear?: () => void | Promise<void>;
    clearing?: boolean;
    /** Used as the type-to-confirm phrase when clearing history. */
    monitorName?: string;
  }

  let { heartbeats, onClear, clearing = false, monitorName }: Props = $props();

  const formatDateTime = timeFormat("%Y-%m-%d %H:%M:%S");

  const rows = $derived(heartbeats.slice(0, 100));

  const statusConfig: Record<
    Heartbeat["status"],
    { label: string; dotClass: string; bgClass: string }
  > = {
    up: {
      label: "Up",
      dotClass: "dot-up",
      bgClass: "bg-success/10 text-success border-success/25",
    },
    down: {
      label: "Down",
      dotClass: "dot-down",
      bgClass: "bg-danger/10 text-danger border-danger/25",
    },
    pending: {
      label: "Pending",
      dotClass: "dot-warn",
      bgClass: "bg-warning/10 text-warning border-warning/25",
    },
    maintenance: {
      label: "Maintenance",
      dotClass: "dot-info",
      bgClass: "bg-info/10 text-info border-info/25",
    },
  };

  async function handleClear() {
    if (!onClear) return;
    // This deletes every heartbeat ever recorded for the monitor, not just the
    // rows in this table — uptime percentages and the response-time chart are
    // wiped with it, and there is no undo. Hence the type-to-confirm gate.
    const ok = await confirmAction({
      title: monitorName
        ? `Erase all heartbeat history for "${monitorName}"?`
        : "Erase all heartbeat history for this monitor?",
      message:
        "Every recorded check is permanently deleted — status history, uptime percentages and the response-time chart all reset to empty. The monitor keeps running and starts collecting again from scratch. This cannot be undone.",
      confirmLabel: "Erase history",
      destructive: true,
      requireText: monitorName,
    });
    if (!ok) return;
    await onClear();
  }
</script>

<div class="rounded-xl border border-border bg-card">
  <div
    class="flex items-center justify-between gap-3 border-b border-border px-5 py-4"
  >
    <h3 class="text-sm font-semibold tracking-tight">Status History</h3>
    {#if onClear && rows.length > 0}
      <button
        type="button"
        onclick={handleClear}
        disabled={clearing}
        class="inline-flex items-center gap-1.5 rounded-lg border border-border px-2.5 py-1 text-xs text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:opacity-50"
      >
        <Trash2 class="h-3.5 w-3.5" />
        {clearing ? "Clearing…" : "Clear history"}
      </button>
    {/if}
  </div>

  {#if rows.length === 0}
    <p class="px-5 py-8 text-sm text-muted-foreground">
      No status changes recorded.
    </p>
  {:else}
    <div class="max-h-96 overflow-y-auto">
      <table class="w-full text-sm">
        <thead class="sticky top-0 z-[1] border-b border-border bg-card">
          <tr class="text-left text-xs text-muted-foreground">
            <th class="px-5 py-2.5 font-medium">Status</th>
            <th class="px-5 py-2.5 font-medium">DateTime</th>
            <th class="px-5 py-2.5 font-medium">Message</th>
          </tr>
        </thead>
        <tbody>
          {#each rows as hb (hb.id)}
            {@const config = statusConfig[hb.status] ?? statusConfig.pending}
            <tr
              class="border-b border-border last:border-0 transition-colors hover:bg-accent/30"
            >
              <td class="px-5 py-2.5">
                <span
                  class="inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium {config.bgClass}"
                >
                  <span class="dot {config.dotClass}"></span>
                  {config.label}
                </span>
              </td>
              <td
                class="px-5 py-2.5 font-mono text-xs tabular-nums text-muted-foreground"
              >
                {formatDateTime(new Date(hb.time))}
              </td>
              <td class="px-5 py-2.5 text-muted-foreground"
                >{hb.message || "—"}</td
              >
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {/if}
</div>
