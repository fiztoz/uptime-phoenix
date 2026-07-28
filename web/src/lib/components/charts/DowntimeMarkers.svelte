<!--
  @component
  Red shaded regions for down/pending periods on response-time charts.
-->
<script lang="ts">
	import { getContext } from 'svelte';
	import type { DowntimeInterval } from '$lib/utils/chart.js';

	interface Props {
		intervals: DowntimeInterval[];
	}


	const { xScale, height } = getContext<any>('LayerCake');

	let { intervals }: Props = $props();
</script>

<g class="downtime-markers" pointer-events="none">
	{#each intervals as interval (interval.start.getTime())}
		{@const x0 = $xScale(interval.start)}
		{@const x1 = $xScale(interval.end)}
		{#if x0 != null && x1 != null}
			{@const rawWidth = Math.abs(x1 - x0)}
			{@const width = Math.max(rawWidth, 8)}
			{@const x = rawWidth < 8 ? (x0 + x1) / 2 - width / 2 : Math.min(x0, x1)}
			<rect
				{x}
				y={0}
				{width}
				height={$height}
				rx="2"
				fill="var(--color-danger)"
				fill-opacity="0.12"
			/>
		{/if}
	{/each}
</g>
