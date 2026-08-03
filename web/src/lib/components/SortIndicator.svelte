<script lang="ts">
  import { ArrowDown, ArrowUp, ArrowUpDown } from "@lucide/svelte";

  let {
    active,
    direction,
  }: {
    active: boolean;
    direction: "asc" | "desc";
  } = $props();
</script>

{#if !active}
  <ArrowUpDown class="h-3.5 w-3.5" aria-hidden="true" />
{:else if direction === "asc"}
  <ArrowUp class="h-3.5 w-3.5" aria-hidden="true" />
{:else}
  <ArrowDown class="h-3.5 w-3.5" aria-hidden="true" />
{/if}
