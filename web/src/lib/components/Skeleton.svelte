<script lang="ts">
  let { class: className = "" }: { class?: string } = $props();
</script>

<div
  class="animate-pulse rounded-lg bg-muted/60 {className}"
  aria-hidden="true"
></div>
