<script lang="ts">
	import { Popover } from 'bits-ui';
	import { Check, ChevronDown } from '@lucide/svelte';

	interface Props {
		id?: string;
		value?: string;
		disabled?: boolean;
		class?: string;
		ariaLabel?: string;
		onValueChange?: (value: string) => void;
	}

	let { id, value = $bindable('#666666'), disabled = false, class: className = '', ariaLabel = 'Choose color', onValueChange }: Props = $props();

	const palette = [
		'#F15B3A',
		'#E5484D',
		'#D97706',
		'#65A30D',
		'#16A085',
		'#0E7490',
		'#2563EB',
		'#4F46E5',
		'#7C3AED',
		'#C026D3',
		'#DB2777',
		'#64748B',
	];

	let open = $state(false);
	let triggerEl = $state<HTMLButtonElement | null>(null);
	let colorButtons: HTMLButtonElement[] = [];
	let draft = $state(value.toUpperCase());
	let lastExternalValue = $state(value);

	$effect(() => {
		if (value !== lastExternalValue) {
			lastExternalValue = value;
			draft = value.toUpperCase();
		}
	});

	function selectColor(color: string) {
		const normalized = color.toUpperCase();
		value = normalized;
		lastExternalValue = normalized;
		draft = normalized;
		onValueChange?.(normalized);
	}

	function handleHexInput(event: Event) {
		draft = (event.currentTarget as HTMLInputElement).value.toUpperCase();
		if (/^#[0-9A-F]{6}$/.test(draft)) selectColor(draft);
	}

	function restoreInvalidDraft() {
		if (!/^#[0-9A-F]{6}$/.test(draft)) draft = value.toUpperCase();
	}

	function focusSelectedColor() {
		queueMicrotask(() => {
			const selectedIndex = palette.findIndex((color) => color === value.toUpperCase());
			colorButtons[Math.max(selectedIndex, 0)]?.focus();
		});
	}

	function handlePaletteKeydown(event: KeyboardEvent) {
		const target = event.target as HTMLElement;
		if (event.key === 'Escape') {
			event.preventDefault();
			open = false;
			queueMicrotask(() => triggerEl?.focus());
			return;
		}
		if (target.getAttribute('role') !== 'option') return;
		const current = colorButtons.indexOf(target as HTMLButtonElement);
		const offsets: Record<string, number> = {
			ArrowRight: 1,
			ArrowLeft: -1,
			ArrowDown: 6,
			ArrowUp: -6,
		};
		if (event.key === 'Home' || event.key === 'End') {
			event.preventDefault();
			colorButtons[event.key === 'Home' ? 0 : colorButtons.length - 1]?.focus();
			return;
		}
		const offset = offsets[event.key];
		if (offset === undefined) return;
		event.preventDefault();
		colorButtons[(current + offset + colorButtons.length) % colorButtons.length]?.focus();
	}
</script>

<Popover.Root bind:open>
	<Popover.Trigger
		bind:ref={triggerEl}
		{id}
		type="button"
		{disabled}
		aria-label={id ? undefined : ariaLabel}
		class="inline-flex h-9 min-w-28 items-center justify-between gap-2 rounded-lg border border-border bg-surface px-2.5 text-sm font-mono text-foreground transition-colors hover:border-primary/35 focus:outline-none focus:ring-2 focus:ring-ring disabled:cursor-not-allowed disabled:opacity-60 {className}"
	>
		<span class="h-5 w-5 shrink-0 rounded-md border border-white/15 shadow-inner" style="background-color: {value}"
		></span>
		<span class="tabular-nums">{value.toUpperCase()}</span>
		<ChevronDown
			class="h-3.5 w-3.5 shrink-0 text-muted-foreground transition-transform duration-200 {open ? 'rotate-180' : ''}"
		/>
	</Popover.Trigger>

	<Popover.Portal>
		<Popover.Content
			class="z-50 w-[min(19rem,calc(100vw-2rem))] rounded-xl border border-border bg-elevated p-3 shadow-xl shadow-black/30 backdrop-blur-sm"
			sideOffset={6}
			align="start"
			onOpenAutoFocus={(event) => {
				event.preventDefault();
				focusSelectedColor();
			}}
			onkeydown={handlePaletteKeydown}
		>
			<div class="flex items-center justify-between gap-3">
				<div>
					<p class="text-sm font-semibold tracking-tight">Tag color</p>
					<p class="text-xs text-muted-foreground">Pick a palette color or enter a hex value.</p>
				</div>
				<span class="h-9 w-9 shrink-0 rounded-lg border border-white/15 shadow-inner" style="background-color: {value}"
				></span>
			</div>

			<div class="mt-3 grid grid-cols-6 gap-2" role="listbox" aria-label="Tag color palette">
				{#each palette as color, index}
					{@const selected = value.toUpperCase() === color}
					<button
						type="button"
						role="option"
						aria-selected={selected}
						aria-label={color}
						tabindex="-1"
						bind:this={colorButtons[index]}
						onclick={() => selectColor(color)}
						class="grid aspect-square place-items-center rounded-lg border border-white/10 outline-none transition-transform hover:scale-105 focus-visible:ring-2 focus-visible:ring-ring active:scale-95"
						style="background-color: {color}"
					>
						{#if selected}<Check class="h-4 w-4 text-white drop-shadow" strokeWidth={3} />{/if}
					</button>
				{/each}
			</div>

			<div class="mt-3 border-t border-border pt-3">
				<label for="{id ?? 'tag-color'}-hex" class="text-xs font-medium text-muted-foreground">Custom hex</label>
				<div
					class="mt-1 flex items-center gap-2 rounded-lg border border-border bg-surface px-3 focus-within:border-primary/40 focus-within:ring-2 focus-within:ring-ring"
				>
					<span class="h-3 w-3 shrink-0 rounded-full" style="background-color: {value}"></span>
					<input
						id="{id ?? 'tag-color'}-hex"
						type="text"
						value={draft}
						maxlength="7"
						spellcheck="false"
						oninput={handleHexInput}
						onblur={restoreInvalidDraft}
						class="h-9 min-w-0 flex-1 bg-transparent font-mono text-sm uppercase outline-none placeholder:text-faint"
						placeholder="#F15B3A"
					/>
				</div>
			</div>
		</Popover.Content>
	</Popover.Portal>
</Popover.Root>
