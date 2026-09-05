<script lang="ts">
  import { onMount, untrack } from "svelte";
  import {
    BarChart3,
    Clock3,
    Flame,
    Gauge,
    RefreshCw,
    Search,
    Server,
  } from "@lucide/svelte";
  import EmptyState from "$lib/components/EmptyState.svelte";
  import Skeleton from "$lib/components/Skeleton.svelte";
  import SortIndicator from "$lib/components/SortIndicator.svelte";
  import { insightsApi, type InsightsMetric, type InsightsPeriod, type InsightsRow } from "$lib/api/insights";
  import { readInsightsCache, writeInsightsCache } from "$lib/insights-cache";
  import { tokenFingerprint } from "$lib/monitor-snapshot-cache";
  import { monitorGroupsApi, type MonitorGroupView } from "$lib/api/monitorGroups";
  import { MONITOR_TYPE_GROUPS, monitorTypeConfig } from "$lib/monitor-types";
  import * as m from "$lib/paraglide/messages.js";

  type SortKey =
    | "monitor_name"
    | "availability_percent"
    | "outage_count"
    | "downtime_seconds"
    | "latency_avg_ms"
    | "flap_count"
    | "coverage_percent";
  type SortDirection = "asc" | "desc";

  const periods: InsightsPeriod[] = ["24h", "7d", "30d", "90d"];
  const metrics: InsightsMetric[] = [
    "availability",
    "outages",
    "downtime",
    "latency",
    "flapping",
  ];
  const typeOptions = MONITOR_TYPE_GROUPS.flatMap((group) => group.types).map((type) => ({
    value: type,
    label: monitorTypeConfig[type]?.label ?? type,
  }));

  let period = $state<InsightsPeriod>("7d");
  let metric = $state<InsightsMetric>("availability");
  let selectedType = $state("");
  let selectedGroup = $state("");
  let search = $state("");
  let rows = $state<InsightsRow[]>([]);
  let groups = $state<MonitorGroupView[]>([]);
  let loading = $state(true);
  let groupsLoading = $state(true);
  let loadError = $state<string | null>(null);
  let sortKey = $state<SortKey>("availability_percent");
  let sortDirection = $state<SortDirection>("asc");
  let requestSerial = 0;
  /** True while a background refresh runs against already-painted rows. */
  let refreshing = $state(false);

  const groupNames = $derived(new Map(groups.map((group) => [group.id, group.name])));
  const visibleRows = $derived(
    rows.filter((row) =>
      row.monitor_name.toLocaleLowerCase().includes(search.trim().toLocaleLowerCase()),
    ),
  );
  const sortedRows = $derived([...visibleRows].sort(compareRows));
  // Highlights use observed extrema even when coverage is below the ranking threshold.
  const lowestAvailability = $derived(
    visibleRows
      .filter((row) => row.availability_percent !== null)
      .toSorted((a, b) => (a.availability_percent ?? 101) - (b.availability_percent ?? 101))[0],
  );
  const mostDowntime = $derived(
    visibleRows
      .filter((row) => row.downtime_seconds > 0)
      .toSorted((a, b) => b.downtime_seconds - a.downtime_seconds)[0],
  );
  const mostFlapping = $derived(
    visibleRows
      .filter((row) => row.flap_count > 0)
      .toSorted((a, b) => b.flap_count - a.flap_count)[0],
  );

  onMount(async () => {
    try {
      groups = await monitorGroupsApi.list();
    } catch {
      groups = [];
    } finally {
      groupsLoading = false;
    }
  });

  $effect(() => {
    // Refetch only when the data WINDOW changes. `metric` merely controls the
    // server-side ordering — every row always carries every metric and the
    // client re-sorts locally (compareRows) — so switching metric tabs must
    // not blank the table into skeletons with a pointless round trip.
    const periodValue = period;
    const typeValue = selectedType;
    const groupValue = selectedGroup;
    const currentSerial = ++requestSerial;
    void loadInsights(currentSerial, periodValue, typeValue, groupValue ? Number(groupValue) : undefined);
  });

  async function loadInsights(
    serial: number,
    periodValue: InsightsPeriod,
    typeValue: string,
    groupId: number | undefined,
  ) {
    loadError = null;
    // Stale-while-revalidate: paint the last known rows instantly and refresh
    // in the background. Skeletons only on a true cache miss.
    const stored =
      typeof localStorage !== "undefined"
        ? localStorage.getItem("phoenix_jwt")
        : null;
    const owner = stored ? tokenFingerprint(stored) : null;
    const cached = readInsightsCache(owner, periodValue, typeValue, groupId);
    if (cached) {
      rows = cached;
      loading = false;
      refreshing = true;
    } else {
      loading = true;
      refreshing = false;
    }
    try {
      const response = await insightsApi.list({
        period: periodValue,
        metric: untrack(() => metric),
        type: typeValue || undefined,
        group_id: groupId,
      });
      if (serial !== requestSerial) return;
      rows = response.rows;
      writeInsightsCache(owner, periodValue, typeValue, groupId, response.rows);
    } catch (error: unknown) {
      if (serial !== requestSerial) return;
      if (cached) {
        // Stale rows beat an error screen; the refresh button retries.
        return;
      }
      loadError = error instanceof Error ? error.message : m.insights_load_failed();
      rows = [];
    } finally {
      if (serial === requestSerial) {
        loading = false;
        refreshing = false;
      }
    }
  }

  function selectMetric(next: InsightsMetric) {
    metric = next;
    if (next === "latency" && !selectedType && typeOptions.length > 0) {
      selectedType = typeOptions[0].value;
    }
    if (next === "availability") {
      sortKey = "availability_percent";
      sortDirection = "asc";
    } else if (next === "outages") {
      sortKey = "outage_count";
      sortDirection = "desc";
    } else if (next === "downtime") {
      sortKey = "downtime_seconds";
      sortDirection = "desc";
    } else if (next === "latency") {
      sortKey = "latency_avg_ms";
      sortDirection = "desc";
    } else {
      sortKey = "flap_count";
      sortDirection = "desc";
    }
  }

  function selectPeriod(next: InsightsPeriod) {
    period = next;
  }

  function sortBy(next: SortKey) {
    if (sortKey === next) {
      sortDirection = sortDirection === "asc" ? "desc" : "asc";
    } else {
      sortKey = next;
      sortDirection = next === "monitor_name" ? "asc" : "desc";
    }
  }

  function compareRows(a: InsightsRow, b: InsightsRow): number {
    const aQualified = a.qualification === "qualified";
    const bQualified = b.qualification === "qualified";
    if (aQualified !== bQualified) return aQualified ? -1 : 1;

    let result = 0;
    if (sortKey === "monitor_name") {
      result = a.monitor_name.localeCompare(b.monitor_name);
    } else {
      const aValue = numericValue(a, sortKey);
      const bValue = numericValue(b, sortKey);
      if (aValue !== bValue) result = aValue - bValue;
      else result = b.coverage_percent - a.coverage_percent;
    }
    if (result === 0) {
      result = a.monitor_name.localeCompare(b.monitor_name) || a.monitor_id - b.monitor_id;
    }
    return sortDirection === "asc" ? result : -result;
  }

  function numericValue(row: InsightsRow, key: Exclude<SortKey, "monitor_name">): number {
    if (key === "availability_percent") return row.availability_percent ?? 101;
    if (key === "latency_avg_ms") return row.latency_avg_ms ?? -1;
    return row[key];
  }

  function formatPercent(value: number | null): string {
    return value === null ? "—" : `${value.toFixed(2)}%`;
  }

  function isPartialHighlight(row: InsightsRow): boolean {
    return row.qualification !== "qualified";
  }

  function formatDuration(seconds: number): string {
    if (seconds < 60) return `${Math.round(seconds)}s`;
    const minutes = Math.round(seconds / 60);
    if (minutes < 60) return `${minutes}m`;
    const hours = Math.floor(minutes / 60);
    const remainingMinutes = minutes % 60;
    if (hours < 24) return remainingMinutes ? `${hours}h ${remainingMinutes}m` : `${hours}h`;
    const days = Math.floor(hours / 24);
    const remainingHours = hours % 24;
    return remainingHours ? `${days}d ${remainingHours}h` : `${days}d`;
  }

  function typeLabel(type: string): string {
    return monitorTypeConfig[type]?.label ?? type;
  }

  function metricLabel(value: InsightsMetric): string {
    if (value === "availability") return m.insights_metric_availability();
    if (value === "outages") return m.insights_metric_outages();
    if (value === "downtime") return m.insights_metric_downtime();
    if (value === "latency") return m.insights_metric_latency();
    return m.insights_metric_flapping();
  }

  function periodLabel(value: InsightsPeriod): string {
    if (value === "24h") return m.insights_period_24h();
    if (value === "7d") return m.insights_period_7d();
    if (value === "30d") return m.insights_period_30d();
    return m.insights_period_90d();
  }

</script>

<svelte:head>
  <title>{m.app_name()} · {m.insights_title()}</title>
</svelte:head>

<div class="space-y-6">
  <div class="flex flex-col justify-between gap-4 lg:flex-row lg:items-end">
    <div>
      <h1 class="text-2xl font-semibold tracking-tight">{m.insights_title()}</h1>
      <p class="mt-1 max-w-2xl text-sm text-muted-foreground">
        {m.insights_subtitle()}
      </p>
    </div>
    <button
      type="button"
      onclick={() => loadInsights(++requestSerial, period, selectedType, selectedGroup ? Number(selectedGroup) : undefined)}
      class="inline-flex items-center justify-center gap-2 rounded-lg border border-border px-3 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      <RefreshCw class="h-4 w-4 {refreshing ? 'animate-spin' : ''}" />
      {m.insights_refresh()}
    </button>
  </div>

  <section class="grid gap-3 sm:grid-cols-3" aria-label={m.insights_summary_label()}>
    <article class="rounded-xl border border-border bg-card p-4">
      <div class="flex items-center justify-between gap-3">
        <span class="text-xs font-medium uppercase tracking-wider text-muted-foreground">{m.insights_lowest_availability()}</span>
        <Gauge class="h-4 w-4 text-danger" />
      </div>
      {#if loading}
        <Skeleton class="mt-4 h-7 w-32" />
      {:else if lowestAvailability}
        <p class="mt-3 truncate text-lg font-semibold">{lowestAvailability.monitor_name}</p>
        <p class="mt-1 text-sm text-danger">{formatPercent(lowestAvailability.availability_percent)}</p>
        {#if isPartialHighlight(lowestAvailability)}
          <p class="mt-2 text-xs text-warning">{m.insights_highlight_partial({ coverage: lowestAvailability.coverage_percent.toFixed(1) })}</p>
        {/if}
      {:else}
        <p class="mt-4 text-sm text-muted-foreground">{m.insights_no_qualified_data()}</p>
      {/if}
    </article>

    <article class="rounded-xl border border-border bg-card p-4">
      <div class="flex items-center justify-between gap-3">
        <span class="text-xs font-medium uppercase tracking-wider text-muted-foreground">{m.insights_most_downtime()}</span>
        <Clock3 class="h-4 w-4 text-warning" />
      </div>
      {#if loading}
        <Skeleton class="mt-4 h-7 w-32" />
      {:else if mostDowntime}
        <p class="mt-3 truncate text-lg font-semibold">{mostDowntime.monitor_name}</p>
        <p class="mt-1 text-sm text-warning">{formatDuration(mostDowntime.downtime_seconds)}</p>
        {#if isPartialHighlight(mostDowntime)}
          <p class="mt-2 text-xs text-warning">{m.insights_highlight_partial({ coverage: mostDowntime.coverage_percent.toFixed(1) })}</p>
        {/if}
      {:else}
        <p class="mt-4 text-sm text-muted-foreground">{m.insights_no_qualified_data()}</p>
      {/if}
    </article>

    <article class="rounded-xl border border-border bg-card p-4">
      <div class="flex items-center justify-between gap-3">
        <span class="text-xs font-medium uppercase tracking-wider text-muted-foreground">{m.insights_most_flapping()}</span>
        <Flame class="h-4 w-4 text-info" />
      </div>
      {#if loading}
        <Skeleton class="mt-4 h-7 w-32" />
      {:else if mostFlapping}
        <p class="mt-3 truncate text-lg font-semibold">{mostFlapping.monitor_name}</p>
        <p class="mt-1 text-sm text-info">{mostFlapping.flap_count} {m.insights_transitions()}</p>
        {#if isPartialHighlight(mostFlapping)}
          <p class="mt-2 text-xs text-warning">{m.insights_highlight_partial({ coverage: mostFlapping.coverage_percent.toFixed(1) })}</p>
        {/if}
      {:else}
        <p class="mt-4 text-sm text-muted-foreground">{m.insights_no_qualified_data()}</p>
      {/if}
    </article>
  </section>

  <section class="space-y-3 rounded-xl border border-border bg-card p-4">
    <div class="flex flex-col gap-3 xl:flex-row xl:items-center xl:justify-between">
      <div class="inline-flex w-full overflow-x-auto rounded-lg border border-border bg-muted/30 p-1 xl:w-auto" aria-label={m.insights_period_label()}>
        {#each periods as value}
          <button
            type="button"
            aria-pressed={period === value}
            onclick={() => selectPeriod(value)}
            class="min-w-16 rounded-md px-3 py-2 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring {period === value ? 'bg-card text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}"
          >{periodLabel(value)}</button>
        {/each}
      </div>
      <div class="flex flex-wrap gap-2" aria-label={m.insights_metric_label()}>
        {#each metrics as value}
          <button
            type="button"
            aria-pressed={metric === value}
            onclick={() => selectMetric(value)}
            class="rounded-full border px-3 py-1.5 text-xs font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring {metric === value ? 'border-primary/40 bg-primary/10 text-primary' : 'border-border text-muted-foreground hover:bg-accent hover:text-foreground'}"
          >{metricLabel(value)}</button>
        {/each}
      </div>
    </div>

    <div class="grid gap-3 md:grid-cols-[minmax(0,1fr)_12rem_12rem]">
      <label class="relative block">
        <span class="sr-only">{m.insights_search_label()}</span>
        <Search class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground" />
        <input
          type="search"
          bind:value={search}
          placeholder={m.insights_search_placeholder()}
          class="w-full rounded-lg border border-border bg-background py-2 pl-9 pr-3 text-sm outline-none transition-colors placeholder:text-muted-foreground focus:border-primary focus:ring-2 focus:ring-ring/30"
        />
      </label>
      <label>
        <span class="sr-only">{m.insights_type_filter()}</span>
        <select bind:value={selectedType} class="w-full rounded-lg border border-border bg-background px-3 py-2 text-sm outline-none focus:border-primary focus:ring-2 focus:ring-ring/30">
          <option value="">{m.insights_all_types()}</option>
          {#each typeOptions as option}
            <option value={option.value}>{option.label}</option>
          {/each}
        </select>
      </label>
      <label>
        <span class="sr-only">{m.insights_group_filter()}</span>
        <select bind:value={selectedGroup} disabled={groupsLoading} class="w-full rounded-lg border border-border bg-background px-3 py-2 text-sm outline-none focus:border-primary focus:ring-2 focus:ring-ring/30 disabled:opacity-60">
          <option value="">{m.insights_all_groups()}</option>
          {#each groups as group}
            <option value={group.id}>{group.name}</option>
          {/each}
        </select>
      </label>
    </div>

    {#if metric === "latency"}
      <p class="flex items-center gap-2 text-xs text-muted-foreground">
        <Server class="h-3.5 w-3.5" />
        {m.insights_latency_scope({ type: typeLabel(selectedType) })}
      </p>
    {/if}
  </section>

  {#if loading}
    <div class="overflow-hidden rounded-xl border border-border bg-card" role="status">
      <span class="sr-only">{m.insights_loading()}</span>
      {#each Array(6) as _, index}
        <div class="flex gap-4 border-b border-border px-4 py-4 last:border-0">
          <Skeleton class="h-4 w-40" />
          <Skeleton class="h-4 w-20" />
          <Skeleton class="h-4 w-16" />
          <Skeleton class="h-4 w-24" />
        </div>
      {/each}
    </div>
  {:else if loadError}
    <EmptyState
      icon={BarChart3}
      title={m.insights_load_failed()}
      description={loadError}
      action={undefined}
    />
  {:else if sortedRows.length === 0}
    <EmptyState
      icon={BarChart3}
      title={m.insights_empty_title()}
      description={m.insights_empty_description()}
    />
  {:else}
    <div class="overflow-hidden rounded-xl border border-border bg-card">
      <div class="flex items-center justify-between gap-3 border-b border-border px-4 py-3">
        <div>
          <h2 class="font-medium">{m.insights_table_title()}</h2>
          <p class="mt-0.5 text-xs text-muted-foreground">{m.insights_table_description()}</p>
        </div>
        <span class="shrink-0 text-xs text-muted-foreground">{sortedRows.length} {m.insights_monitors()}</span>
      </div>
      <div class="overflow-x-auto">
        <table class="w-full min-w-[900px] text-left text-sm">
          <thead class="border-b border-border bg-muted/20 text-xs text-muted-foreground">
            <tr>
              <th class="px-4 py-3 font-medium">
                <button type="button" onclick={() => sortBy("monitor_name")} class="inline-flex items-center gap-1.5 hover:text-foreground">
                  {m.insights_monitor()}
                  <SortIndicator active={sortKey === "monitor_name"} direction={sortDirection} />
                </button>
              </th>
              <th class="px-4 py-3 font-medium">{m.insights_type_group()}</th>
              <th class="px-4 py-3 font-medium">
                <button type="button" onclick={() => sortBy("availability_percent")} class="inline-flex items-center gap-1.5 hover:text-foreground">
                  {m.insights_availability()}
                  <SortIndicator active={sortKey === "availability_percent"} direction={sortDirection} />
                </button>
              </th>
              <th class="px-4 py-3 font-medium">
                <button type="button" onclick={() => sortBy("outage_count")} class="inline-flex items-center gap-1.5 hover:text-foreground">
                  {m.insights_outages()}
                  <SortIndicator active={sortKey === "outage_count"} direction={sortDirection} />
                </button>
              </th>
              <th class="px-4 py-3 font-medium">
                <button type="button" onclick={() => sortBy("downtime_seconds")} class="inline-flex items-center gap-1.5 hover:text-foreground">
                  {m.insights_downtime()}
                  <SortIndicator active={sortKey === "downtime_seconds"} direction={sortDirection} />
                </button>
              </th>
              <th class="px-4 py-3 font-medium">
                <button type="button" onclick={() => sortBy("latency_avg_ms")} class="inline-flex items-center gap-1.5 hover:text-foreground">
                  {m.insights_latency_avg()}
                  <SortIndicator active={sortKey === "latency_avg_ms"} direction={sortDirection} />
                </button>
              </th>
              <th class="px-4 py-3 font-medium">
                <button type="button" onclick={() => sortBy("flap_count")} class="inline-flex items-center gap-1.5 hover:text-foreground">
                  {m.insights_flapping()}
                  <SortIndicator active={sortKey === "flap_count"} direction={sortDirection} />
                </button>
              </th>
              <th class="px-4 py-3 font-medium">
                <button type="button" onclick={() => sortBy("coverage_percent")} class="inline-flex items-center gap-1.5 hover:text-foreground">
                  {m.insights_coverage()}
                  <SortIndicator active={sortKey === "coverage_percent"} direction={sortDirection} />
                </button>
              </th>
              <th class="px-4 py-3 font-medium">{m.insights_status()}</th>
            </tr>
          </thead>
          <tbody>
            {#each sortedRows as row (row.monitor_id)}
              <tr class="border-b border-border/70 last:border-0 hover:bg-muted/20">
                <td class="px-4 py-3">
                  <a href={`/monitors/${row.monitor_id}`} class="inline-flex max-w-[14rem] items-center gap-2 font-medium hover:text-primary">
                    <span class="truncate">{row.monitor_name}</span>
                  </a>
                </td>
                <td class="px-4 py-3 text-xs text-muted-foreground">
                  <div>{typeLabel(row.monitor_type)}</div>
                  <div class="mt-0.5 truncate text-faint">{row.group_id ? (groupNames.get(row.group_id) ?? m.insights_group_fallback({ id: row.group_id })) : m.insights_ungrouped()}</div>
                </td>
                <td class="px-4 py-3 font-medium {row.availability_percent !== null && row.availability_percent < 99 ? 'text-danger' : 'text-foreground'}">{formatPercent(row.availability_percent)}</td>
                <td class="px-4 py-3 tnum">{row.outage_count}</td>
                <td class="px-4 py-3 tnum">{formatDuration(row.downtime_seconds)}</td>
                <td class="px-4 py-3 tnum">{row.latency_avg_ms === null ? "—" : `${row.latency_avg_ms.toFixed(1)} ms`}</td>
                <td class="px-4 py-3 tnum">{row.flap_count}</td>
                <td class="px-4 py-3 tnum">{row.coverage_percent.toFixed(1)}%</td>
                <td class="px-4 py-3">
                  <span class="inline-flex items-center rounded-full border px-2 py-1 text-xs font-medium {row.qualification === 'qualified' ? 'border-success/25 bg-success/10 text-success' : 'border-warning/25 bg-warning/10 text-warning'}">
                    {row.qualification === "qualified" ? m.insights_qualified() : m.insights_insufficient_data()}
                  </span>
                </td>
              </tr>
            {/each}
          </tbody>
        </table>
      </div>
    </div>
  {/if}
</div>
