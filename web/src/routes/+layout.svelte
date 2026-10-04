<script lang="ts">
  import { Toaster } from "svelte-sonner";
  import { page } from "$app/stores";
  import { themeStore } from "$lib/stores/theme.svelte";
  import { publicTheme } from "$lib/stores/publicTheme.svelte";
  import "../app.css";
  import ConfirmDialog from "$lib/components/ConfirmDialog.svelte";
  import type { Snippet } from "svelte";

  let { children }: { children: Snippet } = $props();

  const theme = $derived(
    $page.url.pathname.startsWith("/status/")
      ? publicTheme.resolved
      : themeStore.theme,
  );
</script>

<Toaster position="bottom-right" richColors closeButton {theme} />

<!-- Single instance for the whole app; driven by confirmAction() from anywhere. -->
<ConfirmDialog />

{@render children()}
