<script lang="ts">
  import type { Component, Snippet } from "svelte";

  let {
    icon: Icon,
    title,
    description,
    action,
  }: {
    icon?: Component;
    title: string;
    description?: string;
    action?: Snippet;
  } = $props();
</script>

<div class="rounded-xl border border-dashed border-border p-10 text-center">
  {#if Icon}
    <div
      class="mx-auto mb-3 grid h-11 w-11 place-items-center rounded-xl bg-muted/50 text-muted-foreground"
    >
      <Icon class="h-5 w-5" />
    </div>
  {/if}
  <p class="text-sm text-foreground">{title}</p>
  {#if description}<p class="mt-1 text-xs text-muted-foreground">
      {description}
    </p>{/if}
  {#if action}<div class="mt-4">{@render action()}</div>{/if}
</div>
