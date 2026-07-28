<script lang="ts">
	import ResponseTimeChart from '$lib/components/ResponseTimeChart.svelte';
	import StatusPill from '$lib/components/StatusPill.svelte';
	import UptimeBar from '$lib/components/UptimeBar.svelte';
	import type {
		PublicStatusResponse,
		StatusPageDashboardStyle,
	} from '$lib/api/statuspages';

	type PublicMonitor = PublicStatusResponse['monitors'][number];

	interface Props {
		monitors: PublicMonitor[];
		density: StatusPageDashboardStyle;
	}

	interface DensityLayout {
		section: string;
		heading: string;
		count: string;
		empty: string;
		list: string;
		item: string;
		summary: string;
		identity: string;
		name: string;
		status: string;
		uptime: string;
		showType: boolean;
		showUptime: boolean;
		showChart: boolean;
	}

	// Density is presentation data: every mode uses the same section, loop,
	// status, uptime, and chart slots. A new density adds one token entry rather
	// than another copy of the dashboard shell.
	const DENSITIES: Record<StatusPageDashboardStyle, DensityLayout> = {
		full: {
			section: 'space-y-4',
			heading: 'text-xl font-semibold',
			count: 'text-xs text-muted-foreground',
			empty: 'text-sm text-muted-foreground',
			list: 'space-y-4',
			item: 'rounded-xl border border-border bg-card p-5 transition-colors hover:border-border/70',
			summary:
				'flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between',
			identity: 'min-w-0',
			name: 'text-lg font-medium',
			status: 'mt-1 sm:mt-0',
			uptime: 'mt-4',
			showType: true,
			showUptime: true,
			showChart: true,
		},
		grid: {
			section: 'space-y-4',
			heading: 'text-xl font-semibold',
			count: 'text-xs text-muted-foreground',
			empty: 'text-sm text-muted-foreground',
			list: 'grid min-w-0 grid-cols-1 gap-4 sm:grid-cols-2',
			item: 'min-w-0 rounded-xl border border-border bg-card p-4 transition-colors hover:border-border/70',
			summary: 'flex items-start justify-between gap-3',
			identity: 'min-w-0',
			name: 'truncate font-medium',
			status: 'shrink-0',
			uptime: 'mt-4',
			showType: true,
			showUptime: true,
			showChart: false,
		},
		pills: {
			section:
				'rounded-2xl border border-border bg-card/90 p-4 shadow-sm sm:p-5',
			heading: 'text-sm font-semibold tracking-tight',
			count: 'mt-0.5 text-xs text-muted-foreground',
			empty: 'mt-4 text-sm text-muted-foreground',
			list: 'mt-4 flex flex-wrap gap-2',
			item: 'inline-flex max-w-full items-center rounded-full border border-border bg-surface px-3 py-2 text-sm shadow-sm',
			summary: 'flex min-w-0 items-center gap-2',
			identity: 'min-w-0',
			name: 'truncate font-medium',
			status: '',
			uptime: '',
			showType: false,
			showUptime: false,
			showChart: false,
		},
	};

	let { monitors, density }: Props = $props();
	let layout = $derived(DENSITIES[density]);

	function uptimeLabel(value: number | null | undefined): string {
		return typeof value === 'number' && Number.isFinite(value)
			? `${value.toFixed(1)}%`
			: '—';
	}
</script>

<section class={layout.section} aria-labelledby="services-heading">
	<div class="flex items-center justify-between">
		<div>
			<h2 id="services-heading" class={layout.heading}>Services</h2>
			{#if density === 'pills'}
				<p class={layout.count}>
					{monitors.length} monitored service{monitors.length === 1 ? '' : 's'}
				</p>
			{/if}
		</div>
		{#if density !== 'pills'}
			<span class={layout.count}>
				{monitors.length} service{monitors.length === 1 ? '' : 's'}
			</span>
		{/if}
	</div>

	{#if monitors.length === 0}
		<p class={layout.empty}>No services are assigned to this status page.</p>
	{:else}
		<div class={layout.list}>
			{#each monitors as monitor (monitor.id)}
				<article
					class={layout.item}
					title={`${monitor.name} · ${monitor.type}`}
				>
					<div class={layout.summary}>
						<div class={layout.identity}>
							<div class={layout.name}>{monitor.name}</div>
							{#if layout.showType}
								<div class="text-xs text-muted-foreground">{monitor.type}</div>
							{/if}
							{#if layout.showType && monitor.cert_expiry_date}
								<div class="mt-1 text-xs text-muted-foreground">
									TLS expires {new Date(monitor.cert_expiry_date).toLocaleDateString()}
									{#if typeof monitor.cert_days_left === 'number'}
										({monitor.cert_days_left} day{monitor.cert_days_left === 1
											? ''
											: 's'} left)
									{/if}
								</div>
							{/if}
						</div>
						<div class={layout.status}>
							<StatusPill status={monitor.status} />
						</div>
					</div>

					{#if layout.showUptime}
						<div class={layout.uptime}>
							<div class="mb-1 flex justify-between text-xs">
								<span class="text-muted-foreground">90-day uptime</span>
								<span class="font-mono"
									>{uptimeLabel(monitor.uptime_percent)}</span
								>
							</div>
							<UptimeBar data={monitor.uptime_data ?? []} />
						</div>
					{/if}

					{#if layout.showChart && monitor.chart?.buckets?.length}
						<div class="mt-4 -mx-1 overflow-x-auto sm:mx-0">
							<!-- Public payload is a fixed 24h series; hide the range control. -->
							<ResponseTimeChart
								chart={monitor.chart}
								selectedHours={24}
								showRangeSelector={false}
							/>
						</div>
					{/if}
				</article>
			{/each}
		</div>
	{/if}
</section>
