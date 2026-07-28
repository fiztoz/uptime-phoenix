<!--
  @component
  Linear Y-axis for response-time (ms) charts.
-->
<script lang="ts">
  import { getContext } from "svelte";

  interface Props {
    format?: (d: number) => string;
    ticks?: number | number[] | ((defaultTicks: number[]) => number[]);
    gridlines?: boolean;
  }


  const { xRange, yScale, width } = getContext<any>("LayerCake");

  let {
    format = (d: number) => String(Math.round(d)),
    ticks = 4,
    gridlines = false,
  }: Props = $props();

  const tickVals = $derived.by(() => {
    if (Array.isArray(ticks)) return ticks;
    if (typeof ticks === "function") return ticks($yScale.ticks());
    return $yScale.ticks(ticks);
  });
</script>

<g class="axis y-axis">
  {#each tickVals as tick (tick)}
    {@const tickValPx = $yScale(tick)}
    <g
      class="tick tick-{tick}"
      transform="translate({$xRange[0]}, {tickValPx})"
    >
      {#if gridlines}
        <line class="gridline" x1="0" x2={$width} y1="0" y2="0" />
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
