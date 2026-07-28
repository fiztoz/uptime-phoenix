<!--
  @component
  Per-bucket min/max whiskers. Unlike a connected range polygon, these do not
  imply values between sparse samples and stay truthful for young monitors.
-->
<script lang="ts">
	import { getContext } from 'svelte';

	interface Props {
		y0?: string;
		y1?: string;
		stroke?: string;
		strokeWidth?: number;
		opacity?: number;
	}


	const { data, xGet, yScale } = getContext<any>('LayerCake');

	let {
		y0 = 'min',
		y1 = 'max',
		stroke = 'var(--color-success)',
		strokeWidth = 3,
		opacity = 0.24,
	}: Props = $props();
</script>

<g class="range-bars" pointer-events="none">
	{#each $data as point (`${$xGet(point)}-${point[y0]}-${point[y1]}`)}
		<line
			x1={$xGet(point)}
			x2={$xGet(point)}
			y1={$yScale(Number(point[y0] ?? 0))}
			y2={$yScale(Number(point[y1] ?? 0))}
			{stroke}
			stroke-width={strokeWidth}
			stroke-opacity={opacity}
			stroke-linecap="round"
		/>
	{/each}
</g>
