<script lang="ts">
	import MaintenanceForm from '$lib/components/MaintenanceForm.svelte';
	import { maintenanceApi, type MaintenanceWindow } from '$lib/api/maintenance';
	import { auth } from '$lib/stores/auth.svelte.ts';
	import EmptyState from '$lib/components/EmptyState.svelte';
	import Skeleton from '$lib/components/Skeleton.svelte';
	import { Plus, Edit2, Trash2, CalendarClock, AlertTriangle } from '@lucide/svelte';
	import { confirmAction } from '$lib/stores/confirm.svelte';
	import { toast } from 'svelte-sonner';
	import * as m from '$lib/paraglide/messages.js';

	let windows = $state<MaintenanceWindow[]>([]);
	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let showForm = $state(false);
	let editing = $state<MaintenanceWindow | null>(null);

	const isUserAdmin = $derived(auth.user?.is_admin ?? false);
	const canManage = $derived(isUserAdmin || (auth.user?.can_manage_maintenance ?? false));

	async function load() {
		loading = true;
		loadError = null;
		try {
			windows = await maintenanceApi.list();
		} catch (e: unknown) {
			const msg =
				e && typeof e === 'object' && 'message' in e ? String((e as { message: string }).message) : m.maintenance_page_load_failed();
			loadError = msg;
			toast.error(msg);
			windows = [];
		} finally {
			loading = false;
		}
	}

	$effect(() => {
		load();
	});

	function openCreate() {
		editing = null;
		showForm = true;
	}

	function openEdit(w: MaintenanceWindow) {
		editing = w;
		showForm = true;
	}

	async function handleDelete(id: number, title: string) {
		const ok = await confirmAction({
			title: m.maintenance_page_delete_title({ title }),
			message: m.maintenance_page_delete_message(),
			confirmLabel: m.maintenance_page_delete_confirm(),
			destructive: true
		});
		if (!ok) return;
		try {
			await maintenanceApi.remove(id);
			toast.success(m.maintenance_page_deleted_toast());
			await load();
		} catch (e: unknown) {
			const msg =
				e && typeof e === 'object' && 'message' in e ? String((e as { message: string }).message) : m.maintenance_page_delete_failed();
			toast.error(msg);
		}
	}

	// A window with no monitors linked suppresses nothing. The old label called
	// that "All monitors", which read as the exact opposite of what it does.
	function monitorCountLabel(w: MaintenanceWindow): string {
		const n = w.monitor_ids?.length ?? 0;
		if (n === 0) return m.maintenance_page_no_monitors();
		return n === 1 ? m.maintenance_page_monitor_count_one() : m.maintenance_page_monitor_count_many({ count: n });
	}

	function formatRange(w: MaintenanceWindow): string {
		if (w.strategy === 'cron') {
			return `${w.cron_expr ?? '—'} · ${w.duration ?? 0} min`;
		}
		const s = w.start_date ? new Date(w.start_date).toLocaleString() : '—';
		const e = w.end_date ? new Date(w.end_date).toLocaleString() : '—';
		return `${s} → ${e}`;
	}

	// Status pill class for active/disabled maintenance windows.
	function statusClass(active: boolean): string {
		return active
			? 'bg-success/10 text-success border-success/25'
			: 'bg-muted/40 text-muted-foreground border-border';
	}
	function statusDotClass(active: boolean): string {
		return active ? 'dot-up' : 'dot-muted';
	}
</script>

<svelte:head>
	<title>{m.app_name()} · {m.maintenance_title()}</title>
</svelte:head>

<div class="space-y-6">
	<!-- Page heading row -->
	<div class="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
		<div>
			<h1 class="text-2xl font-semibold tracking-tight">{m.nav_maintenance()}</h1>
			<p class="mt-1 text-sm text-muted-foreground">
				{m.maintenance_page_subtitle()}
			</p>
		</div>
		{#if canManage}
			<button
				type="button"
				onclick={openCreate}
				class="inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
			>
				<Plus class="h-4 w-4" />
				{m.maintenance_page_add_window()}
			</button>
		{/if}
	</div>

	{#snippet retryLoadAction()}
		<button type="button" onclick={load} class="inline-flex items-center rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">{m.monitor_group_form_retry()}</button>
	{/snippet}

	<!-- Mobile cards (hidden on md+) -->
	<div class="space-y-3 md:hidden">
		{#if loading}
			<div class="space-y-3" role="status">
				<span class="sr-only">{m.maintenance_page_loading()}</span>
				{#each Array(3) as _}
					<div class="rounded-xl border border-border bg-card p-4">
						<div class="flex items-center justify-between gap-4">
							<Skeleton class="h-5 w-44" />
							<Skeleton class="h-6 w-20 rounded-full" />
						</div>
						<Skeleton class="mt-4 h-3 w-32" />
						<Skeleton class="mt-2 h-3 w-56 max-w-full" />
					</div>
				{/each}
			</div>
		{:else if loadError}
			<EmptyState icon={AlertTriangle} title={m.maintenance_page_load_failed()} description={loadError} action={retryLoadAction} />
		{:else if windows.length === 0}
			{#snippet emptyAction()}
				{#if canManage}
					<button type="button" onclick={openCreate} class="inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90">
						<Plus class="h-4 w-4" /> {m.maintenance_page_add_window()}
					</button>
				{/if}
			{/snippet}
			<EmptyState icon={CalendarClock} title={m.maintenance_page_empty_title()} description={m.maintenance_page_empty_description()} action={emptyAction} />
		{:else}
			{#each windows as w (w.id)}
				<div class="rounded-xl border border-border bg-card p-4 transition-colors hover:border-border/80">
					<div class="flex items-start justify-between">
						<div class="flex min-w-0 items-center gap-2">
							<CalendarClock class="h-4 w-4 shrink-0 text-muted-foreground" />
							<span class="truncate font-medium">{w.title}</span>
						</div>
						<span
							class="inline-flex shrink-0 items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium {statusClass(w.active)}"
						>
							<span class="dot {statusDotClass(w.active)}"></span>
							{w.active ? m.notifications_active() : m.notifications_disabled()}
						</span>
					</div>
					<div class="mt-2 space-y-1 text-xs text-muted-foreground">
						<div class="font-mono">{w.strategy}</div>
						<div>{formatRange(w)}</div>
						<div>{monitorCountLabel(w)}</div>
					</div>
					<div class="mt-3 flex items-center gap-2 border-t border-border pt-3">
						{#if canManage}
							<button
								type="button"
								onclick={() => openEdit(w)}
								class="inline-flex items-center gap-1 rounded-lg border border-border px-2 py-1 text-xs transition-colors hover:bg-accent"
							>
								<Edit2 class="h-3.5 w-3.5" /> {m.btn_edit()}
							</button>
							<button
								type="button"
								onclick={() => handleDelete(w.id, w.title)}
								class="inline-flex items-center gap-1 rounded-lg border border-destructive/25 px-2 py-1 text-xs text-destructive transition-colors hover:bg-destructive/10"
							>
								<Trash2 class="h-3.5 w-3.5" /> {m.btn_delete()}
							</button>
						{/if}
					</div>
				</div>
			{/each}
		{/if}
	</div>

	<!-- Desktop table (hidden on mobile) -->
	<div class="hidden overflow-x-auto rounded-xl border border-border bg-card md:block">
		<table class="w-full text-sm">
			<thead>
				<tr class="border-b border-border text-left">
					<th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">{m.maintenance_page_col_title()}</th>
					<th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">{m.maintenance_form_strategy_label()}</th>
					<th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">{m.maintenance_page_col_schedule()}</th>
					<th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">{m.nav_monitors()}</th>
					<th class="px-4 py-3 text-[11px] font-semibold uppercase tracking-wider text-faint">{m.monitors_page_col_status()}</th>
					<th class="w-24 px-4 py-3 text-right text-[11px] font-semibold uppercase tracking-wider text-faint">{m.monitors_page_col_actions()}</th>
				</tr>
			</thead>
			<tbody>
				{#if loading}
					{#each Array(3) as _}
						<tr class="border-b border-border last:border-0" aria-hidden="true">
							<td class="px-4 py-4"><Skeleton class="h-4 w-36" /></td>
							<td class="px-4 py-4"><Skeleton class="h-4 w-20" /></td>
							<td class="px-4 py-4"><Skeleton class="h-4 w-48" /></td>
							<td class="px-4 py-4"><Skeleton class="h-4 w-10" /></td>
							<td class="px-4 py-4"><Skeleton class="h-6 w-20 rounded-full" /></td>
							<td class="px-4 py-4"><Skeleton class="ml-auto h-8 w-16" /></td>
						</tr>
					{/each}
				{:else if loadError}
					<tr>
						<td colspan="6" class="p-6">
							<EmptyState icon={AlertTriangle} title={m.maintenance_page_load_failed()} description={loadError} action={retryLoadAction} />
						</td>
					</tr>
				{:else if windows.length === 0}
					<tr>
						<td colspan="6" class="p-6">
							<EmptyState icon={CalendarClock} title={m.maintenance_page_empty_title()} description={m.maintenance_page_empty_description()} />
						</td>
					</tr>
				{:else}
					{#each windows as w (w.id)}
						<tr class="border-b border-border transition-colors last:border-0 hover:bg-accent/40">
							<td class="px-4 py-3 font-medium">
								<div class="flex items-center gap-2">
									<CalendarClock class="h-4 w-4 shrink-0 text-muted-foreground" />
									{w.title}
								</div>
							</td>
							<td class="px-4 py-3 font-mono text-xs text-muted-foreground">{w.strategy}</td>
							<td class="px-4 py-3 text-xs text-muted-foreground">{formatRange(w)}</td>
							<td class="px-4 py-3 text-xs tnum text-muted-foreground">
								{w.monitor_ids?.length ?? 0}
							</td>
							<td class="px-4 py-3">
								<span
									class="inline-flex items-center gap-1.5 rounded-full border px-2.5 py-0.5 text-xs font-medium {statusClass(w.active)}"
								>
									<span class="dot {statusDotClass(w.active)}"></span>
									{w.active ? m.notifications_active() : m.notifications_disabled()}
								</span>
							</td>
							<td class="px-4 py-3 text-right">
								<div class="flex justify-end gap-1">
									{#if canManage}
										<button
											type="button"
											onclick={() => openEdit(w)}
											class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
											title={m.btn_edit()}
											aria-label={m.btn_edit()}
										>
											<Edit2 class="h-4 w-4" />
										</button>
										<button
											type="button"
											onclick={() => handleDelete(w.id, w.title)}
											class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-danger"
											title={m.btn_delete()}
											aria-label={m.btn_delete()}
										>
											<Trash2 class="h-4 w-4" />
										</button>
									{/if}
								</div>
							</td>
						</tr>
					{/each}
				{/if}
			</tbody>
		</table>
	</div>
</div>

{#if showForm}
	<MaintenanceForm
		window={editing}
		onSaved={async () => {
			showForm = false;
			await load();
		}}
		onClose={() => (showForm = false)}
	/>
{/if}
