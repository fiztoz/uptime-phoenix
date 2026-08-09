<!--
  @component
  Full-size response time chart (Uptime Kuma style):
  min/max whiskers, average line, range selector, downtime markers, hover tooltip.
  Chart buckets and downtime intervals come from the Go chart_aggregate API.
-->
<script lang="ts">
	import { LayerCake, Svg, Html } from 'layercake';
	import { scaleTime, scaleLinear } from 'd3-scale';
	import { timeFormat } from 'd3-time-format';
	import { chartTimeDomain, type DowntimeInterval } from '$lib/utils/chart.js';
	import Line from './charts/Line.svelte';
	import Area from './charts/Area.svelte';
	import RangeBars from './charts/RangeBars.svelte';
	import AxisX from './charts/AxisX.svelte';
	import AxisY from './charts/AxisY.svelte';
	import Grid from './charts/Grid.svelte';
	import Points from './charts/Points.svelte';
	import DowntimeMarkers from './charts/DowntimeMarkers.svelte';
	import ChartHoverLayer from './charts/ChartHoverLayer.svelte';
	import ChartTooltip, {
		type TooltipPoint,
	} from './charts/ChartTooltip.svelte';
	import PlotClip from './charts/PlotClip.svelte';
	import Select from '$lib/components/Select.svelte';
	import * as m from '$lib/paraglide/messages.js';

	export interface ChartBucket {
		time: string;
		min: number;
		avg: number;
		max: number;
	}

	export interface ChartPayload {
		buckets: ChartBucket[];
		downtime_intervals: Array<{ start: string; end: string }>;
	}

	interface Props {
		chart: ChartPayload | null;
		selectedHours?: number;
		onRangeChange?: (hours: number) => void;
		loading?: boolean;
		/**
		 * When false, the range dropdown is hidden and the chart is a fixed window
		 * (selectedHours). Public status pages use this: backend embeds a fixed 24h
		 * series with no range API.
		 */
		showRangeSelector?: boolean;
	}

	const RANGE_OPTIONS = [
		{ label: '1h', hours: 1 },
		{ label: '3h', hours: 3 },
		{ label: '6h', hours: 6 },
		{ label: '24h', hours: 24 },
		{ label: '1w', hours: 168 },
	] as const;

	let {
		chart,
		selectedHours = 24,
		onRangeChange,
		loading = false,
		showRangeSelector = true,
	}: Props = $props();

	let hoverPoint = $state<TooltipPoint | null>(null);

	const chartData = $derived.by(() => {
		if (!chart?.buckets?.length) return [];
		return chart.buckets
			.map((b) => ({
				time: new Date(b.time),
				value: Math.round(b.avg),
				min: Math.round(b.min),
				max: Math.round(b.max),
			}))
			.filter((point) => Number.isFinite(point.time.getTime()))
			.sort((a, b) => a.time.getTime() - b.time.getTime());
	});

	const downtimeIntervals = $derived.by((): DowntimeInterval[] => {
		if (!chart?.downtime_intervals?.length) return [];
		return chart.downtime_intervals.map((iv) => ({
			start: new Date(iv.start),
			end: new Date(iv.end),
		}));
	});

	// The range control is a promise: "24h" always means a 24-hour axis. A young
	// monitor therefore occupies only the recent part of the plot instead of
	// making a few minutes look like a full day.
	const xDomain = $derived(chartTimeDomain(selectedHours));

	/** Y domain from max ping (not just avg) so min/max whiskers stay in-bounds. */
	const yDomain = $derived.by((): [number, number] => {
		if (chartData.length === 0) return [0, 100];
		let peak = 0;
		for (const d of chartData) {
			peak = Math.max(peak, d.max, d.value, d.min);
		}
		// Headroom so stroke/markers never clip the top.
		const top = peak <= 0 ? 100 : peak * 1.12;
		return [0, top];
	});

	const xTickFormat = $derived.by(() => {
		if (selectedHours <= 6) return timeFormat('%H:%M');
		if (selectedHours <= 24) return timeFormat('%d %b · %H:%M');
		return timeFormat('%d %b');
	});

	const selectedRange = $derived(
		RANGE_OPTIONS.find((option) => option.hours === selectedHours)?.label ?? `${selectedHours}h`,
	);
</script>

<section class="overflow-hidden rounded-xl border border-border bg-card" aria-busy={loading}>
	<div class="flex flex-wrap items-start justify-between gap-4 border-b border-border/80 px-4 py-4 sm:px-5">
		<div>
			<div class="flex flex-wrap items-center gap-x-4 gap-y-2">
				<h3 class="text-sm font-semibold tracking-tight">
					{m.monitor_detail_chart_title()}
				</h3>
				<p class="text-xs tabular-nums text-muted-foreground">
					{m.monitor_detail_chart_intervals({
						count: chartData.length,
						range: selectedRange,
					})}
				</p>
			</div>
			{#if chartData.length > 1}
				<div class="mt-2 flex items-center gap-4 text-[11px] text-muted-foreground">
					<span class="inline-flex items-center gap-1.5">
						<span class="inline-block h-2.5 w-[3px] rounded-full bg-success/35"></span>
						{m.monitor_detail_chart_range()}
					</span>
					<span class="inline-flex items-center gap-1.5">
						<span class="inline-block h-0.5 w-3 rounded-full bg-success"></span>
						{m.monitor_detail_chart_average()}
					</span>
				</div>
			{/if}
		</div>
		{#if loading || showRangeSelector}
			<div class="flex items-center gap-2">
				{#if loading}
					<span class="text-[11px] text-muted-foreground">
						{m.monitor_detail_chart_updating()}
					</span>
				{/if}
				{#if showRangeSelector}
					<div class="w-24">
						<Select
							options={RANGE_OPTIONS.map((opt) => ({
								value: String(opt.hours),
								label: opt.label,
							}))}
							value={String(selectedHours)}
							onValueChange={(v) => onRangeChange?.(Number(v))}
							ariaLabel="Chart time range"
							size="sm"
							class="w-full"
						/>
					</div>
				{/if}
			</div>
		{/if}
	</div>

	{#if chartData.length > 0}
		<div
			class="chart-container mx-2 my-3 transition-opacity duration-200 sm:mx-4"
			class:opacity-50={loading}
			data-testid="response-chart"
		>
			<LayerCake
				data={chartData}
				x="time"
				y="value"
				xScale={scaleTime()}
				yScale={scaleLinear()}
				xDomain={xDomain as unknown as [number, number]}
				yDomain={yDomain}
				yNice
				padding={{ top: 16, right: 16, bottom: 32, left: 48 }}
			>
				<!-- titleText is LayerCake v9+ a11y API (was `title` on Svg). -->
				<Svg titleText="Response time chart">
					<Grid ticks={5} />
					<PlotClip>
						<DowntimeMarkers intervals={downtimeIntervals} />
						<RangeBars y0="min" y1="max" stroke="var(--color-success)" />
						<Area fill="var(--color-success)" opacity={0.5} linear />
						<Line stroke="var(--color-success)" strokeWidth={2} linear />
						<!-- A path needs 2+ points to paint. Dot a sparse series so a
						     monitor whose samples all land in one bucket still shows up. -->
						{#if chartData.length <= 2}
							<Points fill="var(--color-success)" r={3.5} />
						{/if}
					</PlotClip>
					<AxisX format={xTickFormat} ticks={4} snapLabels />
					<AxisY ticks={5} />
				</Svg>
				<Html>
					<ChartHoverLayer bind:hoverPoint hitRadius={24} />
					<ChartTooltip point={hoverPoint} />
				</Html>
			</LayerCake>
		</div>
	{:else if downtimeIntervals.length > 0}
		<div
			class="mt-3 rounded-lg border border-danger/20 bg-danger/5 px-4 py-6 text-center text-sm"
			data-testid="downtime-markers"
		>
			<span class="text-danger">
				{downtimeIntervals.length === 1
					? m.monitor_detail_chart_downtime_one()
					: m.monitor_detail_chart_downtime_many({ count: downtimeIntervals.length })}
			</span>
			<p class="mt-1 text-muted-foreground">
				{m.monitor_detail_chart_insufficient()}
			</p>
		</div>
	{:else}
		<p class="mt-6 py-16 text-center text-sm text-muted-foreground">
			{m.monitor_detail_chart_no_data()}
		</p>
	{/if}
</section>

<style>
	.chart-container {
		position: relative;
		width: 100%;
		height: 280px;
		overflow: hidden;
		border-radius: 0.5rem;
	}

	.chart-container :global(.layercake-container) {
		position: absolute !important;
		inset: 0;
		width: 100% !important;
		height: 100% !important;
		overflow: hidden;
	}

	.chart-container :global(svg) {
		overflow: hidden;
		display: block;
	}
</style>
