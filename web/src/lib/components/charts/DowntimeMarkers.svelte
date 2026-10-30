<!--
  @component
  Red shaded regions for down/pending periods on response-time charts.
-->
<script lang="ts">
  import { getLayerCakeContext } from "layercake";
  import type { ScaleTime } from "d3-scale";
  import { uniqueKeys, type DowntimeInterval } from "#lib/utils/chart.js";

  interface Props {
    intervals: DowntimeInterval[];
    fill?: string;
    opacity?: number;
  }

  const k = getLayerCakeContext<{ x: ScaleTime<number, number> }>();

  let {
    intervals,
    fill = "var(--color-danger)",
    opacity = 0.12,
  }: Props = $props();

  // Interval bounds arrive at second precision, so starts can repeat (#131).
  const keys = $derived(uniqueKeys(intervals, (iv) => iv.start.getTime()));
</script>

<g class="downtime-markers" pointer-events="none">
  {#each intervals as interval, i (keys[i])}
    {@const x0 = k.xScale(interval.start)}
    {@const x1 = k.xScale(interval.end)}
    {#if x0 != null && x1 != null}
      {@const rawWidth = Math.abs(x1 - x0)}
      {@const width = Math.max(rawWidth, 8)}
      {@const x = rawWidth < 8 ? (x0 + x1) / 2 - width / 2 : Math.min(x0, x1)}
      <rect
        {x}
        y={0}
        {width}
        height={k.height}
        rx="2"
        {fill}
        fill-opacity={opacity}
      />
    {/if}
  {/each}
</g>
