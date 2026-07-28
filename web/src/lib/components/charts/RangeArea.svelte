<!--
  @component
  Filled band between min and max values (Uptime Kuma Min/Max Ping style).
-->
<script lang="ts">
	import { getContext } from 'svelte';
	import { linearLine } from '$lib/utils/smooth.js';

	interface Props {
		/** Accessor key (or function) for the lower bound. Default: min */
		y0?: string;
		/** Accessor key (or function) for the upper bound. Default: max */
		y1?: string;
		fill?: string;
		opacity?: number;
	}


	const { data, xGet, yScale } = getContext<any>('LayerCake');

	let {
		y0 = 'min',
		y1 = 'max',
		fill = 'var(--color-success)',
		opacity = 0.18,
	}: Props = $props();

	function getY(d: Record<string, unknown>, key: string): number {
		return Number(d[key] ?? 0);
	}

	const path = $derived.by(() => {
		const rows = $data as Record<string, unknown>[];
		if (rows.length < 2) return '';

		const upper = rows.map(
			(d) => [$xGet(d) as number, $yScale(getY(d, y1)) as number] as [number, number],
		);
		const lower = rows
			.map(
				(d) => [$xGet(d) as number, $yScale(getY(d, y0)) as number] as [number, number],
			)
			.reverse();

		const top = linearLine(upper);
		// Continue from last upper point along the reversed lower edge, then close.
		let bottom = '';
		for (let i = 0; i < lower.length; i++) {
			bottom += `L${lower[i][0]},${lower[i][1]}`;
		}
		return `${top}${bottom}Z`;
	});
</script>

<path class="range-area" d={path} {fill} fill-opacity={opacity} stroke="none" />
