<script lang="ts">
	import { page } from '$app/stores';
	import { onMount } from 'svelte';
	import EmptyState from '$lib/components/EmptyState.svelte';
	import { extensionsApi, type Extension } from '$lib/api/extensions';
	import { Puzzle } from '@lucide/svelte';
	import * as m from '$lib/paraglide/messages.js';

	let extensions = $state<Extension[]>([]);
	let loading = $state(true);

	let extensionId = $derived($page.params.id ?? '');
	let extension = $derived(extensions.find((ext) => ext.id === extensionId) ?? null);

	onMount(async () => {
		try {
			extensions = await extensionsApi.list();
		} catch {
			extensions = [];
		} finally {
			loading = false;
		}
	});
</script>

{#if loading}
	<div class="flex h-full min-h-0 items-center justify-center text-sm text-muted-foreground">
		<span class="animate-pulse-dot">{m.loading()}</span>
	</div>
{:else if !extension}
	<div class="p-4 md:p-6 lg:p-8">
		<EmptyState
			icon={Puzzle}
			title={m.extension_page_not_found_title()}
			description={m.extension_page_not_found_description()}
		/>
	</div>
{:else}
	<iframe
		src={extension.path}
		title={extension.title}
		class="block h-full min-h-0 w-full flex-1 border-0"
	></iframe>
{/if}
