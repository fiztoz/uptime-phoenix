<script lang="ts">
	/**
	 * Modal that loads the RabbitMQ monitor setup guide (static markdown).
	 * Source of truth:
	 *   web/static/docs/rabbitmq-setup.md  →  /docs/rabbitmq-setup.md
	 *   docs/guides/rabbitmq-setup.md      (repo copy)
	 * User scripts: /docs/rabbitmq-monitor/*
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

	const GUIDE_PATH = '/docs/rabbitmq-setup.md';
	const GUIDE_FILENAME = 'phoenix-rabbitmq-setup.md';

	let content = $state<string | null>(null);
	let loadError = $state<string | null>(null);
	let loading = $state(false);

	$effect(() => {
		if (!open) return;
		let cancelled = false;
		loading = true;
		loadError = null;
		fetch(GUIDE_PATH)
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
		if (content) {
			const blob = new Blob([content], { type: 'text/markdown;charset=utf-8' });
			const url = URL.createObjectURL(blob);
			const a = document.createElement('a');
			a.href = url;
			a.download = GUIDE_FILENAME;
			a.click();
			URL.revokeObjectURL(url);
			return;
		}
		const a = document.createElement('a');
		a.href = GUIDE_PATH;
		a.download = GUIDE_FILENAME;
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
			aria-labelledby="rabbitmq-guide-title"
			tabindex="-1"
		>
			<div class="flex items-start justify-between gap-3 border-b border-border px-4 py-3 sm:px-5">
				<div class="min-w-0">
					<h3 id="rabbitmq-guide-title" class="text-base font-semibold tracking-tight">
						{m.rabbitmq_guide_title()}
					</h3>
					<p class="mt-0.5 text-xs text-muted-foreground">{m.rabbitmq_guide_subtitle()}</p>
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
					<p class="text-sm text-muted-foreground">{m.rabbitmq_guide_loading()}</p>
				{:else if loadError}
					<div
						class="flex gap-2 rounded-lg border border-warning/40 bg-warning/10 px-3 py-2.5 text-sm text-foreground"
						role="alert"
					>
						<AlertTriangle class="mt-0.5 h-4 w-4 shrink-0 text-warning" />
						<div>
							<p class="font-medium">{m.rabbitmq_guide_load_failed()}</p>
							<p class="mt-1 text-xs text-muted-foreground">{loadError}</p>
							<p class="mt-2 text-xs text-muted-foreground">{m.rabbitmq_guide_load_failed_hint()}</p>
						</div>
					</div>
				{:else if content}
					<MarkdownPreview source={content} />
					<div class="mt-6 border-t border-border pt-4">
						<p class="mb-2 text-sm font-medium text-foreground">{m.rabbitmq_guide_scripts_heading()}</p>
						<ul class="space-y-1.5 text-sm text-muted-foreground">
							<li>
								<a
									class="text-primary underline-offset-2 hover:underline"
									href="/docs/rabbitmq-monitor/create-user-rabbitmq.sh"
									download
								>
									create-user-rabbitmq.sh
								</a>
							</li>
							<li>
								<a
									class="text-primary underline-offset-2 hover:underline"
									href="/docs/rabbitmq-monitor/phoenix-monitor-definitions.json"
									download
								>
									phoenix-monitor-definitions.json
								</a>
							</li>
						</ul>
					</div>
				{/if}
			</div>

			<div
				class="flex flex-wrap items-center justify-end gap-2 border-t border-border bg-surface/40 px-4 py-3 sm:px-5"
			>
				<a
					href={GUIDE_PATH}
					target="_blank"
					rel="noopener noreferrer"
					class="inline-flex items-center gap-1.5 rounded-lg border border-border px-3 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
				>
					<ExternalLink class="h-3.5 w-3.5" />
					{m.rabbitmq_guide_open_new_tab()}
				</a>
				<button
					type="button"
					onclick={downloadGuide}
					class="inline-flex items-center gap-1.5 rounded-lg bg-primary px-3 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90"
				>
					<Download class="h-3.5 w-3.5" />
					{m.rabbitmq_guide_download()}
				</button>
			</div>
		</div>
	</div>
{/if}
