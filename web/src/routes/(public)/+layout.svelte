<script lang="ts">
	import '../../app.css';
	import type { Snippet } from 'svelte';

	let { children }: { children: Snippet } = $props();
</script>

<div class="min-h-dvh bg-background text-foreground">
	{@render children()}
</div>