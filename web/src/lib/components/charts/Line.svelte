<!--
  @component
  Draws a smooth SVG line path using LayerCake context.
  When interactive=true, a wide transparent stroke captures hover only on the line.
-->
<script lang="ts">
	import { getContext } from 'svelte';
	import { linearLine, smoothLine } from '$lib/utils/smooth.js';
	import type { TooltipPoint } from './ChartTooltip.svelte';

	interface Props {
		stroke?: string;
		strokeWidth?: number;
		/** Use straight segments (better for bucketed charts); default smooth for sparklines. */
		linear?: boolean;
		interactive?: boolean;
		hoverPoint?: TooltipPoint | null;
	}


	const { data, xGet, yGet } = getContext<any>('LayerCake');

	let {
		stroke = 'var(--color-success)',
		strokeWidth = 2,
		linear = false,
		interactive = false,
		hoverPoint = $bindable<TooltipPoint | null>(null),
	}: Props = $props();

	const path = $derived.by(() => {
		const pts = $data.map((d: Record<string, unknown>) => [$xGet(d), $yGet(d)] as [number, number]);
		return linear ? linearLine(pts) : smoothLine(pts);
	});

	function findNearestByX(svgX: number): TooltipPoint | null {
		const points = $data as TooltipPoint[];
		if (points.length === 0) return null;

		let nearest = points[0];
		let minDist = Math.abs($xGet(nearest) - svgX);

		for (const point of points) {
			const dist = Math.abs($xGet(point) - svgX);
			if (dist < minDist) {
				minDist = dist;
				nearest = point;
			}
		}
		return nearest;
	}

	function handlePathMove(e: MouseEvent) {
		const svg = (e.currentTarget as SVGPathElement).ownerSVGElement;
		if (!svg) return;

		const ctm = svg.getScreenCTM();
		if (!ctm) return;

		const pt = svg.createSVGPoint();
		pt.x = e.clientX;
		pt.y = e.clientY;
		const loc = pt.matrixTransform(ctm.inverse());
		hoverPoint = findNearestByX(loc.x);
	}

	function handlePathLeave() {
		hoverPoint = null;
	}
</script>

<path
	class="path-line"
	d={path}
	fill="none"
	{stroke}
	stroke-width={strokeWidth}
	stroke-linecap="round"
	stroke-linejoin="round"
	style:pointer-events="none"
/>

{#if interactive}
	<path
		class="path-hit"
		role="presentation"
		aria-hidden="true"
		d={path}
		fill="none"
		stroke="transparent"
		stroke-width={16}
		style="pointer-events: stroke; cursor: crosshair;"
		onmousemove={handlePathMove}
		onmouseleave={handlePathLeave}
	/>
{/if}
