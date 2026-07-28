<!--
  @component
  Draws a dot per datum using LayerCake context.

  Exists because a line/area path needs at least two points to render anything:
  a single-sample series produces the path "M x,y", which paints nothing. A young
  monitor whose samples all fall inside one bucket would otherwise show an empty
  plot. Dots make a 1-2 point series visible.
-->
<script lang="ts">
	import { getContext } from 'svelte';

	interface Props {
		fill?: string;
		r?: number;
	}


	const { data, xGet, yGet } = getContext<any>('LayerCake');

	let { fill = 'var(--color-success)', r = 3 }: Props = $props();
</script>

{#each $data as d (`${$xGet(d)}-${$yGet(d)}`)}
	<circle cx={$xGet(d)} cy={$yGet(d)} {r} {fill} style:pointer-events="none" />
{/each}
