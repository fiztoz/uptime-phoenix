<script lang="ts">
	import {
		monitorGroupsApi,
		buildGroupOptions,
		GROUP_CONDITIONS,
		type MonitorGroupView,
		type CreateMonitorGroupInput,
		type GroupCondition,
	} from '$lib/api/monitorGroups';
	import { notificationsApi, type Notification } from '$lib/api/notifications';
	import { escalationApi, type EscalationPolicy } from '$lib/api/escalation';
	import {
		applySyncToBaseline,
		diffNotificationLinks,
		hasLinkChanges,
		initialGroupNotificationSelection,
		syncGroupNotifications,
	} from '$lib/group-notifications';
	import EmptyState from './EmptyState.svelte';
	import Skeleton from './Skeleton.svelte';
	import { toast } from 'svelte-sonner';
	import { onMount, untrack } from 'svelte';
	import { AlertCircle, BellOff, X } from '@lucide/svelte';
	import Select from '$lib/components/Select.svelte';
	import { modalFocus } from '$lib/actions/modalFocus';
	import * as m from '$lib/paraglide/messages.js';

	interface Props {
		/** Pass an existing group to edit it; omit/null to create a new one. */
		group?: MonitorGroupView | null;
		/** Full flat group list, used to build the "parent group" picker. */
		groups: MonitorGroupView[];
		onSaved?: () => void;
		onClose?: () => void;
	}

	let { group, groups, onSaved, onClose }: Props = $props();
	const initialGroup = untrack(() => group);

	let open = $state(true);
	let loading = $state(false);

	// A group can't be nested under itself or any of its own descendants.
	let parentOptions = $derived(buildGroupOptions(groups, group?.id));

	let formData = $state({
		name: initialGroup?.name ?? '',
		description: initialGroup?.description ?? '',
		parentId: initialGroup?.parent_id != null ? String(initialGroup.parent_id) : '',
		condition: (initialGroup?.condition ?? 'worst_of_children') as GroupCondition,
		threshold: initialGroup?.threshold ?? 1,
		thresholdIsPercent: initialGroup?.threshold_is_percent ?? false,
	});

	// ── Notifications ──────────────────────────────────────────────────────
	// A group is an alerting entity in its own right: an attached notification
	// fires on the GROUP's derived status (per the condition picked above), not
	// once per monitor inside it. Links are always explicit — a provider flagged
	// `is_default` auto-attaches to new MONITORS and is never pre-ticked here.
	let providers = $state<Notification[]>([]);
	let notifLoading = $state(true);
	let notifError = $state<string | null>(null);
	/** Server truth: the IDs currently attached to this group. Always [] on create. */
	let baselineIds = $state<number[]>([]);
	/** What the user has ticked. Starts empty on create — including for is_default providers. */
	let selectedIds = $state<number[]>([]);

	const linkDiff = $derived(diffNotificationLinks(baselineIds, selectedIds));
	const conditionLabel = $derived(
		GROUP_CONDITIONS.find((c) => c.value === formData.condition)?.label ?? formData.condition,
	);
	// An `ignore` group derives no status, so nothing attached to it can ever fire.
	const ignoreBlocksAlerts = $derived(formData.condition === 'ignore' && selectedIds.length > 0);

	// ── Escalation policy (F2.3) ───────────────────────────────────────────
	// A folder's policy is inherited by every monitor beneath it that has no
	// policy of its own — nearest assignment wins. This field shows and writes
	// only this group's DIRECT assignment.
	let escalationPolicies = $state<EscalationPolicy[]>([]);
	let escalationPolicyId = $state('');
	let escalationBaselineId = $state('');

	const escalationOptions = $derived([
		{ value: '', label: m.escalation_assign_none() },
		...escalationPolicies.map((p) => ({
			value: String(p.id),
			label: p.enabled ? p.name : `${p.name} (${m.escalation_disabled()})`,
		})),
	]);

	async function loadEscalation() {
		try {
			const [list, assignment] = await Promise.all([
				escalationApi.list(),
				group ? escalationApi.getForGroup(group.id) : Promise.resolve(null),
			]);
			escalationPolicies = list;
			escalationBaselineId = assignment && assignment.policy_id ? String(assignment.policy_id) : '';
			escalationPolicyId = escalationBaselineId;
		} catch {
			// A caller without can_manage_notifications gets 403 here. That is not
			// a save-blocking failure: the picker simply stays empty and the
			// existing assignment is left untouched.
			escalationPolicies = [];
		}
	}

	onMount(loadNotifications);
	onMount(loadEscalation);

	async function loadNotifications() {
		notifLoading = true;
		notifError = null;
		try {
			const [list, attached] = await Promise.all([
				notificationsApi.list(),
				group ? monitorGroupsApi.listNotifications(group.id) : Promise.resolve(null),
			]);
			providers = list;
			// Editing: tick exactly what the server says is attached. Creating:
			// nothing is attached yet, and `is_default` is NOT a reason to tick.
			baselineIds = initialGroupNotificationSelection(
				list,
				attached ? attached.map((n) => n.id) : null,
			);
			selectedIds = [...baselineIds];
		} catch (err: any) {
			notifError = err?.message || m.monitor_group_form_load_notifications_failed();
		} finally {
			notifLoading = false;
		}
	}

	function toggleNotification(id: number) {
		selectedIds = selectedIds.includes(id)
			? selectedIds.filter((n) => n !== id)
			: [...selectedIds, id];
	}

	function providerName(id: number): string {
		return providers.find((p) => p.id === id)?.name ?? `#${id}`;
	}

	async function handleSubmit() {
		if (!formData.name.trim()) {
			toast.error(m.monitor_form_name_required());
			return;
		}
		if (formData.condition === 'threshold') {
			if (!Number.isFinite(formData.threshold) || formData.threshold < 1) {
				toast.error(m.monitor_group_form_threshold_min());
				return;
			}
			if (formData.thresholdIsPercent && (formData.threshold < 1 || formData.threshold > 100)) {
				toast.error(m.monitor_group_form_threshold_percent_range());
				return;
			}
		}

		loading = true;
		try {
			const input: CreateMonitorGroupInput = {
				name: formData.name.trim(),
				description: formData.description.trim(),
				parent_id: formData.parentId === '' ? null : Number(formData.parentId),
				condition: formData.condition,
				threshold: formData.condition === 'threshold' ? Number(formData.threshold) : 0,
				threshold_is_percent: formData.condition === 'threshold' ? formData.thresholdIsPercent : false,
			};

			let groupId: number;
			const savedMsg = group ? m.monitor_group_form_updated_toast() : m.monitor_group_form_created_toast();
			if (group) {
				await monitorGroupsApi.update(group.id, input);
				groupId = group.id;
			} else {
				// Creating: there is no group id until the POST returns, so the
				// notification links can only be attached afterwards.
				groupId = (await monitorGroupsApi.create(input)).id;
			}

			// Escalation assignment. Only written when it actually changed, so a
			// user without the capability (whose picker never loaded) cannot clear
			// an assignment simply by saving an unrelated field.
			if (escalationPolicyId !== escalationBaselineId) {
				try {
					await escalationApi.setForGroup(groupId, escalationPolicyId === '' ? 0 : Number(escalationPolicyId));
					escalationBaselineId = escalationPolicyId;
				} catch (err: any) {
					toast.error(err?.message || m.escalation_assign_failed());
				}
			}

			// Only the difference is sent — never N detaches followed by N attaches.
			if (hasLinkChanges(linkDiff)) {
				const result = await syncGroupNotifications(groupId, linkDiff, {
					attach: (nid, gid) => notificationsApi.attachToGroup(nid, gid),
					detach: (nid, gid) => notificationsApi.detachFromGroup(nid, gid),
				});
				// Re-baseline to what ACTUALLY landed, so a retry re-diffs against
				// reality instead of replaying calls that already succeeded.
				baselineIds = applySyncToBaseline(baselineIds, result);

				if (result.failures.length > 0) {
					// The group itself saved, but some links did not. Say exactly which —
					// never report success for a call that failed.
					selectedIds = [...baselineIds];
					const applied = result.attached.length + result.detached.length;
					const failed = result.failures
						.map((f) => `${f.op} ${providerName(f.id)}`)
						.join(', ');
					toast.error(
						`${savedMsg}, but ${applied > 0 ? m.monitor_group_form_partial_applied({ count: applied }) : ''}${m.monitor_group_form_failed_to({ failed })}`,
					);
					onSaved?.();
					// Stay open so the user can see what stuck and retry the rest.
					return;
				}
			}

			toast.success(savedMsg);
			onSaved?.();
			close();
		} catch (err: any) {
			toast.error(err?.message || m.monitor_form_save_failed());
		} finally {
			loading = false;
		}
	}

	function close() {
		open = false;
		onClose?.();
	}

	// Shared, token-consistent class strings (match MonitorForm/MaintenanceForm).
	const inputClass =
		'w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring';
	const primaryBtn =
		'inline-flex items-center gap-2 rounded-lg bg-primary px-5 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60';
	const ghostBtn =
		'inline-flex items-center gap-2 rounded-lg border border-border px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground';
</script>

{#if open}
	<div
		class="fixed inset-0 z-50 flex items-end justify-center bg-black/60 p-0 backdrop-blur-sm sm:items-center sm:p-4"
	>
		<button
			type="button"
			tabindex="-1"
			class="absolute inset-0 cursor-default"
			onclick={close}
			aria-label={m.btn_close()}
		></button>
		<div
			use:modalFocus={{ onClose: close, initialFocus: '#mg-name' }}
			class="relative z-10 max-h-[90dvh] w-full max-w-lg overflow-y-auto rounded-t-xl border border-border bg-card p-4 shadow-xl sm:rounded-xl sm:p-6"
			role="dialog"
			aria-modal="true"
			aria-labelledby="monitor-group-form-title"
			tabindex="-1"
		>
			<div class="flex items-center justify-between border-b border-border pb-4">
				<h3 id="monitor-group-form-title" class="text-lg font-semibold tracking-tight">{group ? m.monitor_group_form_edit_title() : m.monitor_group_form_new_title()}</h3>
				<button type="button" class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground" onclick={close} aria-label={m.btn_close()}>
					<X class="h-5 w-5" />
				</button>
			</div>

			<div class="mt-6 space-y-4">
				<div>
					<label for="mg-name" class="text-sm font-medium">{m.monitor_form_name_label()}</label>
					<input
						id="mg-name"
						type="text"
						bind:value={formData.name}
						placeholder={m.monitor_group_form_name_placeholder()}
						class="{inputClass} mt-1"
					/>
				</div>

				<div>
					<label for="mg-desc" class="text-sm font-medium">{m.monitor_form_description_label()}</label>
					<textarea
						id="mg-desc"
						bind:value={formData.description}
						rows="2"
						placeholder={m.monitor_group_form_description_placeholder()}
						class="{inputClass} mt-1"
					></textarea>
				</div>

				<div>
					<label for="mg-parent" class="text-sm font-medium">{m.monitor_group_form_parent_label()}</label>
					<div class="mt-1">
						<Select
							id="mg-parent"
							options={[
								{ value: '', label: m.monitor_group_form_parent_none() },
								...parentOptions.map((opt) => ({
									value: String(opt.id),
									label: '\u00A0\u00A0'.repeat(opt.depth) + (opt.depth > 0 ? '↳ ' : '') + opt.name,
								})),
							]}
								value={formData.parentId}
								onValueChange={(v) => { formData.parentId = v; }}
							class="w-full"
						/>
					</div>
					<p class="mt-1 text-xs text-muted-foreground">{m.monitor_group_form_nest_help()}</p>
				</div>

				<div class="border-t border-border pt-4">
					<span class="text-sm font-medium">{m.monitor_group_form_condition_label()}</span>
					<div class="mt-2 space-y-2">
						{#each GROUP_CONDITIONS as c (c.value)}
							<label
								class="flex cursor-pointer items-start gap-2.5 rounded-lg border border-border p-3 text-sm transition-colors hover:bg-accent/40 {formData.condition === c.value ? 'border-primary/40 bg-primary/[0.06]' : ''}"
							>
								<input
									type="radio"
									name="mg-condition"
									value={c.value}
									bind:group={formData.condition}
									class="mt-0.5 accent-primary"
								/>
								<span>
									<span class="block font-medium">{c.label}</span>
									<span class="block text-xs text-muted-foreground">{c.help}</span>
								</span>
							</label>
						{/each}
					</div>
				</div>

				{#if formData.condition === 'threshold'}
					<div class="grid grid-cols-1 gap-4 rounded-lg border border-border p-3 sm:grid-cols-2">
						<div>
							<label for="mg-threshold" class="text-sm font-medium">
								{formData.thresholdIsPercent ? m.monitor_group_form_threshold_percent_label() : m.monitor_group_form_threshold_count_label()}
							</label>
							<input
								id="mg-threshold"
								type="number"
								min="1"
								max={formData.thresholdIsPercent ? 100 : undefined}
								bind:value={formData.threshold}
								class="{inputClass} mt-1"
							/>
							{#if formData.thresholdIsPercent}
								<p class="mt-1 text-xs text-muted-foreground">{m.monitor_group_form_threshold_percent_help()}</p>
							{:else}
								<p class="mt-1 text-xs text-muted-foreground">{m.monitor_group_form_threshold_count_help()}</p>
							{/if}
						</div>
						<div>
							<span class="text-sm font-medium">{m.monitor_group_form_unit_label()}</span>
							<div class="mt-1 flex overflow-hidden rounded-lg border border-border">
								<button
									type="button"
									onclick={() => (formData.thresholdIsPercent = false)}
									class="flex-1 px-3 py-2 text-sm transition-colors {!formData.thresholdIsPercent ? 'bg-primary text-primary-foreground' : 'bg-surface text-muted-foreground hover:bg-accent'}"
								>
									{m.monitor_group_form_unit_count()}
								</button>
								<button
									type="button"
									onclick={() => (formData.thresholdIsPercent = true)}
									class="flex-1 px-3 py-2 text-sm transition-colors {formData.thresholdIsPercent ? 'bg-primary text-primary-foreground' : 'bg-surface text-muted-foreground hover:bg-accent'}"
								>
									{m.monitor_group_form_unit_percent()}
								</button>
							</div>
						</div>
					</div>
				{/if}

				<div class="border-t border-border pt-4">
					<span class="text-sm font-medium">{m.escalation_assign_label()}</span>
					<p class="mt-1 text-xs text-muted-foreground">{m.escalation_assign_help_group()}</p>
					<div class="mt-2">
						<Select
							options={escalationOptions}
							value={escalationPolicyId}
							onValueChange={(v) => (escalationPolicyId = v ?? '')}
							placeholder={m.escalation_assign_none()}
						/>
					</div>
				</div>

				<div class="border-t border-border pt-4">
					<div class="flex items-center justify-between gap-2">
						<span class="text-sm font-medium">{m.nav_notifications()}</span>
						{#if selectedIds.length > 0}
							<span class="text-xs text-muted-foreground">{m.monitor_group_form_attached_count({ count: selectedIds.length })}</span>
						{/if}
					</div>
					<p class="mt-1 text-xs text-muted-foreground">
						{m.monitor_group_form_notif_help_1_pre()}<span class="font-medium text-foreground">{m.monitor_group_form_notif_help_1_own_status()}</span>{m.monitor_group_form_notif_help_1_mid()}
						<span class="font-medium text-foreground">{conditionLabel}</span>{m.monitor_group_form_notif_help_1_post()}
					</p>
					<p class="mt-1.5 text-xs text-muted-foreground">
						{m.monitor_group_form_notif_help_2_pre()}
						<span class="font-medium text-foreground">{m.monitor_group_form_notif_help_2_default()}</span>{m.monitor_group_form_notif_help_2_mid1()}
						<span class="font-medium text-foreground">{m.monitor_group_form_notif_help_2_monitors()}</span>{m.monitor_group_form_notif_help_2_mid2()}
						<span class="font-medium text-foreground">{m.monitor_group_form_notif_help_2_not()}</span>{m.monitor_group_form_notif_help_2_post()}
					</p>

					{#if notifLoading}
						<div class="mt-3 space-y-2">
							<Skeleton class="h-14 w-full" />
							<Skeleton class="h-14 w-full" />
						</div>
					{:else if notifError}
						<div
							class="mt-3 flex items-start gap-2 rounded-lg border border-danger/30 bg-danger/10 p-3 text-sm text-danger"
						>
							<AlertCircle class="mt-0.5 h-4 w-4 shrink-0" />
							<span class="flex-1">{notifError}</span>
							<button
								type="button"
								class="shrink-0 rounded-md px-2 py-0.5 text-xs font-medium underline-offset-2 hover:underline"
								onclick={loadNotifications}
							>
								{m.monitor_group_form_retry()}
							</button>
						</div>
					{:else if providers.length === 0}
						<div class="mt-3">
							<EmptyState
								icon={BellOff}
								title={m.monitor_group_form_no_providers_title()}
								description={m.monitor_group_form_no_providers_description()}
							/>
						</div>
					{:else}
						<div class="mt-3 space-y-2">
							{#each providers as n (n.id)}
								<label
									class="flex cursor-pointer items-start gap-2.5 rounded-lg border border-border p-3 text-sm transition-colors hover:bg-accent/40 {selectedIds.includes(
										n.id,
									)
										? 'border-primary/40 bg-primary/[0.06]'
										: ''}"
								>
									<input
										type="checkbox"
										checked={selectedIds.includes(n.id)}
										onchange={() => toggleNotification(n.id)}
										class="mt-0.5 h-4 w-4 rounded border-border accent-primary"
									/>
									<span class="min-w-0 flex-1">
										<span class="flex flex-wrap items-center gap-1.5">
											<span class="font-medium">{n.name}</span>
											<span
												class="rounded border border-border px-1.5 py-px text-[10px] uppercase tracking-wide text-muted-foreground"
											>
												{n.type}
											</span>
											{#if n.is_default}
												<span
													class="rounded border border-border px-1.5 py-px text-[10px] uppercase tracking-wide text-muted-foreground"
												>
													{m.monitor_group_form_badge_default()}
												</span>
											{/if}
											{#if !n.active}
												<span
													class="rounded border border-warning/40 px-1.5 py-px text-[10px] uppercase tracking-wide text-warning"
												>
													{m.monitor_group_form_badge_inactive()}
												</span>
											{/if}
										</span>
										{#if n.is_default}
											<span class="mt-0.5 block text-xs text-faint">
												{m.monitor_group_form_default_provider_note()}
											</span>
										{/if}
									</span>
								</label>
							{/each}
						</div>

						{#if ignoreBlocksAlerts}
							<div
								class="mt-3 flex items-start gap-2 rounded-lg border border-warning/30 bg-warning/10 p-3 text-xs text-warning"
							>
								<AlertCircle class="mt-0.5 h-4 w-4 shrink-0" />
								<span>
									{m.monitor_group_form_ignore_warning_pre()}<span class="font-medium">{m.monitor_group_form_ignore_warning_ignore()}</span>{selectedIds.length === 1 ? m.monitor_group_form_ignore_warning_singular() : m.monitor_group_form_ignore_warning_plural()}
								</span>
							</div>
						{/if}
					{/if}
				</div>
			</div>

			<div class="mt-8 flex justify-end gap-3 border-t border-border pt-4">
				<button type="button" class={ghostBtn} onclick={close}>{m.btn_cancel()}</button>
				<button type="button" class={primaryBtn} disabled={loading} onclick={handleSubmit}>
					{loading ? m.btn_saving() : group ? m.monitor_group_form_save_changes() : m.monitor_group_form_create()}
				</button>
			</div>
		</div>
	</div>
{/if}
