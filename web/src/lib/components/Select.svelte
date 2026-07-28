<script lang="ts">
	/**
	 * Phoenix Select — styled dropdown using bits-ui Select.
	 *
	 * Drop-in replacement for native `<select>` that matches the DESIGN.md
	 * token system: bg-surface, border-border, focus ring, muted-foreground
	 * chevron, 200ms transitions, and the elevated popover surface.
	 *
	 * Supports optional option groups (section headings) for long lists such
	 * as the monitor-type picker — pass `groups` instead of a flat `options`.
	 *
	 * Usage:
	 *   <Select
	 *     options={[{ value: 'a', label: 'Alpha' }]}
	 *     value={selected}
	 *     onValueChange={(v) => (selected = v)}
	 *     placeholder="Pick one…"
	 *   />
	 *
	 *   <Select
	 *     groups={[
	 *       { label: 'General', options: [{ value: 'http', label: 'HTTP(s)', description: '…' }] },
	 *     ]}
	 *     value={selectedType}
	 *     onValueChange={(v) => (selectedType = v)}
	 *   />
	 */
	import { Select } from 'bits-ui';
	import { ChevronDown, Check } from '@lucide/svelte';
	import type { Snippet } from 'svelte';

	interface Option {
		value: string;
		label: string;
		disabled?: boolean;
		/** Optional secondary line under the label (type picker blurbs). */
		description?: string;
	}

	interface OptionGroup {
		label: string;
		options: Option[];
	}

	interface Props {
		/** ID applied to the trigger so a visible label can target it. */
		id?: string;
		/** Flat options (default mode). Ignored when `groups` is provided. */
		options?: Option[];
		/**
		 * Grouped options with section headings. When set, takes precedence
		 * over `options` for rendering and selection lookup.
		 */
		groups?: OptionGroup[];
		/** Currently selected value (bindable). */
		value?: string;
		/** Placeholder shown when nothing is selected. */
		placeholder?: string;
		/** Message shown when the options array is empty. */
		emptyMessage?: string;
		/** Whether the select is disabled. */
		disabled?: boolean;
		/** Additional CSS class on the trigger button. */
		class?: string;
		/** Unique name for the hidden form input (enables native form submission). */
		name?: string;
		/** aria-label for accessibility when no visible label is paired. */
		ariaLabel?: string;
		/** Called when the selected value changes. */
		onValueChange?: (value: string) => void;
		/** Optional snippet for fully custom trigger content. */
		triggerContent?: Snippet<[{ label: string | undefined; value: string | undefined }]>;
		/** Size variant — "default" for forms, "sm" for filter bars / chart controls. */
		size?: 'default' | 'sm';
	}

	let {
		id,
		options = [],
		groups,
		value = $bindable(undefined),
		placeholder = 'Select…',
		emptyMessage = 'No options available',
		disabled = false,
		class: className = '',
		name,
		ariaLabel,
		onValueChange,
		triggerContent,
		size = 'default',
	}: Props = $props();

	/** Flatten groups so selection lookup and bits-ui `items` stay correct. */
	const flatOptions = $derived(
		groups && groups.length > 0 ? groups.flatMap((g) => g.options) : options,
	);

	const selected = $derived(flatOptions.find((o) => o.value === value));
	const hasDescriptions = $derived(flatOptions.some((o) => Boolean(o.description)));

	/* Trigger height / padding scales with size variant. */
	const triggerClass = $derived.by(() => {
		const base =
			'inline-flex items-center justify-between gap-2 rounded-lg border border-border bg-surface text-foreground transition-colors hover:border-border/80 focus:outline-none focus:ring-2 focus:ring-ring data-[disabled]:opacity-60 data-[disabled]:cursor-not-allowed';
		const sz = size === 'sm' ? 'h-8 px-2.5 text-xs' : 'h-9 px-3 text-sm';
		return `${base} ${sz} ${className}`;
	});

	function handleChange(v: string) {
		value = v;
		onValueChange?.(v);
	}
</script>

{#snippet optionItem(opt: Option)}
	<Select.Item
		value={opt.value}
		label={opt.label}
		disabled={opt.disabled}
		class="relative flex items-center gap-2 rounded-lg px-2.5 py-2 text-sm text-foreground outline-none transition-colors data-[highlighted]:bg-accent data-[highlighted]:text-accent-foreground data-[disabled]:opacity-50"
	>
		{#snippet children({ selected: isSelected })}
			<span class="flex min-w-0 flex-1 flex-col gap-0.5">
				<span class="truncate leading-tight {opt.description ? 'font-medium' : ''}">{opt.label}</span>
				{#if opt.description}
					<span class="truncate text-[11px] leading-snug text-muted-foreground">{opt.description}</span>
				{/if}
			</span>
			{#if isSelected}
				<Check class="h-3.5 w-3.5 shrink-0 text-primary" />
			{/if}
		{/snippet}
	</Select.Item>
{/snippet}

<Select.Root
	type="single"
	{value}
	{disabled}
	items={flatOptions}
	onValueChange={handleChange}
	{name}
>
	<Select.Trigger {id} class={triggerClass} aria-label={ariaLabel}>
		{#if triggerContent}
			{@render triggerContent({ label: selected?.label, value: selected?.value })}
		{:else if selected}
			<span class="truncate">{selected.label}</span>
		{:else}
			<Select.Value class="truncate" {placeholder} />
		{/if}
		<ChevronDown class="h-3.5 w-3.5 shrink-0 text-muted-foreground transition-transform duration-200" />
	</Select.Trigger>

	<Select.Portal>
		<Select.Content
			class="z-50 max-h-80 overflow-hidden rounded-xl border border-border bg-elevated shadow-xl shadow-black/30 backdrop-blur-sm {hasDescriptions
				? 'min-w-[min(22rem,calc(100vw-2rem))]'
				: ''}"
			sideOffset={4}
		>
			<Select.Viewport class="min-w-[var(--bits-select-anchor-width)] p-1">
				{#if flatOptions.length === 0}
					<p class="px-2.5 py-3 text-center text-xs text-muted-foreground">{emptyMessage}</p>
				{:else if groups && groups.length > 0}
					{#each groups as group, gi (group.label)}
						{#if group.options.length > 0}
							<Select.Group>
								<Select.GroupHeading
									class="px-2.5 {gi === 0
										? 'pt-1.5'
										: 'mt-1 border-t border-border/60 pt-2'} pb-1 text-[10px] font-semibold uppercase tracking-wider text-muted-foreground"
								>
									{group.label}
								</Select.GroupHeading>
								{#each group.options as opt (opt.value)}
									{@render optionItem(opt)}
								{/each}
							</Select.Group>
						{/if}
					{/each}
				{:else}
					{#each options as opt (opt.value)}
						{@render optionItem(opt)}
					{/each}
				{/if}
			</Select.Viewport>
		</Select.Content>
	</Select.Portal>
</Select.Root>
