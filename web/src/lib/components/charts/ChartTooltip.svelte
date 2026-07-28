<!--
  @component
  Hover tooltip for LayerCake charts — datetime + avg (and min/max when present).
-->
<script lang="ts">
	import { getContext } from 'svelte';
	import { timeFormat } from 'd3-time-format';

	export interface TooltipPoint {
		time: Date;
		value: number;
		min?: number;
		max?: number;
	}

	interface Props {
		point: TooltipPoint | null;
	}


	const { xGet, yGet, width, padding } = getContext<any>('LayerCake');

	let { point }: Props = $props();

	const formatDateTime = timeFormat('%Y-%m-%d %H:%M:%S');

	const left = $derived(point ? ($xGet(point) as number) : 0);
	const top = $derived(point ? ($yGet(point) as number) : 0);

	// Flip tooltip to the left near the right edge so it stays in-bounds.
	const flipLeft = $derived.by(() => {
		if (!point) return false;
		const plotRight = ($padding?.left ?? 0) + ($width ?? 0);
		return left > plotRight - 120;
	});
</script>

{#if point}
	<!-- Marker dot on the line -->
	<div
		class="pointer-events-none absolute z-10 h-2.5 w-2.5 -translate-x-1/2 -translate-y-1/2 rounded-full border-2 border-background bg-success"
		style:left="{left}px"
		style:top="{top}px"
	></div>
	<div
		class="chart-tooltip pointer-events-none absolute z-10 -translate-y-full rounded-md border border-border bg-elevated px-2.5 py-1.5 text-xs shadow-lg
			{flipLeft ? '-translate-x-full' : '-translate-x-1/2'}"
		style:left="{left}px"
		style:top="{top - 12}px"
	>
		<div class="font-mono text-muted-foreground">
			{formatDateTime(point.time)}
		</div>
		<div class="mt-0.5 font-semibold tabular-nums text-success">
			Avg {Math.round(point.value)} ms
		</div>
		{#if point.min != null && point.max != null && (point.min !== point.value || point.max !== point.value)}
			<div class="mt-0.5 tabular-nums text-muted-foreground">
				Min {Math.round(point.min)} · Max {Math.round(point.max)} ms
			</div>
		{/if}
	</div>
{/if}
