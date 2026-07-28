<!--
  @component
  Draws a smooth SVG area shape with a vertical gradient fill using LayerCake context.
-->
<script lang="ts">
	import { getContext } from 'svelte';
	import { linearLine, smoothLine } from '$lib/utils/smooth.js';

	interface Props {
		fill?: string;
		opacity?: number;
		/** Use straight segments (better for bucketed charts); default smooth for sparklines. */
		linear?: boolean;
	}


	const { data, xGet, yGet, yScale } = getContext<any>('LayerCake');

	let { fill = 'var(--color-success)', opacity = 1, linear = false }: Props = $props();

	const gid = `phx-area-${Math.random().toString(36).slice(2, 8)}`;

	const path = $derived.by(() => {
		const pts = $data.map((d: Record<string, unknown>) => [$xGet(d), $yGet(d)] as [number, number]);
		if (pts.length < 2) return '';
		const top = linear ? linearLine(pts) : smoothLine(pts);
		const baseY = $yScale(0);
		return `${top}L${pts[pts.length - 1][0]},${baseY}L${pts[0][0]},${baseY}Z`;
	});
</script>

<defs>
	<linearGradient id={gid} x1="0" y1="0" x2="0" y2="1">
		<stop offset="0%" stop-color={fill} stop-opacity={0.28 * opacity} />
		<stop offset="100%" stop-color={fill} stop-opacity={0} />
	</linearGradient>
</defs>
<path class="path-area" d={path} fill="url(#{gid})"></path>
