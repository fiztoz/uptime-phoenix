<script lang="ts">
	import { onDestroy } from 'svelte';
	import { page } from '$app/stores';
	import { replaceState } from '$app/navigation';
	import BrandMark from '$lib/components/BrandMark.svelte';
	import EmptyState from '$lib/components/EmptyState.svelte';
	import Skeleton from '$lib/components/Skeleton.svelte';
	import StatusPageFooter from '$lib/components/status-page/StatusPageFooter.svelte';
	import StatusPageHealth from '$lib/components/status-page/StatusPageHealth.svelte';
	import StatusPageIncidents from '$lib/components/status-page/StatusPageIncidents.svelte';
	import StatusPageMonitorList from '$lib/components/status-page/StatusPageMonitorList.svelte';
	import {
		statusPagesApi,
		type PublicStatusResponse,
	} from '$lib/api/statuspages';
	import { computeOverall } from '$lib/status-page-health';
	import { publicTheme } from '$lib/stores/publicTheme.svelte.ts';
	import { AlertTriangle, CalendarDays, LockKeyhole, Moon, Sun, Mail, Rss } from '@lucide/svelte';

	let slug = $derived($page.params.slug);
	let historyURL = $derived(`${$page.url.pathname.replace(/\/$/, '')}/history`);
	let data = $state<PublicStatusResponse | null>(null);
	let loading = $state(true);
	let accessCode = $state('');
	let accessError = $state('');
	let showAccessForm = $state(false);
	let verifyingAccess = $state(false);
	let userTheme = $state<'light' | 'dark' | null>(null);

	// Token action screens (confirm / unsubscribe) driven by query params.
	// Tokens are read once and stripped from the URL/history so they are not
	// retained in later referrers.
	type TokenAction =
		| { kind: 'confirm' | 'unsubscribe'; status: 'working' | 'ok' | 'error'; message: string }
		| null;
	let tokenAction = $state<TokenAction>(null);
	let tokenHandled = $state(false);

	// Subscribe form state (shown after access unlock when subscriptions_available).
	let subscribeEmail = $state('');
	let subscribeError = $state('');
	let subscribeSuccess = $state('');
	let subscribeBusy = $state(false);
	// Remember access code only for the in-memory session so subscribe can
	// re-verify without re-prompting — never persisted.
	let unlockedAccessCode = $state('');

	// Public F3.6 feeds. Protected pages append the in-session access code so a
	// browser/reader can open them without a separate POST verify step.
	let feedQuery = $derived(
		unlockedAccessCode
			? `?access_code=${encodeURIComponent(unlockedAccessCode)}`
			: '',
	);
	let atomFeedURL = $derived(slug ? `/api/status/${slug}/feed.xml${feedQuery}` : '');
	let calendarURL = $derived(slug ? `/api/status/${slug}/calendar.ics${feedQuery}` : '');

	$effect(() => {
		if (slug) void fetchStatus();
	});

	$effect(() => {
		if (userTheme) {
			publicTheme.syncFromSetting(userTheme);
		} else {
			publicTheme.syncFromSetting(data?.status_page?.theme ?? 'auto');
		}
	});

	// Consume confirm/unsubscribe query tokens once per navigation.
	$effect(() => {
		const url = $page.url;
		if (tokenHandled) return;
		const confirm = url.searchParams.get('confirm');
		const unsub = url.searchParams.get('unsubscribe');
		if (!confirm && !unsub) return;
		tokenHandled = true;
		const kind = confirm ? 'confirm' : 'unsubscribe';
		const token = confirm ?? unsub ?? '';
		// Strip token from the visible URL/history before the network call.
		const clean = new URL(url);
		clean.searchParams.delete('confirm');
		clean.searchParams.delete('unsubscribe');
		replaceState(clean.pathname + clean.search + clean.hash, {});
		void runTokenAction(kind as 'confirm' | 'unsubscribe', token);
	});

	onDestroy(() => {
		publicTheme.teardown();
	});

	function toggleTheme() {
		if (userTheme === 'dark') {
			userTheme = 'light';
		} else if (userTheme === 'light') {
			userTheme = 'dark';
		} else {
			userTheme = publicTheme.resolved === 'dark' ? 'light' : 'dark';
		}
	}

	function messageOf(error: unknown): string {
		if (
			typeof error === 'object' &&
			error !== null &&
			'message' in error &&
			typeof error.message === 'string'
		) {
			return error.message;
		}
		return '';
	}

	async function runTokenAction(kind: 'confirm' | 'unsubscribe', token: string) {
		tokenAction = {
			kind,
			status: 'working',
			message:
				kind === 'confirm'
					? 'Confirming your subscription…'
					: 'Unsubscribing…',
		};
		try {
			if (kind === 'confirm') {
				await statusPagesApi.confirmSubscription(token);
				tokenAction = {
					kind,
					status: 'ok',
					message: 'Your subscription is confirmed. You will receive status updates by email.',
				};
			} else {
				await statusPagesApi.unsubscribe(token);
				tokenAction = {
					kind,
					status: 'ok',
					message: 'You have been unsubscribed and will no longer receive emails for this status page.',
				};
			}
		} catch (error: unknown) {
			tokenAction = {
				kind,
				status: 'error',
				message:
					messageOf(error) ||
					(kind === 'confirm'
						? 'This confirmation link is invalid or has expired.'
						: 'This unsubscribe link is invalid or has already been used.'),
			};
		}
	}

	async function fetchStatus() {
		const currentSlug = slug;
		if (!currentSlug) return;

		loading = true;
		accessError = '';
		try {
			const response = await statusPagesApi.getPublic(currentSlug);
			data = response;
			showAccessForm = Boolean(response.status_page.has_access);
			if (!response.status_page.has_access) {
				unlockedAccessCode = '';
			}
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
			accessError = 'Enter the access code';
			return;
		}

		verifyingAccess = true;
		accessError = '';
		try {
			data = await statusPagesApi.verifyAccess(currentSlug, accessCode);
			showAccessForm = false;
			unlockedAccessCode = accessCode;
			accessCode = '';
		} catch (error: unknown) {
			accessError = messageOf(error).includes('access denied')
				? 'Incorrect access code'
				: 'Unable to verify the access code';
		} finally {
			verifyingAccess = false;
		}
	}

	async function submitSubscribe() {
		const currentSlug = slug;
		if (!currentSlug || subscribeBusy) return;
		const email = subscribeEmail.trim();
		if (!email) {
			subscribeError = 'Enter your email address';
			return;
		}
		subscribeBusy = true;
		subscribeError = '';
		subscribeSuccess = '';
		try {
			await statusPagesApi.subscribe(
				currentSlug,
				email,
				unlockedAccessCode || undefined,
			);
			subscribeSuccess =
				'Check your inbox to confirm the subscription. If you are already subscribed, we sent a management email.';
			subscribeEmail = '';
		} catch (error: unknown) {
			const msg = messageOf(error).toLowerCase();
			if (msg.includes('unavailable')) {
				subscribeError =
					'Email subscriptions are not available for this page right now.';
			} else if (msg.includes('access')) {
				subscribeError = 'Access code required or invalid.';
			} else if (msg.includes('validation') || msg.includes('email')) {
				subscribeError = 'Please enter a valid email address.';
			} else {
				subscribeError = messageOf(error) || 'Unable to subscribe right now.';
			}
		} finally {
			subscribeBusy = false;
		}
	}

	const activeIncidents = $derived(
		data?.incidents?.filter((incident) => incident.active) ?? [],
	);
	const downCount = $derived(
		data?.monitors?.filter((monitor) => monitor.status === 'down').length ?? 0,
	);
	const overall = $derived(
		computeOverall(data?.monitors ?? [], activeIncidents),
	);
	const dashboardStyle = $derived(data?.status_page?.dashboard_style ?? 'full');
	const showSubscribe = $derived(
		Boolean(data?.subscriptions_available) && !showAccessForm && !loading,
	);
</script>

<svelte:head>
	<title>{data?.status_page?.title ?? slug} · Status</title>
	<meta
		name="description"
		content={data?.status_page?.description ||
			`Live service status for ${data?.status_page?.title ?? slug}`}
	/>
	<meta
		property="og:title"
		content={`${data?.status_page?.title ?? slug} · Status`}
	/>
	<meta
		property="og:description"
		content={data?.status_page?.description || 'Live service status'}
	/>
	<meta property="og:type" content="website" />
	<meta property="og:url" content={$page.url.href} />
	<meta name="twitter:card" content="summary" />
	{#if data?.status_page?.favicon}
		<link rel="icon" href={data.status_page.favicon} />
	{/if}
	{#if data?.status_page?.icon}
		<meta property="og:image" content={data.status_page.icon} />
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
  data-theme={data?.status_page?.theme || "auto"}
>
  <div
    class="mx-auto px-4 py-12 {dashboardStyle === 'full'
      ? 'max-w-4xl'
      : 'max-w-5xl'}"
  >
    <header class="relative mb-8 text-center">
      <button
        onclick={toggleTheme}
        class="absolute right-0 top-0 rounded-lg border border-border bg-card p-2 text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
        aria-label="Toggle dark mode"
      >
        {#if publicTheme.resolved === "dark"}
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
            class="h-16 w-16 rounded-xl object-contain"
          />
        </div>
      {:else if data?.status_page?.show_powered_by !== false}
        <div class="mb-4 flex items-center justify-center">
          <BrandMark size={64} />
        </div>
      {/if}
      <h1 class="text-2xl font-semibold tracking-tight sm:text-3xl md:text-4xl">
        {data?.status_page?.title || slug}
      </h1>
      <p class="mt-2 text-muted-foreground">
        {data?.status_page?.description || "Service Status"}
      </p>
    </header>

    {#if tokenAction}
      <div
        class="mx-auto mb-8 max-w-md rounded-xl border border-border bg-card p-6 text-center"
        role="status"
        aria-live="polite"
      >
        <Mail class="mx-auto mb-3 h-6 w-6 text-muted-foreground" />
        <h2 class="font-semibold">
          {tokenAction.kind === 'confirm' ? 'Confirm subscription' : 'Unsubscribe'}
        </h2>
        <p
          class="mt-2 text-sm {tokenAction.status === 'error'
            ? 'text-danger'
            : 'text-muted-foreground'}"
        >
          {tokenAction.message}
        </p>
        {#if tokenAction.status !== 'working'}
          <button
            type="button"
            class="mt-4 text-sm font-medium text-primary hover:underline"
            onclick={() => (tokenAction = null)}
          >
            Continue to status page
          </button>
        {/if}
      </div>
    {/if}

    {#if loading}
      <div class="space-y-8" role="status">
        <span class="sr-only">Loading status</span>
        <Skeleton class="h-20 rounded-2xl" />
        <div class="space-y-4">
          {#each Array(3) as _}
            <div class="rounded-xl border border-border bg-card p-5">
              <div class="flex items-center justify-between">
                <Skeleton class="h-5 w-40" />
                <Skeleton class="h-6 w-24 rounded-full" />
              </div>
              <Skeleton class="mt-6 h-3 w-full" />
            </div>
          {/each}
        </div>
      </div>
    {:else if !data}
      <EmptyState
        icon={AlertTriangle}
        title="Status page not found or unpublished."
      />
    {:else if showAccessForm}
      <form
        class="mx-auto max-w-sm rounded-xl border border-border bg-card p-4 text-center sm:p-8"
        onsubmit={(event) => {
          event.preventDefault();
          void submitAccessCode();
        }}
      >
        <LockKeyhole class="mx-auto mb-3 h-6 w-6 text-muted-foreground" />
        <h2 class="font-semibold">This status page is access protected</h2>
        <p class="mt-1 text-sm text-muted-foreground">
          Enter its access code to view service and incident details.
        </p>
        <input
          type="password"
          bind:value={accessCode}
          class="mt-4 w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring"
          placeholder="Enter access code"
          autocomplete="current-password"
          aria-describedby={accessError ? "access-code-error" : undefined}
        />
        {#if accessError}
          <p id="access-code-error" class="mt-1 text-sm text-danger">
            {accessError}
          </p>
        {/if}
        <button
          type="submit"
          disabled={verifyingAccess}
          class="mt-4 inline-flex w-full items-center justify-center rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-60"
        >
          {verifyingAccess ? "Verifying…" : "View Status"}
        </button>
      </form>
    {:else}
      <StatusPageHealth
        level={overall}
        {downCount}
        activeIncidentCount={activeIncidents.length}
        compact={dashboardStyle === "pills"}
      />
      <StatusPageMonitorList
        monitors={data.monitors}
        density={dashboardStyle}
      />
      <div class="mt-5 flex flex-wrap items-center justify-end gap-2">
        <a
          href={atomFeedURL}
          class="inline-flex items-center gap-2 rounded-lg border border-border bg-card px-3 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
        >
          <Rss class="h-4 w-4" />
          Incidents feed
        </a>
        <a
          href={calendarURL}
          class="inline-flex items-center gap-2 rounded-lg border border-border bg-card px-3 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
        >
          <CalendarDays class="h-4 w-4" />
          Maintenance calendar
        </a>
        <a
          href={historyURL}
          class="inline-flex items-center gap-2 rounded-lg border border-border bg-card px-3 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
        >
          <CalendarDays class="h-4 w-4" />
          View uptime history
        </a>
      </div>
      <StatusPageIncidents incidents={activeIncidents} />

      {#if showSubscribe}
        <section
          class="mt-8 rounded-xl border border-border bg-card p-5 sm:p-6"
          aria-labelledby="subscribe-heading"
        >
          <div class="flex items-start gap-3">
            <Mail class="mt-0.5 h-5 w-5 shrink-0 text-muted-foreground" />
            <div class="min-w-0 flex-1">
              <h2 id="subscribe-heading" class="text-sm font-semibold tracking-tight">
                Get status updates by email
              </h2>
              <p class="mt-1 text-xs text-muted-foreground">
                We will send a confirmation link first. You can unsubscribe any time.
              </p>
              <form
                class="mt-4 flex flex-col gap-2 sm:flex-row"
                onsubmit={(event) => {
                  event.preventDefault();
                  void submitSubscribe();
                }}
              >
                <label class="sr-only" for="subscribe-email">Email address</label>
                <input
                  id="subscribe-email"
                  type="email"
                  bind:value={subscribeEmail}
                  autocomplete="email"
                  inputmode="email"
                  required
                  disabled={subscribeBusy}
                  placeholder="you@example.com"
                  class="w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring disabled:opacity-60 sm:flex-1"
                />
                <button
                  type="submit"
                  disabled={subscribeBusy}
                  class="inline-flex items-center justify-center rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:cursor-not-allowed disabled:opacity-60"
                >
                  {subscribeBusy ? 'Sending…' : 'Subscribe'}
                </button>
              </form>
              {#if subscribeError}
                <p class="mt-2 text-sm text-danger" role="alert">{subscribeError}</p>
              {/if}
              {#if subscribeSuccess}
                <p class="mt-2 text-sm text-success" role="status">{subscribeSuccess}</p>
              {/if}
            </div>
          </div>
        </section>
      {/if}

      <StatusPageFooter
        footerText={data.status_page.footer_text}
        density={dashboardStyle}
				level={overall}
				showPoweredBy={data.status_page.show_powered_by !== false}
      />
    {/if}
  </div>
</div>
