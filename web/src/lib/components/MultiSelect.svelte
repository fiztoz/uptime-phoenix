<script lang="ts">
	/**
	 * Phoenix MultiSelect — dropdown for selecting multiple options at once.
	 *
	 * Renders like Uptime Kuma's status/tag filters: each item shows a status
	 * dot (when `dotClass` is provided), the label, a per-option count, and a
	 * checkmark when selected. The popover width matches the trigger via the
	 * bits-ui CSS variable `--bits-popover-anchor-width`.
	 *
	 * Usage:
	 *   <MultiSelect
	 *     options={[{ value: 'up', label: 'Up', count: 5, dotClass: 'dot-up' }]}
	 *     selected={['up']}
	 *     onToggle={(v) => toggle(v)}
	 *     placeholder="Status"
	 *   />
	 */
	import { Popover } from 'bits-ui';
	import { Check, ChevronDown } from '@lucide/svelte';
	import type { Snippet } from 'svelte';

	interface Option {
		value: string;
		label: string;
		disabled?: boolean;
		/** Per-option count to display beside the label (e.g. monitor count). */
		count?: number;
		/** CSS class for the status dot (e.g. 'dot dot-up'). Omit for plain options. */
		dotClass?: string;
		/** Inline style for tag color dot (hex). Omit for status options. */
		dotColor?: string;
	}

	interface Props {
		/** Available options. */
		options: Option[];
		/** Currently selected values. */
		selected?: string[];
		/** Placeholder shown when nothing is selected. */
		placeholder?: string;
		/** Message shown when the options array is empty. */
		emptyMessage?: string;
		/** Whether the entire select is disabled. */
		disabled?: boolean;
		/** Additional CSS class on the trigger button. */
		class?: string;
		/** aria-label for accessibility. */
		ariaLabel?: string;
		/** Called when the user clicks an item (pass the full new selection). */
		onToggle?: (value: string) => void;
		/** Called when the user clicks "Reset". */
		onReset?: () => void;
		/** Size variant — "default" for forms, "sm" for filter bars. */
		size?: 'default' | 'sm';
		/** Custom trigger content snippet. */
		triggerContent?: Snippet<[{ selected: string[]; count: number }]>;
	}

	let {
		options,
		selected = $bindable([]),
		placeholder = 'Select…',
		emptyMessage = 'No options available',
		disabled = false,
		class: className = '',
		ariaLabel,
		onToggle,
		onReset,
		size = 'default',
		triggerContent,
	}: Props = $props();

	let open = $state(false);
	let triggerEl = $state<HTMLButtonElement | null>(null);
	let optionButtons: HTMLButtonElement[] = [];

	const count = $derived(selected.length);

	/** Summary text for the trigger when no custom triggerContent is provided. */
	const summaryText = $derived.by(() => {
		if (count === 0) return placeholder;
		if (count === 1) {
			const item = options.find((o) => o.value === selected[0]);
			return item?.label ?? selected[0];
		}
		return `${count} selected`;
	});

	const triggerClass = $derived.by(() => {
		const base =
			'inline-flex items-center justify-between gap-2 rounded-lg border border-border bg-surface text-foreground transition-colors hover:border-border/80 focus:outline-none focus:ring-2 focus:ring-ring';
		const sz = size === 'sm' ? 'h-8 px-2.5 text-xs' : 'h-9 px-3 text-sm';
		const active = count > 0 ? ' border-primary/30' : '';
		return `${base} ${sz} ${active} ${className}`;
	});

	function handleToggle(value: string) {
		onToggle?.(value);
	}

	function handleReset(e: MouseEvent) {
		e.stopPropagation();
		onReset?.();
	}

	function enabledOptionButtons(): HTMLButtonElement[] {
		return optionButtons.filter((button) => button && !button.disabled);
	}

	function focusEdge(edge: 'first' | 'last') {
		queueMicrotask(() => {
			const buttons = enabledOptionButtons();
			buttons[edge === 'first' ? 0 : buttons.length - 1]?.focus();
		});
	}

	function handleTriggerKeydown(e: KeyboardEvent) {
		if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
			e.preventDefault();
			open = true;
			focusEdge(e.key === 'ArrowDown' ? 'first' : 'last');
		}
	}

	function handleListKeydown(e: KeyboardEvent) {
		const buttons = enabledOptionButtons();
		if (buttons.length === 0) return;
		const current = buttons.indexOf(document.activeElement as HTMLButtonElement);

		if (e.key === 'Escape') {
			e.preventDefault();
			open = false;
			queueMicrotask(() => triggerEl?.focus());
			return;
		}
		if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(e.key)) return;
		e.preventDefault();
		if (e.key === 'Home') buttons[0].focus();
		else if (e.key === 'End') buttons[buttons.length - 1].focus();
		else if (e.key === 'ArrowDown') buttons[(current + 1 + buttons.length) % buttons.length].focus();
		else buttons[(current - 1 + buttons.length) % buttons.length].focus();
	}
</script>

<Popover.Root bind:open>
	<Popover.Trigger
		bind:ref={triggerEl}
		class={triggerClass}
		aria-label={ariaLabel}
		aria-haspopup="listbox"
		aria-expanded={open}
		{disabled}
		onkeydown={handleTriggerKeydown}
	>
		{#if triggerContent}
			{@render triggerContent({ selected, count })}
		{:else}
			<span class="truncate" class:text-muted-foreground={count === 0}>
				{summaryText}
			</span>
		{/if}
		<ChevronDown
			class="h-3.5 w-3.5 shrink-0 text-muted-foreground transition-transform duration-200 {open ? 'rotate-180' : ''}"
		/>
	</Popover.Trigger>

	<Popover.Portal>
		<Popover.Content
			class="z-50 overflow-hidden rounded-xl border border-border bg-elevated shadow-xl shadow-black/30 backdrop-blur-sm"
			sideOffset={4}
			onOpenAutoFocus={(e) => {
				e.preventDefault();
				focusEdge('first');
			}}
			onkeydown={handleListKeydown}
		>
			<!--
				The width is pinned to the trigger via the CSS variable set by
				bits-ui Popover. We read it as `--bits-popover-anchor-width` and
				apply it as min-width so the dropdown always matches its trigger.
			-->
			<div
				class="min-w-[var(--bits-popover-anchor-width)] p-1"
				role="listbox"
				aria-label={ariaLabel ?? placeholder}
				aria-multiselectable="true"
			>
				{#if options.length === 0}
					<p class="px-2.5 py-3 text-center text-xs text-muted-foreground">{emptyMessage}</p>
				{:else}
					{#each options as opt, index (opt.value)}
					{@const isSelected = selected.includes(opt.value)}
					<button
						type="button"
						role="option"
						aria-selected={isSelected}
						disabled={opt.disabled}
						tabindex="-1"
						bind:this={optionButtons[index]}
						onclick={() => handleToggle(opt.value)}
						class="flex w-full items-center gap-2.5 rounded-lg px-2.5 py-2 text-sm text-foreground outline-none transition-colors
							hover:bg-accent hover:text-accent-foreground
							focus-visible:bg-accent focus-visible:text-accent-foreground focus-visible:ring-2 focus-visible:ring-ring
							disabled:cursor-not-allowed disabled:opacity-50
							data-[disabled]:cursor-not-allowed data-[disabled]:opacity-50"
					>
						<!-- Status dot or tag color dot -->
						{#if opt.dotClass}
							<span class="dot {opt.dotClass} shrink-0"></span>
						{:else if opt.dotColor}
							<span
								class="h-2 w-2 shrink-0 rounded-full"
								style="background-color: {opt.dotColor}"
							></span>
						{/if}

						<!-- Label -->
						<span class="flex-1 truncate text-left">{opt.label}</span>

						<!-- Count -->
						{#if opt.count !== undefined}
							<span class="tabular-nums text-muted-foreground">{opt.count}</span>
						{/if}

						<!-- Checkmark when selected -->
						{#if isSelected}
							<Check class="h-3.5 w-3.5 shrink-0 text-primary" />
						{/if}
					</button>
				{/each}
				{/if}

				<!-- Reset button (only when something is selected) -->
				{#if count > 0 && onReset}
					<div class="mt-1 border-t border-border pt-1">
						<button
							type="button"
							onclick={handleReset}
							class="flex w-full items-center justify-center rounded-lg px-2.5 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
						>
							Reset
						</button>
					</div>
				{/if}
			</div>
		</Popover.Content>
	</Popover.Portal>
</Popover.Root>
