<!--
  @component
  Hit-testing overlay — activates when cursor is within `hitRadius` pixels of a chart data point.
  Uses 2D Euclidean distance to data points (`Math.hypot`) with fallback to X-distance when mouseY is omitted.
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

	function findNearest(
		mouseX: number,
		mouseY: number | undefined = undefined,
	): TooltipPoint | null {
		const points = $data as TooltipPoint[];
		if (!points || points.length === 0) return null;

		let nearest: TooltipPoint | null = null;
		let minDist = Infinity;

		const check2D = typeof mouseY === 'number' && Number.isFinite(mouseY);

		for (const point of points) {
			const px = $xGet(point);
			const py = $yGet(point);
			const dist = check2D
				? Math.hypot(px - mouseX, py - mouseY)
				: Math.abs(px - mouseX);

			if (dist < minDist) {
				minDist = dist;
				nearest = point;
			}
		}

		if (hitRadius > 0 && minDist > hitRadius) {
			return null;
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
