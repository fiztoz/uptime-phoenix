<script lang="ts">
  import { onMount } from "svelte";
  import {
    probesApi,
    errorMessage,
    type ProbeOperationView,
  } from "$lib/api/probes";
  import ProbeStatus from "./ProbeStatus.svelte";
  import * as m from "$lib/paraglide/messages.js";
  let {
    receipt,
    onUpdate,
  }: {
    receipt: ProbeOperationView;
    onUpdate?: (receipt: ProbeOperationView) => void;
  } = $props();
  let error = $state("");
  let refreshing = $state(false);
  let attempts = $state(0);
  const terminal = $derived(["succeeded", "failed"].includes(receipt.status));
  async function refresh() {
    if (refreshing) return;
    refreshing = true;
    try {
      onUpdate?.(await probesApi.operation(receipt.operation_id));
      error = "";
    } catch (e) {
      error = errorMessage(e, m.probes_load_failed());
    } finally {
      refreshing = false;
    }
  }
  onMount(() => {
    const timer = setInterval(() => {
      if (terminal || attempts >= 60 || document.hidden) return;
      attempts += 1;
      void refresh();
    }, 3000);
    return () => clearInterval(timer);
  });
</script>

<section
  class="space-y-3 rounded-xl border border-border bg-card p-5"
  aria-label={m.probes_operation()}
  aria-live="polite"
>
  <div class="flex flex-wrap items-center justify-between gap-3">
    <h2 class="text-sm font-semibold">{m.probes_operation()}</h2>
    <ProbeStatus status={receipt.status} />
  </div>
  <p class="break-all font-mono text-xs text-muted-foreground">
    {receipt.operation_id} · {receipt.phase}
  </p>
  {#if receipt.error}<p class="text-sm text-danger" role="alert">
      {receipt.error.message}
    </p>
  {:else if !terminal}<p class="text-sm text-muted-foreground">
      {attempts >= 60 ? m.probes_operation_paused() : m.probes_operation_wait()}
    </p>{/if}
  {#if error}<p class="text-sm text-danger" role="alert">{error}</p>{/if}
  <button
    type="button"
    onclick={refresh}
    disabled={refreshing}
    class="rounded-lg border border-border px-3 py-2 text-sm hover:bg-accent disabled:opacity-60"
    >{m.probes_refresh()}</button
  >
</section>
