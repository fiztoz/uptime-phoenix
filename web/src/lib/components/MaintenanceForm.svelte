<script lang="ts">
	import {
		maintenanceApi,
		type CreateMaintenanceInput,
		type MaintenanceWindow,
		type MaintenanceStrategy,
	} from '$lib/api/maintenance';
	import { monitorsApi } from '$lib/api/monitors';
	import type { Monitor } from '$lib/stores/ws.svelte.ts';
	import { toast } from 'svelte-sonner';
	import { X } from '@lucide/svelte';
	import DateTimePicker from '$lib/components/DateTimePicker.svelte';
	import { modalFocus } from '$lib/actions/modalFocus';
	import { untrack } from 'svelte';
	import * as m from '$lib/paraglide/messages.js';

	interface Props {
		window?: MaintenanceWindow | null;
		onSaved?: () => void;
		onClose?: () => void;
	}

	let { window: mw, onSaved, onClose }: Props = $props();
	const initialWindow = untrack(() => mw);

	let open = $state(true);
	let loading = $state(false);
	let monitors = $state<Monitor[]>([]);

	let strategy = $state<MaintenanceStrategy>(initialWindow?.strategy ?? 'single');
	// Common IANA zones for the cron picker. Operators can still type any valid
	// IANA name; invalid names are rejected by the API with HTTP 400.
	const timezoneOptions = [
		'UTC',
		'America/New_York',
		'America/Chicago',
		'America/Denver',
		'America/Los_Angeles',
		'Europe/London',
		'Europe/Paris',
		'Europe/Berlin',
		'Asia/Bangkok',
		'Asia/Tokyo',
		'Asia/Shanghai',
		'Asia/Singapore',
		'Australia/Sydney',
		'Pacific/Auckland',
	] as const;

	let formData = $state({
		title: initialWindow?.title ?? '',
		description: initialWindow?.description ?? '',
		active: initialWindow?.active ?? true,
		start_date: toLocalInput(initialWindow?.start_date),
		end_date: toLocalInput(initialWindow?.end_date),
		cron_expr: initialWindow?.cron_expr ?? '0 2 * * *',
		duration: initialWindow?.duration ?? 60,
		timezone: initialWindow?.timezone || 'UTC',
		monitor_ids: new Set<number>(initialWindow?.monitor_ids ?? []),
	});

	function toLocalInput(iso: string | undefined): string {
		if (!iso) return '';
		const d = new Date(iso);
		if (Number.isNaN(d.getTime())) return '';
		const pad = (n: number) => String(n).padStart(2, '0');
		return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
	}

	function toISO(local: string): string {
		if (!local) return new Date().toISOString();
		return new Date(local).toISOString();
	}

	$effect(() => {
		monitorsApi
			.list()
			.then((list) => {
				monitors = list;
			})
			.catch(() => {
				monitors = [];
			});
	});

	function toggleMonitor(id: number) {
		const next = new Set(formData.monitor_ids);
		if (next.has(id)) next.delete(id);
		else next.add(id);
		formData.monitor_ids = next;
	}

	async function handleSubmit() {
		if (!formData.title.trim()) {
			toast.error(m.maintenance_form_title_required());
			return;
		}
		if (strategy === 'single' && (!formData.start_date || !formData.end_date)) {
			toast.error(m.maintenance_form_dates_required());
			return;
		}
		if (strategy === 'cron' && !formData.cron_expr.trim()) {
			toast.error(m.maintenance_form_cron_required());
			return;
		}

		loading = true;
		try {
			const payload: CreateMaintenanceInput = {
				title: formData.title.trim(),
				description: formData.description.trim(),
				active: formData.active,
				strategy,
				timezone: formData.timezone || 'UTC',
				monitor_ids: [...formData.monitor_ids],
			};
			if (strategy === 'single') {
				payload.start_date = toISO(formData.start_date);
				payload.end_date = toISO(formData.end_date);
			} else {
				payload.cron_expr = formData.cron_expr.trim();
				payload.duration = Number(formData.duration) || 60;
			}

			if (mw) {
				await maintenanceApi.update(mw.id, payload);
				toast.success(m.maintenance_form_updated_toast());
			} else {
				await maintenanceApi.create(payload);
				toast.success(m.maintenance_form_created_toast());
			}
			onSaved?.();
			close();
		} catch (err: unknown) {
			const msg = err && typeof err === 'object' && 'message' in err ? String((err as { message: string }).message) : m.monitor_form_save_failed();
			toast.error(msg);
		} finally {
			loading = false;
		}
	}

	function close() {
		open = false;
		onClose?.();
	}

	// Shared, token-consistent class strings.
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
			use:modalFocus={{ onClose: close, initialFocus: '#mw-title' }}
			class="relative z-10 max-h-[90dvh] w-full max-w-2xl overflow-y-auto rounded-t-xl border border-border bg-card p-4 shadow-xl sm:rounded-xl sm:p-6"
			role="dialog"
			aria-modal="true"
			aria-labelledby="maintenance-form-title"
			tabindex="-1"
		>
			<div class="flex items-center justify-between border-b border-border pb-4">
				<h3 id="maintenance-form-title" class="text-lg font-semibold tracking-tight">{mw ? m.maintenance_form_edit_title() : m.maintenance_form_new_title()}</h3>
				<button type="button" class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground" onclick={close} aria-label={m.btn_close()}>
					<X class="h-5 w-5" />
				</button>
			</div>

			<div class="mt-6 space-y-4">
				<div>
					<label for="mw-title" class="text-sm font-medium">{m.maintenance_form_title_label()}</label>
					<input
						id="mw-title"
						type="text"
						bind:value={formData.title}
						class="{inputClass} mt-1"
					/>
				</div>
				<div>
					<label for="mw-desc" class="text-sm font-medium">{m.monitor_form_description_label()}</label>
					<textarea
						id="mw-desc"
						bind:value={formData.description}
						rows="2"
						class="{inputClass} mt-1"
					></textarea>
				</div>

				<div class="flex items-center gap-2">
					<input id="mw-active" type="checkbox" bind:checked={formData.active} class="h-4 w-4 rounded border-border accent-primary" />
					<label for="mw-active" class="text-sm text-muted-foreground">{m.maintenance_form_active_label()}</label>
				</div>

				<div>
					<span class="text-sm font-medium">{m.maintenance_form_strategy_label()}</span>
					<div class="mt-2 flex gap-4">
						<label class="flex items-center gap-2 text-sm text-muted-foreground">
							<input type="radio" name="strategy" value="single" bind:group={strategy} class="accent-primary" />
							{m.maintenance_form_strategy_single()}
						</label>
						<label class="flex items-center gap-2 text-sm text-muted-foreground">
							<input type="radio" name="strategy" value="cron" bind:group={strategy} class="accent-primary" />
							{m.maintenance_form_strategy_cron()}
						</label>
					</div>
				</div>

				{#if strategy === 'single'}
					<div class="grid gap-4 sm:grid-cols-2">
						<div>
							<label for="mw-start" class="text-sm font-medium">{m.maintenance_form_start_label()}</label>
							<DateTimePicker
								id="mw-start"
								bind:value={formData.start_date}
								placeholder="Choose start"
								required
								class="mt-1"
							/>
						</div>
						<div>
							<label for="mw-end" class="text-sm font-medium">{m.maintenance_form_end_label()}</label>
							<DateTimePicker
								id="mw-end"
								bind:value={formData.end_date}
								placeholder="Choose end"
								required
								class="mt-1"
							/>
						</div>
					</div>
				{:else}
					<div class="grid gap-4 sm:grid-cols-2">
						<div>
							<label for="mw-cron" class="text-sm font-medium">{m.maintenance_form_cron_label()}</label>
							<input
								id="mw-cron"
								type="text"
								bind:value={formData.cron_expr}
								placeholder="0 2 * * *"
								class="{inputClass} mt-1 font-mono"
							/>
						</div>
						<div>
							<label for="mw-dur" class="text-sm font-medium">{m.maintenance_form_duration_label()}</label>
							<input
								id="mw-dur"
								type="number"
								min="1"
								bind:value={formData.duration}
								class="{inputClass} mt-1"
							/>
						</div>
						<div class="sm:col-span-2">
							<label for="mw-timezone" class="text-sm font-medium">{m.maintenance_form_timezone_label()}</label>
							<select id="mw-timezone" bind:value={formData.timezone} class="{inputClass} mt-1">
								{#each timezoneOptions as tz}
									<option value={tz}>{tz}</option>
								{/each}
								{#if formData.timezone && !(timezoneOptions as readonly string[]).includes(formData.timezone)}
									<option value={formData.timezone}>{formData.timezone}</option>
								{/if}
							</select>
							<p class="mt-1 text-xs text-muted-foreground">{m.maintenance_form_timezone_help()}</p>
						</div>
					</div>
				{/if}

				<div class="border-t border-border pt-4">
					<p class="text-sm font-medium">{m.maintenance_form_affected_monitors()}</p>
					<p class="mt-1 text-xs text-muted-foreground">{m.maintenance_form_affected_monitors_help()}</p>
					<div class="mt-3 max-h-40 overflow-y-auto rounded-lg border border-border">
						{#if monitors.length === 0}
							<p class="p-3 text-sm text-muted-foreground">{m.maintenance_form_no_monitors_loaded()}</p>
						{:else}
							{#each monitors as mon, i (mon.id)}
								<label
									class="flex cursor-pointer items-center gap-2 px-3 py-2 text-sm transition-colors hover:bg-accent/40 {i !==
									monitors.length - 1
										? 'border-b border-border'
										: ''}"
								>
									<input
										type="checkbox"
										class="h-4 w-4 rounded border-border accent-primary"
										checked={formData.monitor_ids.has(mon.id)}
										onchange={() => toggleMonitor(mon.id)}
									/>
									<span>{mon.name}</span>
									<span class="font-mono text-xs text-muted-foreground">{mon.type}</span>
								</label>
							{/each}
						{/if}
					</div>
				</div>
			</div>

			<div class="mt-8 flex justify-end gap-3 border-t border-border pt-4">
				<button type="button" class={ghostBtn} onclick={close}>
					{m.btn_cancel()}
				</button>
				<button
					type="button"
					class={primaryBtn}
					disabled={loading}
					onclick={handleSubmit}
				>
					{loading ? m.btn_saving() : mw ? m.maintenance_form_save_changes() : m.btn_create()}
				</button>
			</div>
		</div>
	</div>
{/if}
