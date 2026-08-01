<script lang="ts">
	import { onMount } from 'svelte';
	import type { Heartbeat as HistoryHeartbeat } from '$lib/api/heartbeats.js';
	import type { Heartbeat as LiveHeartbeat, Monitor } from '$lib/stores/ws.svelte.js';
	import BrandMark from './BrandMark.svelte';
	import WallboardMonitorCard from './WallboardMonitorCard.svelte';
	import { Maximize2, Minimize2, SearchX } from '@lucide/svelte';
	import * as m from '$lib/paraglide/messages.js';

	interface Props {
		monitors: Monitor[];
		allMonitorCount: number;
		heartbeats: Map<number, LiveHeartbeat>;
		heartbeatHistory: Map<number, HistoryHeartbeat[]>;
		connected: boolean;
		avgPing: number | null;
		uptimePct: number | null;
		filtered: boolean;
		/** Explicit dashboard sorts pass through; default mode keeps urgency-first wallboard order. */
		respectOrder: boolean;
	}

	let {
		monitors,
		allMonitorCount,
		heartbeats,
		heartbeatHistory,
		connected,
		avgPing,
		uptimePct,
		filtered,
		respectOrder,
	}: Props = $props();

	const statusPriority: Record<Monitor['status'], number> = {
		down: 0,
		pending: 1,
		maintenance: 2,
		paused: 3,
		up: 4,
	};

	// The default wallboard stays urgency-first. Any operator-selected dashboard
	// sort, including custom status priority, is already reflected in `monitors`.
	let sortedMonitors = $derived(
		respectOrder
			? [...monitors]
			: [...monitors].sort((a, b) => statusPriority[a.status] - statusPriority[b.status]),
	);
	let upCount = $derived(monitors.filter((monitor) => monitor.status === 'up').length);
	let downCount = $derived(monitors.filter((monitor) => monitor.status === 'down').length);
	let pendingCount = $derived(monitors.filter((monitor) => monitor.status === 'pending').length);
	let maintenanceCount = $derived(
		monitors.filter((monitor) => monitor.status === 'maintenance').length,
	);
	let pausedCount = $derived(monitors.filter((monitor) => monitor.status === 'paused').length);

	let active = $state(false);
	let now = $state(new Date());
	let currentTime = $derived(
		new Intl.DateTimeFormat(undefined, {
			hour: '2-digit',
			minute: '2-digit',
			second: '2-digit',
		}).format(now),
	);
	let currentDate = $derived(
		new Intl.DateTimeFormat(undefined, {
			weekday: 'short',
			month: 'short',
			day: 'numeric',
		}).format(now),
	);

	$effect(() => {
		if (!active) return;
		now = new Date();
		const timer = window.setInterval(() => (now = new Date()), 1_000);
		return () => window.clearInterval(timer);
	});

	async function enter() {
		active = true;
		try {
			await document.documentElement.requestFullscreen();
		} catch {
			// Browser or iframe policy may block native fullscreen. The fixed
			// presentation layer remains active as an explicitly escapable fallback.
		}
	}

	async function exit() {
		if (document.fullscreenElement) {
			try {
				await document.exitFullscreen();
			} catch {
				// Removing the fixed wallboard remains a safe fallback.
			}
		}
		active = false;
	}

	onMount(() => {
		const handleFullscreenChange = () => {
			if (active && !document.fullscreenElement) active = false;
		};
		const handleKeydown = (event: KeyboardEvent) => {
			if (active && event.key === 'Escape') void exit();
		};
		document.addEventListener('fullscreenchange', handleFullscreenChange);
		document.addEventListener('keydown', handleKeydown);
		return () => {
			document.removeEventListener('fullscreenchange', handleFullscreenChange);
			document.removeEventListener('keydown', handleKeydown);
		};
	});
</script>

<button
	type="button"
	onclick={enter}
	class="inline-flex items-center gap-1.5 rounded-lg border border-border px-3 py-1.5 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent/50 hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
>
	<Maximize2 class="h-3.5 w-3.5" />
	{m.dashboard_wallboard_enter()}
</button>

{#if active}
	<section
		class="fixed inset-0 z-[100] flex min-h-dvh flex-col overflow-hidden bg-background text-foreground"
		aria-label={m.dashboard_wallboard_title()}
		data-testid="dashboard-wallboard"
	>
		<header
			class="flex shrink-0 items-center gap-4 border-b border-border bg-background/95 px-4 py-3 backdrop-blur md:px-6"
		>
			<div
				class="grid h-10 w-10 shrink-0 place-items-center rounded-xl border border-border bg-elevated"
			>
				<BrandMark size={25} />
			</div>
			<div class="min-w-0">
				<h1 class="truncate text-base font-semibold tracking-tight md:text-lg">
					{m.dashboard_wallboard_title()}
				</h1>
				<p class="truncate text-xs text-muted-foreground">
					{#if filtered}
						{m.dashboard_wallboard_filtered({ shown: monitors.length, total: allMonitorCount })}
					{:else if downCount > 0}
						{m.dashboard_wallboard_attention({ count: downCount })}
					{:else}
						{m.layout_all_systems_operational()}
					{/if}
				</p>
			</div>

			<div class="ml-auto flex items-center gap-3">
				<div
					class="hidden items-center gap-2 rounded-lg border px-3 py-2 text-xs sm:flex
						{connected
							? 'border-success/25 bg-success/[0.07] text-success'
							: 'border-warning/25 bg-warning/[0.08] text-warning'}"
				>
					<span
						class="dot {connected
							? 'dot-up animate-pulse-dot'
							: 'dot-warn animate-pulse-dot'}"
					></span>
					{connected
						? m.dashboard_wallboard_connected()
						: m.dashboard_wallboard_disconnected()}
				</div>
				<div class="hidden text-right md:block">
					<div class="font-mono text-base font-semibold tabular-nums">{currentTime}</div>
					<div class="text-[11px] text-muted-foreground">{currentDate}</div>
				</div>
				<button
					type="button"
					onclick={exit}
					aria-label={m.dashboard_wallboard_exit()}
					class="inline-flex items-center gap-2 rounded-lg border border-border bg-surface px-3 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
				>
					<Minimize2 class="h-4 w-4" />
					<span class="hidden sm:inline">{m.dashboard_wallboard_exit()}</span>
				</button>
			</div>
		</header>

		{#if !connected}
			<div
				class="flex shrink-0 items-center justify-center gap-2 border-b border-warning/25 bg-warning/10 px-4 py-2 text-sm font-medium text-warning"
			>
				<span class="dot dot-warn animate-pulse-dot"></span>
				{m.reconnecting()}
			</div>
		{/if}

		<div
			class="grid shrink-0 grid-cols-2 border-b border-border bg-card sm:grid-cols-4 xl:grid-cols-8"
		>
			<div class="border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.dashboard_total_monitors()}
				</div>
				<div class="mt-0.5 font-mono text-xl font-semibold tabular-nums">{monitors.length}</div>
			</div>
			<div class="border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.dashboard_up()}
				</div>
				<div class="mt-0.5 font-mono text-xl font-semibold tabular-nums text-success">
					{upCount}
				</div>
			</div>
			<div class="border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.dashboard_down()}
				</div>
				<div
					class="mt-0.5 font-mono text-xl font-semibold tabular-nums {downCount > 0
						? 'text-danger'
						: ''}"
				>
					{downCount}
				</div>
			</div>
			<div class="border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.status_pending()}
				</div>
				<div
					class="mt-0.5 font-mono text-xl font-semibold tabular-nums {pendingCount > 0
						? 'text-warning'
						: ''}"
				>
					{pendingCount}
				</div>
			</div>
			<div class="border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.status_maintenance()}
				</div>
				<div class="mt-0.5 font-mono text-xl font-semibold tabular-nums">
					{maintenanceCount}
				</div>
			</div>
			<div class="border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.status_paused()}
				</div>
				<div class="mt-0.5 font-mono text-xl font-semibold tabular-nums">{pausedCount}</div>
			</div>
			<div class="border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.dashboard_avg_response()}
				</div>
				<div class="mt-0.5 font-mono text-xl font-semibold tabular-nums">
					{avgPing === null ? '—' : `${avgPing} ms`}
				</div>
			</div>
			<div class="px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.dashboard_uptime()}
				</div>
				<div class="mt-0.5 font-mono text-xl font-semibold tabular-nums">
					{uptimePct === null ? '—' : `${uptimePct.toFixed(2)}%`}
				</div>
			</div>
		</div>

		<div class="flex-1 overflow-y-auto overscroll-none p-3 md:p-4 xl:p-5">
			{#if sortedMonitors.length === 0}
				<div
					class="grid h-full min-h-64 place-items-center rounded-xl border border-dashed border-border"
				>
					<div class="text-center">
						<SearchX class="mx-auto h-7 w-7 text-muted-foreground" />
						<p class="mt-3 text-sm text-muted-foreground">{m.dashboard_wallboard_empty()}</p>
					</div>
				</div>
			{:else}
				<div class="grid grid-cols-[repeat(auto-fit,minmax(17rem,1fr))] gap-3 xl:gap-4">
					{#each sortedMonitors as monitor (monitor.id)}
						<WallboardMonitorCard
							{monitor}
							heartbeat={heartbeats.get(monitor.id)}
							heartbeatHistory={heartbeatHistory.get(monitor.id) ?? []}
						/>
					{/each}
				</div>
			{/if}
		</div>
	</section>
{/if}
