<script lang="ts">
  import { page } from "$app/state";
  import BrandMark from "$lib/components/BrandMark.svelte";

  const primaryBtn =
    "inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60";
  const ghostBtn =
    "inline-flex items-center gap-2 rounded-lg px-3 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground";

  let message = $derived(
    page.error?.message ||
      (page.status === 404
        ? "This page doesn't exist."
        : "Something went wrong."),
  );
</script>

<svelte:head>
  <title>Phoenix · {page.status}</title>
</svelte:head>

<div
  class="app-glow relative flex min-h-dvh items-center justify-center bg-background p-4"
>
  <div class="relative z-10 w-full max-w-md text-center">
    <div class="mb-2 flex justify-center">
      <BrandMark size={86} class="translate-x-2" />
    </div>
    <p class="mt-8 text-6xl font-semibold tracking-tight text-primary tnum">
      {page.status}
    </p>
    <h1 class="mt-4 text-2xl font-semibold tracking-tight">
      {page.status === 404 ? "Page not found" : "Unexpected error"}
    </h1>
    <p class="mt-2 text-sm text-muted-foreground">{message}</p>
    <div class="mt-8 flex items-center justify-center gap-2">
      <a href="/dashboard" class={primaryBtn}>Go to dashboard</a>
      <button type="button" class={ghostBtn} onclick={() => history.back()}
        >Go back</button
      >
    </div>
  </div>
</div>
