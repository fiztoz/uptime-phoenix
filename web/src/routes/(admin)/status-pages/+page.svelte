<script lang="ts">
	import { goto } from '$app/navigation';
	import { statusPagesApi, type StatusPage } from '$lib/api/statuspages';
	import StatusPageForm from '$lib/components/StatusPageForm.svelte';
	import EmptyState from '$lib/components/EmptyState.svelte';
	import Skeleton from '$lib/components/Skeleton.svelte';
	import { confirmAction } from '$lib/stores/confirm.svelte';
	import { toast } from 'svelte-sonner';
	import { Plus, Edit2, Trash2, ExternalLink, Eye, AlertTriangle } from '@lucide/svelte';
	import * as m from '$lib/paraglide/messages.js';

	let statusPages = $state<StatusPage[]>([]);
	let showForm = $state(false);
	let editing = $state<StatusPage | null>(null);
	let loading = $state(true);
	let loadError = $state<string | null>(null);

	async function load() {
		loading = true;
		loadError = null;
		try {
			statusPages = await statusPagesApi.list();
		} catch (e: any) {
			const message = e?.message || m.status_pages_page_load_failed();
			loadError = message;
			toast.error(message);
		} finally {
			loading = false;
		}
	}

	function openCreate() {
		editing = null;
		showForm = true;
	}

	function openEdit(sp: StatusPage) {
		goto(`/status-pages/${sp.id}`);
	}

	async function handleSaved(saved: StatusPage) {
		showForm = false;
		await load();
		if (!editing) {
			await goto(`/status-pages/${saved.id}`);
		}
	}

	async function handleDelete(id: number, title: string) {
		const ok = await confirmAction({
			title: m.status_pages_page_delete_title({ title }),
			message: m.status_pages_page_delete_message(),
			confirmLabel: m.status_pages_page_delete_confirm(),
			destructive: true
		});
		if (!ok) return;
		try {
			await statusPagesApi.remove(id);
			toast.success(m.status_pages_page_deleted_toast());
			await load();
		} catch (e: any) {
			toast.error(e?.message || m.monitors_page_delete_failed());
		}
	}

	function viewPublic(slug: string) {
		window.open(`/status/${slug}`, '_blank');
	}

	$effect(() => {
		load();
	});
</script>

<svelte:head>
	<title>{m.app_name()} · {m.status_pages_title()}</title>
</svelte:head>

<div class="space-y-6">
	<!-- Page heading row -->
	<div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
		<div>
			<h1 class="text-2xl font-semibold tracking-tight">{m.status_pages_title()}</h1>
			<p class="mt-1 text-sm text-muted-foreground">{m.status_pages_page_subtitle()}</p>
		</div>
		<button
			onclick={openCreate}
			class="inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
		>
			<Plus class="h-4 w-4" /> {m.status_pages_page_new()}
		</button>
	</div>

	{#snippet retryLoadAction()}
		<button type="button" onclick={load} class="inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">{m.monitor_group_form_retry()}</button>
	{/snippet}
	{#snippet createFirstAction()}
		<button type="button" onclick={openCreate} class="inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"><Plus class="h-4 w-4" /> {m.status_pages_page_new()}</button>
	{/snippet}

	{#if loading}
		<div class="grid gap-4 sm:grid-cols-2" role="status">
			<span class="sr-only">{m.loading()}</span>
			{#each Array(2) as _}
				<div class="rounded-xl border border-border bg-card p-5">
					<Skeleton class="h-5 w-40" />
					<Skeleton class="mt-2 h-3 w-28" />
					<Skeleton class="mt-6 h-7 w-24 rounded-full" />
				</div>
			{/each}
			</div>
	{:else if loadError}
		<EmptyState icon={AlertTriangle} title={m.status_pages_page_load_failed()} description={loadError} action={retryLoadAction} />
	{:else if statusPages.length === 0}
		<EmptyState icon={Eye} title={m.status_pages_page_empty_title()} description={m.status_pages_page_empty_description()} action={createFirstAction} />
	{:else}
		<div class="hidden overflow-x-auto rounded-xl border border-border bg-card md:block">
			<table class="w-full text-sm">
				<thead>
					<tr class="border-b border-border text-left">
						<th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">{m.status_pages_page_col_title_slug()}</th>
						<th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">{m.status_page_form_theme_label()}</th>
						<th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">{m.monitors_page_col_status()}</th>
						<th class="w-32 px-4 py-3 text-right text-[11px] font-semibold uppercase tracking-wider text-faint">{m.monitors_page_col_actions()}</th>
					</tr>
				</thead>
				<tbody>
					{#each statusPages as sp (sp.id)}
						<tr class="border-b border-border transition-colors last:border-0 hover:bg-accent/40">
							<td class="px-4 py-3">
								<div class="font-medium">{sp.title}</div>
								<div class="font-mono text-xs text-muted-foreground">/{sp.slug}</div>
							</td>
							<td class="px-4 py-3 text-muted-foreground">{sp.theme}</td>
							<td class="px-4 py-3">
								{#if sp.published}
									<span class="inline-flex items-center gap-1.5 rounded-full border border-success/25 bg-success/10 px-2.5 py-0.5 text-xs font-medium text-success">
										<span class="dot dot-up"></span>{m.status_pages_published()}
									</span>
								{:else}
									<span class="inline-flex items-center gap-1.5 rounded-full border border-border bg-muted/40 px-2.5 py-0.5 text-xs font-medium text-muted-foreground">
										<span class="dot dot-muted"></span>{m.status_pages_draft()}
									</span>
								{/if}
							</td>
							<td class="px-4 py-3 text-right">
								<div class="flex justify-end gap-1">
									<button
										onclick={() => viewPublic(sp.slug)}
										class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
										title={m.status_pages_page_view_public()}
										aria-label={m.status_pages_page_view_public()}
									>
										<ExternalLink class="h-4 w-4" />
									</button>
									<button
										onclick={() => openEdit(sp)}
										class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
										title={m.btn_edit()}
										aria-label={m.btn_edit()}
									>
										<Edit2 class="h-4 w-4" />
									</button>
									<button
										onclick={() => handleDelete(sp.id, sp.title)}
										class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-danger"
										title={m.btn_delete()}
										aria-label={m.btn_delete()}
									>
										<Trash2 class="h-4 w-4" />
									</button>
								</div>
							</td>
						</tr>
					{/each}
				</tbody>
			</table>
		</div>

		<!-- Mobile cards (hidden on md+) -->
		<div class="space-y-3 md:hidden">
			{#each statusPages as sp (sp.id)}
				<div class="rounded-xl border border-border bg-card p-4 transition-colors hover:border-border/80">
					<div class="flex items-start justify-between gap-2">
						<div class="min-w-0">
							<div class="truncate font-medium">{sp.title}</div>
							<div class="truncate font-mono text-xs text-muted-foreground">/{sp.slug}</div>
						</div>
						{#if sp.published}
							<span class="inline-flex shrink-0 items-center gap-1.5 rounded-full border border-success/25 bg-success/10 px-2.5 py-0.5 text-xs font-medium text-success">
								<span class="dot dot-up"></span>Published
							</span>
						{:else}
							<span class="inline-flex shrink-0 items-center gap-1.5 rounded-full border border-border bg-muted/40 px-2.5 py-0.5 text-xs font-medium text-muted-foreground">
								<span class="dot dot-muted"></span>Draft
							</span>
						{/if}
					</div>
					<div class="mt-2 text-xs text-muted-foreground">{m.status_pages_page_theme_label({ theme: sp.theme })}</div>
					<div class="mt-3 flex items-center gap-2 border-t border-border pt-3">
						<button
							onclick={() => viewPublic(sp.slug)}
							class="inline-flex items-center gap-1 rounded-lg border border-border px-2 py-1 text-xs transition-colors hover:bg-accent"
						>
							<ExternalLink class="h-3.5 w-3.5" /> {m.monitors_page_view()}
						</button>
						<button
							onclick={() => openEdit(sp)}
							class="inline-flex items-center gap-1 rounded-lg border border-border px-2 py-1 text-xs transition-colors hover:bg-accent"
						>
							<Edit2 class="h-3.5 w-3.5" /> {m.btn_edit()}
						</button>
						<button
							onclick={() => handleDelete(sp.id, sp.title)}
							class="inline-flex items-center gap-1 rounded-lg border border-destructive/25 px-2 py-1 text-xs text-destructive transition-colors hover:bg-destructive/10"
						>
							<Trash2 class="h-3.5 w-3.5" /> {m.btn_delete()}
						</button>
					</div>
				</div>
			{/each}
		</div>
	{/if}
</div>

{#if showForm}
	<StatusPageForm
		statusPage={editing || undefined}
		onSaved={handleSaved}
		onClose={() => (showForm = false)}
	/>
{/if}
