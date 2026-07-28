<script lang="ts">
	import { page } from '$app/stores';
	import { realtime } from '$lib/stores/ws.svelte.js';
	import {
		statusPagesApi,
		type StatusPage,
		type Incident,
		type IncidentTimelineStatus,
		type StatusPageCNAME,
		type StatusPageMonitorLink,
		type StatusPageSubscriber,
		type SubscriptionChannel,
	} from '$lib/api/statuspages';
	import { monitorsApi } from '$lib/api/monitors';
	import { notificationsApi, type Notification } from '$lib/api/notifications';
	import type { Monitor } from '$lib/stores/ws.svelte.js';
	import StatusPageForm from '$lib/components/StatusPageForm.svelte';
	import MonitorPicker from '$lib/components/MonitorPicker.svelte';
	import StatusPill from '$lib/components/StatusPill.svelte';
	import EmptyState from '$lib/components/EmptyState.svelte';
	import Skeleton from '$lib/components/Skeleton.svelte';
	import MarkdownPreview from '$lib/components/MarkdownPreview.svelte';
	import { confirmAction } from '$lib/stores/confirm.svelte';
	import { toast } from 'svelte-sonner';
	import {
		ArrowLeft,
		Plus,
		CheckCircle,
		ArrowUp,
		ArrowDown,
		AlertTriangle,
	} from '@lucide/svelte';
	import { goto } from '$app/navigation';
	import Select from '$lib/components/Select.svelte';

	let statusPageId = $derived(Number($page.params.id));
	let statusPage = $state<StatusPage | null>(null);
	// Raw status_page_monitors link rows (monitor_id + display_order only —
	// see StatusPageMonitorLink). Joined against `allMonitors` below to get
	// display data; the backend's ListMonitors endpoint does not return it.
	let assignedLinks = $state<StatusPageMonitorLink[]>([]);
	let allMonitors = $state<Monitor[]>([]);
	let monitorsLoading = $state(true);
	let monitorsError = $state<string | null>(null);
	let reordering = $state(false);
	let incidents = $state<Incident[]>([]);
	let cnames = $state<StatusPageCNAME[]>([]);
	let subscribers = $state<StatusPageSubscriber[]>([]);
	let subscriptionChannel = $state<SubscriptionChannel | null>(null);
	let smtpNotifications = $state<Notification[]>([]);
	let channelSaving = $state(false);
	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let showEditForm = $state(false);
	let updateStatus = $state<Record<number, IncidentTimelineStatus>>({});
	let updateContent = $state<Record<number, string>>({});

	let newIncident = $state({
		title: '',
		content: '',
		style: 'warning' as const,
	});
	let newCNAME = $state('');

	// assignedLinks arrives pre-sorted by display_order ASC (sqlite/mariadb repo
	// ListByStatusPage), so this preserves the intended display order.
	const monitorById = $derived(new Map(allMonitors.map((m) => [m.id, m])));
	const assignedRows = $derived.by(() => {
		void realtime.heartbeatSeq;
		const hbMap = realtime.heartbeats;
		// WS monitors carry resolved status from the hub (latest heartbeat).
		// Use as a fallback between the heartbeat map and the REST data,
		// because the REST MonitorView has no status field.
		const wsStatusMap = new Map(
			realtime.monitors.map((m) => [m.id, m.status]),
		);
		return assignedLinks.map((link) => {
			const base = monitorById.get(link.monitor_id) ?? null;
			if (!base) return { ...link, monitor: null };
			// Prefer live heartbeat status, then WS monitor status, then REST.
			const live = hbMap.get(link.monitor_id);
			const status =
				live?.status ?? wsStatusMap.get(link.monitor_id) ?? base.status;
			return { ...link, monitor: { ...base, status } };
		});
	});
	const assignedMonitorIds = $derived(
		new Set(assignedLinks.map((l) => l.monitor_id)),
	);
	const activeIncidents = $derived(
		incidents.filter((incident) => incident.active),
	);

	$effect(() => {
		if (statusPageId) {
			loadAll();
			loadMonitors();
		}
	});

	async function loadAll() {
		loading = true;
		loadError = null;
		try {
			statusPage = await statusPagesApi.get(statusPageId);
			assignedLinks = await statusPagesApi.listMonitors(statusPageId);
			incidents = await statusPagesApi.listIncidents(statusPageId);
			cnames = await statusPagesApi.listCNAMEs(statusPageId);
			const [subs, channel, notifs] = await Promise.all([
				statusPagesApi.listSubscribers(statusPageId).catch(() => [] as StatusPageSubscriber[]),
				statusPagesApi.getSubscriptionChannel(statusPageId).catch(() => null),
				notificationsApi.list().catch(() => [] as Notification[]),
			]);
			subscribers = subs;
			subscriptionChannel = channel;
			smtpNotifications = notifs.filter((n) => n.type === 'smtp' && n.active);
		} catch (e: any) {
			const message = e?.message || 'Load failed';
			loadError = message;
			toast.error(message);
		} finally {
			loading = false;
		}
	}

	async function saveSubscriptionChannel(notificationId: number | null) {
		if (channelSaving) return;
		channelSaving = true;
		try {
			if (notificationId == null || notificationId === 0) {
				await statusPagesApi.clearSubscriptionChannel(statusPageId);
				subscriptionChannel = null;
				toast.success('Email subscription channel cleared');
			} else {
				subscriptionChannel = await statusPagesApi.setSubscriptionChannel(
					statusPageId,
					notificationId,
				);
				toast.success('Email subscription channel saved');
			}
		} catch (e: any) {
			toast.error(e?.message || 'Failed to update subscription channel');
		} finally {
			channelSaving = false;
		}
	}

	async function removeSubscriber(sub: StatusPageSubscriber) {
		const ok = await confirmAction({
			title: `Remove subscriber ${sub.email}?`,
			message: 'They will stop receiving incident and maintenance emails for this page.',
			confirmLabel: 'Remove subscriber',
			destructive: true,
		});
		if (!ok) return;
		try {
			await statusPagesApi.removeSubscriber(statusPageId, sub.id);
			toast.success('Subscriber removed');
			subscribers = subscribers.filter((s) => s.id !== sub.id);
		} catch (e: any) {
			toast.error(e?.message || 'Remove failed');
		}
	}

	async function loadMonitors() {
		monitorsLoading = true;
		monitorsError = null;
		try {
			allMonitors = await monitorsApi.list();
		} catch (e: any) {
			monitorsError = e?.message || 'Failed to load monitors';
		} finally {
			monitorsLoading = false;
		}
	}

	function defaultNextIncidentStatus(inc: Incident): IncidentTimelineStatus {
		const last = inc.updates?.at(-1)?.status;
		if (last === 'investigating') return 'identified';
		if (last === 'identified') return 'monitoring';
		if (last === 'monitoring') return 'resolved';
		if (last === 'resolved') return 'resolved';
		return 'investigating';
	}

	function selectedIncidentStatus(inc: Incident): IncidentTimelineStatus {
		return updateStatus[inc.id] ?? defaultNextIncidentStatus(inc);
	}

	async function addIncidentUpdate(inc: Incident) {
		const content = (updateContent[inc.id] ?? '').trim();
		if (!content) {
			toast.error('Update details are required');
			return;
		}
		try {
			await statusPagesApi.createIncidentUpdate(statusPageId, inc.id, {
				status: selectedIncidentStatus(inc),
				content,
			});
			toast.success('Incident update posted');
			updateContent = { ...updateContent, [inc.id]: '' };
			const nextUpdateStatus = { ...updateStatus };
			delete nextUpdateStatus[inc.id];
			updateStatus = nextUpdateStatus;
			await loadAll();
		} catch (e: any) {
			toast.error(e?.message || 'Post update failed');
		}
	}

	async function saveIncident() {
		if (!newIncident.title.trim()) return;
		try {
			await statusPagesApi.createIncident(statusPageId, newIncident);
			toast.success('Incident created');
			newIncident = { title: '', content: '', style: 'warning' };
			await loadAll();
		} catch (e: any) {
			toast.error(e?.message || 'Create incident failed');
		}
	}

	async function resolveIncident(iid: number) {
		try {
			// Real resolve path: POST …/incidents/:id/resolve → Active=false.
			// Do NOT use updateIncident({ resolved_at }) — the backend never binds that field.
			await statusPagesApi.resolveIncident(statusPageId, iid);
			toast.success('Incident resolved');
			await loadAll();
		} catch (e: any) {
			toast.error(e?.message || 'Resolve failed');
		}
	}

	async function addCNAME() {
		const domain = newCNAME.trim().toLowerCase();
		if (!domain) return;
		try {
			await statusPagesApi.addCNAME(statusPageId, domain);
			toast.success('Custom domain added');
			newCNAME = '';
			await loadAll();
		} catch (e: any) {
			toast.error(e?.message || 'Add domain failed');
		}
	}

	async function removeCNAME(cnameId: number) {
		const domain = cnames.find((c) => c.id === cnameId)?.domain;
		const ok = await confirmAction({
			title: domain ? `Remove custom domain "${domain}"?` : 'Remove this custom domain?',
			message:
				'Anyone visiting the status page on that domain will stop reaching it. The page stays available on its normal URL.',
			confirmLabel: 'Remove domain',
			destructive: true
		});
		if (!ok) return;
		try {
			await statusPagesApi.removeCNAME(statusPageId, cnameId);
			toast.success('Custom domain removed');
			await loadAll();
		} catch (e: any) {
			toast.error(e?.message || 'Remove domain failed');
		}
	}

	async function addMonitor(monitor: Monitor) {
		try {
			// Append after the current max display_order so new additions land at
			// the bottom instead of colliding with the backend's default (1000).
			const nextOrder =
				Math.max(0, ...assignedLinks.map((l) => l.display_order)) + 10;
			await statusPagesApi.addMonitor(statusPageId, monitor.id, nextOrder);
			toast.success(`${monitor.name} added`);
			await loadAll();
		} catch (e: any) {
			toast.error(e?.message || 'Add failed');
		}
	}

	async function removeMonitor(mid: number) {
		const name = monitorById.get(mid)?.name;
		const ok = await confirmAction({
			title: name ? `Remove "${name}" from this status page?` : 'Remove this monitor from the status page?',
			message:
				'It stops being shown to the public. The monitor itself keeps running and its history is untouched.',
			confirmLabel: 'Remove monitor',
			destructive: true
		});
		if (!ok) return;
		try {
			await statusPagesApi.removeMonitor(statusPageId, mid);
			toast.success('Monitor removed');
			await loadAll();
		} catch (e: any) {
			toast.error(e?.message || 'Remove failed');
		}
	}

	/**
	 * Move an assigned monitor up/down. Persists the entire reordered list in
	 * a single transactional PUT, eliminating the old remove-then-re-add race
	 * that could silently drop a monitor from the public page.
	 */
	async function moveMonitor(index: number, direction: -1 | 1) {
		const other = index + direction;
		if (reordering || other < 0 || other >= assignedRows.length) return;

		const next = [...assignedRows];
		[next[index], next[other]] = [next[other], next[index]];
		const orderedIds = next.map((r) => r.monitor_id);

		reordering = true;
		try {
			await statusPagesApi.reorderMonitors(statusPageId, orderedIds);
		} catch (e: any) {
			toast.error(e?.message || 'Reorder failed — list refreshed from server');
		} finally {
			await loadAll();
			reordering = false;
		}
	}

	function goBack() {
		goto('/status-pages');
	}

	// Incident style → status pill classes.
	const incidentStyleClass: Record<string, string> = {
		info: 'border-border bg-muted/40 text-muted-foreground',
		warning: 'border-warning/25 bg-warning/10 text-warning',
		danger: 'border-danger/25 bg-danger/10 text-danger',
		success: 'border-success/25 bg-success/10 text-success',
	};
	const incidentDotClass: Record<string, string> = {
		info: 'dot-muted',
		warning: 'dot-warn',
		danger: 'dot-down',
		success: 'dot-up',
	};

	// Shared, token-consistent class strings.
	const inputClass =
		'w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring';
	const primaryBtn =
		'inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60';
	const ghostBtn =
		'inline-flex items-center gap-2 rounded-lg border border-border px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground';
</script>

<svelte:head>
	<title>Phoenix · {statusPage?.title ?? 'Status Page'}</title>
</svelte:head>

<div class="space-y-6">
	<!-- Back link -->
	<button
		onclick={goBack}
		class="inline-flex items-center gap-2 text-sm text-muted-foreground transition-colors hover:text-foreground"
	>
		<ArrowLeft class="h-4 w-4" /> Back to Status Pages
	</button>

	{#snippet retryLoadAction()}
		<button type="button" onclick={loadAll} class={ghostBtn}>Retry</button>
	{/snippet}

	{#if loading}
		<div class="space-y-5" role="status">
			<span class="sr-only">Loading status page…</span>
			<div class="flex items-start justify-between gap-4">
				<div class="space-y-3"><Skeleton class="h-8 w-64 max-w-full" /><Skeleton class="h-4 w-40" /></div>
				<Skeleton class="h-9 w-28" />
			</div>
			{#each Array(3) as _}
				<div class="rounded-xl border border-border bg-card p-5"><Skeleton class="h-5 w-40" /><Skeleton class="mt-5 h-24 w-full" /></div>
			{/each}
		</div>
	{:else if loadError}
		<EmptyState icon={AlertTriangle} title="Status page could not be loaded" description={loadError} action={retryLoadAction} />
	{:else if !statusPage}
		<EmptyState icon={AlertTriangle} title="Status page not found" description="Return to the status page list and choose another page." />
	{:else}
		<!-- Header -->
		<div
			class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between"
		>
			<div class="min-w-0">
				<h1 class="truncate text-2xl font-semibold tracking-tight">
					{statusPage.title}
				</h1>
				<p class="mt-1 font-mono text-sm text-muted-foreground">
					/{statusPage.slug} • {statusPage.theme} theme
				</p>
			</div>
			<button onclick={() => (showEditForm = true)} class="{ghostBtn} shrink-0"
				>Edit Settings</button
			>
		</div>

		<!-- Monitors -->
		<div class="rounded-xl border border-border bg-card p-5">
			<div
				class="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between"
			>
				<div>
					<h3 class="text-sm font-semibold tracking-tight">
						Assigned Monitors
					</h3>
					<p class="mt-0.5 text-xs text-muted-foreground">
						Search by name to add a monitor to this page.
					</p>
				</div>
				<div class="w-full sm:w-72">
					<MonitorPicker
						monitors={allMonitors}
						exclude={assignedMonitorIds}
						loading={monitorsLoading}
						error={monitorsError}
						placeholder="Add a monitor…"
						emptyText={allMonitors.length === 0
							? 'No monitors yet — create one first.'
							: 'All monitors are already on this page.'}
						onSelect={addMonitor}
					/>
				</div>
			</div>
			{#if assignedRows.length === 0}
				<p class="mt-3 text-sm text-muted-foreground">
					No monitors linked yet. Add some to display on the public page.
				</p>
			{:else}
				<div class="mt-3 overflow-hidden rounded-lg border border-border">
					{#each assignedRows as row, i (row.id)}
						<div
							class="flex items-center justify-between gap-3 px-4 py-3 text-sm transition-colors hover:bg-accent/40 {i !==
							assignedRows.length - 1
								? 'border-b border-border'
								: ''}"
						>
							<div class="flex min-w-0 items-center gap-3">
								{#if row.monitor}
									<div class="min-w-0">
										<p class="truncate font-medium">{row.monitor.name}</p>
										<p class="truncate font-mono text-xs text-muted-foreground">
											<span class="text-faint uppercase"
												>{row.monitor.type}</span
											>
											{#if row.monitor.target}<span class="mx-1 text-faint"
													>·</span
												>{row.monitor.target}{/if}
										</p>
									</div>
									<StatusPill status={row.monitor.status} />
								{:else}
									<div class="min-w-0">
										<p class="truncate font-medium text-muted-foreground">
											Unknown monitor (#{row.monitor_id})
										</p>
										<p class="text-xs text-faint">Deleted or inaccessible</p>
									</div>
								{/if}
							</div>
							<div class="flex shrink-0 items-center gap-1">
								<button
									type="button"
									onclick={() => moveMonitor(i, -1)}
									disabled={i === 0 || reordering}
									aria-label="Move {row.monitor?.name ?? 'monitor'} up"
									class="grid h-7 w-7 place-items-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:pointer-events-none disabled:opacity-30"
								>
									<ArrowUp class="h-3.5 w-3.5" />
								</button>
								<button
									type="button"
									onclick={() => moveMonitor(i, 1)}
									disabled={i === assignedRows.length - 1 || reordering}
									aria-label="Move {row.monitor?.name ?? 'monitor'} down"
									class="grid h-7 w-7 place-items-center rounded-md text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:pointer-events-none disabled:opacity-30"
								>
									<ArrowDown class="h-3.5 w-3.5" />
								</button>
								<button
									onclick={() => removeMonitor(row.monitor_id)}
									class="ml-1 shrink-0 text-xs text-danger transition-colors hover:text-danger/80"
									>Remove</button
								>
							</div>
						</div>
					{/each}
				</div>
			{/if}
		</div>

		<!-- Email subscriptions -->
		<div class="rounded-xl border border-border bg-card p-5">
			<div>
				<h3 class="text-sm font-semibold tracking-tight">Email Subscriptions</h3>
				<p class="mt-0.5 text-xs text-muted-foreground">
					Choose an active SMTP notification to send confirmation, incident, and
					maintenance emails. Requires PUBLIC_URL on the server.
				</p>
			</div>
			<div class="mt-4 flex flex-col gap-3 sm:flex-row sm:items-center">
				<div class="w-full sm:max-w-md">
					<Select
						options={[
							{ value: '', label: 'No SMTP channel (disabled)' },
							...smtpNotifications.map((n) => ({
								value: String(n.id),
								label: n.name,
							})),
						]}
						value={subscriptionChannel ? String(subscriptionChannel.notification_id) : ''}
						ariaLabel="SMTP notification channel"
						disabled={channelSaving}
						onValueChange={(v) => {
							const id = v === '' ? null : Number(v);
							void saveSubscriptionChannel(id);
						}}
						class="w-full"
					/>
				</div>
				{#if smtpNotifications.length === 0}
					<p class="text-xs text-muted-foreground">
						No active SMTP notifications. Create one under Notifications first.
					</p>
				{/if}
			</div>

			<div class="mt-6">
				<h4 class="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
					Subscribers ({subscribers.length})
				</h4>
				{#if subscribers.length === 0}
					<p class="mt-2 text-sm text-muted-foreground">No subscribers yet.</p>
				{:else}
					<div class="mt-2 overflow-hidden rounded-lg border border-border">
						{#each subscribers as sub, i (sub.id)}
							<div
								class="flex items-center justify-between gap-3 px-4 py-3 text-sm transition-colors hover:bg-accent/40 {i !==
								subscribers.length - 1
									? 'border-b border-border'
									: ''}"
							>
								<div class="min-w-0">
									<p class="truncate font-mono text-xs">{sub.email}</p>
									<p class="text-xs text-muted-foreground">
										{sub.active ? 'Confirmed' : 'Pending confirmation'}
										{#if sub.confirmed_at}
											· {new Date(sub.confirmed_at).toLocaleDateString()}
										{/if}
									</p>
								</div>
								<button
									type="button"
									onclick={() => removeSubscriber(sub)}
									class="shrink-0 text-xs text-danger transition-colors hover:text-danger/80"
									>Remove</button
								>
							</div>
						{/each}
					</div>
				{/if}
			</div>
		</div>

		<!-- Custom domains (CNAME aliases) -->
		<div class="rounded-xl border border-border bg-card p-5">
			<div class="flex items-center justify-between gap-3">
				<div>
					<h3 class="text-sm font-semibold tracking-tight">Custom Domains</h3>
					<p class="mt-0.5 text-xs text-muted-foreground">
						Point a CNAME at this Phoenix host, then add the hostname here.
					</p>
				</div>
				<div class="flex gap-2">
					<input
						bind:value={newCNAME}
						placeholder="status.example.com"
						class="{inputClass} w-44 px-2 py-1 text-xs"
					/>
					<button
						onclick={addCNAME}
						class="inline-flex items-center gap-1 rounded-lg bg-primary px-3 py-1 text-xs font-medium text-primary-foreground transition-colors hover:bg-primary/90"
					>
						<Plus class="h-3 w-3" /> Add
					</button>
				</div>
			</div>
			{#if cnames.length === 0}
				<p class="mt-3 text-sm text-muted-foreground">No custom domains yet.</p>
			{:else}
				<div class="mt-3 overflow-hidden rounded-lg border border-border">
					{#each cnames as cn, i (cn.id)}
						<div
							class="flex items-center justify-between gap-3 px-4 py-3 text-sm transition-colors hover:bg-accent/40 {i !==
							cnames.length - 1
								? 'border-b border-border'
								: ''}"
						>
							<span class="truncate font-mono text-xs">{cn.domain}</span>
							<button
								onclick={() => removeCNAME(cn.id)}
								class="shrink-0 text-xs text-danger transition-colors hover:text-danger/80"
								>Remove</button
							>
						</div>
					{/each}
				</div>
			{/if}
		</div>

		<!-- Incidents -->
		<div class="rounded-xl border border-border bg-card p-5">
			<div class="flex items-center justify-between gap-3">
				<div>
					<h3 class="text-sm font-semibold tracking-tight">Active Incidents</h3>
					<p class="mt-0.5 text-xs text-muted-foreground">
						Only ongoing incidents appear here and on the public status page.
					</p>
				</div>
				<a
					href="/incidents"
					class="shrink-0 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
				>
					View history
				</a>
			</div>

			<div class="mt-4 rounded-lg border border-border bg-surface/60 p-4">
				<div class="mb-2 text-sm font-medium">Create New Incident</div>
				<div class="grid grid-cols-1 gap-2 md:grid-cols-4">
					<input
						bind:value={newIncident.title}
						placeholder="Title e.g. API outage"
						class="{inputClass} px-3 py-1.5 text-sm md:col-span-2"
					/>
					<div class="w-32">
						<Select
							options={[
								{ value: 'info', label: 'Info' },
								{ value: 'warning', label: 'Warning' },
								{ value: 'danger', label: 'Danger' },
								{ value: 'success', label: 'Success' },
							]}
							value={newIncident.style}
							ariaLabel="Incident severity"
							onValueChange={(v) => { newIncident.style = v as typeof newIncident.style; }}
							class="w-full"
						/>
					</div>
					<button
						onclick={saveIncident}
						class="{primaryBtn} justify-center px-3 py-1.5 text-sm">Post</button
					>
				</div>
				<textarea
					bind:value={newIncident.content}
					placeholder="Details…"
					class="{inputClass} mt-2 px-3 py-1.5 text-sm"
					rows="2"
				></textarea>
			</div>

			{#if activeIncidents.length > 0}
				<div class="mt-6 space-y-3">
					{#each activeIncidents as inc (inc.id)}
						<div
							class="rounded-xl border border-border bg-card p-4 transition-colors {!inc.active
								? 'opacity-60'
								: 'hover:border-border/80'}"
						>
							<div class="flex items-start justify-between gap-3">
								<div class="flex min-w-0 items-center gap-2">
									<span class="truncate font-medium">{inc.title}</span>
									<span
										class="inline-flex shrink-0 items-center gap-1.5 rounded-full border px-2 py-0.5 text-[11px] font-medium {incidentStyleClass[
											inc.style
										] ?? incidentStyleClass.info}"
									>
										<span
											class="dot {incidentDotClass[inc.style] ?? 'dot-muted'}"
										></span>
										{inc.style}
									</span>
								</div>
								<div class="shrink-0 text-xs text-muted-foreground">
									{new Date(inc.created_at).toLocaleDateString()}
								</div>
							</div>
							{#if inc.content}
								<MarkdownPreview
									source={inc.content}
									class="mt-2 text-sm text-muted-foreground"
								/>
							{/if}

							<div class="mt-4 space-y-3 border-t border-border pt-4">
								<div class="text-xs font-semibold uppercase tracking-[0.18em] text-muted-foreground">
									Timeline
								</div>
								{#if inc.updates?.length > 0}
									<ol class="space-y-3">
										{#each inc.updates as update (update.id)}
											<li class="grid grid-cols-[auto_1fr] gap-3">
												<span class="mt-1 h-2 w-2 rounded-full bg-primary"></span>
												<div class="min-w-0">
													<div class="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
														<span class="font-medium capitalize text-foreground">
															{update.status}
														</span>
														<span>{new Date(update.created_at).toLocaleString()}</span>
													</div>
													<MarkdownPreview
														source={update.content}
														class="mt-1 text-sm text-muted-foreground"
													/>
												</div>
											</li>
										{/each}
									</ol>
								{:else}
									<p class="text-sm text-muted-foreground">No timeline updates yet.</p>
								{/if}
							</div>

							{#if inc.active}
								<div class="mt-4 rounded-lg border border-border bg-surface/50 p-3">
									<div class="mb-2 text-xs font-medium text-muted-foreground">
										Post subscriber update
									</div>
									<div class="grid grid-cols-1 gap-2 md:grid-cols-[10rem_1fr_auto]">
										<Select
											options={[
												{ value: 'investigating', label: 'Investigating' },
												{ value: 'identified', label: 'Identified' },
												{ value: 'monitoring', label: 'Monitoring' },
												{ value: 'resolved', label: 'Resolved' },
											]}
											value={selectedIncidentStatus(inc)}
											ariaLabel="Incident update status"
											onValueChange={(v) => {
												updateStatus = {
													...updateStatus,
													[inc.id]: v as IncidentTimelineStatus,
												};
											}}
											class="w-full"
										/>
										<textarea
											value={updateContent[inc.id] ?? ''}
											oninput={(e) => {
												updateContent = {
													...updateContent,
													[inc.id]: e.currentTarget.value,
												};
											}}
											placeholder="Markdown update for subscribers…"
											class="{inputClass} px-3 py-1.5 text-sm"
											rows="2"
										></textarea>
										<button
											onclick={() => addIncidentUpdate(inc)}
											class="{primaryBtn} justify-center px-3 py-1.5 text-sm"
										>
											Post update
										</button>
									</div>
								</div>

								<div class="mt-3 flex gap-2 border-t border-border pt-3">
									<button
										onclick={() => resolveIncident(inc.id)}
										class="inline-flex items-center gap-1 text-xs font-medium text-success transition-colors hover:text-success/80"
										><CheckCircle class="h-3 w-3" /> Resolve</button
									>
								</div>
							{:else}
								<div
									class="mt-3 flex items-center gap-1.5 border-t border-border pt-3 text-xs text-success"
								>
									<span class="dot dot-up"></span>
									Resolved
								</div>
							{/if}
						</div>
					{/each}
				</div>
			{:else}
				<p class="mt-4 text-sm text-muted-foreground">No active incidents.</p>
			{/if}
		</div>
	{/if}
</div>

{#if showEditForm && statusPage}
	<StatusPageForm
		{statusPage}
		onSaved={async () => {
			showEditForm = false;
			await loadAll();
		}}
		onClose={() => (showEditForm = false)}
	/>
{/if}
