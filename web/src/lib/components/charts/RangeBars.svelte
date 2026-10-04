<!--
  @component
  Per-bucket min/max whiskers. Unlike a connected range polygon, these do not
  imply values between sparse samples and stay truthful for young monitors.
-->
<script lang="ts">
  import { getLayerCakeContext } from "layercake";

  interface Props {
    y0?: string;
    y1?: string;
    stroke?: string;
    strokeWidth?: number;
    opacity?: number;
  }

  const k = getLayerCakeContext();

  let {
    y0 = "min",
    y1 = "max",
    stroke = "var(--color-success)",
    strokeWidth = 3,
    opacity = 0.24,
  }: Props = $props();
</script>

<g class="range-bars" pointer-events="none">
  {#each k.data as point (`${k.xGet(point)}-${point[y0]}-${point[y1]}`)}
    <line
      x1={k.xGet(point)}
      x2={k.xGet(point)}
      y1={k.yScale(Number(point[y0] ?? 0))}
      y2={k.yScale(Number(point[y1] ?? 0))}
      {stroke}
      stroke-width={strokeWidth}
      stroke-opacity={opacity}
      stroke-linecap="round"
    />
  {/each}
</g>
