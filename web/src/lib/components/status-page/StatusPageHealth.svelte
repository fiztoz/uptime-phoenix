<script lang="ts">
	import { AlertOctagon, AlertTriangle, CheckCircle } from '@lucide/svelte';
	import {
		BANNER_TOKENS,
		bannerCopy,
		type OverallLevel,
	} from '$lib/status-page-health';

	interface Props {
		level: OverallLevel;
		downCount: number;
		activeIncidentCount: number;
		compact?: boolean;
	}

	const ICONS: Record<OverallLevel, typeof CheckCircle> = {
		operational: CheckCircle,
		degraded: AlertTriangle,
		outage: AlertOctagon,
	};

	let {
		level,
		downCount,
		activeIncidentCount,
		compact = false,
	}: Props = $props();
	let tokens = $derived(BANNER_TOKENS[level]);
	let copy = $derived(bannerCopy(level, downCount, activeIncidentCount));
	let Icon = $derived(ICONS[level]);
</script>

{#if compact}
	<div
		class="mb-6 flex flex-col gap-3 rounded-2xl border border-border bg-card/90 p-4 shadow-sm sm:flex-row sm:items-center sm:justify-between"
		aria-live="polite"
	>
		<div>
			<p class="text-sm font-semibold tracking-tight">Current health</p>
			<p class="mt-0.5 text-xs text-muted-foreground">{copy.title}</p>
		</div>
		<span
			class="inline-flex items-center gap-1.5 self-start rounded-full border px-2.5 py-1 text-xs font-medium {tokens.pill}"
		>
			<span class="dot {tokens.dot}"></span>
			{tokens.label}
		</span>
	</div>
{:else}
	<div
		class="mb-8 flex flex-col items-center gap-3 rounded-2xl border p-5 sm:flex-row sm:gap-4 sm:p-6 {tokens.tint}"
		aria-live="polite"
	>
		<span
			class="grid h-11 w-11 shrink-0 place-items-center rounded-xl {tokens.chip}"
		>
			<Icon class="h-6 w-6" />
		</span>
		<div class="text-center sm:text-left">
			<p class="text-lg font-semibold">{copy.title}</p>
			<p class="text-sm text-muted-foreground">{copy.subtitle}</p>
		</div>
	</div>
{/if}
