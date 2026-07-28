<script lang="ts">
	import BrandMark from '$lib/components/BrandMark.svelte';
	import type { StatusPageDashboardStyle } from '$lib/api/statuspages';
	import { footerCopy, type OverallLevel } from '$lib/status-page-health';

	interface Props {
		footerText?: string;
		density: StatusPageDashboardStyle;
		level: OverallLevel;
		/** When false, hide the Phoenix mark and “Powered by Phoenix” (F3.5). */
		showPoweredBy?: boolean;
	}

	const WRAPPER: Record<StatusPageDashboardStyle, string> = {
		full: 'mt-16 border-t border-border pt-6',
		grid: 'mt-16 border-t border-border pt-6',
		pills: 'mt-8',
	};

	let { footerText, density, level, showPoweredBy = true }: Props = $props();
	let copy = $derived(footerCopy(footerText, level));
</script>

<footer class="text-center text-xs text-muted-foreground {WRAPPER[density]}">
	{copy}
	{#if showPoweredBy}
		<div class="mt-3 flex items-center justify-center gap-1.5 text-faint">
			<BrandMark size={18} />
			<span>Powered by Phoenix</span>
		</div>
	{/if}
</footer>
