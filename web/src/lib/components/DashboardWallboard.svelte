<script lang="ts">
	import { onMount } from 'svelte';
	import type { Heartbeat as HistoryHeartbeat } from '$lib/api/heartbeats.js';
	import type { MonitorCondition } from '$lib/api/conditions';
	import type { DashboardCardBody } from '$lib/dashboard-card';
	import type { Heartbeat as LiveHeartbeat, Monitor } from '$lib/stores/ws.svelte.js';
	import {
		WALLBOARD_CARD_DEFAULT_PX,
		WALLBOARD_CARD_MAX_PX,
		WALLBOARD_CARD_MIN_PX,
		WALLBOARD_CARD_STEP_PX,
		WALLBOARD_CARDS_MIN_PX,
		WALLBOARD_SPLITTER_PX,
		WALLBOARD_STATS_MIN_PX,
		WALLBOARD_STATS_STEP_PX,
		clampWallboardCardPx,
		clampWallboardStatsPx,
		readWallboardCardPx,
		readWallboardStatsPx,
		wallboardStatFontPx,
		writeWallboardCardPx,
		writeWallboardStatsPx,
	} from '$lib/wallboard-split';
	import BrandMark from './BrandMark.svelte';
	import WallboardMonitorCard from './WallboardMonitorCard.svelte';
	import { Grip, LayoutGrid, Maximize2, Minimize2, SearchX } from '@lucide/svelte';
	import * as m from '$lib/paraglide/messages.js';

	interface Props {
		monitors: Monitor[];
		allMonitorCount: number;
		heartbeats: Map<number, LiveHeartbeat>;
		heartbeatHistory: Map<number, HistoryHeartbeat[]>;
		conditionsByMonitor?: Map<number, MonitorCondition[]>;
		conditionNow?: number;
		cardBody?: DashboardCardBody;
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
		conditionsByMonitor = new Map(),
		conditionNow = Date.now(),
		cardBody = 'response',
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
	let statsPx = $state<number | null>(null);
	let cardPx = $state(WALLBOARD_CARD_DEFAULT_PX);
	let dragging = $state(false);
	let bodyEl = $state<HTMLDivElement | undefined>(undefined);
	let statsEl = $state<HTMLDivElement | undefined>(undefined);

	let statFontPx = $derived(
		statsPx == null ? null : wallboardStatFontPx(statsPx),
	);
	let splitMaxPx = $derived(
		bodyEl
			? Math.max(
					WALLBOARD_STATS_MIN_PX,
					bodyEl.clientHeight - WALLBOARD_CARDS_MIN_PX - WALLBOARD_SPLITTER_PX,
				)
			: WALLBOARD_STATS_MIN_PX,
	);
	let splitNowPx = $derived(statsPx ?? WALLBOARD_STATS_MIN_PX);
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

	$effect(() => {
		if (!active) return;
		statsPx = readWallboardStatsPx();
		cardPx = clampWallboardCardPx(readWallboardCardPx());
	});

	$effect(() => {
		if (!active || !bodyEl) return;
		const el = bodyEl;
		const observer = new ResizeObserver(() => {
			if (statsPx == null) return;
			const next = clampWallboardStatsPx(statsPx, el.clientHeight);
			if (next !== statsPx) statsPx = next;
		});
		observer.observe(el);
		return () => observer.disconnect();
	});

	function applyStatsPx(next: number) {
		if (!bodyEl) return;
		statsPx = clampWallboardStatsPx(next, bodyEl.clientHeight);
		writeWallboardStatsPx(statsPx);
	}

	function resetStatsPx() {
		statsPx = null;
		writeWallboardStatsPx(null);
	}

	function onSplitterPointerDown(event: PointerEvent) {
		if (event.button !== 0) return;
		const body = bodyEl;
		const stats = statsEl;
		if (!body || !stats) return;
		const splitBody = body;
		event.preventDefault();
		const handle = event.currentTarget as HTMLElement;
		handle.setPointerCapture(event.pointerId);
		const startY = event.clientY;
		const startH = stats.getBoundingClientRect().height;
		dragging = true;

		function move(ev: PointerEvent) {
			statsPx = clampWallboardStatsPx(
				startH + (ev.clientY - startY),
				splitBody.clientHeight,
			);
		}
		function stop(ev: PointerEvent) {
			dragging = false;
			handle.releasePointerCapture(ev.pointerId);
			handle.removeEventListener('pointermove', move);
			handle.removeEventListener('pointerup', stop);
			handle.removeEventListener('pointercancel', stop);
			if (statsPx != null) writeWallboardStatsPx(statsPx);
		}
		handle.addEventListener('pointermove', move);
		handle.addEventListener('pointerup', stop);
		handle.addEventListener('pointercancel', stop);
	}

	function applyCardPx(next: number) {
		cardPx = clampWallboardCardPx(next);
		writeWallboardCardPx(cardPx);
	}

	function resetCardPx() {
		applyCardPx(WALLBOARD_CARD_DEFAULT_PX);
	}

	function onCardResizeStart(event: PointerEvent) {
		if (event.button !== 0) return;
		const handle = event.currentTarget as HTMLElement;
		handle.setPointerCapture(event.pointerId);
		const startX = event.clientX;
		const startPx = cardPx;
		dragging = true;

		function move(ev: PointerEvent) {
			cardPx = clampWallboardCardPx(startPx + ev.clientX - startX);
		}
		function stop(ev: PointerEvent) {
			dragging = false;
			handle.releasePointerCapture(ev.pointerId);
			handle.removeEventListener('pointermove', move);
			handle.removeEventListener('pointerup', stop);
			handle.removeEventListener('pointercancel', stop);
			writeWallboardCardPx(cardPx);
		}
		handle.addEventListener('pointermove', move);
		handle.addEventListener('pointerup', stop);
		handle.addEventListener('pointercancel', stop);
	}

	function onCardSizeKeydown(event: KeyboardEvent) {
		if (event.key === 'Enter' || event.key === ' ') {
			event.preventDefault();
			resetCardPx();
			return;
		}
		let next: number | null = null;
		if (event.key === 'ArrowRight' || event.key === 'ArrowUp') {
			next = cardPx + WALLBOARD_CARD_STEP_PX;
		} else if (event.key === 'ArrowLeft' || event.key === 'ArrowDown') {
			next = cardPx - WALLBOARD_CARD_STEP_PX;
		} else if (event.key === 'Home') next = WALLBOARD_CARD_MIN_PX;
		else if (event.key === 'End') next = WALLBOARD_CARD_MAX_PX;
		if (next == null) return;
		event.preventDefault();
		applyCardPx(next);
	}

	function onSplitterKeydown(event: KeyboardEvent) {
		if (!bodyEl) return;
		const current = statsEl?.getBoundingClientRect().height ?? WALLBOARD_STATS_MIN_PX;
		if (event.key === 'Enter' || event.key === ' ') {
			event.preventDefault();
			resetStatsPx();
			return;
		}
		let next: number | null = null;
		if (event.key === 'ArrowDown') next = current + WALLBOARD_STATS_STEP_PX;
		else if (event.key === 'ArrowUp') next = current - WALLBOARD_STATS_STEP_PX;
		else if (event.key === 'Home') next = WALLBOARD_STATS_MIN_PX;
		else if (event.key === 'End') {
			next = bodyEl.clientHeight - WALLBOARD_CARDS_MIN_PX - WALLBOARD_SPLITTER_PX;
		}
		if (next == null) return;
		event.preventDefault();
		applyStatsPx(next);
	}

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
				<label
					class="flex items-center gap-2 rounded-lg border border-border bg-surface px-2.5 py-1.5"
					title={m.dashboard_wallboard_card_size_reset()}
				>
					<LayoutGrid class="h-3.5 w-3.5 shrink-0 text-muted-foreground" />
					<span class="sr-only">{m.dashboard_wallboard_card_size()}</span>
					<input
						type="range"
						min={WALLBOARD_CARD_MIN_PX}
						max={WALLBOARD_CARD_MAX_PX}
						step={WALLBOARD_CARD_STEP_PX}
						value={cardPx}
						aria-label={m.dashboard_wallboard_card_size()}
						data-testid="wallboard-card-size"
						class="h-1 w-24 cursor-ew-resize accent-primary"
						oninput={(event) =>
							applyCardPx(Number((event.currentTarget as HTMLInputElement).value))}
						ondblclick={resetCardPx}
						onkeydown={onCardSizeKeydown}
					/>
				</label>
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
			bind:this={bodyEl}
			class="flex min-h-0 flex-1 flex-col {dragging ? 'select-none' : ''}"
		>
		<div
			bind:this={statsEl}
			id="wallboard-stats"
			class="grid shrink-0 grid-cols-2 overflow-hidden border-b border-border bg-card sm:grid-cols-4 xl:grid-cols-8"
			style:height={statsPx != null ? `${statsPx}px` : undefined}
		>
			<div class="flex flex-col justify-center border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.dashboard_total_monitors()}
				</div>
				<div
					class="mt-0.5 font-mono font-semibold tabular-nums {statFontPx == null ? 'text-xl' : ''}"
					style:font-size={statFontPx != null ? `${statFontPx}px` : undefined}
				>
					{monitors.length}
				</div>
			</div>
			<div class="flex flex-col justify-center border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.dashboard_up()}
				</div>
				<div
					class="mt-0.5 font-mono font-semibold tabular-nums text-success {statFontPx == null
						? 'text-xl'
						: ''}"
					style:font-size={statFontPx != null ? `${statFontPx}px` : undefined}
				>
					{upCount}
				</div>
			</div>
			<div class="flex flex-col justify-center border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.dashboard_down()}
				</div>
				<div
					class="mt-0.5 font-mono font-semibold tabular-nums {downCount > 0
						? 'text-danger'
						: ''} {statFontPx == null ? 'text-xl' : ''}"
					style:font-size={statFontPx != null ? `${statFontPx}px` : undefined}
				>
					{downCount}
				</div>
			</div>
			<div class="flex flex-col justify-center border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.status_pending()}
				</div>
				<div
					class="mt-0.5 font-mono font-semibold tabular-nums {pendingCount > 0
						? 'text-warning'
						: ''} {statFontPx == null ? 'text-xl' : ''}"
					style:font-size={statFontPx != null ? `${statFontPx}px` : undefined}
				>
					{pendingCount}
				</div>
			</div>
			<div class="flex flex-col justify-center border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.status_maintenance()}
				</div>
				<div
					class="mt-0.5 font-mono font-semibold tabular-nums {statFontPx == null ? 'text-xl' : ''}"
					style:font-size={statFontPx != null ? `${statFontPx}px` : undefined}
				>
					{maintenanceCount}
				</div>
			</div>
			<div class="flex flex-col justify-center border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.status_paused()}
				</div>
				<div
					class="mt-0.5 font-mono font-semibold tabular-nums {statFontPx == null ? 'text-xl' : ''}"
					style:font-size={statFontPx != null ? `${statFontPx}px` : undefined}
				>
					{pausedCount}
				</div>
			</div>
			<div class="flex flex-col justify-center border-r border-border px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.dashboard_avg_response()}
				</div>
				<div
					class="mt-0.5 font-mono font-semibold tabular-nums {statFontPx == null ? 'text-xl' : ''}"
					style:font-size={statFontPx != null ? `${statFontPx}px` : undefined}
				>
					{avgPing === null ? '—' : `${avgPing} ms`}
				</div>
			</div>
			<div class="flex flex-col justify-center px-4 py-2.5">
				<div class="text-[10px] font-medium uppercase tracking-wider text-faint">
					{m.dashboard_uptime()}
				</div>
				<div
					class="mt-0.5 font-mono font-semibold tabular-nums {statFontPx == null ? 'text-xl' : ''}"
					style:font-size={statFontPx != null ? `${statFontPx}px` : undefined}
				>
					{uptimePct === null ? '—' : `${uptimePct.toFixed(2)}%`}
				</div>
			</div>
		</div>

		<button
			type="button"
			role="slider"
			aria-orientation="vertical"
			aria-valuemin={WALLBOARD_STATS_MIN_PX}
			aria-valuemax={splitMaxPx}
			aria-valuenow={splitNowPx}
			aria-controls="wallboard-stats"
			aria-label={m.dashboard_wallboard_resize()}
			title={m.dashboard_wallboard_resize_reset()}
			data-testid="wallboard-splitter"
			onpointerdown={onSplitterPointerDown}
			ondblclick={resetStatsPx}
			onkeydown={onSplitterKeydown}
			class="group relative flex h-2 w-full shrink-0 cursor-row-resize items-center justify-center bg-card
				hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring
				{dragging ? 'bg-accent' : ''}"
		>
			<span
				class="h-1 w-10 rounded-full bg-border transition-colors group-hover:bg-primary/50 group-focus-visible:bg-primary/50 {dragging
					? 'bg-primary/50'
					: ''}"
			></span>
		</button>

		<div id="wallboard-cards" class="relative min-h-0 flex-1">
			<div class="h-full overflow-y-auto overscroll-none p-3 md:p-4 xl:p-5">
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
					<div
						class="grid gap-3 xl:gap-4"
						style="grid-template-columns: repeat(auto-fit, minmax({cardPx}px, 1fr))"
					>
						{#each sortedMonitors as monitor (monitor.id)}
							<WallboardMonitorCard
								{monitor}
								heartbeat={heartbeats.get(monitor.id)}
								heartbeatHistory={heartbeatHistory.get(monitor.id) ?? []}
								conditions={conditionsByMonitor.get(monitor.id) ?? []}
								{conditionNow}
								{cardBody}
								sizePx={cardPx}
							/>
						{/each}
					</div>
				{/if}
			</div>
			{#if sortedMonitors.length > 0}
				<button
					type="button"
					class="absolute right-3 bottom-3 z-10 grid h-8 w-8 place-items-center rounded-md border border-border bg-card text-muted-foreground shadow-sm hover:bg-accent hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
					style="cursor: ew-resize"
					aria-label={m.dashboard_wallboard_card_size()}
					title={m.dashboard_wallboard_card_size_reset()}
					data-testid="wallboard-card-resize"
					onpointerdown={onCardResizeStart}
					ondblclick={resetCardPx}
					onkeydown={onCardSizeKeydown}
				>
					<Grip class="h-3.5 w-3.5 rotate-90" />
				</button>
			{/if}
		</div>
		</div>
	</section>
{/if}
