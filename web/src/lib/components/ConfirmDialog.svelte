<script lang="ts">
	/**
	 * Renders whatever confirmation is pending in the confirm store. Mounted once
	 * in the root layout — never instantiate it per call site.
	 *
	 * Call it through `confirmAction()`; see $lib/stores/confirm.svelte.ts.
	 */
	import { confirmController } from '$lib/stores/confirm.svelte';
	import { AlertTriangle, HelpCircle } from '@lucide/svelte';
	import * as m from '$lib/paraglide/messages.js';

	const pending = $derived(confirmController.current);
	const destructive = $derived(pending?.destructive ?? false);

	let typed = $state('');
	let dialogEl = $state<HTMLDivElement | null>(null);
	let confirmEl = $state<HTMLButtonElement | null>(null);
	let cancelEl = $state<HTMLButtonElement | null>(null);

	// A type-to-confirm gate is only satisfied by an exact match.
	const armed = $derived(!pending?.requireText || typed === pending.requireText);

	// Each new request starts with an empty box and focus inside the dialog.
	// Destructive actions focus Cancel: a stray Enter should not delete anything.
	$effect(() => {
		if (!pending) return;
		typed = '';
		const returnFocus = document.activeElement as HTMLElement | null;
		queueMicrotask(() => (destructive ? cancelEl : confirmEl)?.focus());
		return () => returnFocus?.focus?.();
	});

	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'Escape') {
			e.preventDefault();
			confirmController.cancel();
			return;
		}
		if (e.key === 'Enter' && armed && e.target !== cancelEl) {
			e.preventDefault();
			confirmController.confirm();
			return;
		}
		if (e.key !== 'Tab' || !dialogEl) return;

		// Focus trap: keep Tab inside the dialog while it is open.
		const focusable = dialogEl.querySelectorAll<HTMLElement>(
			'button:not([disabled]), input, [href], [tabindex]:not([tabindex="-1"])'
		);
		if (focusable.length === 0) return;
		const first = focusable[0];
		const last = focusable[focusable.length - 1];
		if (e.shiftKey && document.activeElement === first) {
			e.preventDefault();
			last.focus();
		} else if (!e.shiftKey && document.activeElement === last) {
			e.preventDefault();
			first.focus();
		}
	}

	const inputClass =
		'w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring';
	const ghostBtn =
		'inline-flex items-center justify-center gap-2 rounded-lg border border-border px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring';
	const confirmBtn = $derived(
		destructive
			? 'inline-flex items-center justify-center gap-2 rounded-lg bg-danger px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-danger/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-danger/40 disabled:opacity-50'
			: 'inline-flex items-center justify-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50'
	);
</script>

<svelte:window onkeydown={pending ? onKeydown : undefined} />

{#if pending}
	<!-- Backdrop. Clicking it cancels, matching Escape and the Cancel button. -->
	<div
		class="fixed inset-0 z-[100] flex items-end justify-center bg-black/60 p-0 backdrop-blur-sm sm:items-center sm:p-4"
		role="presentation"
		data-testid="confirm-backdrop"
	>
		<button
			type="button"
			tabindex="-1"
			class="absolute inset-0 cursor-default"
			onclick={() => confirmController.cancel()}
			aria-label={m.btn_cancel()}
		></button>
		<div
			bind:this={dialogEl}
			class="relative z-10 w-full max-w-md rounded-t-xl border border-border bg-card p-5 shadow-xl sm:rounded-xl sm:p-6"
			role="alertdialog"
			aria-modal="true"
			aria-labelledby="confirm-title"
			aria-describedby={pending.message ? 'confirm-message' : undefined}
			tabindex="-1"
			data-testid="confirm-dialog"
		>
			<div class="flex gap-4">
				<span
					class="grid h-10 w-10 shrink-0 place-items-center rounded-full {destructive
						? 'bg-danger/10 text-danger ring-1 ring-danger/20'
						: 'bg-primary/10 text-primary ring-1 ring-primary/20'}"
				>
					{#if destructive}
						<AlertTriangle class="h-5 w-5" />
					{:else}
						<HelpCircle class="h-5 w-5" />
					{/if}
				</span>
				<div class="min-w-0 flex-1">
					<h2 id="confirm-title" class="text-base font-semibold tracking-tight">{pending.title}</h2>
					{#if pending.message}
						<p id="confirm-message" class="mt-1.5 text-sm text-muted-foreground">{pending.message}</p>
					{/if}

					{#if pending.requireText}
						<label for="confirm-typed" class="mt-4 block text-xs text-muted-foreground">
							{m.confirm_dialog_type_pre()} <span class="font-mono font-medium text-foreground">{pending.requireText}</span> {m.confirm_dialog_type_post()}
						</label>
						<!-- The dialog deliberately moves focus here so typed confirmation starts inside the focus trap. -->
						<!-- svelte-ignore a11y_autofocus -->
						<input
							id="confirm-typed"
							type="text"
							bind:value={typed}
							autocomplete="off"
							class="{inputClass} mt-1 font-mono"
							data-testid="confirm-input"
						/>
					{/if}
				</div>
			</div>

			<div class="mt-6 flex flex-col-reverse gap-2 sm:flex-row sm:justify-end sm:gap-3">
				<button
					bind:this={cancelEl}
					type="button"
					class={ghostBtn}
					onclick={() => confirmController.cancel()}
					data-testid="confirm-cancel"
				>
					{pending.cancelLabel ?? m.btn_cancel()}
				</button>
				<button
					bind:this={confirmEl}
					type="button"
					class={confirmBtn}
					disabled={!armed}
					onclick={() => confirmController.confirm()}
					data-testid="confirm-accept"
				>
					{pending.confirmLabel ?? (destructive ? m.btn_delete() : m.btn_confirm())}
				</button>
			</div>
		</div>
	</div>
{/if}
