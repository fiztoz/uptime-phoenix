<script lang="ts">
	/**
	 * Reverse assignment panel for one escalation policy: lists monitors and
	 * folders that use it, and (for admins) allows assign/unassign via the
	 * existing monitor/group PUT endpoints.
	 */
	import {
		escalationApi,
		type EscalationEntityRef,
		type EscalationPolicyAssignments,
	} from '$lib/api/escalation';
	import { monitorsApi } from '$lib/api/monitors';
	import { monitorGroupsApi, type MonitorGroupView } from '$lib/api/monitorGroups';
	import type { Monitor } from '$lib/stores/ws.svelte.ts';
	import Select from '$lib/components/Select.svelte';
	import { ChevronDown, ChevronRight, Folder, Link2Off, Loader2 } from '@lucide/svelte';
	import { toast } from 'svelte-sonner';
	import * as m from '$lib/paraglide/messages.js';

	interface Props {
		policyId: number;
		/** Assignment writes are admin-only on the backend. */
		canAssign: boolean;
	}

	let { policyId, canAssign }: Props = $props();

	let expanded = $state(false);
	let loading = $state(true);
	let loadError = $state<string | null>(null);
	let assignments = $state<EscalationPolicyAssignments>({ monitors: [], groups: [] });
	let loaded = $state(false);

	let allMonitors = $state<Monitor[]>([]);
	let allGroups = $state<MonitorGroupView[]>([]);
	let pickersLoaded = $state(false);
	let pickersLoading = $state(false);
	let pickersError = $state<string | null>(null);

	let monitorPick = $state('');
	let groupPick = $state('');
	let busyKey = $state<string | null>(null);

	const assignedMonitorIds = $derived(new Set(assignments.monitors.map((x) => x.id)));
	const assignedGroupIds = $derived(new Set(assignments.groups.map((x) => x.id)));

	const monitorOptions = $derived(
		allMonitors
			.filter((mon) => !assignedMonitorIds.has(mon.id))
			.map((mon) => ({ value: String(mon.id), label: mon.name })),
	);
	const groupOptions = $derived(
		allGroups
			.filter((g) => !assignedGroupIds.has(g.id))
			.map((g) => ({ value: String(g.id), label: g.name })),
	);

	async function loadAssignments() {
		loading = true;
		loadError = null;
		try {
			assignments = await escalationApi.listAssignments(policyId);
			loaded = true;
		} catch (error: unknown) {
			loadError =
				error && typeof error === 'object' && 'message' in error
					? String((error as { message: string }).message)
					: m.escalation_assignments_load_failed();
		} finally {
			loading = false;
		}
	}

	async function ensurePickers() {
		if (pickersLoaded || pickersLoading || !canAssign) return;
		pickersLoading = true;
		pickersError = null;
		try {
			const [mons, grps] = await Promise.all([monitorsApi.list(), monitorGroupsApi.list()]);
			allMonitors = mons;
			allGroups = grps;
			pickersLoaded = true;
		} catch (error: unknown) {
			pickersError =
				error && typeof error === 'object' && 'message' in error
					? String((error as { message: string }).message)
					: m.escalation_assignments_load_failed();
		} finally {
			pickersLoading = false;
		}
	}

	// Counts on the card need a fetch even when collapsed. Capture policyId
	// synchronously so a late response from a previous id cannot clobber state.
	$effect(() => {
		const id = policyId;
		let cancelled = false;
		loading = true;
		loadError = null;
		loaded = false;
		void escalationApi
			.listAssignments(id)
			.then((data) => {
				if (cancelled) return;
				assignments = data;
				loaded = true;
			})
			.catch((error: unknown) => {
				if (cancelled) return;
				loadError =
					error && typeof error === 'object' && 'message' in error
						? String((error as { message: string }).message)
						: m.escalation_assignments_load_failed();
			})
			.finally(() => {
				if (!cancelled) loading = false;
			});
		return () => {
			cancelled = true;
		};
	});

	async function toggle() {
		expanded = !expanded;
		if (expanded && canAssign) {
			await ensurePickers();
		}
	}

	async function unassignMonitor(ref: EscalationEntityRef) {
		const key = `m:${ref.id}`;
		busyKey = key;
		try {
			await escalationApi.setForMonitor(ref.id, 0);
			assignments = {
				...assignments,
				monitors: assignments.monitors.filter((x) => x.id !== ref.id),
			};
			toast.success(m.escalation_assignments_unassigned_toast());
		} catch {
			toast.error(m.escalation_assignments_unassign_failed());
		} finally {
			busyKey = null;
		}
	}

	async function unassignGroup(ref: EscalationEntityRef) {
		const key = `g:${ref.id}`;
		busyKey = key;
		try {
			await escalationApi.setForGroup(ref.id, 0);
			assignments = {
				...assignments,
				groups: assignments.groups.filter((x) => x.id !== ref.id),
			};
			toast.success(m.escalation_assignments_unassigned_toast());
		} catch {
			toast.error(m.escalation_assignments_unassign_failed());
		} finally {
			busyKey = null;
		}
	}

	async function assignMonitor(value: string) {
		const id = Number(value);
		if (!id) return;
		const mon = allMonitors.find((x) => x.id === id);
		busyKey = `am:${id}`;
		try {
			await escalationApi.setForMonitor(id, policyId);
			if (mon && !assignedMonitorIds.has(id)) {
				assignments = {
					...assignments,
					monitors: [...assignments.monitors, { id, name: mon.name }].sort(
						(a, b) => a.id - b.id,
					),
				};
			}
			monitorPick = '';
			toast.success(m.escalation_assignments_assigned_toast());
		} catch {
			toast.error(m.escalation_assignments_assign_failed());
		} finally {
			busyKey = null;
		}
	}

	async function assignGroup(value: string) {
		const id = Number(value);
		if (!id) return;
		const grp = allGroups.find((x) => x.id === id);
		busyKey = `ag:${id}`;
		try {
			await escalationApi.setForGroup(id, policyId);
			if (grp && !assignedGroupIds.has(id)) {
				assignments = {
					...assignments,
					groups: [...assignments.groups, { id, name: grp.name }].sort((a, b) => a.id - b.id),
				};
			}
			groupPick = '';
			toast.success(m.escalation_assignments_assigned_toast());
		} catch {
			toast.error(m.escalation_assignments_assign_failed());
		} finally {
			busyKey = null;
		}
	}
</script>

<div class="mt-3 border-t border-border pt-3" data-testid="escalation-policy-assignments">
	<button
		type="button"
		onclick={toggle}
		class="flex w-full items-center justify-between gap-2 text-left text-xs text-muted-foreground transition-colors hover:text-foreground"
		aria-expanded={expanded}
		data-testid="escalation-assignments-toggle"
	>
		<span class="inline-flex items-center gap-1.5">
			{#if expanded}
				<ChevronDown class="h-3.5 w-3.5" />
			{:else}
				<ChevronRight class="h-3.5 w-3.5" />
			{/if}
			{expanded ? m.escalation_assignments_hide() : m.escalation_assignments_show()}
		</span>
		<span class="tabular-nums">
			{#if loaded}
				{m.escalation_assignments_summary({
					monitors: assignments.monitors.length,
					groups: assignments.groups.length,
				})}
			{:else if loading}
				…
			{:else if loadError}
				—
			{/if}
		</span>
	</button>

	{#if expanded}
		<div class="mt-3 space-y-3">
			{#if loading}
				<p class="inline-flex items-center gap-2 text-xs text-muted-foreground" role="status">
					<Loader2 class="h-3.5 w-3.5 animate-spin" />
					{m.escalation_assignments_loading()}
				</p>
			{:else if loadError}
				<div class="space-y-2">
					<p class="text-xs text-destructive">{loadError}</p>
					<button
						type="button"
						onclick={loadAssignments}
						class="rounded-md border border-border px-2 py-1 text-xs font-medium transition-colors hover:bg-accent"
					>
						{m.monitor_group_form_retry()}
					</button>
				</div>
			{:else}
				{#if assignments.monitors.length === 0 && assignments.groups.length === 0}
					<p class="text-xs text-muted-foreground">{m.escalation_assignments_empty()}</p>
				{:else}
					{#if assignments.monitors.length > 0}
						<div>
							<p class="mb-1.5 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
								{m.escalation_assignments_monitors()}
							</p>
							<ul class="space-y-1">
								{#each assignments.monitors as mon (mon.id)}
									<li
										class="flex items-center justify-between gap-2 rounded-md border border-border/60 bg-muted/20 px-2 py-1.5 text-xs"
										data-testid="escalation-assigned-monitor"
									>
										<a
											href="/monitors/{mon.id}"
											class="min-w-0 truncate font-medium text-foreground underline-offset-2 hover:underline"
										>
											{mon.name}
										</a>
										{#if canAssign}
											<button
												type="button"
												disabled={busyKey === `m:${mon.id}`}
												onclick={() => unassignMonitor(mon)}
												class="inline-flex shrink-0 items-center gap-1 rounded border border-border px-1.5 py-0.5 text-[11px] text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:opacity-50"
												title={m.escalation_assignments_unassign()}
											>
												<Link2Off class="h-3 w-3" />
												{m.escalation_assignments_unassign()}
											</button>
										{/if}
									</li>
								{/each}
							</ul>
						</div>
					{/if}

					{#if assignments.groups.length > 0}
						<div>
							<p class="mb-1.5 text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
								{m.escalation_assignments_groups()}
							</p>
							<ul class="space-y-1">
								{#each assignments.groups as grp (grp.id)}
									<li
										class="flex items-center justify-between gap-2 rounded-md border border-border/60 bg-muted/20 px-2 py-1.5 text-xs"
										data-testid="escalation-assigned-group"
									>
										<a
											href="/monitors"
											class="inline-flex min-w-0 items-center gap-1.5 truncate font-medium text-foreground underline-offset-2 hover:underline"
										>
											<Folder class="h-3 w-3 shrink-0 text-primary/80" />
											{grp.name}
										</a>
										{#if canAssign}
											<button
												type="button"
												disabled={busyKey === `g:${grp.id}`}
												onclick={() => unassignGroup(grp)}
												class="inline-flex shrink-0 items-center gap-1 rounded border border-border px-1.5 py-0.5 text-[11px] text-muted-foreground transition-colors hover:bg-accent hover:text-foreground disabled:opacity-50"
												title={m.escalation_assignments_unassign()}
											>
												<Link2Off class="h-3 w-3" />
												{m.escalation_assignments_unassign()}
											</button>
										{/if}
									</li>
								{/each}
							</ul>
						</div>
					{/if}
				{/if}

				{#if canAssign}
					<div class="space-y-2 border-t border-border/60 pt-3">
						{#if pickersLoading}
							<p class="inline-flex items-center gap-2 text-xs text-muted-foreground">
								<Loader2 class="h-3.5 w-3.5 animate-spin" />
								{m.escalation_assignments_loading()}
							</p>
						{:else if pickersError}
							<p class="text-xs text-destructive">{pickersError}</p>
						{:else}
							<div class="space-y-1">
								<label class="text-[11px] font-medium text-muted-foreground" for={`esc-assign-mon-${policyId}`}>
									{m.escalation_assignments_assign_monitor()}
								</label>
								{#if monitorOptions.length === 0}
									<p class="text-[11px] text-muted-foreground">
										{m.escalation_assignments_none_available_monitors()}
									</p>
								{:else}
									<Select
										id={`esc-assign-mon-${policyId}`}
										size="sm"
										options={monitorOptions}
										value={monitorPick}
										placeholder={m.escalation_assignments_assign_placeholder_monitor()}
										disabled={busyKey !== null}
										onValueChange={(v) => {
											monitorPick = v;
											assignMonitor(v);
										}}
									/>
								{/if}
							</div>
							<div class="space-y-1">
								<label class="text-[11px] font-medium text-muted-foreground" for={`esc-assign-grp-${policyId}`}>
									{m.escalation_assignments_assign_group()}
								</label>
								{#if groupOptions.length === 0}
									<p class="text-[11px] text-muted-foreground">
										{m.escalation_assignments_none_available_groups()}
									</p>
								{:else}
									<Select
										id={`esc-assign-grp-${policyId}`}
										size="sm"
										options={groupOptions}
										value={groupPick}
										placeholder={m.escalation_assignments_assign_placeholder_group()}
										disabled={busyKey !== null}
										onValueChange={(v) => {
											groupPick = v;
											assignGroup(v);
										}}
									/>
								{/if}
							</div>
						{/if}
					</div>
				{/if}
			{/if}
		</div>
	{/if}
</div>
