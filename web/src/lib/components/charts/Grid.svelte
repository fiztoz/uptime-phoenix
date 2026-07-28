<!--
  @component
  Horizontal grid lines for LayerCake charts.
-->
<script lang="ts">
  import { getContext } from "svelte";

  interface Props {
    ticks?: number | number[] | ((defaultTicks: number[]) => number[]);
  }


  const { xRange, yScale, width } = getContext<any>("LayerCake");

  let { ticks = 4 }: Props = $props();

  const tickVals = $derived.by(() => {
    if (Array.isArray(ticks)) return ticks;
    if (typeof ticks === "function") return ticks($yScale.ticks());
    return $yScale.ticks(ticks);
  });
</script>

<g class="grid">
  {#each tickVals as tick (tick)}
    {@const y = $yScale(tick)}
    <line
      class="gridline"
      x1={$xRange[0]}
      x2={$xRange[0] + $width}
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
