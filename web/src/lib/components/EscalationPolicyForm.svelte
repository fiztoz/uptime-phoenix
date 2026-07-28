<script lang="ts">
	/**
	 * Escalation policy editor (F2.3).
	 *
	 * The ladder is edited as an ordered list; step numbers are derived from
	 * array position and assigned by the server, so reordering is just moving an
	 * element. `steps` is a REPLACE-SET on save — the whole ladder goes every
	 * time, and anything not sent is deleted.
	 *
	 * The wait on step 1 is measured from the initial DOWN notification, which
	 * this form never controls: that notification belongs to the monitor's own
	 * channels and always fires first. Steps are additional, never a replacement.
	 */
	import {
		escalationApi,
		type EscalationPolicy,
		type EscalationPolicyInput,
	} from '$lib/api/escalation';
	import { notificationsApi, type Notification } from '$lib/api/notifications';
	import EmptyState from './EmptyState.svelte';
	import Skeleton from './Skeleton.svelte';
	import { toast } from 'svelte-sonner';
	import { onMount, untrack } from 'svelte';
	import { AlertCircle, ArrowDown, ArrowUp, BellOff, Plus, Trash2, X } from '@lucide/svelte';
	import { modalFocus } from '$lib/actions/modalFocus';
	import * as m from '$lib/paraglide/messages.js';

	interface Props {
		/** Pass an existing policy to edit it; omit/null to create a new one. */
		policy?: EscalationPolicy | null;
		onSaved?: () => void;
		onClose?: () => void;
	}

	let { policy, onSaved, onClose }: Props = $props();
	const initial = untrack(() => policy);

	interface StepDraft {
		waitMinutes: number;
		notificationIds: number[];
	}

	let open = $state(true);
	let saving = $state(false);

	let formData = $state({
		name: initial?.name ?? '',
		description: initial?.description ?? '',
		enabled: initial?.enabled ?? true,
	});

	let steps = $state<StepDraft[]>(
		initial?.steps?.length
			? initial.steps.map((s) => ({
					waitMinutes: s.wait_minutes,
					notificationIds: [...s.notification_ids],
				}))
			: [{ waitMinutes: 5, notificationIds: [] }],
	);

	let providers = $state<Notification[]>([]);
	let providersLoading = $state(true);
	let providersError = $state<string | null>(null);

	async function loadProviders() {
		providersLoading = true;
		providersError = null;
		try {
			providers = await notificationsApi.list();
		} catch (err: unknown) {
			providersError =
				err && typeof err === 'object' && 'message' in err
					? String((err as { message: string }).message)
					: m.escalation_form_load_channels_failed();
		} finally {
			providersLoading = false;
		}
	}

	onMount(loadProviders);

	function addStep() {
		steps = [...steps, { waitMinutes: 10, notificationIds: [] }];
	}

	function removeStep(index: number) {
		steps = steps.filter((_, i) => i !== index);
	}

	function moveStep(index: number, delta: number) {
		const target = index + delta;
		if (target < 0 || target >= steps.length) return;
		const next = [...steps];
		[next[index], next[target]] = [next[target], next[index]];
		steps = next;
	}

	function toggleChannel(index: number, notificationId: number) {
		const step = steps[index];
		const has = step.notificationIds.includes(notificationId);
		step.notificationIds = has
			? step.notificationIds.filter((id) => id !== notificationId)
			: [...step.notificationIds, notificationId];
	}

	function close() {
		open = false;
		onClose?.();
	}

	async function handleSubmit() {
		if (!formData.name.trim()) {
			toast.error(m.escalation_form_name_required());
			return;
		}
		if (steps.length === 0) {
			toast.error(m.escalation_form_needs_step());
			return;
		}
		for (let i = 0; i < steps.length; i++) {
			if (steps[i].notificationIds.length === 0) {
				toast.error(m.escalation_form_step_needs_channel({ index: i + 1 }));
				return;
			}
			if (steps[i].waitMinutes < 0) {
				toast.error(m.escalation_form_wait_negative({ index: i + 1 }));
				return;
			}
		}

		saving = true;
		try {
			const input: EscalationPolicyInput = {
				name: formData.name.trim(),
				description: formData.description.trim(),
				enabled: formData.enabled,
				steps: steps.map((s) => ({
					wait_minutes: Number(s.waitMinutes),
					notification_ids: s.notificationIds,
				})),
			};
			if (policy) {
				await escalationApi.update(policy.id, input);
				toast.success(m.escalation_form_updated_toast());
			} else {
				await escalationApi.create(input);
				toast.success(m.escalation_form_created_toast());
			}
			onSaved?.();
			close();
		} catch (err: unknown) {
			const msg =
				err && typeof err === 'object' && 'message' in err
					? String((err as { message: string }).message)
					: m.escalation_form_save_failed();
			toast.error(msg);
		} finally {
			saving = false;
		}
	}
</script>

{#if open}
	<div class="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4">
		<div
			use:modalFocus={{ onClose: close }}
			class="max-h-[90vh] w-full max-w-2xl overflow-y-auto rounded-xl border border-border bg-card shadow-2xl"
			role="dialog"
			aria-modal="true"
			aria-label={policy ? m.escalation_form_edit_title() : m.escalation_form_create_title()}
		>
			<div
				class="sticky top-0 z-10 flex items-center justify-between border-b border-border bg-card px-6 py-4"
			>
				<h2 class="text-lg font-semibold">
					{policy ? m.escalation_form_edit_title() : m.escalation_form_create_title()}
				</h2>
				<button
					type="button"
					onclick={close}
					class="rounded-lg p-1.5 text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
					aria-label={m.btn_close()}
				>
					<X class="h-4 w-4" />
				</button>
			</div>

			<form
				onsubmit={(e) => {
					e.preventDefault();
					handleSubmit();
				}}
				class="space-y-5 px-6 py-5"
			>
				<div>
					<label for="esc-name" class="mb-1.5 block text-sm font-medium"
						>{m.escalation_form_name()}</label
					>
					<input
						id="esc-name"
						type="text"
						bind:value={formData.name}
						placeholder={m.escalation_form_name_placeholder()}
						class="w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
					/>
				</div>

				<div>
					<label for="esc-description" class="mb-1.5 block text-sm font-medium"
						>{m.escalation_form_description()}</label
					>
					<input
						id="esc-description"
						type="text"
						bind:value={formData.description}
						placeholder={m.escalation_form_description_placeholder()}
						class="w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
					/>
				</div>

				<label class="flex items-start gap-3">
					<input
						type="checkbox"
						bind:checked={formData.enabled}
						class="mt-0.5 h-4 w-4 rounded border-border accent-primary"
					/>
					<span>
						<span class="block text-sm font-medium">{m.escalation_form_enabled_label()}</span>
						<span class="mt-0.5 block text-xs text-muted-foreground"
							>{m.escalation_form_enabled_help()}</span
						>
					</span>
				</label>

				<div class="border-t border-border pt-4">
					<div class="flex items-center justify-between gap-2">
						<span class="text-sm font-medium">{m.escalation_form_steps()}</span>
						<button
							type="button"
							onclick={addStep}
							class="inline-flex items-center gap-1 rounded-lg border border-border px-2.5 py-1 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
						>
							<Plus class="h-3 w-3" />
							{m.escalation_form_add_step()}
						</button>
					</div>
					<p class="mt-1 text-xs text-muted-foreground">{m.escalation_form_steps_help()}</p>

					{#if providersLoading}
						<div class="mt-3 space-y-2">
							<Skeleton class="h-24 w-full" />
							<Skeleton class="h-24 w-full" />
						</div>
					{:else if providersError}
						<div
							class="mt-3 flex items-start gap-2 rounded-lg border border-danger/30 bg-danger/10 p-3 text-sm text-danger"
						>
							<AlertCircle class="mt-0.5 h-4 w-4 shrink-0" />
							<span class="flex-1">{providersError}</span>
							<button
								type="button"
								class="shrink-0 rounded-md px-2 py-0.5 text-xs font-medium underline-offset-2 hover:underline"
								onclick={loadProviders}
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
						<div class="mt-3 space-y-3">
							{#each steps as stepDraft, i (i)}
								<div
									data-testid="escalation-step"
									data-step-index={i}
									class="rounded-lg border border-border bg-surface p-3"
								>
									<div class="flex items-center justify-between gap-2">
										<span class="text-sm font-medium"
											>{m.escalation_form_step_label({ index: i + 1 })}</span
										>
										<div class="flex items-center gap-1">
											<button
												type="button"
												onclick={() => moveStep(i, -1)}
												disabled={i === 0}
												class="rounded-md p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground disabled:opacity-30"
												aria-label={m.escalation_form_move_up()}
											>
												<ArrowUp class="h-3.5 w-3.5" />
											</button>
											<button
												type="button"
												onclick={() => moveStep(i, 1)}
												disabled={i === steps.length - 1}
												class="rounded-md p-1 text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground disabled:opacity-30"
												aria-label={m.escalation_form_move_down()}
											>
												<ArrowDown class="h-3.5 w-3.5" />
											</button>
											<button
												type="button"
												onclick={() => removeStep(i)}
												class="rounded-md p-1 text-destructive transition-colors hover:bg-destructive/10"
												aria-label={m.escalation_form_remove_step()}
											>
												<Trash2 class="h-3.5 w-3.5" />
											</button>
										</div>
									</div>

									<div class="mt-3">
										<label for="esc-wait-{i}" class="mb-1 block text-xs font-medium"
											>{m.escalation_form_wait_label()}</label
										>
										<input
											id="esc-wait-{i}"
											type="number"
											min="0"
											bind:value={stepDraft.waitMinutes}
											class="w-32 rounded-lg border border-border bg-card px-3 py-1.5 text-sm transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
										/>
										<p class="mt-1 text-xs text-muted-foreground">
											{i === 0
												? m.escalation_form_wait_help_first()
												: m.escalation_form_wait_help_next()}
										</p>
									</div>

									<div class="mt-3">
										<span class="mb-1 block text-xs font-medium"
											>{m.escalation_form_channels()}</span
										>
										<div class="space-y-1.5">
											{#each providers as n (n.id)}
												<label class="flex items-center gap-2 text-sm">
													<input
														type="checkbox"
														checked={stepDraft.notificationIds.includes(n.id)}
														onchange={() => toggleChannel(i, n.id)}
														class="h-4 w-4 rounded border-border accent-primary"
													/>
													<span class="truncate">{n.name}</span>
													{#if !n.active}
														<span class="text-xs text-muted-foreground"
															>({m.notifications_disabled()})</span
														>
													{/if}
												</label>
											{/each}
										</div>
									</div>
								</div>
							{/each}
						</div>
					{/if}
				</div>

				<div class="flex justify-end gap-2 border-t border-border pt-4">
					<button
						type="button"
						onclick={close}
						class="rounded-lg border border-border px-4 py-2 text-sm font-medium transition-colors hover:bg-accent"
					>
						{m.btn_cancel()}
					</button>
					<button
						type="submit"
						disabled={saving}
						class="rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 disabled:opacity-50"
					>
						{saving ? m.btn_saving() : m.btn_save()}
					</button>
				</div>
			</form>
		</div>
	</div>
{/if}
