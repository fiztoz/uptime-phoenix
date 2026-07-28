<!--
  @component
  Sparkline chart — compact latency visualization using LayerCake.
  Shows a line + area chart of heartbeat latency over time.
-->
<script lang="ts">
	import { LayerCake, Svg } from 'layercake';
	import Line from './Line.svelte';
	import Area from './Area.svelte';
	import Points from './Points.svelte';

	interface DataPoint {
		time: number; // timestamp ms
		value: number; // latency ms
		status: string;
	}

	interface Props {
		data: DataPoint[];
		width?: number | string;
		height?: number;
		stroke?: string;
		fill?: string;
	}

	let { data, width = 200, height = 40, stroke, fill }: Props = $props();

	const widthStyle = $derived(typeof width === 'number' ? `${width}px` : width);

	// Compute color from majority status if not overridden.
	const majorityStatus = $derived(() => {
		const counts: Record<string, number> = {};
		data.forEach((d) => {
			counts[d.status] = (counts[d.status] || 0) + 1;
		});
		return Object.entries(counts).sort((a, b) => b[1] - a[1])[0]?.[0] ?? 'up';
	});

	const lineStroke = $derived(
		stroke ?? (majorityStatus() === 'down' ? 'var(--color-danger)' : 'var(--color-success)')
	);
	const areaFill = $derived(
		fill ?? (majorityStatus() === 'down' ? 'var(--color-danger)' : 'var(--color-success)')
	);

	const yDomain = $derived.by((): [number, number] => {
		const values = data.map((point) => point.value);
		const low = Math.min(...values);
		const high = Math.max(...values);
		const spread = high - low;
		const pad = Math.max(2, spread * 0.16, high * 0.06);
		return [Math.max(0, low - pad), high + pad];
	});
</script>

{#if data.length > 0}
	<div
		class="sparkline-container"
		style="width: {widthStyle}; height: {height}px;"
		role="img"
		aria-label="Latency trend with {data.length} samples"
	>
		<LayerCake
			{data}
			x="time"
			y="value"
			{yDomain}
			padding={{ top: 3, right: 2, bottom: 3, left: 2 }}
		>
			<Svg>
				<Area fill={areaFill} opacity={0.7} linear />
				<Line stroke={lineStroke} strokeWidth={2} linear />
				<!-- A path needs 2+ points to paint. Dot a single-sample series so a
				     young monitor's only ping still shows up. -->
				{#if data.length === 1}
					<Points fill={lineStroke} r={2.5} />
				{/if}
			</Svg>
		</LayerCake>
	</div>
{:else}
	<div style="width: {widthStyle}; height: {height}px;" class="flex items-center justify-center text-xs text-muted-foreground">
		No data
	</div>
{/if}

<style>
	.sparkline-container :global(.layercake-container) {
		width: 100% !important;
		height: 100% !important;
	}
</style>
