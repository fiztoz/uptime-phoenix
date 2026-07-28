<script lang="ts">
	/**
	 * Modal that loads the remote Docker setup guide (static markdown) for
	 * in-app viewing and download. Source of truth:
	 *   web/static/docs/docker-remote-setup.md  →  /docs/docker-remote-setup.md
	 *   docs/guides/docker-remote-setup.md      (repo copy)
	 */
	import { X, Download, ExternalLink, AlertTriangle } from '@lucide/svelte';
	import { modalFocus } from '$lib/actions/modalFocus';
	import MarkdownPreview from '$lib/components/MarkdownPreview.svelte';
	import * as m from '$lib/paraglide/messages.js';

	interface Props {
		open?: boolean;
		onClose?: () => void;
	}

	let { open = $bindable(false), onClose }: Props = $props();

	/** Public URL of the static guide (SvelteKit static/ → site root). */
	const DOCKER_GUIDE_PATH = '/docs/docker-remote-setup.md';
	const DOCKER_GUIDE_FILENAME = 'phoenix-docker-remote-setup.md';

	let content = $state<string | null>(null);
	let loadError = $state<string | null>(null);
	let loading = $state(false);

	$effect(() => {
		if (!open) return;
		let cancelled = false;
		loading = true;
		loadError = null;
		fetch(DOCKER_GUIDE_PATH)
			.then(async (res) => {
				if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
				const text = await res.text();
				if (!cancelled) {
					content = text;
					loading = false;
				}
			})
			.catch((err: unknown) => {
				if (!cancelled) {
					loadError = err instanceof Error ? err.message : String(err);
					loading = false;
				}
			});
		return () => {
			cancelled = true;
		};
	});

	function close() {
		open = false;
		onClose?.();
	}

	function downloadGuide() {
		// Prefer already-fetched body so download works even if a second fetch fails.
		if (content) {
			const blob = new Blob([content], { type: 'text/markdown;charset=utf-8' });
			const url = URL.createObjectURL(blob);
			const a = document.createElement('a');
			a.href = url;
			a.download = DOCKER_GUIDE_FILENAME;
			a.click();
			URL.revokeObjectURL(url);
			return;
		}
		const a = document.createElement('a');
		a.href = DOCKER_GUIDE_PATH;
		a.download = DOCKER_GUIDE_FILENAME;
		a.target = '_blank';
		a.rel = 'noopener';
		a.click();
	}
</script>

{#if open}
	<div class="fixed inset-0 z-[60] flex items-end justify-center bg-black/60 p-0 backdrop-blur-sm sm:items-center sm:p-4">
		<button
			type="button"
			tabindex="-1"
			class="absolute inset-0 cursor-default"
			onclick={close}
			aria-label={m.btn_close()}
		></button>
		<div
			use:modalFocus={{ onClose: close }}
			class="relative z-10 flex max-h-[90dvh] w-full max-w-3xl flex-col overflow-hidden rounded-t-xl border border-border bg-card shadow-xl sm:rounded-xl"
			role="dialog"
			aria-modal="true"
			aria-labelledby="docker-guide-title"
			tabindex="-1"
		>
			<div class="flex items-start justify-between gap-3 border-b border-border px-4 py-3 sm:px-5">
				<div class="min-w-0">
					<h3 id="docker-guide-title" class="text-base font-semibold tracking-tight">
						{m.docker_guide_title()}
					</h3>
					<p class="mt-0.5 text-xs text-muted-foreground">{m.docker_guide_subtitle()}</p>
				</div>
				<button
					type="button"
					onclick={close}
					class="grid h-8 w-8 shrink-0 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
					aria-label={m.btn_close()}
				>
					<X class="h-5 w-5" />
				</button>
			</div>

			<div class="min-h-0 flex-1 overflow-y-auto px-4 py-4 sm:px-5">
				{#if loading}
					<p class="text-sm text-muted-foreground">{m.docker_guide_loading()}</p>
				{:else if loadError}
					<div
						class="flex gap-2 rounded-lg border border-warning/40 bg-warning/10 px-3 py-2.5 text-sm text-foreground"
						role="alert"
					>
						<AlertTriangle class="mt-0.5 h-4 w-4 shrink-0 text-warning" />
						<div>
							<p class="font-medium">{m.docker_guide_load_failed()}</p>
							<p class="mt-1 text-xs text-muted-foreground">{loadError}</p>
							<p class="mt-2 text-xs text-muted-foreground">{m.docker_guide_load_failed_hint()}</p>
						</div>
					</div>
				{:else if content}
					<MarkdownPreview source={content} />
				{/if}
			</div>

			<div
				class="flex flex-wrap items-center justify-end gap-2 border-t border-border bg-surface/40 px-4 py-3 sm:px-5"
			>
				<a
					href={DOCKER_GUIDE_PATH}
					target="_blank"
					rel="noopener noreferrer"
					class="inline-flex items-center gap-1.5 rounded-lg border border-border px-3 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
				>
					<ExternalLink class="h-3.5 w-3.5" />
					{m.docker_guide_open_new_tab()}
				</a>
				<button
					type="button"
					onclick={downloadGuide}
					class="inline-flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90"
				>
					<Download class="h-3.5 w-3.5" />
					{m.docker_guide_download()}
				</button>
			</div>
		</div>
	</div>
{/if}
