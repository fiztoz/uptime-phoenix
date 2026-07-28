<script lang="ts">
	import { Popover } from 'bits-ui';
	import { CalendarDays, ChevronLeft, ChevronRight, Clock3 } from '@lucide/svelte';

	interface Props {
		id?: string;
		value?: string;
		placeholder?: string;
		disabled?: boolean;
		required?: boolean;
		class?: string;
		onValueChange?: (value: string) => void;
	}

	let {
		id,
		value = $bindable(''),
		placeholder = 'Select date and time',
		disabled = false,
		required = false,
		class: className = '',
		onValueChange,
	}: Props = $props();

	type Parts = {
		year: number;
		month: number;
		day: number;
		hour: number;
		minute: number;
	};
	type DayCell = Parts & {
		key: string;
		currentMonth: boolean;
		today: boolean;
		selected: boolean;
	};

	const weekdays = ['Su', 'Mo', 'Tu', 'We', 'Th', 'Fr', 'Sa'];
	let open = $state(false);
	let triggerEl = $state<HTMLButtonElement | null>(null);
	let dayButtons: HTMLButtonElement[] = [];
	let now = new Date();
	let viewYear = $state(now.getFullYear());
	let viewMonth = $state(now.getMonth());
	let hour = $state(String(now.getHours()).padStart(2, '0'));
	let minute = $state(String(now.getMinutes()).padStart(2, '0'));
	let lastExternalValue = $state(value);

	function parseValue(raw: string): Parts | null {
		const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(raw);
		if (!match) return null;
		return {
			year: Number(match[1]),
			month: Number(match[2]),
			day: Number(match[3]),
			hour: Number(match[4]),
			minute: Number(match[5]),
		};
	}

	function syncFromValue(raw: string) {
		const parsed = parseValue(raw);
		if (!parsed) return;
		viewYear = parsed.year;
		viewMonth = parsed.month - 1;
		hour = String(parsed.hour).padStart(2, '0');
		minute = String(parsed.minute).padStart(2, '0');
	}

	$effect(() => {
		if (value !== lastExternalValue) {
			lastExternalValue = value;
			syncFromValue(value);
		}
	});

	const selected = $derived(parseValue(value));
	const monthLabel = $derived(
		new Intl.DateTimeFormat(undefined, {
			month: 'long',
			year: 'numeric',
		}).format(new Date(viewYear, viewMonth, 1)),
	);
	const displayValue = $derived.by(() => {
		if (!selected) return '';
		const date = new Date(selected.year, selected.month - 1, selected.day, selected.hour, selected.minute);
		return new Intl.DateTimeFormat(undefined, {
			day: '2-digit',
			month: 'short',
			year: 'numeric',
			hour: '2-digit',
			minute: '2-digit',
		}).format(date);
	});

	const days = $derived.by<DayCell[]>(() => {
		const first = new Date(viewYear, viewMonth, 1);
		const start = new Date(viewYear, viewMonth, 1 - first.getDay());
		const today = new Date();
		return Array.from({ length: 42 }, (_, index) => {
			const date = new Date(start.getFullYear(), start.getMonth(), start.getDate() + index);
			const year = date.getFullYear();
			const month = date.getMonth() + 1;
			const day = date.getDate();
			return {
				year,
				month,
				day,
				hour: Number(hour),
				minute: Number(minute),
				key: `${year}-${month}-${day}`,
				currentMonth: date.getMonth() === viewMonth,
				today: year === today.getFullYear() && month === today.getMonth() + 1 && day === today.getDate(),
				selected: selected?.year === year && selected.month === month && selected.day === day,
			};
		});
	});

	function localValue(parts: Parts): string {
		const pad = (number: number) => String(number).padStart(2, '0');
		return `${parts.year}-${pad(parts.month)}-${pad(parts.day)}T${pad(parts.hour)}:${pad(parts.minute)}`;
	}

	function commit(next: string) {
		value = next;
		lastExternalValue = next;
		onValueChange?.(next);
	}

	function chooseDay(day: DayCell) {
		hour = sanitizeTime(hour, 23);
		minute = sanitizeTime(minute, 59);
		viewYear = day.year;
		viewMonth = day.month - 1;
		commit(localValue({ ...day, hour: Number(hour), minute: Number(minute) }));
	}

	function changeMonth(offset: number) {
		const next = new Date(viewYear, viewMonth + offset, 1);
		viewYear = next.getFullYear();
		viewMonth = next.getMonth();
	}

	function sanitizeTime(raw: string, maximum: number): string {
		const digits = raw.replace(/\D/g, '').slice(0, 2);
		if (!digits) return '00';
		return String(Math.min(Number(digits), maximum)).padStart(2, '0');
	}

	function applyTime() {
		hour = sanitizeTime(hour, 23);
		minute = sanitizeTime(minute, 59);
		if (selected) commit(localValue({ ...selected, hour: Number(hour), minute: Number(minute) }));
	}

	function selectNow() {
		now = new Date();
		viewYear = now.getFullYear();
		viewMonth = now.getMonth();
		hour = String(now.getHours()).padStart(2, '0');
		minute = String(now.getMinutes()).padStart(2, '0');
		commit(
			localValue({
				year: now.getFullYear(),
				month: now.getMonth() + 1,
				day: now.getDate(),
				hour: now.getHours(),
				minute: now.getMinutes(),
			}),
		);
	}

	function clearValue() {
		commit('');
	}

	function handleCalendarKeydown(event: KeyboardEvent) {
		if (event.key === 'Escape') {
			event.preventDefault();
			open = false;
			queueMicrotask(() => triggerEl?.focus());
			return;
		}
		const current = dayButtons.indexOf(event.target as HTMLButtonElement);
		if (current < 0) return;
		if (event.key === 'Home' || event.key === 'End') {
			event.preventDefault();
			const weekStart = current - (current % 7);
			dayButtons[event.key === 'Home' ? weekStart : Math.min(weekStart + 6, dayButtons.length - 1)]?.focus();
			return;
		}
		const offsets: Record<string, number> = {
			ArrowRight: 1,
			ArrowLeft: -1,
			ArrowDown: 7,
			ArrowUp: -7,
		};
		const offset = offsets[event.key];
		if (offset === undefined) return;
		event.preventDefault();
		dayButtons[Math.max(0, Math.min(dayButtons.length - 1, current + offset))]?.focus();
	}
</script>

<Popover.Root bind:open onOpenChange={(next) => next && syncFromValue(value)}>
	<Popover.Trigger
		bind:ref={triggerEl}
		{id}
		type="button"
		{disabled}
		aria-required={required}
		class="inline-flex h-9 w-full items-center gap-2 rounded-lg border border-border bg-surface px-3 text-left text-sm text-foreground transition-colors hover:border-primary/35 focus:outline-none focus:ring-2 focus:ring-ring disabled:cursor-not-allowed disabled:opacity-60 {className}"
	>
		<CalendarDays class="h-4 w-4 shrink-0 text-muted-foreground" />
		<span class="min-w-0 flex-1 truncate tabular-nums" class:text-faint={!displayValue}
			>{displayValue || placeholder}</span
		>
	</Popover.Trigger>

	<Popover.Portal>
		<Popover.Content
			class="z-50 w-[min(21rem,calc(100vw-2rem))] rounded-xl border border-border bg-elevated p-3 shadow-xl shadow-black/30 backdrop-blur-sm"
			sideOffset={6}
			align="start"
			collisionPadding={12}
			onkeydown={handleCalendarKeydown}
		>
			<div class="flex items-center justify-between">
				<button
					type="button"
					onclick={() => changeMonth(-1)}
					class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
					aria-label="Previous month"
				>
					<ChevronLeft class="h-4 w-4" />
				</button>
				<p class="text-sm font-semibold tracking-tight">{monthLabel}</p>
				<button
					type="button"
					onclick={() => changeMonth(1)}
					class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
					aria-label="Next month"
				>
					<ChevronRight class="h-4 w-4" />
				</button>
			</div>

			<div class="mt-2 grid grid-cols-7 gap-1" role="grid" aria-label={monthLabel}>
				{#each weekdays as weekday}
					<span role="columnheader" class="grid h-6 place-items-center text-[11px] font-medium text-muted-foreground">{weekday}</span>
				{/each}
				{#each days as day, index (day.key)}
					<button
						type="button"
						bind:this={dayButtons[index]}
						onclick={() => chooseDay(day)}
						aria-label={new Date(day.year, day.month - 1, day.day).toLocaleDateString()}
						aria-pressed={day.selected}
						class="relative grid h-8 place-items-center rounded-lg text-sm tabular-nums outline-none transition-colors focus-visible:ring-2 focus-visible:ring-ring
							{day.selected
							? 'bg-primary font-semibold text-primary-foreground'
							: day.currentMonth
								? 'text-foreground hover:bg-accent'
								: 'text-faint hover:bg-accent hover:text-muted-foreground'}"
					>
						{day.day}
						{#if day.today && !day.selected}<span class="absolute bottom-1 h-1 w-1 rounded-full bg-primary"></span>{/if}
					</button>
				{/each}
			</div>

			<div class="mt-3 flex items-end justify-between gap-3 border-t border-border pt-3">
				<div>
					<label
						class="flex items-center gap-1.5 text-xs font-medium text-muted-foreground"
						for="{id ?? 'date-time'}-hour"
					>
						<Clock3 class="h-3.5 w-3.5" /> Time
					</label>
					<div class="mt-1 flex items-center gap-1.5 font-mono text-sm">
						<input
							id="{id ?? 'date-time'}-hour"
							type="text"
							inputmode="numeric"
							maxlength="2"
							bind:value={hour}
							onblur={applyTime}
							class="h-8 w-10 rounded-lg border border-border bg-surface text-center tabular-nums outline-none focus:ring-2 focus:ring-ring"
							aria-label="{id ?? placeholder} hour"
						/>
						<span class="text-muted-foreground">:</span>
						<input
							type="text"
							inputmode="numeric"
							maxlength="2"
							bind:value={minute}
							onblur={applyTime}
							class="h-8 w-10 rounded-lg border border-border bg-surface text-center tabular-nums outline-none focus:ring-2 focus:ring-ring"
							aria-label="{id ?? placeholder} minute"
						/>
					</div>
				</div>
				<div class="flex gap-1">
					{#if value}
						<button
							type="button"
							onclick={clearValue}
							class="rounded-lg px-2.5 py-2 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
							>Clear</button
						>
					{/if}
					<button
						type="button"
						onclick={selectNow}
						class="rounded-lg px-3 py-2 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
						>Now</button
					>
					<button
						type="button"
						onclick={() => (open = false)}
						disabled={!value}
						class="rounded-lg bg-primary px-3 py-2 text-xs font-medium text-primary-foreground transition-colors hover:bg-primary/90 disabled:opacity-50"
						>Done</button
					>
				</div>
			</div>
		</Popover.Content>
	</Popover.Portal>
</Popover.Root>
