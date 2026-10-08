<script lang="ts">
  import { onMount } from "svelte";
  import {
    probesApi,
    errorCode,
    errorMessage,
    type ProbeView,
  } from "$lib/api/probes";
  import {
    regionalApi,
    type MonitorProbeAssignmentsView,
  } from "$lib/api/regional";
  import type { MonitorInitialAssignments } from "$lib/api/monitors";
  import { probeWriteErrorMessage } from "$lib/probe-errors";
  import ProbeStatus from "./ProbeStatus.svelte";
  import * as m from "$lib/paraglide/messages.js";
  let {
    monitorId,
    monitorType,
    standalone = false,
    onSaved,
  }: {
    monitorId?: number;
    monitorType: string;
    standalone?: boolean;
    onSaved?: () => void;
  } = $props();
  let probes = $state<ProbeView[]>([]);
  let cursor = $state<string | null>(null);
  let baseline = $state<MonitorProbeAssignmentsView | null>(null);
  let selected = $state<string[]>(["local"]);
  let policy = $state("any_down");
  let bindings = $state<Record<string, { kind: string; binding_key: string }>>(
    {},
  );
  let bindingsEdited = $state(false);
  let loading = $state(true);
  let saving = $state(false);
  let unavailable = $state(false);
  let error = $state("");
  let saved = $state(false);
  const inputClass =
    "w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring";
  const options = $derived([
    ...probes,
    ...(baseline?.assignments ?? [])
      .filter((a) => !probes.some((p) => p.id === a.probe_id))
      .map((a) => ({
        id: a.probe_id,
        name: a.name,
        location: a.location,
        enabled: true,
        execution_status: "unknown",
      })),
  ]);
  async function load() {
    loading = true;
    error = "";
    try {
      const [fleet, assignments] = await Promise.all([
        probesApi.list(),
        monitorId ? regionalApi.assignments(monitorId) : Promise.resolve(null),
      ]);
      probes = fleet.items;
      cursor = fleet.next_cursor;
      baseline = assignments;
      unavailable = false;
      if (assignments) {
        selected = assignments.assignments.map((a) => a.probe_id);
        policy = assignments.health_policy;
        bindings = Object.fromEntries(
          assignments.assignments
            .filter((a) => a.bindings.length)
            .map((a) => [a.probe_id, { ...a.bindings[0] }]),
        );
      }
      bindingsEdited = false;
      saved = false;
    } catch (e) {
      unavailable = errorCode(e) === "probes_disabled";
      if (!unavailable) error = errorMessage(e, m.probes_load_failed());
    } finally {
      loading = false;
    }
  }
  async function more() {
    try {
      const next = await probesApi.list(cursor ?? undefined);
      probes = [...probes, ...next.items];
      cursor = next.next_cursor;
    } catch (e) {
      error = errorMessage(e, m.probes_load_failed());
    }
  }
  function toggle(id: string, checked: boolean) {
    selected = checked ? [...selected, id] : selected.filter((p) => p !== id);
    saved = false;
  }
  function setBinding(
    id: string,
    field: "kind" | "binding_key",
    value: string,
  ) {
    bindings = {
      ...bindings,
      [id]: {
        kind: bindings[id]?.kind ?? "docker_socket",
        binding_key: bindings[id]?.binding_key ?? "",
        [field]: value,
      },
    };
    bindingsEdited = true;
    saved = false;
  }
  /**
   * The initial assignment draft for the atomic create path (issue #60).
   *
   * Throws while the editor cannot answer honestly — still loading, load
   * failed, or nothing selected — so the form aborts before any request and
   * shows the message. When probes are disabled on this server the local
   * default is returned and no explicit-assignment API is ever needed: the
   * create POST omits the set and the backend local-default path runs the
   * monitor on "local". Only bindings of SELECTED probes ride the draft.
   */
  export function initialAssignments(): MonitorInitialAssignments {
    if (unavailable) return { probe_ids: ["local"], health_policy: "any_down" };
    if (loading) throw new Error(m.probes_loading());
    if (error) throw new Error(error);
    if (selected.length === 0) {
      error = m.probes_select_one();
      throw new Error(error);
    }
    return {
      probe_ids: [...selected],
      health_policy: policy,
      probe_bindings: selected.flatMap((id) => {
        const binding = bindings[id];
        const binding_key = binding?.binding_key.trim();
        return binding && binding_key
          ? [{ probe_id: id, kind: binding.kind, binding_key }]
          : [];
      }),
    };
  }
  export async function save(id: number): Promise<boolean> {
    if (unavailable) return true;
    if (loading || error || selected.length === 0) {
      if (!error) error = m.probes_select_one();
      return false;
    }
    const unchanged =
      baseline &&
      !bindingsEdited &&
      policy === baseline.health_policy &&
      selected.length === baseline.assignments.length &&
      baseline.assignments.every((a) => selected.includes(a.probe_id));
    if (
      unchanged ||
      (!baseline &&
        !bindingsEdited &&
        policy === "any_down" &&
        selected.length === 1 &&
        selected[0] === "local")
    )
      return true;
    saving = true;
    error = "";
    try {
      const current = baseline ?? (await regionalApi.assignments(id));
      baseline = await regionalApi.replace(id, {
        expected_revision: current.revision,
        probe_ids: selected,
        health_policy: policy,
        alert_delivery: current.alert_delivery || "regional",
        ...(bindingsEdited
          ? {
              bindings: selected
                .filter((p) => bindings[p]?.binding_key.trim())
                .map((p) => ({
                  probe_id: p,
                  kind: bindings[p].kind,
                  binding_key: bindings[p].binding_key.trim(),
                })),
            }
          : {}),
      });
      bindingsEdited = false;
      saved = true;
      onSaved?.();
      return true;
    } catch (e) {
      error = probeWriteErrorMessage(e, m.probes_save_failed());
      return false;
    } finally {
      saving = false;
    }
  }
  onMount(() => {
    void load();
  });
</script>

{#if !unavailable}
  <section
    class="space-y-4 rounded-xl border border-border bg-card p-5"
    aria-label={m.probes_regions()}
  >
    <h2 class="text-sm font-semibold">{m.probes_regions()}</h2>
    <p class="text-xs text-muted-foreground">{m.probes_regions_help()}</p>
    {#if loading}<p
        role="status"
        class="animate-pulse text-sm text-muted-foreground"
      >
        {m.probes_loading()}
      </p>
    {:else}
      {#if error}<div role="alert" class="space-y-2 text-sm text-danger">
          <p>{error}</p>
          <button
            type="button"
            class="rounded-lg border border-border px-3 py-2 text-foreground hover:bg-accent"
            onclick={load}>{m.probes_refresh()}</button
          >
        </div>{/if}
      <div class="space-y-2">
        {#each options as probe (probe.id)}
          <label
            class="flex cursor-pointer items-start gap-3 rounded-lg border border-border p-3"
            ><input
              type="checkbox"
              class="mt-1 h-4 w-4 accent-primary"
              checked={selected.includes(probe.id)}
              disabled={!probe.enabled && !selected.includes(probe.id)}
              onchange={(e) => toggle(probe.id, e.currentTarget.checked)}
            /><span class="min-w-0 flex-1 text-sm"
              ><span class="block font-medium">{probe.name}</span><span
                class="text-xs text-muted-foreground">{probe.location}</span
              ></span
            ><ProbeStatus
              status={baseline?.assignments.find((a) => a.probe_id === probe.id)
                ?.sync_status ?? probe.execution_status}
            /></label
          >
        {/each}
      </div>
      {#if cursor}<button
          type="button"
          class="text-sm text-primary"
          onclick={more}>{m.probes_more()}</button
        >{/if}
      <label class="block space-y-1 text-sm"
        >{m.probes_health_policy()}<select
          class={inputClass}
          bind:value={policy}
          ><option value="any_down">{m.probes_any_down()}</option><option
            value="all_down">{m.probes_all_down()}</option
          ></select
        ></label
      >
      <p class="text-xs text-muted-foreground">
        {m.probes_regional_delivery()}
      </p>
      {#if baseline?.alert_delivery_pending}
        <p class="text-xs text-muted-foreground" role="status">
          {m.probes_aggregate_pending()}
        </p>
      {/if}
      {#if monitorType === "docker" || Object.keys(bindings).length}
        <details>
          <summary class="cursor-pointer text-sm">{m.probes_bindings()}</summary
          >
          <div class="mt-3 space-y-4">
            <p class="text-xs text-muted-foreground">
              {m.probes_binding_help()}
            </p>
            {#each selected.filter((id) => id !== "local") as id (id)}<fieldset
                class="space-y-2"
              >
                <legend class="text-sm font-medium"
                  >{options.find((p) => p.id === id)?.name ?? id}</legend
                ><label class="block space-y-1 text-xs"
                  >{m.probes_binding_kind()}<select
                    class={inputClass}
                    value={bindings[id]?.kind ?? "docker_socket"}
                    onchange={(e) =>
                      setBinding(id, "kind", e.currentTarget.value)}
                    ><option value="docker_socket">Docker socket</option><option
                      value="docker_api">Docker API</option
                    ></select
                  ></label
                ><label class="block space-y-1 text-xs"
                  >{m.probes_binding_key()}<input
                    class={inputClass}
                    value={bindings[id]?.binding_key ?? ""}
                    oninput={(e) => {
                      if (!bindings[id])
                        setBinding(id, "kind", "docker_socket");
                      setBinding(id, "binding_key", e.currentTarget.value);
                    }}
                  /></label
                >
              </fieldset>{/each}
          </div>
        </details>
      {/if}
      {#if saved}<p role="status" class="text-sm text-warning">
          {m.probes_assignment_saved()}
        </p>{/if}
      {#if standalone && monitorId}<button
          type="button"
          disabled={saving || selected.length === 0}
          onclick={() => monitorId && save(monitorId)}
          class="rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground hover:bg-primary/90 disabled:opacity-60"
          >{saving ? m.probes_saving() : m.probes_save()}</button
        >{/if}
    {/if}
  </section>
{/if}
