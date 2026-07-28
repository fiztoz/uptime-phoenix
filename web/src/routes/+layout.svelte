<script lang="ts">
	import { Toaster } from 'svelte-sonner';
	import { onMount } from 'svelte';
	import '../app.css';
	import ConfirmDialog from '$lib/components/ConfirmDialog.svelte';
	import type { Snippet } from 'svelte';

	let { children }: { children: Snippet } = $props();

	let theme = $state<'light' | 'dark'>('dark');

	onMount(() => {
		// Initialize theme — default to dark
		document.documentElement.classList.add('dark');
	});
</script>

<Toaster
	position="bottom-right"
	richColors
	closeButton
	theme={theme}
/>

<!-- Single instance for the whole app; driven by confirmAction() from anywhere. -->
<ConfirmDialog />

{@render children()}
