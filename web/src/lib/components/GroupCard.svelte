<script lang="ts">
	import type { MonitorGroupView } from '$lib/api/monitorGroups';
	import { rollupStatusToPillStatus, type RollupStatus } from '$lib/api/monitorGroups';
	import type { GroupSummary } from '$lib/monitor-filter';
	import StatusPill from './StatusPill.svelte';
	import { Folder, Layers, ArrowUpRight } from '@lucide/svelte';
	import * as m from '$lib/paraglide/messages.js';

	interface Props {
		group: MonitorGroupView;
		/** Recursive counts for everything inside the group (subgroups included). */
		summary: GroupSummary;
		/**
		 * Live rolled-up status from resolveGroupStatuses(), or null when the group
		 * has none to show ("ignore" condition, or no children at all) — in which
		 * case no pill is rendered, matching the tree view's old behaviour.
		 */
		status: RollupStatus | null;
		/** Drills in: sets the dashboard's group filter to this group. */
		ondrill: () => void;
	}

	let { group, summary, status, ondrill }: Props = $props();

	const isDown = $derived(status === 0);
	const hasDown = $derived(summary.down > 0);
</script>

<button
	type="button"
	onclick={ondrill}
	class="group relative block w-full overflow-hidden rounded-xl border border-border bg-card p-5 text-left transition-all duration-200 hover:-translate-y-0.5 hover:border-primary/30 hover:shadow-[0_18px_40px_-24px_rgba(0,0,0,0.8)]"
>
	<!-- Left status accent, same language as MonitorCard: loud when down, primary on hover. -->
	<span
		class="absolute inset-y-0 left-0 w-[3px] transition-opacity duration-200
		{isDown || hasDown ? 'bg-danger opacity-100' : 'bg-primary opacity-0 group-hover:opacity-100'}"
	></span>

	<div class="flex items-start justify-between gap-3">
		<div class="min-w-0">
			<h3 class="flex items-center gap-1.5 truncate font-semibold tracking-tight">
				<Folder class="h-4 w-4 shrink-0 text-primary/80" />
				{group.name}
				<ArrowUpRight
					class="h-3.5 w-3.5 shrink-0 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100"
				/>
			</h3>
			{#if group.description}
				<p class="mt-0.5 truncate text-xs text-muted-foreground">{group.description}</p>
			{:else}
				<p class="mt-0.5 truncate text-xs text-faint">
					{summary.total}
					{summary.total === 1 ? m.group_card_monitor_singular() : m.group_card_monitor_plural()}
				</p>
			{/if}
		</div>
		{#if status !== null}
			<StatusPill status={rollupStatusToPillStatus(status)} />
		{/if}
	</div>

	<!-- Summary of everything inside, recursively. -->
	<div class="mt-4 grid grid-cols-3 gap-2">
		<div>
			<div class="eyebrow">{m.nav_monitors()}</div>
			<div class="mt-0.5 text-xl font-semibold tabular-nums">{summary.total}</div>
		</div>
		<div>
			<div class="eyebrow">{m.status_up()}</div>
			<div
				class="mt-0.5 text-xl font-semibold tabular-nums {summary.up > 0
					? 'text-success'
					: 'text-muted-foreground'}"
			>
				{summary.up}
			</div>
		</div>
		<div>
			<div class="eyebrow">{m.dashboard_down()}</div>
			<div
				class="mt-0.5 text-xl font-semibold tabular-nums {hasDown
					? 'text-danger'
					: 'text-muted-foreground'}"
			>
				{summary.down}
			</div>
		</div>
	</div>

	{#if hasDown || summary.pending > 0 || summary.idle > 0 || summary.subgroups > 0}
		<div
			class="mt-3 flex flex-wrap items-center gap-x-3 gap-y-1 border-t border-border pt-3 text-xs"
		>
			{#if hasDown}
				<!-- Loud: the one number that should pull the eye across a wall of cards. -->
				<span
					class="inline-flex items-center gap-1 rounded-full border border-danger/25 bg-danger/10 px-2 py-0.5 font-semibold text-danger tabular-nums"
				>
					<span class="dot dot-down"></span>
					{summary.down}
					{summary.down === 1 ? m.group_card_down_singular() : m.group_card_down_plural()}
				</span>
			{/if}
			{#if summary.pending > 0}
				<span class="text-warning tabular-nums">{m.group_card_pending_count({ count: summary.pending })}</span>
			{/if}
			{#if summary.idle > 0}
				<span class="text-muted-foreground tabular-nums">{m.group_card_paused_count({ count: summary.idle })}</span>
			{/if}
			{#if summary.subgroups > 0}
				<span class="inline-flex items-center gap-1 text-faint tabular-nums">
					<Layers class="h-3 w-3" />
					{summary.subgroups}
					{summary.subgroups === 1 ? m.group_card_subgroup_singular() : m.group_card_subgroup_plural()}
				</span>
			{/if}
		</div>
	{/if}
</button>
