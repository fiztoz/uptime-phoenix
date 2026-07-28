<script lang="ts">
	import { BellRing, Check } from '@lucide/svelte';
	import { alertsApi, type Alert } from '$lib/api/alerts';
	import { realtime } from '$lib/stores/ws.svelte.js';
	import { toast } from 'svelte-sonner';
	import Skeleton from '$lib/components/Skeleton.svelte';
	import EmptyState from '$lib/components/EmptyState.svelte';
	import * as m from '$lib/paraglide/messages.js';

	let alerts = $state<Alert[]>([]);
	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let filter = $state<'open' | 'all'>('open');
	let acking = $state<Record<number, boolean>>({});

	async function load() {
		loading = true;
		loadError = null;
		try {
			alerts = await alertsApi.list(filter === 'open' ? { open: true, limit: 100 } : { limit: 100 });
		} catch (e: unknown) {
			const message = e instanceof Error ? e.message : m.alerts_page_load_failed();
			const msg =
				typeof e === 'object' && e && 'message' in e && typeof (e as { message: unknown }).message === 'string'
					? (e as { message: string }).message
					: message;
			loadError = msg;
			toast.error(msg);
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		// Reload when filter changes (also runs on mount).
		void filter;
		void load();
	});

	function monitorName(id: number): string {
		const mon = realtime.monitors.find((x) => x.id === id);
		return mon?.name ?? m.alerts_page_monitor_fallback({ id });
	}

	function statusClass(status: string): string {
		if (status === 'firing') return 'bg-danger/10 text-danger border-danger/25';
		if (status === 'acked') return 'bg-warning/10 text-warning border-warning/25';
		return 'bg-muted/40 text-muted-foreground border-border';
	}

	function escalationClass(status: string): string {
		if (status === 'pending') return 'bg-warning/10 text-warning border-warning/25';
		return 'bg-muted/40 text-muted-foreground border-border';
	}

	function escalationLabel(esc: NonNullable<Alert['escalation']>): string {
		if (esc.status === 'pending') return m.alerts_escalation_pending({ step: esc.next_step });
		if (esc.status === 'canceled') return m.alerts_escalation_canceled();
		return m.alerts_escalation_done();
	}

	function statusLabel(status: string): string {
		if (status === 'firing') return m.alerts_status_firing();
		if (status === 'acked') return m.alerts_status_acked();
		return m.alerts_status_resolved();
	}

	async function ack(alert: Alert) {
		if (alert.status !== 'firing') return;
		acking[alert.id] = true;
		try {
			const updated = await alertsApi.acknowledge(alert.id);
			alerts = alerts.map((a) => (a.id === updated.id ? updated : a));
			toast.success(m.alerts_page_acked_toast());
		} catch (e: unknown) {
			const msg =
				typeof e === 'object' && e && 'message' in e && typeof (e as { message: unknown }).message === 'string'
					? (e as { message: string }).message
					: m.alerts_page_ack_failed();
			toast.error(msg);
		} finally {
			acking[alert.id] = false;
		}
	}
</script>

<svelte:head>
	<title>{m.app_name()} · {m.alerts_title()}</title>
</svelte:head>

<div class="space-y-6">
	<div class="flex flex-wrap items-end justify-between gap-3">
		<div>
			<h1 class="text-2xl font-semibold tracking-tight">{m.alerts_title()}</h1>
			<p class="mt-1 text-sm text-muted-foreground">{m.alerts_page_subtitle()}</p>
		</div>
		<div class="inline-flex rounded-lg border border-border p-0.5 text-sm">
			<button
				type="button"
				class="rounded-md px-3 py-1.5 transition-colors {filter === 'open'
					? 'bg-accent font-medium'
					: 'text-muted-foreground hover:text-foreground'}"
				onclick={() => (filter = 'open')}
			>
				{m.alerts_filter_open()}
			</button>
			<button
				type="button"
				class="rounded-md px-3 py-1.5 transition-colors {filter === 'all'
					? 'bg-accent font-medium'
					: 'text-muted-foreground hover:text-foreground'}"
				onclick={() => (filter = 'all')}
			>
				{m.alerts_filter_all()}
			</button>
		</div>
	</div>

	{#if loading}
		<div class="rounded-xl border border-border bg-card" role="status">
			<span class="sr-only">{m.alerts_page_loading()}</span>
			{#each Array(3) as _, i}
				<div class="px-5 py-4 {i < 2 ? 'border-b border-border' : ''}">
					<Skeleton class="h-4 w-44" /><Skeleton class="mt-3 h-3 w-2/3" />
				</div>
			{/each}
		</div>
	{:else if loadError}
		{#snippet retryAction()}
			<button
				type="button"
				onclick={load}
				class="inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
			>
				{m.monitor_group_form_retry()}
			</button>
		{/snippet}
		<EmptyState icon={BellRing} title={m.alerts_page_load_failed()} description={loadError} action={retryAction} />
	{:else if alerts.length === 0}
		<EmptyState
			icon={BellRing}
			title={m.alerts_page_empty_title()}
			description={m.alerts_page_empty_description()}
		/>
	{:else}
		<div class="overflow-hidden rounded-xl border border-border bg-card">
			{#each alerts as alert, i (alert.id)}
				<div class="px-5 py-4 {i !== alerts.length - 1 ? 'border-b border-border' : ''}">
					<div class="flex flex-wrap items-start justify-between gap-3">
						<div class="min-w-0">
							<h3 class="font-medium tracking-tight">
								<a class="hover:underline" href="/monitors/{alert.monitor_id}">{monitorName(alert.monitor_id)}</a>
							</h3>
							<p class="mt-1 text-sm text-muted-foreground line-clamp-2">{alert.message || '—'}</p>
						</div>
						<div class="flex shrink-0 flex-wrap items-center gap-2">
							<span
								class="inline-flex rounded-full border px-2.5 py-0.5 text-xs font-medium {statusClass(
									alert.status,
								)}">{statusLabel(alert.status)}</span
							>
							{#if alert.escalation}
								<span
									data-testid="alert-escalation-badge"
									data-escalation-status={alert.escalation.status}
									class="inline-flex rounded-full border px-2.5 py-0.5 text-xs font-medium {escalationClass(
										alert.escalation.status,
									)}">{escalationLabel(alert.escalation)}</span
								>
							{/if}
							{#if alert.status === 'firing'}
								<button
									type="button"
									class="inline-flex items-center gap-1.5 rounded-lg border border-border bg-background px-3 py-1.5 text-xs font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50"
									disabled={acking[alert.id]}
									onclick={() => ack(alert)}
								>
									<Check class="size-3.5" />
									{acking[alert.id] ? m.alerts_acking() : m.alerts_ack()}
								</button>
							{/if}
						</div>
					</div>
					<p class="mt-2 text-xs text-muted-foreground">
						{m.alerts_page_fired_at({ time: new Date(alert.fired_at).toLocaleString() })}
						{#if alert.acked_at}
							· {m.alerts_page_acked_at({ time: new Date(alert.acked_at).toLocaleString() })}
						{/if}
						{#if alert.resolved_at}
							· {m.alerts_page_resolved_at({ time: new Date(alert.resolved_at).toLocaleString() })}
						{/if}
					</p>
				</div>
			{/each}
		</div>
	{/if}
</div>
