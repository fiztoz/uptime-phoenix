<script lang="ts">
	/**
	 * Reusable searchable monitor combobox.
	 *
	 * Consumer supplies the full monitor list (typically from `monitorsApi.list()`)
	 * plus a set of monitor IDs to exclude (e.g. monitors already assigned
	 * elsewhere) and an `onSelect` callback. This component owns no network
	 * calls and no assignment semantics — it is purely "pick one monitor from
	 * a filtered, keyboard-navigable list" so it can be reused anywhere a
	 * monitor needs picking (status pages today, maintenance windows later).
	 */
	import type { Monitor } from '$lib/stores/ws.svelte.js';
	import { Search, Loader2, AlertCircle } from '@lucide/svelte';

	interface Props {
		/** Full candidate monitor list (unfiltered). */
		monitors: Monitor[];
		/** Monitor IDs to hide from the pick list (e.g. already assigned). */
		exclude?: Iterable<number>;
		/** Shows a loading state instead of the list (monitors still loading). */
		loading?: boolean;
		/** Shows an error state instead of the list. */
		error?: string | null;
		/** Input placeholder text. */
		placeholder?: string;
		/** Message shown when there are no pickable monitors left. */
		emptyText?: string;
		/** Called with the chosen monitor. The picker clears its own query after. */
		onSelect: (monitor: Monitor) => void;
	}

	let {
		monitors,
		exclude,
		loading = false,
		error = null,
		placeholder = 'Search monitors by name…',
		emptyText = 'No monitors available to add.',
		onSelect,
	}: Props = $props();

	let query = $state('');
	let open = $state(false);
	let activeIndex = $state(-1);
	let inputEl: HTMLInputElement | undefined = $state();
	let rootEl: HTMLDivElement | undefined = $state();

	// Stable-ish id suffix so multiple pickers on one page don't collide on aria ids.
	const uid = Math.random().toString(36).slice(2, 9);
	const listboxId = `monitor-picker-listbox-${uid}`;
	const optionId = (i: number) => `monitor-picker-option-${uid}-${i}`;

	const excludeSet = $derived(new Set(exclude ?? []));

	const filtered = $derived.by(() => {
		const q = query.trim().toLowerCase();
		return monitors
			.filter((m) => !excludeSet.has(m.id))
			.filter((m) => {
				if (!q) return true;
				return (
					m.name.toLowerCase().includes(q) ||
					m.type.toLowerCase().includes(q) ||
					(m.target ?? '').toLowerCase().includes(q)
				);
			});
	});

	const dotClass: Record<string, string> = {
		up: 'dot-up',
		down: 'dot-down',
		pending: 'dot-warn',
		maintenance: 'dot-muted',
		paused: 'dot-muted',
	};

	function openList() {
		if (loading || error) return;
		open = true;
		activeIndex = filtered.length > 0 ? 0 : -1;
	}

	function closeList() {
		open = false;
		activeIndex = -1;
	}

	function pick(monitor: Monitor) {
		onSelect(monitor);
		query = '';
		closeList();
		inputEl?.focus();
	}

	function onInputFocus() {
		openList();
	}

	function onFocusOut(e: FocusEvent) {
		// Close only when focus leaves the whole widget (not moving from input to a list item).
		const next = e.relatedTarget as Node | null;
		if (rootEl && next && rootEl.contains(next)) return;
		closeList();
	}

	function onKeydown(e: KeyboardEvent) {
		if (e.key === 'ArrowDown') {
			e.preventDefault();
			if (!open) {
				openList();
				return;
			}
			if (filtered.length === 0) return;
			activeIndex = (activeIndex + 1) % filtered.length;
		} else if (e.key === 'ArrowUp') {
			e.preventDefault();
			if (!open) {
				openList();
				return;
			}
			if (filtered.length === 0) return;
			activeIndex = (activeIndex - 1 + filtered.length) % filtered.length;
		} else if (e.key === 'Enter') {
			if (open && activeIndex >= 0 && filtered[activeIndex]) {
				e.preventDefault();
				pick(filtered[activeIndex]);
			}
		} else if (e.key === 'Escape') {
			if (open) {
				e.preventDefault();
				e.stopPropagation();
				closeList();
			}
		}
	}

	const inputClass =
		'w-full rounded-lg border border-border bg-surface py-2 pl-9 pr-3 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring';
</script>

<div bind:this={rootEl} class="relative" onfocusout={onFocusOut}>
	<div class="relative">
		<Search
			class="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-faint"
		/>
		<input
			bind:this={inputEl}
			type="text"
			role="combobox"
			aria-expanded={open}
			aria-controls={listboxId}
			aria-autocomplete="list"
			aria-activedescendant={open && activeIndex >= 0
				? optionId(activeIndex)
				: undefined}
			autocomplete="off"
			bind:value={query}
			{placeholder}
			disabled={loading || !!error}
			onfocus={onInputFocus}
			onclick={openList}
			onkeydown={onKeydown}
			class={inputClass}
		/>
	</div>

	{#if open}
		<div
			id={listboxId}
			role="listbox"
			class="absolute z-20 mt-1.5 max-h-64 w-full overflow-y-auto rounded-lg border border-border bg-card p-1 shadow-[0_18px_40px_-24px_rgba(0,0,0,0.8)]"
		>
			{#if loading}
				<div
					class="flex items-center gap-2 px-3 py-3 text-sm text-muted-foreground"
				>
					<Loader2 class="h-4 w-4 animate-spin" /> Loading monitors…
				</div>
			{:else if error}
				<div class="flex items-center gap-2 px-3 py-3 text-sm text-danger">
					<AlertCircle class="h-4 w-4 shrink-0" />
					{error}
				</div>
			{:else if filtered.length === 0}
				<p class="px-3 py-3 text-sm text-muted-foreground">
					{query.trim() ? 'No monitors match your search.' : emptyText}
				</p>
			{:else}
				{#each filtered as monitor, i (monitor.id)}
					<button
						id={optionId(i)}
						type="button"
						role="option"
						aria-selected={i === activeIndex}
						tabindex="-1"
						onmousedown={(e) => e.preventDefault()}
						onclick={() => pick(monitor)}
						onmouseenter={() => (activeIndex = i)}
						class="flex w-full items-center gap-2.5 rounded-md px-2.5 py-2 text-left text-sm transition-colors {i ===
						activeIndex
							? 'bg-accent text-accent-foreground'
							: 'hover:bg-accent/50'}"
					>
						<span class="dot {dotClass[monitor.status] ?? 'dot-muted'} shrink-0"
						></span>
						<span class="min-w-0 flex-1">
							<span class="block truncate font-medium">{monitor.name}</span>
							<span
								class="block truncate font-mono text-xs text-muted-foreground"
							>
								<span class="text-faint uppercase">{monitor.type}</span>
								{#if monitor.target}<span class="mx-1 text-faint">·</span
									>{monitor.target}{/if}
							</span>
						</span>
					</button>
				{/each}
			{/if}
		</div>
	{/if}
</div>
