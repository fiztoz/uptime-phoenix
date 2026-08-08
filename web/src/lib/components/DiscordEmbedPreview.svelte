<script lang="ts">
	import BrandMark from '$lib/components/BrandMark.svelte';
	import { renderMarkdownToSafeHtml } from '$lib/utils/markdown.js';
	import type { DiscordEmbedFieldTemplate } from '$lib/api/notification-templates';

	interface PreviewField extends DiscordEmbedFieldTemplate {
		name: string;
		value: string;
	}

	interface Props {
		title: string;
		description: string;
		titleUrl?: string;
		fields?: PreviewField[];
		footer?: string;
		showTimestamp?: boolean;
		color: string;
	}

	let {
		title,
		description,
		titleUrl = '',
		fields = [],
		footer = '',
		showTimestamp = true,
		color,
	}: Props = $props();

	function expandDiscordTimestamps(source: string): string {
		return source.replace(/<t:(\d+):([tTdDfFR])>/g, (_match, seconds: string, style: string) => {
			const date = new Date(Number(seconds) * 1000);
			if (Number.isNaN(date.getTime())) return _match;
			if (style === 'R') return 'a few minutes ago';
			if (style === 't') return date.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
			if (style === 'T') return date.toLocaleTimeString();
			if (style === 'd') return date.toLocaleDateString();
			return date.toLocaleString([], {
				weekday: style === 'F' ? 'long' : undefined,
				year: 'numeric',
				month: style === 'D' || style === 'F' ? 'long' : 'short',
				day: 'numeric',
				hour: style === 'f' || style === 'F' ? '2-digit' : undefined,
				minute: style === 'f' || style === 'F' ? '2-digit' : undefined,
			});
		});
	}

	function discordMarkdown(source: string): string {
		return renderMarkdownToSafeHtml(expandDiscordTimestamps(source));
	}

	const renderedDescription = $derived(discordMarkdown(description));
	const renderedFields = $derived(fields.map((field) => ({ ...field, html: discordMarkdown(field.value) })));
	const renderedFooter = $derived(expandDiscordTimestamps(footer));
</script>

<div class="discord-preview overflow-hidden rounded-xl" aria-label="Discord message preview">
	<div class="discord-channel-bar flex items-center gap-2 px-4 py-3 text-sm font-semibold">
		<span class="text-lg leading-none text-[var(--discord-muted)]">#</span>
		<span>alerts</span>
	</div>
	<div class="discord-message flex gap-3 px-4 py-5 sm:px-5">
		<div class="discord-avatar grid h-10 w-10 shrink-0 place-items-center rounded-full">
			<BrandMark size={24} />
		</div>
		<div class="min-w-0 flex-1">
			<div class="flex flex-wrap items-baseline gap-1.5">
				<span class="font-semibold text-[var(--discord-heading)]">Phoenix Monitor</span>
				<span class="rounded bg-[var(--discord-bot)] px-1 py-px text-[10px] font-semibold uppercase leading-none text-[var(--discord-heading)]">App</span>
				<span class="text-xs text-[var(--discord-muted)]">Today at 09:41</span>
			</div>

			<div class="discord-embed relative mt-1 max-w-[520px] overflow-hidden rounded border border-[var(--discord-embed-border)] bg-[var(--discord-embed)] p-4 pl-5" style={`--embed-color: ${color}`}>
				<span class="absolute inset-y-0 left-0 w-1 bg-[var(--embed-color)]"></span>
				{#if title}
					{#if titleUrl}
						<a href={titleUrl} target="_blank" rel="noreferrer" class="block break-words text-base font-semibold leading-5 text-[var(--discord-link)] hover:underline">{title}</a>
					{:else}
						<p class="break-words text-base font-semibold leading-5 text-[var(--discord-heading)]">{title}</p>
					{/if}
				{/if}
				{#if renderedDescription}
					<div class="discord-markdown mt-2 break-words text-sm leading-[1.375rem] text-[var(--discord-text)]">{@html renderedDescription}</div>
				{/if}
				{#if renderedFields.length > 0}
					<div class="mt-4 grid grid-cols-3 gap-x-3 gap-y-3">
						{#each renderedFields as field, index (`${field.name}-${index}`)}
							<div class:min-w-0={field.inline} class:col-span-3={!field.inline}>
								<p class="break-words text-sm font-semibold leading-[1.125rem] text-[var(--discord-heading)]">{field.name}</p>
								<div class="discord-markdown mt-0.5 break-words text-sm leading-[1.125rem] text-[var(--discord-text)]">{@html field.html}</div>
							</div>
						{/each}
					</div>
				{/if}
				{#if renderedFooter || showTimestamp}
					<div class="mt-4 flex flex-wrap items-center gap-1 text-xs text-[var(--discord-muted)]">
						{#if renderedFooter}<span>{renderedFooter}</span>{/if}
						{#if renderedFooter && showTimestamp}<span>•</span>{/if}
						{#if showTimestamp}<span>Today at 09:41</span>{/if}
					</div>
				{/if}
			</div>
		</div>
	</div>
</div>

<style>
	.discord-preview {
		background: var(--discord-canvas);
		color: var(--discord-text);
	}

	.discord-channel-bar {
		background: var(--discord-channel);
		border-bottom: 1px solid var(--discord-embed-border);
		color: var(--discord-heading);
	}

	.discord-avatar {
		background: var(--discord-avatar);
	}

	.discord-markdown :global(p) {
		margin: 0;
	}

	.discord-markdown :global(p + p) {
		margin-top: 0.5rem;
	}

	.discord-markdown :global(strong) {
		font-weight: 600;
		color: var(--discord-heading);
	}

	.discord-markdown :global(a) {
		color: var(--discord-link);
		text-decoration: none;
	}

	.discord-markdown :global(a:hover) {
		text-decoration: underline;
	}

	.discord-markdown :global(code) {
		border-radius: 0.25rem;
		background: var(--discord-embed-border);
		padding: 0.05rem 0.25rem;
		font-family: var(--font-mono);
		font-size: 0.85em;
	}
</style>
