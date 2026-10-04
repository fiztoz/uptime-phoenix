<script lang="ts">
  import { onMount, untrack } from "svelte";
  import { realtime } from "$lib/stores/ws.svelte.js";
  import { goto } from "$app/navigation";
  import { resolve } from "$app/paths";
  import { Network, Plus } from "@lucide/svelte";
  import { auth } from "$lib/stores/auth.svelte.js";
  import {
    probesApi,
    errorCode,
    errorMessage,
    type ProbeView,
  } from "$lib/api/probes";
  import ProbeStatus from "$lib/components/ProbeStatus.svelte";
  import EmptyState from "$lib/components/EmptyState.svelte";
  import * as m from "$lib/paraglide/messages.js";
  let items = $state<ProbeView[]>([]);
  let cursor = $state<string | null>(null);
  let loading = $state(true);
  let error = $state("");
  let saving = $state(false);
  let creating = $state(false);
  let search = $state("");
  let registration = $state({
    probe_id: "",
    key: "",
    name: "",
    location: "",
    endpoint: "",
    tls_fingerprint: "",
  });
  const filtered = $derived(
    items.filter((p) =>
      `${p.name} ${p.location} ${p.key}`
        .toLowerCase()
        .includes(search.toLowerCase()),
    ),
  );
  const inputClass =
    "w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring";
  const buttonClass =
    "inline-flex items-center gap-2 rounded-lg border border-border px-4 py-2 text-sm hover:bg-accent disabled:opacity-60";
  const primaryButtonClass =
    "inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60";
  async function load(more = false) {
    if (!auth.user?.is_admin) {
      loading = false;
      return;
    }
    loading = true;
    error = "";
    try {
      const result = await probesApi.list(
        more ? (cursor ?? undefined) : undefined,
      );
      items = more ? [...items, ...result.items] : result.items;
      cursor = result.next_cursor;
    } catch (e) {
      error =
        errorCode(e) === "probes_disabled"
          ? m.probes_disabled()
          : errorMessage(e, m.probes_load_failed());
    } finally {
      loading = false;
    }
  }
  async function create(event: SubmitEvent) {
    event.preventDefault();
    saving = true;
    error = "";
    try {
      const created = await probesApi.create(registration);
      await goto(resolve(`/probes/${encodeURIComponent(created.id)}`));
    } catch (e) {
      error = errorMessage(e, m.probes_save_failed());
    } finally {
      saving = false;
    }
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
        items.some((p) => p.id === payload.probe_id)
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

<svelte:head><title>{m.probes_title()} · {m.app_name()}</title></svelte:head>
<div class="space-y-6">
  <div class="flex flex-wrap items-start justify-between gap-4">
    <div>
      <h1 class="text-2xl font-semibold tracking-tight">{m.probes_title()}</h1>
      <p class="mt-2 max-w-2xl text-sm text-muted-foreground">
        {m.probes_description()}
      </p>
    </div>
    {#if auth.user?.is_admin}<button
        class="inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90"
        onclick={() => (creating = !creating)}
        aria-expanded={creating}
        ><Plus class="h-4 w-4" />{m.probes_add()}</button
      >{/if}
  </div>
  {#if !auth.user?.is_admin}<p
      role="alert"
      class="text-sm text-muted-foreground"
    >
      {m.probes_admin_only()}
    </p>
  {:else}
    {#if creating}<form
        onsubmit={create}
        class="space-y-4 rounded-xl border border-border bg-card p-5"
      >
        <h2 class="text-sm font-semibold">{m.probes_add()}</h2>
        <p class="text-sm text-muted-foreground">
          {m.probes_registration_help()}
        </p>
        <div class="grid gap-4 sm:grid-cols-2">
          <div class="space-y-1 text-sm sm:col-span-2">
            <label for="probe-registration-id">{m.probes_id()}</label>
            <input
              id="probe-registration-id"
              class="{inputClass} font-mono"
              bind:value={registration.probe_id}
              required
              maxlength="36"
              pattern={"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"}
              aria-describedby="probe-registration-id-help"
              spellcheck={false}
              autocomplete="off"
            />
            <span
              id="probe-registration-id-help"
              class="block text-xs text-muted-foreground"
              >{m.probes_id_help()}</span
            >
          </div>
          <label class="space-y-1 text-sm"
            >{m.probes_key()}<input
              class={inputClass}
              bind:value={registration.key}
              required
              maxlength="63"
              pattern="[a-z][a-z0-9\-]*"
              placeholder="singapore"
            /></label
          >
          <label class="space-y-1 text-sm"
            >{m.probes_name()}<input
              class={inputClass}
              bind:value={registration.name}
              required
              maxlength="255"
            /></label
          >
          <label class="space-y-1 text-sm"
            >{m.probes_location()}<input
              class={inputClass}
              bind:value={registration.location}
              maxlength="255"
            /></label
          >
          <label class="space-y-1 text-sm"
            >{m.probes_endpoint()}<input
              class={inputClass}
              bind:value={registration.endpoint}
              required
              placeholder="probe.example.com:8443"
            /></label
          >
          <label class="space-y-1 text-sm sm:col-span-2"
            >{m.probes_tls()}<input
              class="{inputClass} font-mono"
              bind:value={registration.tls_fingerprint}
              required
              pattern={"[a-fA-F0-9]{64}"}
              maxlength="64"
            /></label
          >
        </div>
        <div class="flex gap-3">
          <button type="submit" class={primaryButtonClass} disabled={saving}
            >{saving ? m.probes_saving() : m.probes_add()}</button
          ><button
            type="button"
            class={buttonClass}
            onclick={() => (creating = false)}>{m.probes_cancel()}</button
          >
        </div>
      </form>{/if}
    {#if error}<div
        class="space-y-3 rounded-xl border border-danger/25 bg-danger/10 p-5"
        role="alert"
      >
        <p class="text-sm">{error}</p>
        <button class={buttonClass} onclick={() => load()}
          >{m.probes_retry()}</button
        >
      </div>{/if}
    <div class="flex flex-wrap gap-3">
      <input
        aria-label={m.probes_search()}
        placeholder={m.probes_search()}
        class="{inputClass} min-w-0 flex-1"
        bind:value={search}
      /><button class={buttonClass} onclick={() => load()} disabled={loading}
        >{m.probes_refresh()}</button
      >
    </div>
    {#if loading && items.length === 0}<p
        role="status"
        class="animate-pulse text-sm text-muted-foreground"
      >
        {m.probes_loading()}
      </p>
    {:else if !error && items.length === 0}<EmptyState
        icon={Network}
        title={m.probes_empty()}
        description={m.probes_empty_help()}
      />
    {:else if filtered.length === 0 && !error}<EmptyState
        icon={Network}
        title={m.probes_no_results()}
      />
    {:else}<div class="space-y-3">
        {#each filtered as probe (probe.id)}
          <a
            href={resolve(`/probes/${encodeURIComponent(probe.id)}`)}
            class="block rounded-xl border border-border bg-card p-5 transition-colors hover:border-primary/30"
            data-testid="probe-row"
          >
            <div class="flex flex-wrap items-start justify-between gap-3">
              <div>
                <h2 class="font-medium">{probe.name}</h2>
                <p class="mt-1 text-xs text-muted-foreground">
                  {probe.location || probe.key}
                </p>
              </div>
              <ProbeStatus status={probe.enrollment_state} />
            </div>
            <dl class="mt-4 grid grid-cols-2 gap-4 sm:grid-cols-4">
              <div>
                <dt class="mb-1 text-xs text-muted-foreground">
                  {m.probes_connection()}
                </dt>
                <dd><ProbeStatus status={probe.connection_status} /></dd>
              </div>
              <div>
                <dt class="mb-1 text-xs text-muted-foreground">
                  {m.probes_execution()}
                </dt>
                <dd><ProbeStatus status={probe.execution_status} /></dd>
              </div>
              <div>
                <dt class="mb-1 text-xs text-muted-foreground">
                  {m.probes_config()}
                </dt>
                <dd class="break-all font-mono text-xs">
                  {probe.applied_config_revision} / {probe.desired_config_revision}
                </dd>
              </div>
              <div>
                <dt class="mb-1 text-xs text-muted-foreground">
                  {m.probes_last_seen()}
                </dt>
                <dd class="text-xs">
                  {probe.last_seen_at
                    ? new Date(probe.last_seen_at).toLocaleString()
                    : m.probes_unreported()}
                </dd>
              </div>
            </dl>
          </a>{/each}
      </div>{/if}
    {#if cursor}<button
        class={buttonClass}
        onclick={() => load(true)}
        disabled={loading}>{m.probes_more()}</button
      >{/if}
  {/if}
</div>
