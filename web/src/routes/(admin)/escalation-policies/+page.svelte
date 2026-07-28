<script lang="ts">
	import { escalationApi, type EscalationPolicy } from '$lib/api/escalation';
	import EscalationPolicyForm from '$lib/components/EscalationPolicyForm.svelte';
	import Skeleton from '$lib/components/Skeleton.svelte';
	import EmptyState from '$lib/components/EmptyState.svelte';
	import { auth } from '$lib/stores/auth.svelte.ts';
	import { CheckCircle, Edit2, ListOrdered, Plus, Trash2, XCircle } from '@lucide/svelte';
	import { confirmAction } from '$lib/stores/confirm.svelte';
	import { toast } from 'svelte-sonner';
	import * as m from '$lib/paraglide/messages.js';

	let policies = $state<EscalationPolicy[]>([]);
	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let showForm = $state(false);
	let editing = $state<EscalationPolicy | null>(null);

	// The admin's RAW capability flags are both false while they may still do
	// everything — enforcement ORs them server-side. Gate on `is_admin ||
	// can_manage_x`, never the flag alone, or the admin loses their own page.
	const canManage = $derived(
		(auth.user?.is_admin ?? false) || (auth.user?.can_manage_notifications ?? false),
	);

	async function load() {
		loading = true;
		loadError = null;
		try {
			policies = await escalationApi.list();
		} catch (error: unknown) {
			loadError =
				error && typeof error === 'object' && 'message' in error
					? String((error as { message: string }).message)
					: m.escalation_load_failed();
			toast.error(loadError);
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		load();
	});

	function handleCreate() {
		editing = null;
		showForm = true;
	}

	function handleEdit(p: EscalationPolicy) {
		editing = p;
		showForm = true;
	}

	async function handleDelete(p: EscalationPolicy) {
		const ok = await confirmAction({
			title: m.escalation_delete_title({ name: p.name }),
			message: m.escalation_delete_message(),
			confirmLabel: m.btn_delete(),
			destructive: true,
		});
		if (!ok) return;
		try {
			await escalationApi.remove(p.id);
			toast.success(m.escalation_deleted_toast());
			await load();
		} catch {
			toast.error(m.escalation_delete_failed());
		}
	}

	function handleSaved() {
		showForm = false;
		editing = null;
		load();
	}

	/** "5m → 10m → 30m" — the shape of the ladder at a glance. */
	function ladderSummary(p: EscalationPolicy): string {
		return p.steps.map((s) => `${s.wait_minutes}m`).join(' → ');
	}
</script>

<svelte:head>
	<title>{m.app_name()} · {m.escalation_title()}</title>
</svelte:head>

<div class="space-y-6">
	<div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
		<div>
			<h1 class="text-2xl font-semibold tracking-tight">{m.escalation_title()}</h1>
			<p class="mt-1 text-sm text-muted-foreground">{m.escalation_subtitle()}</p>
		</div>
		<button
			onclick={handleCreate}
			disabled={!canManage}
			class="inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50"
		>
			<Plus class="h-4 w-4" />
			{m.escalation_create()}
		</button>
	</div>

	{#if loading}
		<div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3" role="status">
			<span class="sr-only">{m.escalation_loading()}</span>
			{#each Array(3) as _}
				<div class="rounded-xl border border-border bg-card p-5">
					<Skeleton class="h-4 w-32" />
					<Skeleton class="mt-2 h-3 w-20" />
					<Skeleton class="mt-6 h-8 w-full" />
				</div>
			{/each}
		</div>
	{:else if loadError}
		{#snippet retryAction()}
			<button
				type="button"
				onclick={load}
				class="inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
				>{m.monitor_group_form_retry()}</button
			>
		{/snippet}
		<EmptyState icon={XCircle} title={m.escalation_load_failed()} description={loadError} action={retryAction} />
	{:else if policies.length === 0}
		{#snippet emptyAction()}
			{#if canManage}
				<button
					onclick={handleCreate}
					class="inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
					><Plus class="h-4 w-4" />{m.escalation_add_first()}</button
				>
			{/if}
		{/snippet}
		<EmptyState
			icon={ListOrdered}
			title={m.escalation_empty_title()}
			description={m.escalation_empty_description()}
			action={emptyAction}
		/>
	{:else}
		<div class="grid gap-4 sm:grid-cols-2 xl:grid-cols-3">
			{#each policies as p (p.id)}
				<div
					data-testid="escalation-policy-card"
					class="group relative overflow-hidden rounded-xl border border-border bg-card p-5 transition-all duration-200 hover:-translate-y-0.5 hover:border-border/80"
				>
					<div class="flex items-start justify-between gap-3">
						<div class="flex min-w-0 items-center gap-3">
							<div
								class="grid h-10 w-10 shrink-0 place-items-center rounded-full {p.enabled
									? 'bg-success/10 text-success ring-1 ring-success/20'
									: 'bg-muted text-muted-foreground'}"
							>
								{#if p.enabled}
									<CheckCircle class="h-5 w-5" />
								{:else}
									<XCircle class="h-5 w-5" />
								{/if}
							</div>
							<div class="min-w-0">
								<h3 class="truncate font-medium">{p.name}</h3>
								<p class="truncate text-sm text-muted-foreground">
									{m.escalation_steps_count({ count: p.steps.length })}
								</p>
							</div>
						</div>
						<span
							class="inline-flex shrink-0 items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium {p.enabled
								? 'border-success/25 bg-success/10 text-success'
								: 'border-border bg-muted/40 text-muted-foreground'}"
						>
							<span class="dot {p.enabled ? 'dot-up' : 'dot-muted'}"></span>
							{p.enabled ? m.escalation_enabled() : m.escalation_disabled()}
						</span>
					</div>

					{#if p.description}
						<p class="mt-3 line-clamp-2 text-sm text-muted-foreground">{p.description}</p>
					{/if}
					{#if p.steps.length > 0}
						<p class="mt-3 font-mono text-xs text-muted-foreground">{ladderSummary(p)}</p>
					{/if}

					<div class="mt-4 flex items-center gap-2 border-t border-border pt-3">
						{#if canManage}
							<button
								onclick={() => handleEdit(p)}
								class="inline-flex items-center gap-1 rounded-lg border border-border px-3 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
								title={m.btn_edit()}
							>
								<Edit2 class="h-3 w-3" />
								{m.btn_edit()}
							</button>
							<button
								onclick={() => handleDelete(p)}
								class="inline-flex items-center gap-1 rounded-lg border border-destructive/25 px-3 py-1.5 text-xs font-medium text-destructive transition-colors hover:bg-destructive/10"
								title={m.btn_delete()}
							>
								<Trash2 class="h-3 w-3" />
								{m.btn_delete()}
							</button>
						{/if}
					</div>
				</div>
			{/each}
		</div>
	{/if}
</div>

{#if showForm}
	<EscalationPolicyForm
		policy={editing}
		onSaved={handleSaved}
		onClose={() => {
			showForm = false;
			editing = null;
		}}
	/>
{/if}
