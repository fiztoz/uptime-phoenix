<script lang="ts">
	import { page } from '$app/stores';
	import { goto } from '$app/navigation';
	import { realtime } from '$lib/stores/ws.svelte.js';
	import { heartbeatsApi, type Heartbeat } from '$lib/api/heartbeats.js';
	import type { MonitorWithGroup } from '$lib/api/monitors';
	import {
		monitorGroupsApi,
		indexGroupChildren,
		resolveGroupStatuses,
		monitorToRollupStatus,
		buildGroupOptions,
		type MonitorGroupView,
	} from '$lib/api/monitorGroups';
	import { tagsApi, type Tag } from '$lib/api/tags';
	import {
		EMPTY_CRITERIA,
		collectTypes,
		criteriaFromParams,
		criteriaToSearchString,
		filterMonitors,
		groupPath,
		hasActiveFilters,
		monitorTags,
		summarizeGroup,
		type FilterCriteria,
		type MonitorStatus,
		type NormalizedTag,
	} from '$lib/monitor-filter';
	import MonitorCard from '$lib/components/MonitorCard.svelte';
	import GroupCard from '$lib/components/GroupCard.svelte';
	import DashboardFilters from '$lib/components/DashboardFilters.svelte';
	import StatusPill from '$lib/components/StatusPill.svelte';
	import EmptyState from '$lib/components/EmptyState.svelte';
	import Skeleton from '$lib/components/Skeleton.svelte';
	import {
		Activity,
		AlertTriangle,
		CheckCircle,
		Server,
		Gauge,
		ArrowRight,
		ChevronRight,
		SearchX,
	} from '@lucide/svelte';
	import * as m from '$lib/paraglide/messages.js';

	// --- Monitor groups (folders) -----------------------------------------
	let groups = $state<MonitorGroupView[]>([]);
	let groupsLoading = $state(true);
	let groupsError = $state<string | null>(null);

	// Tag catalog (Settings-created tags). The filter must list every tag that
	// exists, not only tags already assigned to a monitor — otherwise a newly
	// created tag looks "missing" until something is tagged with it.
	let catalogTags = $state<Tag[]>([]);

	async function loadGroups() {
		groupsLoading = true;
		groupsError = null;
		try {
			groups = await monitorGroupsApi.list();
		} catch (error: unknown) {
			groups = [];
			groupsError = error && typeof error === 'object' && 'message' in error
				? String((error as { message: string }).message)
				: m.error_generic();
		} finally {
			groupsLoading = false;
		}
	}

	async function loadTags() {
		try {
			catalogTags = await tagsApi.list();
		} catch {
			// Keep whatever we had; the filter still works with an empty catalog
			// (emptyMessage) rather than failing the whole dashboard.
			catalogTags = [];
		}
	}

	$effect(() => {
		loadGroups();
		loadTags();
	});

	const pageLoading = $derived(!realtime.hasMonitorSnapshot || groupsLoading);
	const pageError = $derived(
		groupsError ?? (!realtime.hasMonitorSnapshot ? realtime.lastError : null),
	);

	function retryPageLoad() {
		loadGroups();
		loadTags();
		if (!realtime.hasMonitorSnapshot) realtime.connect();
	}

	// --- Filters ----------------------------------------------------------
	// The URL query string is the source of truth for filters, so a filtered
	// view survives a refresh and can be shared. We mirror it into local state
	// so typing re-renders instantly rather than waiting on the navigation, and
	// push each change back into the URL with replaceState (no history spam).
	let criteria = $state<FilterCriteria>(criteriaFromParams($page.url.searchParams));

	/**
	 * The query string we last pushed ourselves. Plain `let`, NOT $state — the
	 * sync effect below must not take a reactive dependency on it.
	 *
	 * goto() is async, so between a keystroke and the URL actually changing, our
	 * local `criteria` is ahead of `$page.url`. Without this marker the effect
	 * would read that gap as an external navigation and clobber the keystroke.
	 * Comparing against what we pushed lets us ignore our own echo and react
	 * only to genuinely external URL changes (back/forward, a pasted link).
	 */
	let selfPushed = $page.url.search;

	$effect(() => {
		const search = $page.url.search;
		if (search === selfPushed) return; // our own replaceState echoing back
		selfPushed = search;
		criteria = criteriaFromParams(new URLSearchParams(search));
	});

	function applyCriteria(next: FilterCriteria) {
		criteria = next; // filter instantly; don't wait on the navigation
		const url = $page.url;
		const search = criteriaToSearchString(url.searchParams, next);
		selfPushed = search;
		goto(`${url.pathname}${search}`, {
			replaceState: true,
			keepFocus: true,
			noScroll: true,
		});
	}

	/** Drill into a group card: filter to that group rather than route away. */
	function drillIntoGroup(groupId: number) {
		applyCriteria({ ...EMPTY_CRITERIA, group: groupId });
	}

	// --- Monitors ---------------------------------------------------------
	// The backend embeds `tags` on every monitor payload (REST MonitorView.Tags +
	// the WS wire). Filtering still uses those assignments; the dropdown options
	// come from the tag catalog so created-but-unassigned tags are selectable.
	let allMonitors = $derived(realtime.monitors as MonitorWithGroup[]);

	let filteredMonitors = $derived.by(() => {
		void realtime.heartbeatSeq; // re-filter on every heartbeat (status may have flipped)
		return filterMonitors(allMonitors, criteria, groups);
	});

	let filterActive = $derived(hasActiveFilters(criteria));
	let tagOptions = $derived.by((): NormalizedTag[] => {
		return [...catalogTags]
			.map((t) => ({ id: t.id, name: t.name, color: t.color ?? '', value: '' }))
			.sort((a, b) => a.name.localeCompare(b.name));
	});
	let typeOptions = $derived(collectTypes(allMonitors));

	/** Per-status counts across ALL monitors (not filtered) for the multi-select dots. */
	let statusCounts = $derived.by(() => {
		const counts: Record<MonitorStatus, number> = {
			up: 0, down: 0, pending: 0, maintenance: 0, paused: 0,
		};
		for (const m of allMonitors) {
			if (m.status in counts) counts[m.status]++;
		}
		return counts;
	});

	/** Per-tag counts across ALL monitors for the multi-select badges. */
	let tagCounts = $derived.by(() => {
		const counts: Record<string, number> = {};
		for (const m of allMonitors) {
			for (const t of monitorTags(m)) {
				counts[t.name] = (counts[t.name] ?? 0) + 1;
			}
		}
		return counts;
	});
	let groupOptions = $derived(buildGroupOptions(groups));
	let activeGroupPath = $derived(
		typeof criteria.group === 'number' ? groupPath(groups, criteria.group) : []
	);

	/**
	 * Live-recomputed group statuses, ported from internal/core/domain/monitor_group.go
	 * (see $lib/api/monitorGroups.ts). The server's initial `status` is only
	 * trustworthy for first paint — recompute on every heartbeat instead.
	 */
	let groupStatusMap = $derived.by(() => {
		void realtime.heartbeatSeq;
		const hbMap = realtime.heartbeats;
		const withStatus = allMonitors.map((mon) => ({
			id: mon.id,
			group_id: mon.group_id ?? null,
			status: monitorToRollupStatus(mon.status, hbMap.get(mon.id)?.status),
		}));
		return resolveGroupStatuses(groups, withStatus);
	});

	// Top-level groups + the monitors that sit in no group at all. Only used for
	// the DEFAULT (unfiltered) browse view — once a filter is on we show a flat
	// grid of matching monitors instead.
	let groupIndex = $derived(indexGroupChildren(groups, allMonitors));
	let topGroups = $derived(groupIndex.subgroupsByParent.get(null) ?? []);
	let ungroupedMonitors = $derived(groupIndex.monitorsByGroup.get(null) ?? []);

	// --- Stats ------------------------------------------------------------
	// The tiles, the health banner and "Needs attention" all describe the
	// FILTERED set: while a filter is on, the header is the summary OF THAT
	// VIEW (e.g. drilling into a group shows that group's health). With no
	// filters active the filtered set IS every monitor, so this is a no-op.
	let scopedMonitors = $derived(filteredMonitors);

	let totalMonitors = $derived(scopedMonitors.length);
	let upCount = $derived(scopedMonitors.filter((mon) => mon.status === 'up').length);
	let downCount = $derived(scopedMonitors.filter((mon) => mon.status === 'down').length);
	let attention = $derived(
		scopedMonitors.filter((mon) => mon.status === 'down' || mon.status === 'pending')
	);

	// Average response time across the scoped monitors that currently report a ping.
	let avgPing = $derived.by(() => {
		void realtime.heartbeatSeq;
		const hbMap = realtime.heartbeats;
		const pings: number[] = [];
		for (const mon of scopedMonitors) {
			const hb = hbMap.get(mon.id);
			if (hb && hb.ping > 0) pings.push(hb.ping);
		}
		if (pings.length === 0) return null;
		return Math.round(pings.reduce((a, b) => a + b, 0) / pings.length);
	});

	let uptimePct = $derived(totalMonitors === 0 ? null : (upCount / totalMonitors) * 100);

	// --- Heartbeat history (sparklines) -----------------------------------
	let heartbeatHistory = $state<Map<number, Heartbeat[]>>(new Map());

	$effect(() => {
		const monitors = realtime.monitors;
		if (monitors.length === 0) return;
		for (const mon of monitors) {
			if (!heartbeatHistory.has(mon.id)) {
				heartbeatsApi
					.listOptions(mon.id, { hours: 24, limit: 120, order: 'asc' })
					.then((hbs) => {
						const liveRows = heartbeatHistory.get(mon.id) ?? [];
						const merged = [...hbs, ...liveRows]
							.sort((a, b) => new Date(a.time).getTime() - new Date(b.time).getTime())
							.slice(-120);
						heartbeatHistory.set(mon.id, merged);
						heartbeatHistory = new Map(heartbeatHistory);
					})
					.catch(() => {});
			}
		}
	});

	// Append live WS heartbeats into sparkline history.
	$effect(() => {
		void realtime.heartbeatSeq;
		const hbMap = realtime.heartbeats;
		let changed = false;
		for (const [monitorId, live] of hbMap) {
			const rows = heartbeatHistory.get(monitorId) ?? [];
			const last = rows[rows.length - 1];
			if (last?.time === live.time) continue;
			const entry: Heartbeat = {
				id: -Date.now(),
				monitor_id: monitorId,
				status: live.status,
				ping: live.ping,
				message: live.msg ?? '',
				time: live.time,
				important: false,
			};
			heartbeatHistory.set(monitorId, [...rows, entry].slice(-120));
			changed = true;
		}
		if (changed) heartbeatHistory = new Map(heartbeatHistory);
	});

	const tiles = $derived([
		{
			label: m.dashboard_total_monitors(),
			value: String(totalMonitors),
			icon: Server,
			tone: 'default' as const,
			hint: filterActive ? m.dashboard_hint_in_view() : m.dashboard_hint_all_checks(),
		},
		{
			label: m.dashboard_up(),
			value: String(upCount),
			icon: CheckCircle,
			tone: 'success' as const,
			hint: totalMonitors > 0 ? m.dashboard_hint_healthy_pct({ pct: Math.round((upCount / totalMonitors) * 100) }) : '—',
		},
		{
			label: m.dashboard_down(),
			value: String(downCount),
			icon: AlertTriangle,
			tone: downCount > 0 ? ('danger' as const) : ('default' as const),
			hint: downCount > 0 ? m.dashboard_hint_needs_attention() : m.dashboard_hint_no_incidents(),
		},
		{
			label: m.dashboard_avg_response(),
			value: avgPing === null ? '—' : `${avgPing} ms`,
			icon: Gauge,
			tone: 'primary' as const,
			hint: uptimePct === null ? '—' : m.dashboard_hint_uptime_pct({ pct: uptimePct.toFixed(2) }),
		},
	]);

	const toneRing: Record<string, string> = {
		default: 'text-muted-foreground',
		success: 'text-success',
		danger: 'text-danger',
		primary: 'text-primary',
	};
	const toneValue: Record<string, string> = {
		default: 'text-foreground',
		success: 'text-success',
		danger: 'text-danger',
		primary: 'text-foreground',
	};
</script>

<svelte:head>
	<title>{m.app_name()} · {m.dashboard_title()}</title>
</svelte:head>

{#snippet retryPageAction()}
	<button type="button" onclick={retryPageLoad} class="inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">{m.monitor_group_form_retry()}</button>
{/snippet}

{#if pageLoading}
	<div class="space-y-6" role="status">
		<span class="sr-only">{m.loading()}</span>
		<div class="grid grid-cols-2 gap-4 lg:grid-cols-4">
			{#each Array(4) as _}
				<div class="rounded-xl border border-border bg-card p-5"><Skeleton class="h-3 w-24" /><Skeleton class="mt-4 h-8 w-16" /><Skeleton class="mt-2 h-3 w-28" /></div>
			{/each}
		</div>
		<div class="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
			{#each Array(3) as _}<div class="rounded-xl border border-border bg-card p-5"><Skeleton class="h-5 w-40" /><Skeleton class="mt-4 h-3 w-full" /><Skeleton class="mt-3 h-10 w-full" /></div>{/each}
		</div>
	</div>
{:else if pageError}
	<EmptyState icon={AlertTriangle} title={m.error_generic()} description={pageError} action={retryPageAction} />
{:else}
<div class="space-y-8">
	<!-- Health summary banner -->
	{#if totalMonitors > 0}
		<div
			class="flex items-center gap-3 rounded-xl border p-4
			{downCount > 0
				? 'border-danger/20 bg-danger/[0.06]'
				: 'border-success/20 bg-success/[0.06]'}"
		>
			{#if downCount > 0}
				<span class="grid h-9 w-9 place-items-center rounded-lg bg-danger/15 text-danger">
					<AlertTriangle class="h-5 w-5" />
				</span>
				<div class="min-w-0">
					<p class="font-medium">
						{downCount}
						{downCount === 1 ? m.dashboard_banner_monitor_needs_attention() : m.dashboard_banner_monitors_need_attention()}
					</p>
					<p class="text-sm text-muted-foreground">{m.dashboard_banner_investigate()}</p>
				</div>
			{:else}
				<span class="grid h-9 w-9 place-items-center rounded-lg bg-success/15 text-success">
					<CheckCircle class="h-5 w-5" />
				</span>
				<div class="min-w-0">
					<p class="font-medium">{m.layout_all_systems_operational()}</p>
					<p class="text-sm text-muted-foreground">
						{filterActive
							? m.dashboard_banner_healthy_view()
							: m.dashboard_banner_healthy_all()}
					</p>
				</div>
			{/if}
		</div>
	{/if}

	<!-- Stat tiles -->
	<div class="grid grid-cols-2 gap-4 lg:grid-cols-4">
		{#each tiles as t (t.label)}
			<div
				class="group relative overflow-hidden rounded-xl border border-border bg-card p-5 transition-colors hover:border-border/80"
			>
				<div class="flex items-center justify-between">
					<span class="eyebrow">{t.label}</span>
					<t.icon class="h-4 w-4 {toneRing[t.tone]}" />
				</div>
				<p class="mt-3 text-3xl font-semibold tracking-tight tnum {toneValue[t.tone]}">{t.value}</p>
				<p class="mt-1 text-xs text-muted-foreground">{t.hint}</p>
				{#if t.tone === 'primary'}
					<div
						class="pointer-events-none absolute -right-6 -top-6 h-20 w-20 rounded-full bg-primary/10 blur-2xl"
					></div>
				{/if}
			</div>
		{/each}
	</div>

	<!-- Needs attention -->
	{#if attention.length > 0}
		<section class="space-y-3">
			<h2 class="text-sm font-semibold text-muted-foreground">{m.dashboard_needs_attention_heading()}</h2>
			<div class="overflow-hidden rounded-xl border border-border bg-card">
				{#each attention as mon (mon.id)}
					<a
						href="/monitors/{mon.id}"
						class="flex items-center gap-3 border-b border-border px-4 py-3 transition-colors last:border-0 hover:bg-accent/50"
					>
						<Activity class="h-4 w-4 shrink-0 text-muted-foreground" />
						<span class="min-w-0 flex-1 truncate font-medium">{mon.name}</span>
						<span class="hidden truncate font-mono text-xs text-muted-foreground sm:block"
							>{mon.target}</span
						>
						<StatusPill status={mon.status} />
					</a>
				{/each}
			</div>
		</section>
	{/if}

	<!-- Monitors -->
	<section class="space-y-4">
		<div class="flex items-center justify-between">
			<h2 class="text-lg font-semibold tracking-tight">{m.nav_monitors()}</h2>
			<a
				href="/monitors"
				class="inline-flex items-center gap-1 text-sm text-muted-foreground transition-colors hover:text-foreground"
			>
				{m.dashboard_view_all()} <ArrowRight class="h-3.5 w-3.5" />
			</a>
		</div>

		<DashboardFilters
			{criteria}
			{groupOptions}
			{tagOptions}
			{typeOptions}
			{statusCounts}
			{tagCounts}
			shown={filteredMonitors.length}
			total={allMonitors.length}
			onchange={applyCriteria}
		/>

		<!-- Drill-in breadcrumb: the way back out of a group. -->
		{#if activeGroupPath.length > 0}
			<nav class="flex flex-wrap items-center gap-1 text-sm" aria-label={m.dashboard_breadcrumb_aria()}>
				<button
					type="button"
					onclick={() => applyCriteria(EMPTY_CRITERIA)}
					class="text-muted-foreground transition-colors hover:text-foreground"
				>
					{m.dashboard_breadcrumb_all_monitors()}
				</button>
				{#each activeGroupPath as g, i (g.id)}
					<ChevronRight class="h-3.5 w-3.5 shrink-0 text-faint" />
					{#if i === activeGroupPath.length - 1}
						<span class="font-medium">{g.name}</span>
					{:else}
						<button
							type="button"
							onclick={() => drillIntoGroup(g.id)}
							class="text-muted-foreground transition-colors hover:text-foreground"
						>
							{g.name}
						</button>
					{/if}
				{/each}
			</nav>
		{/if}

		{#if allMonitors.length === 0}
			<div class="rounded-xl border border-dashed border-border p-12 text-center">
				<div
					class="mx-auto mb-3 grid h-11 w-11 place-items-center rounded-xl bg-muted/50 text-muted-foreground"
				>
					<Server class="h-5 w-5" />
				</div>
				<p class="text-sm text-muted-foreground">{m.monitors_empty()}</p>
			</div>
		{:else if filterActive}
			<!-- Filtered: the user is searching, not browsing — flat grid of matching
			     monitors only, no group cards. -->
			{#if filteredMonitors.length === 0}
				<div class="rounded-xl border border-dashed border-border p-12 text-center">
					<div
						class="mx-auto mb-3 grid h-11 w-11 place-items-center rounded-xl bg-muted/50 text-muted-foreground"
					>
						<SearchX class="h-5 w-5" />
					</div>
					<p class="text-sm text-muted-foreground">{m.dashboard_no_matches()}</p>
					<button
						type="button"
						onclick={() => applyCriteria(EMPTY_CRITERIA)}
						class="mt-3 inline-flex items-center gap-1 rounded-lg border border-border px-3 py-1.5 text-xs text-muted-foreground transition-colors hover:bg-accent/50 hover:text-foreground"
					>
						{m.dashboard_filters_clear()}
					</button>
				</div>
			{:else}
				<div class="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
					{#each filteredMonitors as mon (mon.id)}
						<MonitorCard
							monitor={mon}
							heartbeat={realtime.heartbeats.get(mon.id)}
							heartbeatHistory={heartbeatHistory.get(mon.id) ?? []}
						/>
					{/each}
				</div>
			{/if}
		{:else}
			<!-- Default browse view: one flat grid of top-level GROUP cards followed
			     by the ungrouped monitors. No tree, no nesting, no chevrons. -->
			<div class="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
				{#each topGroups as g (g.id)}
					<GroupCard
						group={g}
						summary={summarizeGroup(groups, allMonitors, g.id)}
						status={groupStatusMap.get(g.id) ?? null}
						ondrill={() => drillIntoGroup(g.id)}
					/>
				{/each}
				{#each ungroupedMonitors as mon (mon.id)}
					<MonitorCard
						monitor={mon}
						heartbeat={realtime.heartbeats.get(mon.id)}
						heartbeatHistory={heartbeatHistory.get(mon.id) ?? []}
					/>
				{/each}
			</div>
		{/if}
	</section>
</div>
{/if}
