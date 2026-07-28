<!--
  @component
  Time axis for LayerCake charts using d3-scale time + d3-time-format.
-->
<script lang="ts">
  import { getContext } from "svelte";
  import { timeFormat } from "d3-time-format";

  interface Props {
    format?: (d: Date) => string;
    ticks?: number | Date[] | ((defaultTicks: Date[]) => Date[]);
    gridlines?: boolean;
    snapLabels?: boolean;
  }


  const { width, height, xScale, yRange } = getContext<any>("LayerCake");

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
    if (Array.isArray(ticks)) return ticks;
    if (typeof ticks === "function") return ticks($xScale.ticks());
    if (typeof ticks === "number") return $xScale.ticks(ticks);
    return $xScale.ticks();
  });
</script>

<g class="axis x-axis" class:snapLabels>
  {#each tickVals as tick, i (tick.getTime())}
    <g
      class="tick tick-{i}"
      transform="translate({$xScale(tick)},{Math.max(...$yRange)})"
    >
      {#if gridlines}
        <line class="gridline" x1="0" x2="0" y1={-$height} y2="0" />
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
