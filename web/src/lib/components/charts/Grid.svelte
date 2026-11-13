<!--
  @component
  Horizontal grid lines for LayerCake charts.
-->
<script lang="ts">
  import { getLayerCakeContext } from "layercake";
  import type { ScaleLinear } from "d3-scale";
  import { uniqueKeys } from "#lib/utils/chart.js";

  interface Props {
    ticks?: number | number[] | ((defaultTicks: number[]) => number[]);
  }

  const k = getLayerCakeContext<{ y: ScaleLinear<number, number> }>();

  let { ticks = 4 }: Props = $props();

  const tickVals = $derived.by(() => {
    let raw: number[];
    if (Array.isArray(ticks)) raw = ticks;
    else if (typeof ticks === "function") raw = ticks(k.yScale.ticks());
    else raw = k.yScale.ticks(ticks);
    return (raw ?? []).filter((d): d is number => Number.isFinite(d));
  });

  const tickKeys = $derived(uniqueKeys(tickVals, (tick) => tick));
  const x0 = $derived(k.xRange?.[0] ?? 0);
</script>

<g class="grid">
  {#each tickVals as tick, i (tickKeys[i])}
    {@const y = k.yScale(tick)}
    <line
      class="gridline"
      x1={x0}
      x2={x0 + k.width}
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
