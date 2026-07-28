<!--
  @component
  Hit-testing overlay — only activates when the cursor is near the chart line.
  Uses 2D distance to data points (not X-only snapping).
-->
<script lang="ts">
	import { getContext } from 'svelte';
	import type { TooltipPoint } from './ChartTooltip.svelte';

	interface Props {
		hoverPoint?: TooltipPoint | null;
		/** Max pixel distance from a data point to show the tooltip */
		hitRadius?: number;
	}

	let { hoverPoint = $bindable<TooltipPoint | null>(null), hitRadius = 14 }: Props = $props();


	const { data, xGet, yGet } = getContext<any>('LayerCake');

	function findNearest(mouseX: number, _mouseY: number): TooltipPoint | null {
		const points = $data as TooltipPoint[];
		if (points.length === 0) return null;

		let nearest = points[0];
		let minXDist = Math.abs($xGet(nearest) - mouseX);

		for (const point of points) {
			const xDist = Math.abs($xGet(point) - mouseX);
			if (xDist < minXDist) {
				minXDist = xDist;
				nearest = point;
			}
		}

		return nearest;
	}

	function handleMove(e: MouseEvent) {
		const rect = (e.currentTarget as HTMLDivElement).getBoundingClientRect();
		hoverPoint = findNearest(e.clientX - rect.left, e.clientY - rect.top);
	}

	function handleLeave() {
		hoverPoint = null;
	}
</script>

<!-- This pointer-only hit plane has no action; chart values remain available in the surrounding monitor metrics. -->
<!-- svelte-ignore a11y_no_static_element_interactions -->
<div
	class="hover-layer absolute inset-0"
	class:on-line={hoverPoint != null}
	onmousemove={handleMove}
	onmouseleave={handleLeave}
	role="presentation"
></div>

<style>
	.hover-layer {
		cursor: default;
	}

	.hover-layer.on-line {
		cursor: crosshair;
	}
</style>
