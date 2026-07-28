<!--
  @component
  Compact metric card for monitor detail stats (Uptime Kuma style).
-->
<script lang="ts">
  interface Props {
    label: string;
    value: string | number;
    unit?: string;
    /** Underline accent for the current response metric */
    highlight?: boolean;
  }

  let { label, value, unit, highlight = false }: Props = $props();
</script>

<div class="rounded-xl border border-border bg-card p-4">
  <div class="eyebrow">{label}</div>
  <div
    class="mt-1 text-2xl font-semibold tabular-nums tracking-tight
		{highlight
      ? 'underline decoration-success decoration-2 underline-offset-4'
      : ''}"
  >
    {value}{#if unit}<span
        class="ml-0.5 text-sm font-normal text-muted-foreground">{unit}</span
      >{/if}
  </div>
</div>
