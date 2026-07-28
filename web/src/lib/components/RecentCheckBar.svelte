<!--
  @component
  Recent-check timeline bar (Uptime Kuma style) with per-check hover tooltips.
  Freezes the bar while hovered so live WS updates do not shift segments under the cursor.
-->
<script lang="ts">
	import { timeFormat } from 'd3-time-format';
	import type { Heartbeat } from '$lib/api/heartbeats.js';

	interface Props {
		heartbeats: Heartbeat[];
		maxChecks?: number;
	}

	let { heartbeats, maxChecks = 60 }: Props = $props();

	const formatTime = timeFormat('%Y-%m-%d %H:%M:%S');

	const liveSegments = $derived.by(() => {
		return [...heartbeats]
			.sort((a, b) => new Date(a.time).getTime() - new Date(b.time).getTime())
			.slice(-maxChecks);
	});

	let isHovering = $state(false);
	let frozenSegments = $state<Heartbeat[]>([]);
	let hoveredIndex = $state<number | null>(null);
	let barEl = $state<HTMLDivElement | null>(null);
	let tooltipX = $state(0);

	const segments = $derived(isHovering ? frozenSegments : liveSegments);

	function color(status: Heartbeat['status']): string {
		if (status === 'up') return 'bg-success';
		if (status === 'down') return 'bg-danger';
		if (status === 'pending') return 'bg-warning';
		return 'bg-muted';
	}

	function tooltipLines(hb: Heartbeat): { status: string; ping: string; time: string; message?: string } {
		return {
			status: hb.status.toUpperCase(),
			ping: hb.ping > 0 ? `${hb.ping} ms` : '—',
			time: formatTime(new Date(hb.time)),
			message: hb.message || undefined,
		};
	}

	function handleBarEnter() {
		frozenSegments = liveSegments;
		isHovering = true;
	}

	function handleBarLeave() {
		isHovering = false;
		hoveredIndex = null;
		frozenSegments = [];
	}

	function handleSegmentEnter(index: number, event: MouseEvent | FocusEvent) {
		hoveredIndex = index;
		const target = event.currentTarget as HTMLDivElement;
		const bar = barEl;
		if (!bar) return;
		const barRect = bar.getBoundingClientRect();
		const segRect = target.getBoundingClientRect();
		tooltipX = segRect.left - barRect.left + segRect.width / 2;
	}

	function handleSegmentLeave() {
		hoveredIndex = null;
	}

	function handleSegmentFocus(index: number, event: FocusEvent) {
		if (!isHovering) handleBarEnter();
		handleSegmentEnter(index, event);
	}

	function handleSegmentBlur() {
		queueMicrotask(() => {
			if (!barEl?.contains(document.activeElement)) handleBarLeave();
		});
	}

	const hoveredBeat = $derived(hoveredIndex != null ? segments[hoveredIndex] ?? null : null);
</script>

<div class="rounded-xl border border-border bg-card p-5">
	<h3 class="text-sm font-semibold tracking-tight text-muted-foreground">Recent Checks</h3>

	{#if segments.length === 0}
		<p class="mt-4 text-sm text-muted-foreground">No checks recorded yet.</p>
	{:else}
		<div class="relative mt-3">
			<div
				bind:this={barEl}
				class="flex h-8 w-full items-stretch gap-[2px]"
				role="group"
				aria-label="Recent check timeline"
				data-testid="recent-check-timeline"
				data-hover-paused={isHovering}
				onmouseenter={handleBarEnter}
				onmouseleave={handleBarLeave}
			>
				{#each segments as hb, index (hb.id + hb.time)}
					<button
						type="button"
						class="h-full min-w-0 flex-1 rounded-[2px] {color(hb.status)} opacity-85 outline-none transition-all hover:opacity-100 hover:brightness-110 focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-background {hoveredIndex ===
						index
							? 'ring-1 ring-foreground/40'
							: ''}"
						onmouseenter={(e) => handleSegmentEnter(index, e)}
						onmouseleave={handleSegmentLeave}
						onfocus={(e) => handleSegmentFocus(index, e)}
						onblur={handleSegmentBlur}
						aria-label={`${tooltipLines(hb).status}, ${tooltipLines(hb).ping}, ${tooltipLines(hb).time}`}
					></button>
				{/each}
			</div>

			{#if hoveredBeat}
				{@const lines = tooltipLines(hoveredBeat)}
				<div
					class="pointer-events-none absolute bottom-full z-20 mb-2 -translate-x-1/2 rounded-md border border-border bg-elevated px-2.5 py-1.5 text-xs shadow-lg"
					style:left="{tooltipX}px"
					data-testid="recent-check-tooltip"
				>
					<div class="font-semibold {hoveredBeat.status === 'up'
						? 'text-success'
						: hoveredBeat.status === 'down'
							? 'text-danger'
							: 'text-warning'}">
						{lines.status} · {lines.ping}
					</div>
					<div class="mt-0.5 font-mono text-muted-foreground">{lines.time}</div>
					{#if lines.message}
						<div class="mt-0.5 max-w-[220px] truncate text-muted-foreground">{lines.message}</div>
					{/if}
				</div>
			{/if}
		</div>
		<p class="mt-2 text-xs text-muted-foreground">
			{segments.length} checks · hover for details{isHovering ? ' · live updates paused' : ''}
		</p>
	{/if}
</div>
