<script lang="ts">
	import type { Monitor, Heartbeat } from '$lib/stores/ws.svelte.js';
	import StatusPill from './StatusPill.svelte';
	import Sparkline from './charts/Sparkline.svelte';
	import { sparklinePoints } from '$lib/utils/chart.js';
	import { ArrowUpRight } from '@lucide/svelte';
	import type { MonitorCondition } from '$lib/api/conditions';
	import ConditionChip from './ConditionChip.svelte';
	import * as m from '$lib/paraglide/messages.js';

	interface Props {
		monitor: Monitor;
		heartbeat?: Heartbeat;
		heartbeatHistory?: Heartbeat[];
		conditions?: MonitorCondition[];
		conditionNow?: number;
	}

	let {
		monitor,
		heartbeat,
		heartbeatHistory = [],
		conditions = [],
		conditionNow = Date.now(),
	}: Props = $props();

	/** Transform heartbeats into sparkline data points. */
	const sparklineData = $derived(sparklinePoints(heartbeatHistory));

	const isDown = $derived(monitor.status === 'down');
</script>

<a
	href="/monitors/{monitor.id}"
	class="group relative block overflow-hidden rounded-xl border border-border bg-card p-5 transition-all duration-200 hover:-translate-y-0.5 hover:border-primary/30 hover:shadow-[0_18px_40px_-24px_rgba(0,0,0,0.8)]"
>
	<!-- Left status accent on hover / when down -->
	<span
		class="absolute inset-y-0 left-0 w-[3px] transition-opacity duration-200
		{isDown ? 'bg-danger opacity-100' : 'bg-primary opacity-0 group-hover:opacity-100'}"
	></span>

	<div class="flex items-start justify-between gap-3">
		<div class="min-w-0">
			<h3 class="flex items-center gap-1.5 truncate font-semibold tracking-tight">
				{monitor.name}
				<ArrowUpRight
					class="h-3.5 w-3.5 shrink-0 text-muted-foreground opacity-0 transition-opacity group-hover:opacity-100"
				/>
			</h3>
			<p class="mt-0.5 truncate font-mono text-xs text-muted-foreground">
				<span class="text-faint uppercase">{monitor.type}</span>
				{#if monitor.target}<span class="mx-1 text-faint">·</span>{monitor.target}{/if}
			</p>
		</div>
		<StatusPill status={monitor.status} />
	</div>

	{#if conditions.length > 0}
		<div class="mt-3 flex flex-wrap gap-1.5">
			{#each conditions as condition (`${condition.monitor_id}:${condition.kind}`)}
				<ConditionChip {condition} now={conditionNow} compact />
			{/each}
		</div>
	{/if}

	<!-- Metric + sparkline -->
	<div class="mt-5 grid grid-cols-[auto_minmax(7rem,1fr)] items-end gap-5">
		<div class="min-w-[5.25rem]">
			<div class="eyebrow">{m.monitor_card_response()}</div>
			<div class="mt-0.5 text-xl font-semibold tabular-nums">
				{#if heartbeat && heartbeat.ping > 0}
					{heartbeat.ping}<span class="ml-0.5 text-sm font-normal text-muted-foreground">ms</span>
				{:else}
					<span class="text-muted-foreground">—</span>
				{/if}
			</div>
		</div>
		{#if sparklineData.length > 0}
			<div class="h-12 min-w-0 overflow-hidden rounded-md bg-muted/20 px-1 py-1">
				<Sparkline data={sparklineData} width="100%" height={40} />
			</div>
		{:else}
			<div
				class="flex h-12 items-center justify-center rounded-md border border-dashed border-border/70 text-[11px] text-faint"
			>
				{m.monitor_card_no_history()}
			</div>
		{/if}
	</div>

	{#if heartbeat?.msg}
		<p class="mt-3 truncate border-t border-border pt-3 text-xs text-muted-foreground">
			{heartbeat.msg}
		</p>
	{/if}
</a>
