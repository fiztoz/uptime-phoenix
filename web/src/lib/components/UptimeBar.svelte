<script lang="ts">
	interface Props {
		data: Array<{ date: string; status: 'up' | 'down' | 'pending' | 'maintenance' | 'none' }>;
	}

	let { data = [] }: Props = $props();

	// Show last 90 days, pad if needed
	const displayData = $derived.by(() => {
		const sorted = [...data].sort((a, b) => a.date.localeCompare(b.date));
		if (sorted.length >= 90) return sorted.slice(-90);
		// pad with none
		const pad = Array.from({ length: 90 - sorted.length }, (_, i) => ({
			date: `pad-${i}`,
			status: 'none' as const,
		}));
		return [...pad, ...sorted];
	});

	// No info/blue token exists in app.css (see StatusPill); "maintenance" gets
	// the stronger neutral (--color-muted-foreground) so it stays visually
	// distinct from a bare "none" (--color-muted) no-data day rather than
	// collapsing into the same washed-out segment.
	function getColor(status: string) {
		if (status === 'up') return 'bg-success';
		if (status === 'down') return 'bg-danger';
		if (status === 'pending') return 'bg-warning';
		if (status === 'maintenance') return 'bg-muted-foreground/50';
		return 'bg-muted';
	}
</script>

<div
	class="flex h-9 w-full items-stretch gap-[2px]"
	title="90-day uptime (green=up, red=down, gray=no data)"
>
	{#each displayData as day, i (i)}
		<div
			class="h-full flex-1 rounded-[2px] {getColor(
				day.status
			)} opacity-80 transition-all hover:opacity-100 hover:brightness-110"
			title={day.date + ': ' + day.status}
		></div>
	{/each}
</div>
