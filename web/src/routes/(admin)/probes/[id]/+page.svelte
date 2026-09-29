<script lang="ts">
  import { onMount, untrack } from "svelte";
  import { realtime } from "$lib/stores/ws.svelte.js";
  import { page } from "$app/stores";
  import { goto } from "$app/navigation";
  import { resolve } from "$app/paths";
  import { ArrowLeft } from "@lucide/svelte";
  import { auth } from "$lib/stores/auth.svelte.js";
  import {
    probesApi,
    errorCode,
    errorMessage,
    type ProbeDetailView,
    type ProbeOperationView,
  } from "$lib/api/probes";
  import { probeWriteErrorMessage } from "$lib/probe-errors";
  import ProbeStatus from "$lib/components/ProbeStatus.svelte";
  import ProbeOperation from "$lib/components/ProbeOperation.svelte";
  import * as m from "$lib/paraglide/messages.js";
  let probe = $state<ProbeDetailView | null>(null);
  let loading = $state(true);
  let busy = $state(false);
  let error = $state("");
  let formRevision = $state("");
  let name = $state("");
  let location = $state("");
  let token = $state("");
  let enrollmentStream = $state("");
  let reason = $state("");
  let stream = $state("");
  let resetOperationId = $state("");
  let receipt = $state<ProbeOperationView | null>(null);
  let destructive = $state<"revoke" | "delete" | null>(null);
  let remoteUnconfirmed = $state(false);
  const inputClass =
    "w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring";
  const buttonClass =
    "rounded-lg border border-border px-4 py-2 text-sm hover:bg-accent disabled:opacity-60";
  const primaryButtonClass =
    "inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60";
  const remote = $derived(probe !== null && probe.id !== "local");
  const certificateDays = $derived(
    probe?.certificate_expires_at
      ? Math.ceil(
          (Date.parse(probe.certificate_expires_at) - Date.now()) / 86_400_000,
        )
      : null,
  );
  const showTime = (value: string | null | undefined) =>
    value ? new Date(value).toLocaleString() : m.probes_unreported();
  async function load(restoreForm = true) {
    if (!auth.user?.is_admin) {
      loading = false;
      return;
    }
    try {
      const next = await probesApi.detail($page.params.id ?? "");
      probe = next;
      if (restoreForm) {
        formRevision = next.revision;
        name = next.name;
        location = next.location;
      }
      if (restoreForm) error = "";
    } catch (e) {
      error =
        errorCode(e) === "probes_disabled"
          ? m.probes_disabled()
          : errorMessage(e, m.probes_load_failed());
    } finally {
      loading = false;
    }
  }
  async function run(action: () => Promise<void>) {
    if (busy) return;
    busy = true;
    error = "";
    try {
      await action();
    } catch (e) {
      error = probeWriteErrorMessage(e, m.probes_save_failed());
    } finally {
      busy = false;
    }
  }
  function updateReceipt(next: ProbeOperationView) {
    const wasTerminal =
      receipt && ["succeeded", "failed"].includes(receipt.status);
    receipt = next;
    if (!wasTerminal && ["succeeded", "failed"].includes(next.status))
      void load(false);
  }
  function save(event: SubmitEvent) {
    event.preventDefault();
    if (!probe) return;
    void run(async () => {
      if (!probe) return;
      probe = await probesApi.update(probe.id, {
        name,
        location,
        enabled: probe.enabled,
        revision: formRevision,
      });
      formRevision = probe.revision;
    });
  }
  function toggle() {
    void run(async () => {
      if (!probe) return;
      probe = await probesApi.update(probe.id, {
        name: probe.name,
        location: probe.location,
        enabled: !probe.enabled,
        revision: probe.revision,
      });
    });
  }
  function enroll(event: SubmitEvent) {
    event.preventDefault();
    const submittedToken = token;
    token = "";
    void run(async () => {
      if (probe)
        receipt = await probesApi.enroll(
          probe.id,
          submittedToken,
          enrollmentStream.trim(),
        );
    });
  }
  function rotate() {
    void run(async () => {
      if (probe?.credential_version)
        receipt = await probesApi.rotate(
          probe.id,
          (BigInt(probe.credential_version) + 1n).toString(),
        );
    });
  }
  function reset(event: SubmitEvent) {
    event.preventDefault();
    if (!resetOperationId) resetOperationId = crypto.randomUUID();
    void run(async () => {
      if (probe)
        receipt = await probesApi.reset(probe.id, stream, resetOperationId);
    });
  }
  function confirmAction(event: SubmitEvent) {
    event.preventDefault();
    void run(async () => {
      if (!probe) return;
      if (destructive === "delete") {
        await probesApi.remove(probe.id);
        await goto(resolve("/probes"));
      } else {
        const result = await probesApi.revoke(probe.id, reason);
        receipt = result;
        remoteUnconfirmed = !result.remote_confirmed;
        await load(false);
      }
      destructive = null;
    });
  }
  let eventTimer: ReturnType<typeof setTimeout> | undefined;
  let seenEpoch = 0;
  $effect(() => {
    const epoch = realtime.connectionEpoch;
    untrack(() => {
      if (seenEpoch && epoch !== seenEpoch) schedule();
      seenEpoch = epoch;
    });
  });
  function schedule() {
    if (eventTimer) return;
    eventTimer = setTimeout(() => {
      eventTimer = undefined;
      void load(false);
    }, 500);
  }
  onMount(() => {
    void load();
    const changed = (payload: unknown) => {
      if (
        typeof payload === "object" &&
        payload !== null &&
        "probe_id" in payload &&
        payload.probe_id === $page.params.id
      )
        schedule();
    };
    const offStatus = realtime.on("probe.status", changed);
    const offConfig = realtime.on("probe.config.status", changed);
    return () => {
      clearTimeout(eventTimer);
      offStatus();
      offConfig();
    };
  });
</script>

<svelte:head
  ><title>{probe?.name ?? m.probes_title()} · {m.app_name()}</title
  ></svelte:head
>
<div class="space-y-6">
  <a
    href={resolve("/probes")}
    class="inline-flex items-center gap-2 text-sm text-muted-foreground hover:text-foreground"
    ><ArrowLeft class="h-4 w-4" aria-hidden="true" />{m.probes_back()}</a
  >
  {#if !auth.user?.is_admin}<p role="alert">{m.probes_admin_only()}</p>
  {:else}
    {#if error}<div
        role="alert"
        class="space-y-3 rounded-xl border border-danger/25 bg-danger/10 p-5"
      >
        <p class="text-sm">{error}</p>
        <button class={buttonClass} onclick={() => load()}
          >{m.probes_retry()}</button
        >
      </div>{/if}
    {#if loading}<p
        role="status"
        class="animate-pulse text-sm text-muted-foreground"
      >
        {m.probes_loading()}
      </p>
    {:else if probe}
      <header class="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 class="text-2xl font-semibold tracking-tight">{probe.name}</h1>
          <p class="mt-2 break-all text-xs text-muted-foreground">
            {m.probes_id()}: <span class="font-mono">{probe.id}</span>
          </p>
          <p class="mt-2 text-sm text-muted-foreground">
            {probe.location} · <span class="font-mono">{probe.key}</span>
          </p>
        </div>
        <button class={buttonClass} onclick={() => load()} disabled={busy}
          >{m.probes_refresh()}</button
        >
      </header>
      <div class="grid gap-4 sm:grid-cols-3">
        {#each [[m.probes_enrollment(), probe.enrollment_state], [m.probes_connection(), probe.connection_status], [m.probes_execution(), probe.execution_status]] as [label, status] (label)}<div
            class="space-y-3 rounded-xl border border-border bg-card p-5"
          >
            <p class="eyebrow">{label}</p>
            <ProbeStatus {status} />
          </div>{/each}
      </div>
      {#if remoteUnconfirmed || probe.enrollment_state === "revoked"}<p
          role="status"
          class="rounded-xl border border-warning/25 bg-warning/10 p-5 text-sm"
        >
          {m.probes_remote_unconfirmed()}
        </p>{/if}
      {#if certificateDays !== null && certificateDays <= 30}<p
          class="rounded-xl border border-warning/25 bg-warning/10 p-5 text-sm"
          role="status"
        >
          {certificateDays <= 0
            ? m.probes_certificate_expired()
            : m.probes_certificate_expiring()}
        </p>{/if}
      {#if receipt}{#key receipt.operation_id}<ProbeOperation
            {receipt}
            onUpdate={updateReceipt}
          />{/key}{/if}
      {#if remote}
        <form
          onsubmit={save}
          class="space-y-4 rounded-xl border border-border bg-card p-5"
        >
          <div class="grid gap-4 sm:grid-cols-2">
            <label class="space-y-1 text-sm"
              >{m.probes_name()}<input
                class={inputClass}
                bind:value={name}
                required
              /></label
            ><label class="space-y-1 text-sm"
              >{m.probes_location()}<input
                class={inputClass}
                bind:value={location}
              /></label
            >
          </div>
          <div class="flex flex-wrap gap-3">
            <button type="submit" class={primaryButtonClass} disabled={busy}
              >{m.probes_save()}</button
            >{#if remote}<button
                type="button"
                class={buttonClass}
                onclick={toggle}
                disabled={busy || probe.enrollment_state === "revoked"}
                >{probe.enabled ? m.probes_pause() : m.probes_resume()}</button
              >{/if}
          </div>
          {#if remote}<p class="text-xs text-muted-foreground">
              {m.probes_pause_help()}
            </p>{/if}
        </form>
      {/if}
      {#if remote && probe.enrollment_state !== "revoked"}
        <form
          onsubmit={enroll}
          class="space-y-4 rounded-xl border border-border bg-card p-5"
        >
          <h2 class="text-sm font-semibold">{m.probes_enroll()}</h2>
          <p class="text-sm text-muted-foreground">{m.probes_token_help()}</p>
          <div class="space-y-1 text-sm">
            <label for="probe-enrollment-stream">{m.probes_stream_id()}</label>
            <input
              id="probe-enrollment-stream"
              class="{inputClass} font-mono"
              bind:value={enrollmentStream}
              required
              maxlength="36"
              pattern={"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"}
              aria-describedby="probe-enrollment-stream-help"
              spellcheck={false}
              autocomplete="off"
            />
            <span
              id="probe-enrollment-stream-help"
              class="block text-xs text-muted-foreground"
              >{m.probes_stream_help()}</span
            >
          </div>
          <label class="block space-y-1 text-sm"
            >{m.probes_token()}<input
              type="password"
              autocomplete="off"
              class={inputClass}
              bind:value={token}
              required
            /></label
          ><button
            type="submit"
            class={primaryButtonClass}
            disabled={busy || !token || !enrollmentStream}
            >{m.probes_enroll()}</button
          >
        </form>
      {/if}
      <section class="space-y-4 rounded-xl border border-border bg-card p-5">
        <div class="flex flex-wrap items-center justify-between gap-3">
          <h2 class="text-sm font-semibold">{m.probes_diagnostics()}</h2>
          <ProbeStatus status={probe.diagnostics.config.sync_status} />
        </div>
        <dl class="grid gap-5 sm:grid-cols-2 lg:grid-cols-3">
          {#each [[m.probes_endpoint(), probe.endpoint], [m.probes_tls(), probe.tls_fingerprint], [m.probes_desired_revision(), probe.diagnostics.config.desired?.revision], [m.probes_applied_revision(), probe.diagnostics.config.applied?.revision], [m.probes_certificate_expiry(), showTime(probe.certificate_expires_at)], [m.probes_credential_version(), probe.credential_version], [m.probes_agent_version(), probe.agent_version], [m.probes_protocol_version(), probe.protocol_version?.toString()], [m.probes_queue_bytes(), probe.queue_bytes?.toLocaleString()], [m.probes_oldest_queued(), showTime(probe.oldest_queued_at)], [m.probes_last_seen(), showTime(probe.last_seen_at)], [m.probes_capabilities(), probe.capabilities.join(", ") || null], [m.probes_connection_lease(), showTime(probe.diagnostics.connection?.lease_until)], [m.probes_runtime_lease(), showTime(probe.diagnostics.runtime?.lease_until)]] as [label, value] (label)}<div
            >
              <dt class="text-xs text-muted-foreground">{label}</dt>
              <dd class="mt-1 break-all font-mono text-xs">
                {value ?? m.probes_unreported()}
              </dd>
            </div>{/each}
          <div>
            <dt class="mb-1 text-xs text-muted-foreground">
              {m.probes_watchdog()}
            </dt>
            <dd><ProbeStatus status={probe.diagnostics.watchdog?.status} /></dd>
          </div>
        </dl>
      </section>
      {#if remote}
        <details class="rounded-xl border border-border bg-card p-5">
          <summary class="cursor-pointer text-sm font-semibold"
            >{m.probes_rotate()} / {m.probes_reset()}</summary
          >
          <div class="mt-4 space-y-6">
            <button
              class={buttonClass}
              onclick={rotate}
              disabled={busy ||
                !probe.credential_version ||
                probe.enrollment_state === "revoked"}
              >{m.probes_rotate()}</button
            >
            <form class="space-y-3" onsubmit={reset}>
              <p class="text-xs text-muted-foreground">
                {m.probes_reset_help()}
              </p>
              <label class="block space-y-1 text-sm"
                >{m.probes_stream_id()}<input
                  class={inputClass}
                  bind:value={stream}
                  required
                  onchange={() => (resetOperationId = "")}
                /></label
              ><button
                class={buttonClass}
                disabled={busy ||
                  !stream ||
                  probe.enrollment_state === "revoked"}
                >{m.probes_reset()}</button
              >
            </form>
          </div>
        </details>
        <section
          class="space-y-4 rounded-xl border border-danger/25 bg-card p-5"
        >
          <div class="flex flex-wrap gap-3">
            <button
              class="{buttonClass} text-danger"
              disabled={busy || probe.enrollment_state === "revoked"}
              onclick={() => (destructive = "revoke")}
              >{m.probes_revoke()}</button
            ><button
              class="{buttonClass} text-danger"
              disabled={busy}
              onclick={() => (destructive = "delete")}
              >{m.probes_delete()}</button
            >
          </div>
          {#if destructive}<form onsubmit={confirmAction} class="space-y-3">
              <p class="text-sm text-muted-foreground">
                {destructive === "revoke"
                  ? m.probes_revoke_help()
                  : m.probes_delete_help()}
              </p>
              {#if destructive === "revoke"}<label
                  class="block space-y-1 text-sm"
                  >{m.probes_reason()}<input
                    class={inputClass}
                    bind:value={reason}
                    required
                    maxlength="512"
                  /></label
                >{/if}
              <div class="flex gap-3">
                <button class="{buttonClass} text-danger" disabled={busy}
                  >{m.probes_confirm()}</button
                ><button
                  type="button"
                  class={buttonClass}
                  onclick={() => (destructive = null)}
                  >{m.probes_cancel()}</button
                >
              </div>
            </form>{/if}
        </section>
      {/if}
    {/if}
  {/if}
</div>
