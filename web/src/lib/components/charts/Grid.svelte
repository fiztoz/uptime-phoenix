<!--
  @component
  Horizontal grid lines for LayerCake charts.
-->
<script lang="ts">
  import { getLayerCakeContext } from "layercake";
  import type { ScaleLinear } from "d3-scale";

  interface Props {
    ticks?: number | number[] | ((defaultTicks: number[]) => number[]);
  }

  const k = getLayerCakeContext<{ y: ScaleLinear<number, number> }>();

  let { ticks = 4 }: Props = $props();

  const tickVals = $derived.by(() => {
    if (Array.isArray(ticks)) return ticks;
    if (typeof ticks === "function") return ticks(k.yScale.ticks());
    return k.yScale.ticks(ticks);
  });
</script>

<g class="grid">
  {#each tickVals as tick (tick)}
    {@const y = k.yScale(tick)}
    <line
      class="gridline"
      x1={k.xRange[0]}
      x2={k.xRange[0] + k.width}
      y1={y}
      y2={y}
    />
  {/each}
</g>

<style>
  .gridline {
    stroke: var(--color-border);
    stroke-dasharray: 2;
    opacity: 0.7;
  }
</style>
