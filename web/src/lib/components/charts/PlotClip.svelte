<!--
  @component
  Clips child SVG geometry to the LayerCake plot area so lines/areas cannot
  paint outside the chart bounds.
-->
<script lang="ts">
  import type { Snippet } from "svelte";
  import { getLayerCakeContext } from "layercake";

  interface Props {
    children?: Snippet;
  }

  const k = getLayerCakeContext();

  let { children }: Props = $props();

  const clipId = `phx-plot-clip-${Math.random().toString(36).slice(2, 9)}`;
</script>

<defs>
  <clipPath id={clipId}>
    <rect x="0" y="0" width={k.width} height={k.height} />
  </clipPath>
</defs>
<g clip-path="url(#{clipId})">
  {#if children}
    {@render children()}
  {/if}
</g>
