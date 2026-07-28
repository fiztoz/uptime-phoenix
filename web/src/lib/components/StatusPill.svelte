<script lang="ts">
  import type { Monitor } from "$lib/stores/ws.svelte.js";

  interface Props {
    status: Monitor["status"];
  }

  let { status }: Props = $props();

  const statusConfig: Record<
    Monitor["status"],
    { label: string; dotClass: string; bgClass: string }
  > = {
    up: {
      label: "Operational",
      dotClass: "dot-up",
      bgClass: "bg-success/10 text-success border-success/25",
    },
    down: {
      label: "Down",
      dotClass: "dot-down",
      bgClass: "bg-danger/10 text-danger border-danger/25",
    },
    pending: {
      label: "Pending",
      dotClass: "dot-warn",
      bgClass: "bg-warning/10 text-warning border-warning/25",
    },
    maintenance: {
      label: "Maintenance",
      dotClass: "dot-info",
      bgClass: "bg-info/10 text-info border-info/25",
    },
    paused: {
      label: "Paused",
      dotClass: "dot-muted",
      bgClass: "bg-muted/40 text-muted-foreground border-border",
    },
  };

  let config = $derived(statusConfig[status] ?? statusConfig.pending);
</script>

<span
  class="inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium {config.bgClass}"
>
  <span class="dot {config.dotClass}"></span>
  {config.label}
</span>
