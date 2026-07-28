<!--
  @component
  Phoenix wordmark — hand-drawn rounded letterforms (SVG paths, no font
  dependency) that echo the mascot: plump round-capped strokes filled with
  the mascot's body gradient, and its golden three-feather crest above the I.

  Geometry: cap height 100 (y 0→100), stroke 27 with round caps/joins,
  stroke centerlines inset 13.5 so caps kiss the cap/base lines exactly.
  Letter advances P63 H72 O91 E56 N72 I27 X70, gap 22 → word ends x=583.
  Gradients are userSpaceOnUse: bbox gradients vanish on zero-width paths
  like the I stem.

  Props:
    size   — rendered height in px (width scales via viewBox aspect)
    mono   — render in currentColor (no gradient, no shadow)
    class  — pass-through CSS class
-->
<script lang="ts">
  interface Props {
    size?: number;
    mono?: boolean;
    class?: string;
  }

  let { size = 28, mono = false, class: klass = "" }: Props = $props();

  const uid = Math.random().toString(36).slice(2, 8);
  const bodyId = `pw-body-${uid}`;
  const crestId = `pw-crest-${uid}`;
  const shadowId = `pw-shadow-${uid}`;

  // Tight bounds: crest tip y=-33, baseline 100 + soft shadow spill.
  const vx = -8;
  const vy = -36;
  const vw = 600;
  const vh = 148;

  const width = $derived(Math.round((size * vw) / vh));
</script>

<svg
  {width}
  height={size}
  viewBox="{vx} {vy} {vw} {vh}"
  class={klass}
  role="img"
  aria-label="Phoenix"
>
  {#if !mono}
    <defs>
      <!-- Body gradient: amber highlight → mascot body orange → deep ember -->
      <linearGradient id={bodyId} gradientUnits="userSpaceOnUse" x1="0" y1="0" x2="0" y2="100">
        <stop offset="0%" stop-color="#ffa03a" />
        <stop offset="52%" stop-color="#fd6a26" />
        <stop offset="100%" stop-color="#f2491c" />
      </linearGradient>
      <!-- Crest gradient: golden, like the mascot's head feathers -->
      <linearGradient id={crestId} gradientUnits="userSpaceOnUse" x1="0" y1="-33" x2="0" y2="-3">
        <stop offset="0%" stop-color="#ffc14d" />
        <stop offset="100%" stop-color="#fc9d2c" />
      </linearGradient>
      <filter id={shadowId} x="-6%" y="-14%" width="112%" height="132%">
        <feDropShadow dx="0" dy="3.5" stdDeviation="5" flood-color="#2a0f08" flood-opacity="0.45" />
      </filter>
    </defs>
  {/if}

  <!-- Letterforms -->
  <g
    fill="none"
    stroke={mono ? "currentColor" : `url(#${bodyId})`}
    filter={mono ? undefined : `url(#${shadowId})`}
    stroke-width="27"
    stroke-linecap="round"
    stroke-linejoin="round"
  >
    <!-- P -->
    <path d="M 13.5,86.5 V 13.5 H 29 A 19.75 19.75 0 1 1 29 53 H 15" />
    <!-- H -->
    <path transform="translate(85,0)" d="M 13.5,13.5 V 86.5 M 58.5,13.5 V 86.5 M 13.5,52 H 58.5" />
    <!-- O -->
    <ellipse transform="translate(179,0)" cx="45.5" cy="50" rx="32" ry="36.5" />
    <!-- E -->
    <path transform="translate(292,0)" d="M 13.5,13.5 V 86.5 M 13.5,13.5 H 42.5 M 13.5,51 H 38 M 13.5,86.5 H 42.5" />
    <!-- N -->
    <path transform="translate(370,0)" d="M 13.5,86.5 V 13.5 L 58.5,86.5 V 13.5" />
    <!-- I -->
    <path transform="translate(464,0)" d="M 13.5,13.5 V 86.5" />
    <!-- X -->
    <path transform="translate(513,0)" d="M 13.5,13.5 L 56.5,86.5 M 56.5,13.5 L 13.5,86.5" />
  </g>

  <!-- Three-feather crest above the I, mirroring the mascot's head -->
  <g
    fill={mono ? "currentColor" : `url(#${crestId})`}
    filter={mono ? undefined : `url(#${shadowId})`}
    transform="translate(464,0)"
  >
    <path d="M 13.5,-4 C 8,-10 7,-20 11,-30 C 13,-33 16,-33 17,-29 C 19,-19 18,-10 13.5,-4 Z" />
    <path d="M 4,-3 C -1,-6 -4,-12 -4,-19 C -4,-22 -1.5,-23 0.5,-20 C 4,-15 5.5,-9 4,-3 Z" />
    <path d="M 23,-3 C 28,-6 31,-12 31,-19 C 31,-22 28.5,-23 26.5,-20 C 23,-15 21.5,-9 23,-3 Z" />
  </g>
</svg>
