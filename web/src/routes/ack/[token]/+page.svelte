<script lang="ts">
	import { onMount } from 'svelte';
	import { page } from '$app/stores';
	import { CheckCircle2, XCircle, Loader2 } from '@lucide/svelte';
	import { alertsApi, type Alert } from '$lib/api/alerts';
	import BrandMark from '$lib/components/BrandMark.svelte';
	import * as m from '$lib/paraglide/messages.js';

	type AckPhase = 'loading' | 'ok' | 'error';
	let phase = $state<AckPhase>('loading');
	let acked = $state<Alert | null>(null);
	let errorMsg = $state('');

	onMount(async () => {
		const token = $page.params.token ?? '';
		if (!token) {
			phase = 'error';
			errorMsg = m.alerts_ack_link_invalid();
			return;
		}
		try {
			acked = await alertsApi.acknowledgeByToken(token);
			phase = 'ok';
		} catch (e: unknown) {
			phase = 'error';
			errorMsg =
				typeof e === 'object' && e && 'message' in e && typeof (e as { message: unknown }).message === 'string'
					? (e as { message: string }).message
					: m.alerts_ack_link_failed();
		}
	});
</script>

<svelte:head>
	<title>{m.app_name()} · {m.alerts_ack_link_title()}</title>
</svelte:head>

<div class="flex min-h-dvh flex-col items-center justify-center px-4">
	<div class="w-full max-w-md rounded-xl border border-border bg-card p-8 text-center shadow-sm">
		<div class="mb-6 flex justify-center">
			<BrandMark size={40} />
		</div>

		{#if phase === 'loading'}
			<div class="flex flex-col items-center gap-3 text-muted-foreground">
				<Loader2 class="size-8 animate-spin" />
				<p class="text-sm">{m.alerts_ack_link_working()}</p>
			</div>
		{:else if phase === 'ok' && acked}
			<div class="flex flex-col items-center gap-3">
				<CheckCircle2 class="size-10 text-success" />
				<h1 class="text-xl font-semibold tracking-tight">{m.alerts_ack_link_success_title()}</h1>
				<p class="text-sm text-muted-foreground">
					{m.alerts_ack_link_success_body({ message: acked.message })}
				</p>
				<a
					href="/alerts"
					class="mt-4 inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent"
				>
					{m.alerts_ack_link_view_alerts()}
				</a>
			</div>
		{:else}
			<div class="flex flex-col items-center gap-3">
				<XCircle class="size-10 text-danger" />
				<h1 class="text-xl font-semibold tracking-tight">{m.alerts_ack_link_failed_title()}</h1>
				<p class="text-sm text-muted-foreground">{errorMsg}</p>
				<a
					href="/login"
					class="mt-4 inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent"
				>
					{m.alerts_ack_link_go_login()}
				</a>
			</div>
		{/if}
	</div>
</div>
