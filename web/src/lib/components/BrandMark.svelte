<!--
  @component
  Phoenix mascot mark. The vector mascot (traced from the confirmed artwork,
  background removed) is the canonical asset — crisp at all sizes, no raster blur.
  The viewBox is cropped to the mascot's visible bounds (source canvas has large
  transparent margins) so the `size` prop scales the actual artwork, not padding.
-->
<script lang="ts">
  interface Props {
    size?: number;
    /** Render the confirmed mascot alpha silhouette in currentColor. */
    mono?: boolean;
    class?: string;
  }

  let { size = 48, mono = false, class: klass = "" }: Props = $props();
  const maskId = `phx-mascot-${Math.random().toString(36).slice(2, 8)}`;
</script>

<svg
  width={size}
  height={size}
  viewBox="16.54 6.12 109.52 109.52"
  class={klass}
  role="img"
  aria-label="Phoenix"
>
  {#if mono}
    <defs>
      <mask
        id={maskId}
        maskUnits="userSpaceOnUse"
        x="0"
        y="0"
        width="128"
        height="128"
      >
        <image href="/brand/phoenix-mascot.svg" width="128" height="128" />
      </mask>
    </defs>
    <rect width="128" height="128" fill="currentColor" mask={`url(#${maskId})`} />
  {:else}
    <image href="/brand/phoenix-mascot.svg" width="128" height="128" />
  {/if}
</svg>
