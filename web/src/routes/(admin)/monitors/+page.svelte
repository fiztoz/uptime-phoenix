<script lang="ts">
	import { realtime } from '$lib/stores/ws.svelte.js';
	import StatusPill from '$lib/components/StatusPill.svelte';
	import MonitorForm from '$lib/components/MonitorForm.svelte';
	import MonitorGroupForm from '$lib/components/MonitorGroupForm.svelte';
	import EmptyState from '$lib/components/EmptyState.svelte';
	import Skeleton from '$lib/components/Skeleton.svelte';
	import { monitorsApi, type MonitorWithGroup } from '$lib/api/monitors';
	import {
		monitorGroupsApi,
		indexGroupChildren,
		resolveGroupStatuses,
		monitorToRollupStatus,
		rollupStatusToPillStatus,
		sortMonitors,
		type MonitorGroupView,
		type RollupStatus,
	} from '$lib/api/monitorGroups';
	import { tagsApi, type Tag } from '$lib/api/tags';
	import {
		STATUS_FILTERS,
		monitorTags,
		type MonitorStatus,
	} from '$lib/monitor-filter';
	import {
		Plus,
		FolderPlus,
		Search,
		Edit2,
		Trash2,
		Activity,
		Folder,
		FolderOpen,
		ChevronRight,
		ChevronDown,
		ChevronsDownUp,
		ChevronsUpDown,
		AlertTriangle,
	} from '@lucide/svelte';
	import { toast } from 'svelte-sonner';
	import { confirmAction } from '$lib/stores/confirm.svelte';
	import MultiSelect from '$lib/components/MultiSelect.svelte';
	import { monitorTypes as MONITOR_TYPES } from '$lib/monitor-types';
	import * as m from '$lib/paraglide/messages.js';

	let showForm = $state(false);
	let editingMonitor = $state<any>(null);
	let showGroupForm = $state(false);
	let editingGroup = $state<MonitorGroupView | null>(null);
	let searchQuery = $state('');
	/** Multi-select type filter (OR). */
	let filterTypes = $state<string[]>([]);
	/** Multi-select status filter (OR) — same model as the dashboard. */
	let filterStatuses = $state<MonitorStatus[]>([]);
	/** Multi-select tag filter by name (OR) — same model as the dashboard. */
	let filterTags = $state<string[]>([]);
	let allTags = $state<Tag[]>([]);
	let groups = $state<MonitorGroupView[]>([]);
	let groupsLoading = $state(true);
	let groupsError = $state<string | null>(null);
	/** Session-local overrides of a group's collapsed state, keyed by group id. */
	let collapseOverrides = $state<Map<number, boolean>>(new Map());

	async function loadTags() {
		try {
			allTags = await tagsApi.list();
		} catch {
			allTags = [];
		}
	}

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

	$effect(() => {
		loadTags();
		loadGroups();
	});

	// Progressive loading: the page only waits for the monitor snapshot —
	// rows, counters and filters all derive from it. Groups fill in on their
	// own: a skeleton strip holds their place in the tree while they load, and
	// a failed groups fetch degrades to an inline retry instead of blanking
	// the whole page.
	const pageLoading = $derived(!realtime.hasMonitorSnapshot);
	const pageError = $derived(
		!realtime.hasMonitorSnapshot ? realtime.lastError : null,
	);

	function retryPageLoad() {
		loadTags();
		loadGroups();
		if (!realtime.hasMonitorSnapshot) realtime.connect();
	}

	let filteredMonitors = $derived.by(() => {
		void realtime.heartbeatSeq;
		let list = realtime.monitors as MonitorWithGroup[];
		if (searchQuery) {
			const q = searchQuery.toLowerCase();
			list = list.filter(
				(m) =>
					m.name.toLowerCase().includes(q) ||
					m.type.toLowerCase().includes(q) ||
					(m.target && m.target.toLowerCase().includes(q))
			);
		}
		if (filterTypes.length > 0) {
			list = list.filter((m) => filterTypes.includes(m.type));
		}
		if (filterTags.length > 0) {
			list = list.filter((m) =>
				monitorTags(m).some((t) => filterTags.includes(t.name)),
			);
		}
		if (filterStatuses.length > 0) {
			list = list.filter((m) => filterStatuses.includes(m.status as MonitorStatus));
		}
		return sortMonitors(list);
	});

	let filterActive = $derived(
		Boolean(
			searchQuery ||
				filterTypes.length > 0 ||
				filterStatuses.length > 0 ||
				filterTags.length > 0,
		),
	);
	/** null = no filter active (show everything); otherwise the set of monitor ids that matched. */
	let matchedMonitorIds = $derived(filterActive ? new Set(filteredMonitors.map((m) => m.id)) : null);

	// Counts across ALL monitors (not the filtered set) so MultiSelect badges
	// stay stable while the user narrows the list.
	let statusCounts = $derived.by(() => {
		const counts: Record<MonitorStatus, number> = {
			up: 0, down: 0, pending: 0, maintenance: 0, paused: 0,
		};
		for (const mo of realtime.monitors) {
			if (mo.status in counts) counts[mo.status as MonitorStatus]++;
		}
		return counts;
	});

	let typeCounts = $derived.by(() => {
		const counts: Record<string, number> = {};
		for (const mo of realtime.monitors) {
			if (mo.type) counts[mo.type] = (counts[mo.type] ?? 0) + 1;
		}
		return counts;
	});

	let tagCounts = $derived.by(() => {
		const counts: Record<string, number> = {};
		for (const mo of realtime.monitors) {
			for (const t of monitorTags(mo)) {
				counts[t.name] = (counts[t.name] ?? 0) + 1;
			}
		}
		return counts;
	});

	const STATUS_LABELS = $derived<Record<MonitorStatus, string>>({
		up: m.dashboard_up(),
		down: m.status_down(),
		pending: m.status_pending(),
		maintenance: m.status_maintenance(),
		paused: m.status_paused(),
	});

	const STATUS_DOT_CLASSES: Record<MonitorStatus, string> = {
		up: 'dot-up',
		down: 'dot-down',
		pending: 'dot-warn',
		maintenance: 'dot-info',
		paused: 'dot-muted',
	};

	const typeItems = $derived(
		MONITOR_TYPES.map((t) => ({
			value: t,
			label: t.toUpperCase(),
			count: typeCounts[t] ?? 0,
		})),
	);

	const statusItems = $derived(
		STATUS_FILTERS.map((s) => ({
			value: s,
			label: STATUS_LABELS[s],
			count: statusCounts[s] ?? 0,
			dotClass: STATUS_DOT_CLASSES[s],
		})),
	);

	const tagItems = $derived(
		[...allTags]
			.sort((a, b) => a.name.localeCompare(b.name))
			.map((t) => ({
				value: t.name,
				label: t.name,
				count: tagCounts[t.name] ?? 0,
				dotColor: t.color || undefined,
			})),
	);

	function toggleType(value: string) {
		filterTypes = filterTypes.includes(value)
			? filterTypes.filter((t) => t !== value)
			: [...filterTypes, value];
	}

	function toggleStatus(value: string) {
		const status = value as MonitorStatus;
		filterStatuses = filterStatuses.includes(status)
			? filterStatuses.filter((s) => s !== status)
			: [...filterStatuses, status];
	}

	function toggleTag(value: string) {
		filterTags = filterTags.includes(value)
			? filterTags.filter((t) => t !== value)
			: [...filterTags, value];
	}

	/**
	 * Live-recomputed group statuses. The server's initial `status` on each
	 * MonitorGroupView is only trustworthy for first paint — after that we
	 * roll up from the same WS heartbeat stream the monitor rows use, so a
	 * group's badge updates the instant a child's heartbeat lands.
	 */
	let groupStatusMap = $derived.by(() => {
		void realtime.heartbeatSeq;
		const hbMap = realtime.heartbeats;
		const all = realtime.monitors as MonitorWithGroup[];
		const withStatus = all.map((m) => ({
			id: m.id,
			group_id: m.group_id ?? null,
			status: monitorToRollupStatus(m.status, hbMap.get(m.id)?.status),
		}));
		return resolveGroupStatuses(groups, withStatus);
	});

	function isGroupCollapsed(g: MonitorGroupView): boolean {
		return collapseOverrides.get(g.id) ?? g.collapsed;
	}

	function persistGroupCollapsed(id: number, collapsed: boolean) {
		// Persist the UI default; best-effort, no need to block the toggle on it.
		monitorGroupsApi.update(id, { collapsed }).catch(() => {});
	}

	function toggleGroupCollapse(g: MonitorGroupView) {
		const next = !isGroupCollapsed(g);
		collapseOverrides.set(g.id, next);
		collapseOverrides = new Map(collapseOverrides);
		persistGroupCollapsed(g.id, next);
	}

	function setAllGroupsCollapsed(collapsed: boolean) {
		const next = new Map(collapseOverrides);
		for (const g of groups) {
			if ((next.get(g.id) ?? g.collapsed) === collapsed) continue;
			next.set(g.id, collapsed);
			persistGroupCollapsed(g.id, collapsed);
		}
		collapseOverrides = next;
	}

	interface GroupRow {
		kind: 'group';
		key: string;
		group: MonitorGroupView;
		status: RollupStatus | null;
		depth: number;
		childCount: number;
		collapsed: boolean;
	}
	interface MonitorRow {
		kind: 'monitor';
		key: string;
		monitor: MonitorWithGroup;
		ping?: number;
		depth: number;
	}
	type DisplayRow = GroupRow | MonitorRow;

	/**
	 * Recursive tree of arbitrary depth (groups containing groups containing
	 * monitors), flattened into a row list so both the desktop table and the
	 * mobile card list can render it as a simple `{#each}`. Collapsed groups
	 * omit their descendants. When a filter is active a group still renders if
	 * ANY descendant monitor matches, even if the group itself doesn't.
	 * Monitors with no group render at the top level.
	 */
	let displayRows = $derived.by(() => {
		void realtime.heartbeatSeq;
		const hbMap = realtime.heartbeats;
		const allMonitors = realtime.monitors as MonitorWithGroup[];
		const matchIds = matchedMonitorIds;
		const statusMap = groupStatusMap;
		const { subgroupsByParent, monitorsByGroup } = indexGroupChildren(groups, allMonitors);

		function groupHasMatch(id: number): boolean {
			if (!matchIds) return true;
			for (const m of monitorsByGroup.get(id) ?? []) {
				if (matchIds.has(m.id)) return true;
			}
			for (const sub of subgroupsByParent.get(id) ?? []) {
				if (groupHasMatch(sub.id)) return true;
			}
			return false;
		}

		const rows: DisplayRow[] = [];

		function walkGroup(g: MonitorGroupView, depth: number) {
			if (!groupHasMatch(g.id)) return;
			const childMonitors = (monitorsByGroup.get(g.id) ?? []).filter((m) => !matchIds || matchIds.has(m.id));
			const childGroups = (subgroupsByParent.get(g.id) ?? []).filter((sub) => groupHasMatch(sub.id));
			const collapsed = isGroupCollapsed(g);

			rows.push({
				kind: 'group',
				key: `g${g.id}`,
				group: g,
				status: statusMap.get(g.id) ?? null,
				depth,
				childCount: childMonitors.length + childGroups.length,
				collapsed,
			});

			if (collapsed) return;
			for (const sub of childGroups) walkGroup(sub, depth + 1);
			for (const m of childMonitors) {
				rows.push({ kind: 'monitor', key: `m${m.id}`, monitor: m, ping: hbMap.get(m.id)?.ping, depth: depth + 1 });
			}
		}

		for (const g of subgroupsByParent.get(null) ?? []) walkGroup(g, 0);
		for (const m of monitorsByGroup.get(null) ?? []) {
			if (matchIds && !matchIds.has(m.id)) continue;
			rows.push({ kind: 'monitor', key: `m${m.id}`, monitor: m, ping: hbMap.get(m.id)?.ping, depth: 0 });
		}

		return rows;
	});

	const hasVisibleGroups = $derived(displayRows.some((row) => row.kind === 'group'));
	const allGroupsCollapsed = $derived(
		groups.length > 0 && groups.every((g) => isGroupCollapsed(g)),
	);
	const allGroupsExpanded = $derived(
		groups.length > 0 && groups.every((g) => !isGroupCollapsed(g)),
	);

	function openCreate() {
		editingMonitor = null;
		showForm = true;
	}

	function openEdit(m: any) {
		editingMonitor = m;
		showForm = true;
	}

	async function handleDelete(id: number, name: string) {
		const ok = await confirmAction({
			title: m.monitors_page_delete_title({ name }),
			message: m.monitors_page_delete_message(),
			confirmLabel: m.monitors_page_delete_confirm(),
			destructive: true
		});
		if (!ok) return;

		try {
			await monitorsApi.remove(id);
			toast.success(m.monitors_page_deleted_toast());
			// WS will push delete event
		} catch (e: any) {
			toast.error(e?.message || m.monitors_page_delete_failed());
		}
	}

	function handleSaved() {
		showForm = false;
		editingMonitor = null;
		// realtime will receive update/create via WS
	}

	function closeForm() {
		showForm = false;
		editingMonitor = null;
	}

	function openCreateGroup() {
		editingGroup = null;
		showGroupForm = true;
	}

	function openEditGroup(g: MonitorGroupView) {
		editingGroup = g;
		showGroupForm = true;
	}

	async function handleDeleteGroup(g: MonitorGroupView) {
		const dest = g.parent_id != null ? m.monitors_page_delete_group_dest_parent() : m.monitors_page_delete_group_dest_top();
		const ok = await confirmAction({
			title: m.monitors_page_delete_group_title({ name: g.name }),
			message: m.monitors_page_delete_group_message({ dest }),
			confirmLabel: m.monitors_page_delete_group_confirm(),
			destructive: true
		});
		if (!ok) return;

		try {
			await monitorGroupsApi.remove(g.id);
			toast.success(m.monitors_page_group_deleted_toast());
			await loadGroups();
			// The backend re-homes children by writing their group_id directly,
			// which may not emit a WS monitor.update per re-homed monitor. Refresh
			// from REST so the store reflects any group_id changes right away.
			try {
				const ms = await monitorsApi.list();
				for (const m of ms) realtime.patchMonitor(m);
			} catch {
				// best-effort sync; WS will eventually catch up regardless
			}
		} catch (e: any) {
			toast.error(e?.message || m.monitors_page_delete_failed());
		}
	}

	function handleGroupSaved() {
		showGroupForm = false;
		editingGroup = null;
		loadGroups();
	}

	function closeGroupForm() {
		showGroupForm = false;
		editingGroup = null;
	}

	let upCount = $derived(realtime.monitors.filter((mo) => mo.status === 'up').length);
</script>

<svelte:head>
	<title>{m.app_name()} · {m.monitors_title()}</title>
</svelte:head>

{#snippet retryPageAction()}
	<button type="button" onclick={retryPageLoad} class="inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">{m.monitor_group_form_retry()}</button>
{/snippet}

{#if pageLoading}
	<div class="space-y-5" role="status">
		<span class="sr-only">{m.loading()}</span>
		<div class="flex items-center justify-between gap-4"><div><Skeleton class="h-8 w-40" /><Skeleton class="mt-2 h-4 w-56" /></div><Skeleton class="h-9 w-36" /></div>
		<div class="rounded-xl border border-border bg-card p-4"><Skeleton class="h-9 w-full" />{#each Array(4) as _}<Skeleton class="mt-4 h-12 w-full" />{/each}</div>
	</div>
{:else if pageError}
	<EmptyState icon={AlertTriangle} title={m.error_generic()} description={pageError} action={retryPageAction} />
{:else}
<div class="space-y-6">
	<div class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
		<div>
			<h1 class="text-2xl font-semibold tracking-tight">{m.monitors_title()}</h1>
			<p class="mt-1 text-sm text-muted-foreground">
				{m.monitors_page_total({ count: realtime.monitors.length })}
				<span class="mx-1 text-faint">·</span>
				<span class="text-success">{m.monitors_page_operational_count({ count: upCount })}</span>
				{#if groups.length > 0}
					<span class="mx-1 text-faint">·</span>
					{groups.length} {groups.length === 1 ? m.monitors_page_group_singular() : m.monitors_page_group_plural()}
				{/if}
			</p>
		</div>
		<div class="flex items-center gap-2">
			<button
				onclick={openCreateGroup}
				class="inline-flex items-center gap-2 rounded-lg border border-border px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
			>
				<FolderPlus class="h-4 w-4" />
				{m.monitors_page_new_group()}
			</button>
			<button
				onclick={openCreate}
				class="inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground shadow-sm transition-colors hover:bg-primary/90"
			>
				<Plus class="h-4 w-4" />
				{m.monitors_page_add_monitor()}
			</button>
		</div>
	</div>

	<!-- Filters — MultiSelect for type / status / tags (same component as dashboard). -->
	<div class="flex flex-wrap items-center gap-2">
		<div class="relative min-w-56 flex-1">
			<Search
				class="pointer-events-none absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
			/>
			<input
				type="search"
				bind:value={searchQuery}
				placeholder={m.monitors_page_search_placeholder()}
				aria-label={m.monitors_page_search_placeholder()}
				class="h-9 w-full rounded-lg border border-border bg-surface pl-8 pr-2.5 text-sm text-foreground placeholder:text-faint transition-colors hover:border-border/80 focus:outline-none focus:ring-2 focus:ring-ring"
			/>
		</div>
		<MultiSelect
			options={typeItems}
			selected={filterTypes}
			onToggle={toggleType}
			onReset={() => (filterTypes = [])}
			placeholder={m.dashboard_filters_all_types()}
			ariaLabel={m.dashboard_filters_type_aria()}
			size="sm"
		/>
		<MultiSelect
			options={statusItems}
			selected={filterStatuses}
			onToggle={toggleStatus}
			onReset={() => (filterStatuses = [])}
			placeholder={m.dashboard_filters_all_statuses()}
			ariaLabel={m.dashboard_filters_statuses_aria()}
			size="sm"
		/>
		<MultiSelect
			options={tagItems}
			selected={filterTags}
			onToggle={toggleTag}
			onReset={() => (filterTags = [])}
			placeholder={m.dashboard_filters_all_tags()}
			emptyMessage={m.dashboard_filters_no_tags()}
			ariaLabel={m.dashboard_filters_tags_aria()}
			size="sm"
		/>
		{#if hasVisibleGroups}
			<div class="inline-flex h-8 shrink-0 items-stretch rounded-lg border border-border bg-surface">
				<button
					type="button"
					onclick={() => setAllGroupsCollapsed(true)}
					disabled={allGroupsCollapsed}
					title={m.monitors_page_collapse_all()}
					class="inline-flex items-center gap-1.5 rounded-l-lg px-2.5 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:bg-accent focus-visible:text-accent-foreground disabled:pointer-events-none disabled:opacity-40"
				>
					<ChevronsDownUp class="h-4 w-4" />
					<span class="sr-only sm:not-sr-only">{m.monitors_page_collapse_all()}</span>
				</button>
				<span class="my-1.5 w-px shrink-0 bg-border" aria-hidden="true"></span>
				<button
					type="button"
					onclick={() => setAllGroupsCollapsed(false)}
					disabled={allGroupsExpanded}
					title={m.monitors_page_expand_all()}
					class="inline-flex items-center gap-1.5 rounded-r-lg px-2.5 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:bg-accent focus-visible:text-accent-foreground disabled:pointer-events-none disabled:opacity-40"
				>
					<ChevronsUpDown class="h-4 w-4" />
					<span class="sr-only sm:not-sr-only">{m.monitors_page_expand_all()}</span>
				</button>
			</div>
		{/if}
	</div>

	{#if groupsLoading}
		<!-- Group rows arrive with the groups fetch; the monitor rows below are
		     already live from the snapshot, so only the tree header is skeletoned. -->
		<div class="space-y-4 rounded-xl border border-border bg-card p-4" role="status">
			<span class="sr-only">{m.loading()}</span>
			<Skeleton class="h-9 w-1/3" />
			<Skeleton class="h-12 w-full" />
			<Skeleton class="h-12 w-full" />
		</div>
	{:else if groupsError}
		<div class="flex flex-wrap items-center justify-between gap-3 rounded-xl border border-danger/25 bg-danger/5 px-4 py-3">
			<span class="flex items-center gap-2 text-sm text-danger"><AlertTriangle class="h-4 w-4 shrink-0" />{groupsError}</span>
			<button type="button" onclick={() => loadGroups()} class="inline-flex items-center rounded-lg border border-border px-3 py-1.5 text-xs font-medium transition-colors hover:bg-accent">{m.monitor_group_form_retry()}</button>
		</div>
	{/if}

	<!-- Mobile cards (hidden on md+) -->
	<div class="space-y-3 md:hidden">
		{#if displayRows.length === 0}
			<div class="rounded-xl border border-dashed border-border p-10 text-center text-sm text-muted-foreground">
				{m.monitors_page_none_found()}
			</div>
		{:else}
			{#each displayRows as row (row.key)}
				{#if row.kind === 'group'}
					<div
						class="flex items-center gap-2 rounded-xl border border-border bg-muted/20 px-3 py-2.5"
						style="margin-left: {row.depth * 0.9}rem"
					>
						<button
							type="button"
							onclick={() => toggleGroupCollapse(row.group)}
							class="grid h-6 w-6 shrink-0 place-items-center rounded text-muted-foreground hover:bg-accent hover:text-foreground"
							aria-label={row.collapsed ? m.monitors_page_expand_group() : m.monitors_page_collapse_group()}
						>
							{#if row.collapsed}
								<ChevronRight class="h-4 w-4" />
							{:else}
								<ChevronDown class="h-4 w-4" />
							{/if}
						</button>
						{#if row.collapsed}
							<Folder class="h-4 w-4 shrink-0 text-primary/80" />
						{:else}
							<FolderOpen class="h-4 w-4 shrink-0 text-primary/80" />
						{/if}
						<span class="min-w-0 flex-1 truncate font-medium">{row.group.name}</span>
						<span class="text-xs text-faint">{row.childCount}</span>
						{#if row.status !== null}
							<StatusPill status={rollupStatusToPillStatus(row.status)} />
						{/if}
						{#if row.group.can_edit || row.group.can_edit_metadata}
							<button onclick={() => openEditGroup(row.group)} class="rounded p-1 hover:bg-accent" aria-label={m.monitors_page_edit_group()}><Edit2 class="h-3.5 w-3.5" /></button>
						{/if}
						{#if row.group.can_edit}
							<button onclick={() => handleDeleteGroup(row.group)} class="rounded p-1 text-danger hover:bg-accent" aria-label={m.monitors_page_delete_group()}><Trash2 class="h-3.5 w-3.5" /></button>
						{/if}
					</div>
				{:else}
					<div class="rounded-xl border border-border bg-card p-4" style="margin-left: {row.depth * 0.9}rem">
						<div class="flex items-start justify-between">
							<div class="flex items-center gap-2">
								<Activity class="h-4 w-4 text-muted-foreground" />
								<span class="font-medium">{row.monitor.name}</span>
							</div>
							<StatusPill status={row.monitor.status} />
						</div>
						<div class="mt-2 flex items-center gap-3 text-xs text-muted-foreground">
							<span class="font-mono">{row.monitor.type}</span>
							{#if row.monitor.target}
								<span class="truncate">{row.monitor.target}</span>
							{/if}
							{#if row.ping}
								<span>{row.ping}ms</span>
							{/if}
						</div>
						{#if row.monitor.tags?.length}
							<div class="mt-2 flex flex-wrap gap-1">
								{#each row.monitor.tags as tag (tag.id)}
									<span
										class="rounded px-1.5 py-0.5 text-[10px] font-medium text-white"
										style="background-color: {tag.color}"
									>{tag.name}{#if tag.value}: {tag.value}{/if}</span>
								{/each}
							</div>
						{/if}
						<div class="mt-3 flex items-center gap-2 border-t pt-3">
							<a href={`/monitors/${row.monitor.id}`} class="rounded border px-2 py-1 text-xs hover:bg-accent flex items-center gap-1">
								<Activity class="h-3.5 w-3.5" /> {m.monitors_page_view()}
							</a>
							<button onclick={() => openEdit(row.monitor)} class="rounded border px-2 py-1 text-xs hover:bg-accent flex items-center gap-1">
								<Edit2 class="h-3.5 w-3.5" /> {m.btn_edit()}
							</button>
							<button onclick={() => handleDelete(row.monitor.id, row.monitor.name)} class="rounded border border-destructive/30 px-2 py-1 text-xs text-destructive hover:bg-destructive/10 flex items-center gap-1">
								<Trash2 class="h-3.5 w-3.5" /> {m.btn_delete()}
							</button>
						</div>
					</div>
				{/if}
			{/each}
		{/if}
	</div>

	<!-- Desktop table (hidden on mobile) -->
	<div class="hidden overflow-x-auto rounded-xl border border-border bg-card md:block">
		<table class="w-full text-sm">
			<thead>
				<tr class="border-b border-border text-left">
					<th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">{m.monitors_page_col_name()}</th>
					<th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">{m.monitors_type()}</th>
					<th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">{m.monitors_page_col_status()}</th>
					<th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">{m.monitors_target()}</th>
					<th class="px-4 py-3 text-right text-[11px] font-semibold uppercase tracking-wider text-faint">{m.monitors_page_col_latency()}</th>
					<th class="w-24 px-4 py-3 text-right text-[11px] font-semibold uppercase tracking-wider text-faint">{m.monitors_page_col_actions()}</th>
				</tr>
			</thead>
			<tbody>
				{#if displayRows.length === 0}
					<tr>
						<td colspan="6" class="px-4 py-12 text-center text-muted-foreground">
							{m.monitors_page_none_found()}
						</td>
					</tr>
				{:else}
					{#each displayRows as row (row.key)}
						{#if row.kind === 'group'}
							<tr class="border-b border-border bg-muted/20 transition-colors last:border-0 hover:bg-accent/30">
								<td class="px-4 py-3 font-medium" colspan="4">
									<div class="flex items-center gap-2" style="padding-left: {row.depth * 1.25}rem">
										<button
											type="button"
											onclick={() => toggleGroupCollapse(row.group)}
											class="grid h-5 w-5 shrink-0 place-items-center rounded text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
											aria-label={row.collapsed ? m.monitors_page_expand_group() : m.monitors_page_collapse_group()}
										>
											{#if row.collapsed}
												<ChevronRight class="h-4 w-4" />
											{:else}
												<ChevronDown class="h-4 w-4" />
											{/if}
										</button>
										{#if row.collapsed}
											<Folder class="h-4 w-4 shrink-0 text-primary/80" />
										{:else}
											<FolderOpen class="h-4 w-4 shrink-0 text-primary/80" />
										{/if}
										<span class="truncate">{row.group.name}</span>
										<span class="text-xs text-faint">{row.childCount}</span>
										{#if row.status !== null}
											<StatusPill status={rollupStatusToPillStatus(row.status)} />
										{/if}
									</div>
								</td>
								<td class="px-4 py-3"></td>
								<td class="px-4 py-3 text-right">
									<div class="flex justify-end gap-1">
										{#if row.group.can_edit || row.group.can_edit_metadata}
											<button onclick={() => openEditGroup(row.group)} class="rounded p-1.5 hover:bg-accent" title={m.monitors_page_edit_group()}>
												<Edit2 class="h-4 w-4" />
											</button>
										{/if}
										{#if row.group.can_edit}
											<button onclick={() => handleDeleteGroup(row.group)} class="rounded p-1.5 text-danger hover:bg-accent" title={m.monitors_page_delete_group()}>
												<Trash2 class="h-4 w-4" />
											</button>
										{/if}
									</div>
								</td>
							</tr>
						{:else}
							<tr class="border-b border-border transition-colors last:border-0 hover:bg-accent/40">
								<td class="px-4 py-3 font-medium">
									<div class="flex flex-col gap-1" style="padding-left: {row.depth * 1.25}rem">
										<div class="flex items-center gap-2">
											<Activity class="h-4 w-4 text-muted-foreground" />
											{row.monitor.name}
										</div>
										{#if row.monitor.tags?.length}
											<div class="flex flex-wrap gap-1 pl-6">
												{#each row.monitor.tags as tag (tag.id)}
													<span
														class="rounded px-1.5 py-0.5 text-[10px] font-medium text-white"
														style="background-color: {tag.color}"
													>{tag.name}{#if tag.value}: {tag.value}{/if}</span>
												{/each}
											</div>
										{/if}
									</div>
								</td>
								<td class="px-4 py-3 font-mono text-xs text-muted-foreground">{row.monitor.type}</td>
								<td class="px-4 py-3"><StatusPill status={row.monitor.status} /></td>
								<td class="px-4 py-3 font-mono text-xs text-muted-foreground truncate max-w-[200px]">{row.monitor.target ?? '-'}</td>
								<td class="px-4 py-3 text-right tabular-nums text-muted-foreground">
									{row.ping ?? '-'}
									{row.ping ? 'ms' : ''}
								</td>
								<td class="px-4 py-3 text-right">
									<div class="flex justify-end gap-1">
										<a href={`/monitors/${row.monitor.id}`} class="rounded p-1.5 hover:bg-accent" title={m.monitors_page_view_details()}>
											<Activity class="h-4 w-4" />
										</a>
										<button onclick={() => openEdit(row.monitor)} class="rounded p-1.5 hover:bg-accent" title={m.btn_edit()}>
											<Edit2 class="h-4 w-4" />
										</button>
										<button onclick={() => handleDelete(row.monitor.id, row.monitor.name)} class="rounded p-1.5 text-danger hover:bg-accent" title={m.btn_delete()}>
											<Trash2 class="h-4 w-4" />
										</button>
									</div>
								</td>
							</tr>
						{/if}
					{/each}
				{/if}
			</tbody>
		</table>
	</div>

	{#if showForm}
		<MonitorForm
			monitor={editingMonitor}
			onSaved={handleSaved}
			onClose={closeForm}
		/>
	{/if}

	{#if showGroupForm}
		<MonitorGroupForm
			group={editingGroup}
			{groups}
			onSaved={handleGroupSaved}
			onClose={closeGroupForm}
		/>
	{/if}
</div>
{/if}
