<!--
  @component
  Linear Y-axis for response-time (ms) charts.
-->
<script lang="ts">
  import { getLayerCakeContext } from "layercake";
  import type { ScaleLinear } from "d3-scale";
  import { uniqueKeys } from "#lib/utils/chart.js";

  interface Props {
    format?: (d: number) => string;
    ticks?: number | number[] | ((defaultTicks: number[]) => number[]);
    gridlines?: boolean;
  }

  const k = getLayerCakeContext<{ y: ScaleLinear<number, number> }>();

  let {
    format = (d: number) => String(Math.round(d)),
    ticks = 4,
    gridlines = false,
  }: Props = $props();

  const tickVals = $derived.by(() => {
    let raw: number[];
    if (Array.isArray(ticks)) raw = ticks;
    else if (typeof ticks === "function") raw = ticks(k.yScale.ticks());
    else raw = k.yScale.ticks(ticks);
    return (raw ?? []).filter((d): d is number => Number.isFinite(d));
  });

  const tickKeys = $derived(uniqueKeys(tickVals, (tick) => tick));
</script>

<g class="axis y-axis">
  {#each tickVals as tick, i (tickKeys[i])}
    {@const tickValPx = k.yScale(tick)}
    <g
      class="tick tick-{tick}"
      transform="translate({k.xRange?.[0] ?? 0}, {tickValPx})"
    >
      {#if gridlines}
        <line class="gridline" x1="0" x2={k.width} y1="0" y2="0" />
      {/if}
      <text x="-6" y="0" dy="4" text-anchor="end">{format(tick)}</text>
    </g>
  {/each}
</g>

<style>
  .tick {
    font-size: 10px;
  }

  .tick text {
    fill: var(--color-muted-foreground);
  }

  .tick .gridline {
    stroke: var(--color-border);
    stroke-dasharray: 2;
  }
</style>
