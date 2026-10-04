<script lang="ts">
  import * as m from "$lib/paraglide/messages.js";
  let { status }: { status: string | null | undefined } = $props();
  const labels = $derived<Record<string, string>>({
    firing: m.probes_status_firing(),
    acked: m.probes_status_acked(),
    resolved: m.probes_status_resolved(),
    never_connected: m.probes_status_never_connected(),
    disconnected: m.probes_status_disconnected(),
    degraded: m.probes_status_degraded(),
    online: m.probes_status_online(),
    offline: m.probes_status_offline(),
    suspect: m.probes_status_suspect(),
    ready: m.probes_status_ready(),
    paused: m.probes_status_paused(),
    unconfigured: m.probes_status_unconfigured(),
    unknown: m.probes_status_unknown(),
    pending: m.probes_status_pending(),
    applied: m.probes_status_applied(),
    rejected: m.probes_status_rejected(),
    enrolled: m.probes_status_enrolled(),
    active: m.probes_status_active(),
    revoked: m.probes_status_revoked(),
    succeeded: m.probes_status_succeeded(),
    failed: m.probes_status_failed(),
    running: m.probes_status_running(),
    queued: m.probes_status_queued(),
    up: m.probes_status_up(),
    down: m.probes_status_down(),
    maintenance: m.probes_status_maintenance(),
    disabled: m.probes_status_disabled(),
  });
  const color = $derived(
    [
      "online",
      "ready",
      "applied",
      "active",
      "enrolled",
      "up",
      "succeeded",
    ].includes(status ?? "")
      ? "border-success/25 bg-success/10 text-success"
      : ["down", "failed", "rejected", "revoked", "firing"].includes(
            status ?? "",
          )
        ? "border-danger/25 bg-danger/10 text-danger"
        : ["pending", "suspect", "running", "queued", "degraded"].includes(
              status ?? "",
            )
          ? "border-warning/25 bg-warning/10 text-warning"
          : "border-border bg-muted/40 text-muted-foreground",
  );
</script>

<span
  class="inline-flex rounded-full border px-2.5 py-0.5 text-xs font-medium {color}"
  >{status ? (labels[status] ?? status) : m.probes_unreported()}</span
>
