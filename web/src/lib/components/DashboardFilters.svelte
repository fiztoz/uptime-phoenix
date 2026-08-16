<script lang="ts">
	import {
		DEFAULT_STATUS_ORDER,
		EMPTY_CRITERIA,
		STATUS_FILTERS,
		UNGROUPED,
		hasActiveFilters,
		type DashboardSort,
		type FilterCriteria,
		type MonitorStatus,
		type NormalizedTag,
	} from '$lib/monitor-filter';
	import type { DashboardCardBody } from '$lib/dashboard-card';
	import { ChevronLeft, ChevronRight, RotateCcw, Search, X } from '@lucide/svelte';
	import Select from '$lib/components/Select.svelte';
	import MultiSelect from '$lib/components/MultiSelect.svelte';
	import * as m from '$lib/paraglide/messages.js';

	interface Props {
		criteria: FilterCriteria;
		/** Indented tree labels from buildGroupOptions() — [] hides the group select. */
		groupOptions: { id: number; name: string; depth: number }[];
		/** Tag catalog for the multi-select. Empty → "No tags created yet". */
		tagOptions: NormalizedTag[];
		/** Distinct monitor types actually present — [] hides the type select. */
		typeOptions: string[];
		shown: number;
		total: number;
		/** Per-status monitor counts for the dropdown. */
		statusCounts?: Record<MonitorStatus, number>;
		/** Per-tag monitor counts for the dropdown. */
		tagCounts?: Record<string, number>;
		cardBody?: DashboardCardBody;
		onCardBodyChange?: (next: DashboardCardBody) => void;
		onchange: (next: FilterCriteria) => void;
	}

	let {
		criteria,
		groupOptions,
		tagOptions,
		typeOptions,
		shown,
		total,
		statusCounts = { up: 0, down: 0, pending: 0, maintenance: 0, paused: 0 },
		tagCounts = {},
		cardBody = 'response',
		onCardBodyChange,
		onchange,
	}: Props = $props();

	const active = $derived(hasActiveFilters(criteria));

	const STATUS_LABELS = $derived<Record<string, string>>({
		up: m.dashboard_up(),
		down: m.status_down(),
		pending: m.status_pending(),
		maintenance: m.status_maintenance(),
		paused: m.status_paused(),
	});

	const STATUS_DOT_CLASSES: Record<string, string> = {
		up: 'dot-up',
		down: 'dot-down',
		pending: 'dot-warn',
		maintenance: 'dot-info',
		paused: 'dot-muted',
	};

	/** NBSP-padded labels for the group tree. */
	function groupLabel(depth: number, name: string): string {
		return '\u00A0\u00A0'.repeat(depth) + name;
	}

	function patch(part: Partial<FilterCriteria>) {
		onchange({ ...criteria, ...part });
	}

	function selectGroup(raw: string) {
		if (raw === '') patch({ group: null });
		else if (raw === UNGROUPED) patch({ group: UNGROUPED });
		else patch({ group: Number(raw) });
	}

	function toggleStatus(value: string) {
		const status = value as MonitorStatus;
		const next = criteria.statuses.includes(status)
			? criteria.statuses.filter((s) => s !== status)
			: [...criteria.statuses, status];
		patch({ statuses: next });
	}

	function resetStatuses() {
		patch({ statuses: [] });
	}

	function toggleTag(value: string) {
		const next = criteria.tags.includes(value)
			? criteria.tags.filter((t) => t !== value)
			: [...criteria.tags, value];
		patch({ tags: next });
	}

	function resetTags() {
		patch({ tags: [] });
	}

	function selectSort(value: string) {
		patch({ sort: value as DashboardSort });
	}

	function moveStatus(index: number, direction: -1 | 1) {
		const target = index + direction;
		if (target < 0 || target >= criteria.statusOrder.length) return;
		const next = [...criteria.statusOrder];
		const currentStatus = next[index];
		const targetStatus = next[target];
		if (!currentStatus || !targetStatus) return;
		next[index] = targetStatus;
		next[target] = currentStatus;
		patch({ statusOrder: next });
	}

	function resetStatusOrder() {
		patch({ statusOrder: [...DEFAULT_STATUS_ORDER] });
	}

	const groupValue = $derived(criteria.group === null ? '' : String(criteria.group));

	const groupItems = $derived([
		{ value: '', label: m.dashboard_filters_all_groups() },
		...groupOptions.map((g) => ({ value: String(g.id), label: groupLabel(g.depth, g.name) })),
		{ value: UNGROUPED, label: m.dashboard_filters_ungrouped() },
	]);

	const statusItems = $derived(
		STATUS_FILTERS.map((s) => ({
			value: s,
			label: STATUS_LABELS[s],
			count: statusCounts[s] ?? 0,
			dotClass: STATUS_DOT_CLASSES[s],
		})),
	);

	const tagItems = $derived(
		tagOptions.map((t) => ({
			value: t.name,
			label: t.name,
			count: tagCounts[t.name] ?? 0,
			dotColor: t.color || undefined,
		})),
	);

	const typeItems = $derived([
		{ value: '', label: m.dashboard_filters_all_types() },
		...typeOptions.map((t) => ({ value: t, label: t.toUpperCase() })),
	]);

	const sortItems = $derived([
		{ value: 'default', label: m.dashboard_sort_default() },
		{ value: 'status', label: m.dashboard_sort_status() },
		{ value: 'name-asc', label: m.dashboard_sort_name_asc() },
		{ value: 'name-desc', label: m.dashboard_sort_name_desc() },
		{ value: 'response-asc', label: m.dashboard_sort_response_asc() },
		{ value: 'response-desc', label: m.dashboard_sort_response_desc() },
	]);

	const cardItems = $derived([
		{ value: 'response', label: m.dashboard_card_response() },
		{ value: 'signals', label: m.dashboard_card_signals() },
	]);
</script>

<div class="rounded-xl border border-border bg-card p-3">
	<div class="flex flex-wrap items-center gap-2">
		<!-- Search: name (contains) + target/URL -->
		<div class="relative min-w-56 flex-1">
			<Search
				class="pointer-events-none absolute left-2.5 top-1/2 h-4 w-4 -translate-y-1/2 text-muted-foreground"
			/>
			<input
				type="search"
				placeholder={m.dashboard_filters_search_placeholder()}
				value={criteria.search}
				oninput={(e) => patch({ search: e.currentTarget.value })}
				aria-label={m.dashboard_filters_search_aria()}
				class="h-9 w-full rounded-lg border border-border bg-surface pl-8 pr-2.5 text-sm text-foreground placeholder:text-faint transition-colors hover:border-border/80 focus:outline-none focus:ring-2 focus:ring-ring"
			/>
		</div>

		{#if groupOptions.length > 0}
			<Select
				options={groupItems}
				value={groupValue}
				onValueChange={selectGroup}
				ariaLabel={m.dashboard_filters_group_aria()}
				size="sm"
			/>
		{/if}

		<MultiSelect
			options={tagItems}
			selected={criteria.tags}
			onToggle={toggleTag}
			onReset={resetTags}
			placeholder={m.dashboard_filters_all_tags()}
			emptyMessage={m.dashboard_filters_no_tags()}
			ariaLabel={m.dashboard_filters_tags_aria()}
			size="sm"
		/>

		<MultiSelect
			options={statusItems}
			selected={criteria.statuses}
			onToggle={toggleStatus}
			onReset={resetStatuses}
			placeholder={m.dashboard_filters_all_statuses()}
			ariaLabel={m.dashboard_filters_statuses_aria()}
			size="sm"
		/>

		{#if typeOptions.length > 1}
			<Select
				options={typeItems}
				value={criteria.type}
				onValueChange={(v) => patch({ type: v })}
				ariaLabel={m.dashboard_filters_type_aria()}
				size="sm"
			/>
		{/if}

		<Select
			options={sortItems}
			value={criteria.sort}
			onValueChange={selectSort}
			ariaLabel={m.dashboard_sort_aria()}
			class="min-w-36"
			size="sm"
		/>

		<Select
			options={cardItems}
			value={cardBody}
			onValueChange={(value) => onCardBodyChange?.(value as DashboardCardBody)}
			ariaLabel={m.dashboard_card_aria()}
			class="min-w-40"
			size="sm"
		/>
	</div>

	{#if criteria.sort === 'status'}
		<div class="mt-2.5 border-t border-border pt-2.5">
			<div class="flex flex-wrap items-center gap-2">
				<span class="mr-1 text-xs font-medium text-muted-foreground">
					{m.dashboard_sort_status_priority()}
				</span>
				{#each criteria.statusOrder as status, index (status)}
					<div class="inline-flex h-8 items-center rounded-lg border border-border bg-surface">
						<span class="border-r border-border px-2 font-mono text-[10px] text-faint">
							{index + 1}
						</span>
						<span class="dot {STATUS_DOT_CLASSES[status]} ml-2 shrink-0"></span>
						<span class="min-w-20 px-2 text-xs font-medium">{STATUS_LABELS[status]}</span>
						<button
							type="button"
							onclick={() => moveStatus(index, -1)}
							disabled={index === 0}
							aria-label={m.dashboard_sort_move_earlier({ status: STATUS_LABELS[status] })}
							class="grid h-full w-7 place-items-center border-l border-border text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:cursor-not-allowed disabled:opacity-30"
						>
							<ChevronLeft class="h-3.5 w-3.5" />
						</button>
						<button
							type="button"
							onclick={() => moveStatus(index, 1)}
							disabled={index === criteria.statusOrder.length - 1}
							aria-label={m.dashboard_sort_move_later({ status: STATUS_LABELS[status] })}
							class="grid h-full w-7 place-items-center border-l border-border text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:cursor-not-allowed disabled:opacity-30"
						>
							<ChevronRight class="h-3.5 w-3.5" />
						</button>
					</div>
				{/each}
				<button
					type="button"
					onclick={resetStatusOrder}
					class="inline-flex h-8 items-center gap-1.5 rounded-lg px-2 text-xs text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
				>
					<RotateCcw class="h-3.5 w-3.5" />
					{m.dashboard_sort_reset_order()}
				</button>
			</div>
		</div>
	{/if}

	<div class="mt-2.5 flex items-center justify-between gap-3 border-t border-border pt-2.5">
		<p class="text-xs text-muted-foreground tabular-nums">
			{#if active}
				{m.dashboard_filters_showing_pre()} <span class="font-semibold text-foreground">{shown}</span> {m.dashboard_filters_showing_of()} {total}
				{total === 1 ? m.group_card_monitor_singular() : m.group_card_monitor_plural()}
			{:else}
				{total}
				{total === 1 ? m.group_card_monitor_singular() : m.group_card_monitor_plural()}
			{/if}
		</p>
		{#if active}
			<button
				type="button"
				onclick={() =>
					onchange({
						...EMPTY_CRITERIA,
						sort: criteria.sort,
						statusOrder: [...criteria.statusOrder],
					})}
				class="inline-flex items-center gap-1 rounded-lg border border-border px-2 py-1 text-xs text-muted-foreground transition-colors hover:bg-accent/50 hover:text-foreground"
			>
				<X class="h-3 w-3" />
				{m.dashboard_filters_clear()}
			</button>
		{/if}
	</div>
</div>
