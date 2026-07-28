<script lang="ts">
	/**
	 * Renders Markdown as sanitized HTML with Phoenix design tokens.
	 * For trusted static guides (not arbitrary user content from the network).
	 */
	import { renderMarkdownToSafeHtml } from '$lib/utils/markdown.js';

	interface Props {
		/** Raw Markdown source. */
		source: string;
		/** Extra classes on the root wrapper. */
		class?: string;
	}

	let { source, class: className = '' }: Props = $props();

	const html = $derived(renderMarkdownToSafeHtml(source));
</script>

{#if html}
	<div class="markdown-preview {className}">
		{@html html}
	</div>
{/if}

<style>
	/* Scoped via wrapper class so static guide markup inherits design tokens. */
	.markdown-preview {
		color: var(--color-foreground, inherit);
		font-size: 0.875rem;
		line-height: 1.6;
		word-wrap: break-word;
	}

	.markdown-preview :global(:first-child) {
		margin-top: 0;
	}

	.markdown-preview :global(:last-child) {
		margin-bottom: 0;
	}

	.markdown-preview :global(h1),
	.markdown-preview :global(h2),
	.markdown-preview :global(h3),
	.markdown-preview :global(h4) {
		font-weight: 600;
		letter-spacing: -0.01em;
		color: var(--color-foreground, inherit);
		line-height: 1.3;
	}

	.markdown-preview :global(h1) {
		font-size: 1.25rem;
		margin: 0 0 0.75rem;
		padding-bottom: 0.5rem;
		border-bottom: 1px solid color-mix(in oklab, var(--color-border, #333) 80%, transparent);
	}

	.markdown-preview :global(h2) {
		font-size: 1.05rem;
		margin: 1.5rem 0 0.5rem;
	}

	.markdown-preview :global(h3) {
		font-size: 0.95rem;
		margin: 1.25rem 0 0.4rem;
	}

	.markdown-preview :global(p) {
		margin: 0.65rem 0;
		color: color-mix(in oklab, var(--color-foreground, #eee) 88%, transparent);
	}

	.markdown-preview :global(a) {
		color: var(--color-primary, #6ea8fe);
		text-decoration: underline;
		text-underline-offset: 2px;
	}

	.markdown-preview :global(a:hover) {
		opacity: 0.9;
	}

	.markdown-preview :global(ul),
	.markdown-preview :global(ol) {
		margin: 0.5rem 0;
		padding-left: 1.35rem;
	}

	.markdown-preview :global(li) {
		margin: 0.25rem 0;
	}

	.markdown-preview :global(li > p) {
		margin: 0.2rem 0;
	}

	.markdown-preview :global(hr) {
		border: 0;
		border-top: 1px solid color-mix(in oklab, var(--color-border, #333) 80%, transparent);
		margin: 1.25rem 0;
	}

	.markdown-preview :global(blockquote) {
		margin: 0.75rem 0;
		padding: 0.4rem 0.75rem;
		border-left: 3px solid color-mix(in oklab, var(--color-primary, #6ea8fe) 55%, transparent);
		background: color-mix(in oklab, var(--color-muted, #222) 45%, transparent);
		color: color-mix(in oklab, var(--color-foreground, #eee) 80%, transparent);
		border-radius: 0 0.4rem 0.4rem 0;
	}

	.markdown-preview :global(blockquote p) {
		margin: 0.35rem 0;
	}

	.markdown-preview :global(code) {
		font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
		font-size: 0.8em;
		padding: 0.1em 0.35em;
		border-radius: 0.3rem;
		background: color-mix(in oklab, var(--color-muted, #222) 70%, transparent);
		border: 1px solid color-mix(in oklab, var(--color-border, #333) 60%, transparent);
	}

	.markdown-preview :global(pre) {
		margin: 0.75rem 0;
		padding: 0.75rem 0.9rem;
		overflow-x: auto;
		border-radius: 0.5rem;
		border: 1px solid color-mix(in oklab, var(--color-border, #333) 70%, transparent);
		background: color-mix(in oklab, var(--color-muted, #1a1a1a) 85%, transparent);
		font-size: 0.75rem;
		line-height: 1.5;
	}

	.markdown-preview :global(pre code) {
		padding: 0;
		border: 0;
		background: transparent;
		font-size: inherit;
		border-radius: 0;
	}

	.markdown-preview :global(table) {
		width: 100%;
		border-collapse: collapse;
		margin: 0.75rem 0;
		font-size: 0.8rem;
		display: block;
		overflow-x: auto;
	}

	.markdown-preview :global(thead) {
		background: color-mix(in oklab, var(--color-muted, #222) 55%, transparent);
	}

	.markdown-preview :global(th),
	.markdown-preview :global(td) {
		border: 1px solid color-mix(in oklab, var(--color-border, #333) 75%, transparent);
		padding: 0.4rem 0.6rem;
		text-align: left;
		vertical-align: top;
	}

	.markdown-preview :global(th) {
		font-weight: 600;
		white-space: nowrap;
	}

	.markdown-preview :global(tr:nth-child(even) td) {
		background: color-mix(in oklab, var(--color-muted, #222) 25%, transparent);
	}

	.markdown-preview :global(input[type='checkbox']) {
		margin-right: 0.4rem;
		accent-color: var(--color-primary, #6ea8fe);
		pointer-events: none;
	}

	.markdown-preview :global(strong) {
		font-weight: 600;
		color: var(--color-foreground, inherit);
	}
</style>
