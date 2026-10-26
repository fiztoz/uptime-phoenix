<script lang="ts">
	import { onMount } from 'svelte';
	import { goto } from '$app/navigation';
	import { auth } from '#lib/stores/auth.svelte.js';

	onMount(async () => {
		await auth.loadUser();
		if (auth.isAuthenticated) {
			goto('/dashboard', { replace: true });
		} else {
			goto('/login', { replace: true });
		}
	});
</script>
