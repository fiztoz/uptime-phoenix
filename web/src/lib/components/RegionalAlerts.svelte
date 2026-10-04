<script lang="ts">
  import { onMount, untrack } from "svelte";
  import { realtime } from "$lib/stores/ws.svelte.js";
  import {
    regionalApi,
    type RegionalAlert,
    type RegionalAckReceipt,
  } from "$lib/api/regional";
  import { errorCode, errorMessage } from "$lib/api/probes";
  import ProbeStatus from "./ProbeStatus.svelte";
  import * as m from "$lib/paraglide/messages.js";
  let { monitorId }: { monitorId: number } = $props();
  let incidents = $state<RegionalAlert[]>([]);
  let receipts = $state<Record<string, RegionalAckReceipt>>({});
  const commandIds = new Map<string, string>();
  let busy = $state<Record<string, boolean>>({});
  let error = $state("");
  let unavailable = $state(false);
  let loaded = $state(false);
  let active = true;
  let polling = false;
  let polls = 0;
  async function load() {
    try {
      const next = await regionalApi.alerts(monitorId);
      if (active) {
        incidents = next;
        error = "";
      }
    } catch (e) {
      if (active) {
        unavailable = errorCode(e) === "probes_disabled";
        if (!unavailable) error = errorMessage(e, m.probes_load_failed());
      }
    } finally {
      loaded = true;
    }
  }
  async function ack(incident: RegionalAlert) {
    const id = incident.source_alert_id;
    if (busy[id]) return;
    busy[id] = true;
    error = "";
    if (receipts[id] && ["failed", "expired"].includes(receipts[id].status))
      commandIds.delete(id);
    const commandId = commandIds.get(id) ?? crypto.randomUUID();
    commandIds.set(id, commandId);
    try {
      receipts[id] = await regionalApi.acknowledge(monitorId, id, commandId);
      polls = 0;
      if (receipts[id].remote_confirmed && receipts[id].status === "applied")
        await load();
    } catch (e) {
      error = errorMessage(e, m.probes_ack_failed());
    } finally {
      busy[id] = false;
    }
  }
  async function refreshReceipts() {
    if (polling) return;
    polling = true;
    try {
      for (const [id, receipt] of Object.entries(receipts)) {
        if (receipt.status !== "pending") continue;
        const next = await regionalApi.acknowledgement(
          monitorId,
          id,
          receipt.command_id,
        );
        if (!active) return;
        receipts[id] = next;
        if (next.remote_confirmed && next.status === "applied") await load();
      }
    } catch (e) {
      if (active) error = errorMessage(e, m.probes_ack_failed());
    } finally {
      polling = false;
    }
  }
  async function refresh() {
    polls = 0;
    await refreshReceipts();
    await load();
  }
  let eventTimer: ReturnType<typeof setTimeout> | undefined;
  let seenEpoch = 0;
  $effect(() => {
    const epoch = realtime.connectionEpoch;
    untrack(() => {
      if (seenEpoch && epoch !== seenEpoch) void refresh();
      seenEpoch = epoch;
    });
  });
  function schedule() {
    if (eventTimer) return;
    eventTimer = setTimeout(() => {
      eventTimer = undefined;
      void refresh();
    }, 500);
  }
  onMount(() => {
    active = true;
    void load();
    const offStatus = realtime.on("monitor.probe.status", (payload) => {
      if (
        typeof payload === "object" &&
        payload !== null &&
        "monitor_id" in payload &&
        payload.monitor_id === monitorId
      )
        schedule();
    });
    const offCommand = realtime.on("probe.command.status", (payload) => {
      if (
        typeof payload === "object" &&
        payload !== null &&
        "command_id" in payload &&
        Object.values(receipts).some((r) => r.command_id === payload.command_id)
      )
        schedule();
    });
    const interval = setInterval(() => {
      if (polls++ < 60 && !document.hidden) void refreshReceipts();
    }, 5000);
    return () => {
      active = false;
      clearInterval(interval);
      clearTimeout(eventTimer);
      offStatus();
      offCommand();
    };
  });
</script>

{#if !unavailable && (incidents.length || error || !loaded)}
  <section
    class="space-y-4 rounded-xl border border-border bg-card p-5"
    aria-label={m.probes_regional_incidents()}
  >
    <div class="flex flex-wrap items-center justify-between gap-3">
      <h2 class="text-sm font-semibold">{m.probes_regional_incidents()}</h2>
      <button
        type="button"
        onclick={refresh}
        class="rounded-lg border border-border px-3 py-2 text-xs hover:bg-accent"
        >{m.probes_refresh()}</button
      >
    </div>
    {#if error}<p role="alert" class="text-sm text-danger">{error}</p>{/if}
    {#if !loaded}<p
        role="status"
        class="animate-pulse text-sm text-muted-foreground"
      >
        {m.probes_loading()}
      </p>{/if}
    {#each incidents as incident (incident.source_alert_id)}
      {@const receipt = receipts[incident.source_alert_id]}
      <article
        class="space-y-3 border-t border-border pt-4"
        data-testid="regional-incident"
      >
        <div class="flex flex-wrap items-center justify-between gap-3">
          <div>
            <h3 class="text-sm font-medium">
              {m.probes_region_label({
                name: incident.probe_name || incident.probe_id,
              })}
            </h3>
            <p class="mt-1 text-xs text-muted-foreground">
              {incident.location} · {new Date(
                incident.started_at,
              ).toLocaleString()}
            </p>
          </div>
          <ProbeStatus status={incident.status} />
        </div>
        <p class="text-sm">{incident.reason}</p>
        {#if receipt}<div
            aria-live="polite"
            class="space-y-1 text-sm {receipt.status === 'applied' &&
            receipt.remote_confirmed
              ? 'text-success'
              : 'text-warning'}"
          >
            <p>
              {receipt.status === "applied" && receipt.remote_confirmed
                ? m.alerts_page_acked_toast()
                : receipt.status === "pending"
                  ? m.probes_ack_pending()
                  : m.probes_ack_failed()}
            </p>
            <p class="break-all font-mono text-xs text-muted-foreground">
              {m.probes_ack_receipt()}: {receipt.command_id}
            </p>
          </div>{/if}
        {#if incident.subject_kind === "availability" && incident.probe_id !== "local" && !incident.acked_at && !incident.resolved_at && (!receipt || ["failed", "expired"].includes(receipt.status))}
          <button
            type="button"
            disabled={busy[incident.source_alert_id]}
            onclick={() => ack(incident)}
            class="rounded-lg border border-border px-3 py-2 text-sm hover:bg-accent disabled:opacity-60"
            >{m.alerts_ack()}</button
          >
        {/if}
      </article>
    {/each}
  </section>
{/if}
