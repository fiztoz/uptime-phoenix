<script lang="ts">
  import { onMount } from "svelte";
  import { AlertTriangle, History, Radio, Trash2 } from "@lucide/svelte";
  import { incidentsApi, type Incident } from "$lib/api/incidents";
  import { auth } from "$lib/stores/auth.svelte";
  import { confirmAction } from "$lib/stores/confirm.svelte";
  import { toast } from "svelte-sonner";
  import Skeleton from "$lib/components/Skeleton.svelte";
  import EmptyState from "$lib/components/EmptyState.svelte";
  import * as m from "$lib/paraglide/messages.js";

  type IncidentView = "active" | "history";

  let incidents = $state<Incident[]>([]);
  let loading = $state(true);
  let loadError = $state<string | null>(null);
  let selectedView = $state<IncidentView>("active");
  let deletingIds = $state<Set<number>>(new Set());

  const isUserAdmin = $derived(auth.user?.is_admin ?? false);
  const activeIncidents = $derived(
    incidents.filter((incident) => incident.active),
  );
  const resolvedIncidents = $derived(
    incidents.filter((incident) => !incident.active),
  );
  const visibleIncidents = $derived(
    selectedView === "active" ? activeIncidents : resolvedIncidents,
  );

  async function load() {
    loading = true;
    loadError = null;
    try {
      incidents = await incidentsApi.list();
    } catch (error: unknown) {
      const message =
        error && typeof error === "object" && "message" in error
          ? String((error as { message: string }).message)
          : m.incidents_page_load_failed();
      loadError = message;
      toast.error(message);
    } finally {
      loading = false;
    }
  }

  onMount(load);

  async function deleteResolvedIncident(incident: Incident) {
    if (!isUserAdmin || incident.active || deletingIds.has(incident.id)) return;
    const confirmed = await confirmAction({
      title: m.incidents_delete_confirm_title({ title: incident.title }),
      message: m.incidents_delete_confirm_description(),
      confirmLabel: m.incidents_delete(),
      destructive: true,
    });
    if (!confirmed) return;

    deletingIds = new Set([...deletingIds, incident.id]);
    try {
      await incidentsApi.remove(incident);
      incidents = incidents.filter((item) => item.id !== incident.id);
      toast.success(m.incidents_delete_success());
    } catch (error: unknown) {
      const message =
        error && typeof error === "object" && "message" in error
          ? String((error as { message: string }).message)
          : m.incidents_delete_failed();
      toast.error(message);
    } finally {
      const next = new Set(deletingIds);
      next.delete(incident.id);
      deletingIds = next;
    }
  }

  function styleClass(style: string): string {
    if (style === "danger") return "bg-danger/10 text-danger border-danger/25";
    if (style === "warning")
      return "bg-warning/10 text-warning border-warning/25";
    if (style === "success")
      return "bg-success/10 text-success border-success/25";
    return "bg-muted/40 text-muted-foreground border-border";
  }
</script>

<svelte:head>
  <title>{m.app_name()} · {m.incidents_title()}</title>
</svelte:head>

<div class="space-y-6">
  <div>
    <h1 class="text-2xl font-semibold tracking-tight">{m.incidents_title()}</h1>
    <p class="mt-1 text-sm text-muted-foreground">
      {m.incidents_page_subtitle()}
    </p>
  </div>

  {#if loading}
    <div class="rounded-xl border border-border bg-card" role="status">
      <span class="sr-only">{m.incidents_page_loading()}</span>
      {#each Array(3) as _, index}
        <div class="px-5 py-4 {index < 2 ? 'border-b border-border' : ''}">
          <Skeleton class="h-4 w-44" /><Skeleton class="mt-3 h-3 w-2/3" />
        </div>
      {/each}
    </div>
  {:else if loadError}
    {#snippet retryAction()}
      <button
        type="button"
        onclick={load}
        class="inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >
        {m.monitor_group_form_retry()}
      </button>
    {/snippet}
    <EmptyState
      icon={AlertTriangle}
      title={m.incidents_page_load_failed()}
      description={loadError}
      action={retryAction}
    />
  {:else}
    <div
      class="inline-flex w-full rounded-xl border border-border bg-muted/30 p-1 sm:w-auto"
      aria-label={m.incidents_view_selector()}
    >
      <button
        type="button"
        aria-pressed={selectedView === "active"}
        onclick={() => (selectedView = "active")}
        class="inline-flex min-w-0 flex-1 items-center justify-center gap-2 rounded-lg px-3 py-2 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring sm:flex-none {selectedView ===
        'active'
          ? 'bg-card text-foreground shadow-sm'
          : 'text-muted-foreground hover:text-foreground'}"
      >
        <Radio class="h-4 w-4" />
        {m.incidents_active()}
        <span class="tnum text-xs text-muted-foreground"
          >{activeIncidents.length}</span
        >
      </button>
      <button
        type="button"
        aria-pressed={selectedView === "history"}
        onclick={() => (selectedView = "history")}
        class="inline-flex min-w-0 flex-1 items-center justify-center gap-2 rounded-lg px-3 py-2 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring sm:flex-none {selectedView ===
        'history'
          ? 'bg-card text-foreground shadow-sm'
          : 'text-muted-foreground hover:text-foreground'}"
      >
        <History class="h-4 w-4" />
        {m.incidents_history()}
        <span class="tnum text-xs text-muted-foreground"
          >{resolvedIncidents.length}</span
        >
      </button>
    </div>

    <div id="incident-list">
      {#if visibleIncidents.length === 0}
        <EmptyState
          icon={selectedView === "active" ? Radio : History}
          title={selectedView === "active"
            ? m.incidents_active_empty_title()
            : m.incidents_history_empty_title()}
          description={selectedView === "active"
            ? m.incidents_active_empty_description()
            : m.incidents_history_empty_description()}
        />
      {:else}
        <div class="overflow-hidden rounded-xl border border-border bg-card">
          {#each visibleIncidents as incident, index (incident.id)}
            <article
              class="px-5 py-4 {index !== visibleIncidents.length - 1
                ? 'border-b border-border'
                : ''}"
            >
              <div class="flex items-start justify-between gap-4">
                <div class="min-w-0 flex-1">
                  <div class="flex flex-wrap items-center gap-2">
                    <h2 class="font-medium tracking-tight">{incident.title}</h2>
                    <span
                      class="inline-flex rounded-full border px-2.5 py-0.5 text-xs font-medium {styleClass(
                        incident.style,
                      )}"
                    >
                      {incident.style}
                    </span>
                  </div>
                  <p class="mt-1 line-clamp-2 text-sm text-muted-foreground">
                    {incident.content || m.incidents_no_details()}
                  </p>
                  <p class="mt-2 text-xs text-muted-foreground">
                    {m.incidents_page_status_page_ref({
                      id: incident.status_page_id,
                    })}
                    <span class="mx-1">·</span>
                    {new Date(incident.created_at).toLocaleString()}
                  </p>
                </div>
                {#if selectedView === "history" && isUserAdmin}
                  <button
                    type="button"
                    disabled={deletingIds.has(incident.id)}
                    onclick={() => deleteResolvedIncident(incident)}
                    class="inline-flex shrink-0 items-center gap-1.5 rounded-lg border border-danger/25 px-3 py-1.5 text-xs font-medium text-danger transition-colors hover:bg-danger/10 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-60"
                  >
                    <Trash2 class="h-3.5 w-3.5" />
                    {deletingIds.has(incident.id)
                      ? m.incidents_deleting()
                      : m.incidents_delete()}
                  </button>
                {/if}
              </div>
            </article>
          {/each}
        </div>
      {/if}
    </div>
  {/if}
</div>
