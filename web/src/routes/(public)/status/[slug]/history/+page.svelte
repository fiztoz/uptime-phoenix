<script lang="ts">
  import { onDestroy } from "svelte";
  import { page } from "$app/stores";
  import BrandMark from "$lib/components/BrandMark.svelte";
  import EmptyState from "$lib/components/EmptyState.svelte";
  import Skeleton from "$lib/components/Skeleton.svelte";
  import StatusPageFooter from "$lib/components/status-page/StatusPageFooter.svelte";
  import StatusPageUptimeHistory from "$lib/components/status-page/StatusPageUptimeHistory.svelte";
  import {
    statusPagesApi,
    type PublicStatusResponse,
  } from "$lib/api/statuspages";
  import { computeOverall } from "$lib/status-page-health";
  import { publicTheme } from "$lib/stores/publicTheme.svelte.ts";
  import {
    AlertTriangle,
    ArrowLeft,
    LockKeyhole,
    Moon,
    Sun,
  } from "@lucide/svelte";

  let slug = $derived($page.params.slug);
  let statusURL = $derived(
    $page.url.pathname.replace(/\/history\/?$/, "") || "/",
  );
  let data = $state<PublicStatusResponse | null>(null);
  let loading = $state(true);
  let accessCode = $state("");
  let accessError = $state("");
  let showAccessForm = $state(false);
  let verifyingAccess = $state(false);
  let userTheme = $state<"light" | "dark" | null>(null);

  $effect(() => {
    if (slug) void fetchStatus();
  });

  $effect(() => {
    if (userTheme) {
      publicTheme.syncFromSetting(userTheme);
    } else {
      publicTheme.syncFromSetting(data?.status_page?.theme ?? "auto");
    }
  });

  onDestroy(() => {
    publicTheme.teardown();
  });

  function toggleTheme() {
    if (userTheme === "dark") userTheme = "light";
    else if (userTheme === "light") userTheme = "dark";
    else userTheme = publicTheme.resolved === "dark" ? "light" : "dark";
  }

  function messageOf(error: unknown): string {
    if (
      typeof error === "object" &&
      error !== null &&
      "message" in error &&
      typeof error.message === "string"
    ) {
      return error.message;
    }
    return "";
  }

  async function fetchStatus() {
    const currentSlug = slug;
    if (!currentSlug) return;
    loading = true;
    accessError = "";
    try {
      const response = await statusPagesApi.getPublic(currentSlug);
      data = response;
      showAccessForm = Boolean(response.status_page.has_access);
    } catch {
      data = null;
      showAccessForm = false;
    } finally {
      loading = false;
    }
  }

  async function submitAccessCode() {
    const currentSlug = slug;
    if (!currentSlug || verifyingAccess) return;
    if (!accessCode) {
      accessError = "Enter the access code";
      return;
    }
    verifyingAccess = true;
    accessError = "";
    try {
      data = await statusPagesApi.verifyAccess(currentSlug, accessCode);
      showAccessForm = false;
      accessCode = "";
    } catch (error: unknown) {
      accessError = messageOf(error).includes("access denied")
        ? "Incorrect access code"
        : "Unable to verify the access code";
    } finally {
      verifyingAccess = false;
    }
  }

  let activeIncidents = $derived(
    data?.incidents?.filter((incident) => incident.active) ?? [],
  );
  let overall = $derived(computeOverall(data?.monitors ?? [], activeIncidents));
  let dashboardStyle = $derived(data?.status_page?.dashboard_style ?? "full");
</script>

<svelte:head>
  <title>Uptime history · {data?.status_page?.title ?? slug}</title>
  <meta
    name="description"
    content={`Monthly and quarterly uptime history for ${data?.status_page?.title ?? slug}`}
  />
  {#if data?.status_page?.favicon}
    <link rel="icon" href={data.status_page.favicon} />
  {/if}
  {#if data?.status_page?.custom_css}
    <!-- prettier-ignore -->
    <style>
{data.status_page.custom_css}
    </style>
  {/if}
</svelte:head>

<div
	class="min-h-dvh bg-background text-foreground dark:bg-background dark:text-foreground"
	data-theme={data?.status_page?.theme || 'auto'}
>
	<div class="mx-auto max-w-7xl px-4 py-10 sm:px-6 sm:py-12">
		<header class="relative mb-8 text-center">
			<button
				onclick={toggleTheme}
				class="absolute right-0 top-0 rounded-lg border border-border bg-card p-2 text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
				aria-label="Toggle dark mode"
			>
				{#if publicTheme.resolved === 'dark'}
					<Sun class="h-4 w-4" />
				{:else}
					<Moon class="h-4 w-4" />
				{/if}
			</button>
			{#if data?.status_page?.icon}
				<div class="mb-4 flex items-center justify-center">
					<img
						src={data.status_page.icon}
						alt=""
						class="h-14 w-14 rounded-xl object-contain"
					/>
				</div>
			{:else if data?.status_page?.show_powered_by !== false}
				<div class="mb-4 flex items-center justify-center">
					<BrandMark size={56} />
				</div>
			{/if}
			<p class="text-xs font-medium uppercase tracking-[0.18em] text-muted-foreground">Uptime history</p>
			<h1 class="mt-2 text-2xl font-semibold tracking-tight sm:text-3xl md:text-4xl">
				{data?.status_page?.title || slug}
			</h1>
		</header>

		{#if loading}
			<div class="space-y-5" role="status">
				<span class="sr-only">Loading uptime history</span>
				<Skeleton class="h-16 rounded-xl" />
				<Skeleton class="h-80 rounded-xl" />
			</div>
		{:else if !data}
			<EmptyState icon={AlertTriangle} title="Status page not found or unpublished." />
		{:else if showAccessForm}
			<form
				class="mx-auto max-w-sm rounded-xl border border-border bg-card p-4 text-center sm:p-8"
				onsubmit={(event) => {
					event.preventDefault();
					void submitAccessCode();
				}}
			>
				<LockKeyhole class="mx-auto mb-3 h-6 w-6 text-muted-foreground" />
				<h2 class="font-semibold">This uptime history is access protected</h2>
				<p class="mt-1 text-sm text-muted-foreground">Enter the status page access code to continue.</p>
				<input
					type="password"
					bind:value={accessCode}
					class="mt-4 w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring"
					placeholder="Enter access code"
					autocomplete="current-password"
					aria-describedby={accessError ? 'history-access-code-error' : undefined}
				/>
				{#if accessError}
					<p id="history-access-code-error" class="mt-1 text-sm text-danger">{accessError}</p>
				{/if}
				<button
					type="submit"
					disabled={verifyingAccess}
					class="mt-4 inline-flex w-full items-center justify-center rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-60"
				>
					{verifyingAccess ? 'Verifying…' : 'View uptime history'}
				</button>
			</form>
		{:else}
			<nav class="mb-6" aria-label="Status page navigation">
				<a
					href={statusURL}
					class="inline-flex items-center gap-2 text-sm font-medium text-muted-foreground transition-colors hover:text-foreground"
				>
					<ArrowLeft class="h-4 w-4" />
					Current status
				</a>
			</nav>
			<StatusPageUptimeHistory monitors={data.monitors} slaTarget={data.status_page.sla_target} />
			<StatusPageFooter
				footerText={data.status_page.footer_text}
				density={dashboardStyle}
				level={overall}
				showPoweredBy={data.status_page.show_powered_by !== false}
			/>
		{/if}
	</div>
</div>
