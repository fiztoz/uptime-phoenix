<!--
  @component
  Inline "couldn't load" notice with an optional Retry, for a section whose
  request failed. Rendered instead of an empty state so a backend failure does
  not read as "no data".
-->
<script lang="ts">
  import { AlertTriangle } from "@lucide/svelte";
  import * as m from "#lib/paraglide/messages.js";

  interface Props {
    message: string;
    onRetry?: () => void;
    class?: string;
  }

  let { message, onRetry, class: className = "" }: Props = $props();
</script>

<div
  role="alert"
  class="flex flex-wrap items-center gap-x-3 gap-y-2 text-sm text-danger {className}"
  data-testid="load-error-notice"
>
  <span class="inline-flex items-center gap-1.5">
    <AlertTriangle class="h-4 w-4 shrink-0" />
    {message}
  </span>
  {#if onRetry}
    <button
      type="button"
      onclick={onRetry}
      class="rounded-lg border border-border px-2.5 py-1 text-xs font-medium text-foreground transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
      >{m.monitor_group_form_retry()}</button
    >
  {/if}
</div>
