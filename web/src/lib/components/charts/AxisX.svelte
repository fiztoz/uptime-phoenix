<!--
  @component
  Time axis for LayerCake charts using d3-scale time + d3-time-format.
-->
<script lang="ts">
  import { getLayerCakeContext } from "layercake";
  import type { ScaleTime } from "d3-scale";
  import { timeFormat } from "d3-time-format";
  import { uniqueKeys } from "#lib/utils/chart.js";

  interface Props {
    format?: (d: Date) => string;
    ticks?: number | Date[] | ((defaultTicks: Date[]) => Date[]);
    gridlines?: boolean;
    snapLabels?: boolean;
  }

  const k = getLayerCakeContext<{ x: ScaleTime<number, number> }>();

  let {
    format = timeFormat("%H:%M"),
    ticks = undefined,
    gridlines = false,
    snapLabels = true,
  }: Props = $props();

  function textAnchor(i: number, sl: boolean) {
    if (sl) {
      if (i === 0) return "start";
      if (i === tickVals.length - 1) return "end";
    }
    return "middle";
  }

  const tickVals = $derived.by(() => {
    let raw: Date[];
    if (Array.isArray(ticks)) raw = ticks;
    else if (typeof ticks === "function") raw = ticks(k.xScale.ticks());
    else if (typeof ticks === "number") raw = k.xScale.ticks(ticks);
    else raw = k.xScale.ticks();
    return (raw ?? []).filter(
      (d): d is Date => d instanceof Date && Number.isFinite(d.getTime()),
    );
  });

  const tickKeys = $derived(uniqueKeys(tickVals, (tick) => tick.getTime()));
  const yMax = $derived(
    Array.isArray(k.yRange) && k.yRange.length > 0 ? Math.max(...k.yRange) : 0,
  );
</script>

<g class="axis x-axis" class:snapLabels>
  {#each tickVals as tick, i (tickKeys[i])}
    <g
      class="tick tick-{i}"
      transform="translate({k.xScale(tick)},{yMax})"
    >
      {#if gridlines}
        <line class="gridline" x1="0" x2="0" y1={-k.height} y2="0" />
      {/if}
      <text x="0" y="0" dy="14" text-anchor={textAnchor(i, snapLabels)}
        >{format(tick)}</text
      >
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
