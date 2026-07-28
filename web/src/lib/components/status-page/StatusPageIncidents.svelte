<script lang="ts">
	import {
		AlertOctagon,
		AlertTriangle,
		CheckCircle,
		Info,
	} from '@lucide/svelte';
	import type { Incident } from '$lib/api/statuspages';
	import MarkdownPreview from '$lib/components/MarkdownPreview.svelte';
	import { severityOf, type IncidentStyle } from '$lib/status-page-health';

	interface Props {
		incidents: Incident[];
	}

	const ICONS: Record<IncidentStyle, typeof CheckCircle> = {
		danger: AlertOctagon,
		warning: AlertTriangle,
		info: Info,
		success: CheckCircle,
	};

	let { incidents }: Props = $props();
	const activeIncidents = $derived(
		incidents.filter((incident) => incident.active),
	);
</script>

{#if activeIncidents.length > 0}
	<section class="mt-8 space-y-3" aria-labelledby="incidents-heading">
		<div class="flex items-center justify-between gap-3">
			<h2
				id="incidents-heading"
				class="flex items-center gap-2 text-base font-semibold"
			>
				<AlertTriangle class="h-4 w-4 text-warning" /> Active incidents
			</h2>
			<span class="text-xs text-muted-foreground">
				{activeIncidents.length}
				{activeIncidents.length === 1 ? 'ongoing issue' : 'ongoing issues'}
			</span>
		</div>
		{#each activeIncidents as incident (incident.id)}
			{@const severity = severityOf(incident.style)}
			{@const Icon = ICONS[severity.style]}
			{@const latestUpdate = incident.updates?.at(-1)}
			<div
				class="rounded-lg border border-border border-l-4 p-4 {severity.card}"
			>
				<div class="flex items-start gap-3">
					<span
						class="mt-0.5 grid h-8 w-8 shrink-0 place-items-center rounded-lg {severity.chip}"
					>
						<Icon class="h-4 w-4" />
					</span>
					<div class="min-w-0 flex-1">
						<div class="flex flex-wrap items-center gap-2">
							<span class="font-medium">{incident.title}</span>
							<span
								class="inline-flex items-center rounded-full border px-2 py-0.5 text-xs font-medium capitalize {severity.badge}"
							>
								{severity.label}
							</span>
							<span
								class="inline-flex items-center rounded-full border border-warning/25 bg-warning/10 px-2 py-0.5 text-xs font-medium text-warning"
							>
								Active
							</span>
						</div>
						{#if latestUpdate?.content}
							<MarkdownPreview
								source={latestUpdate.content}
								class="mt-1 text-sm text-muted-foreground"
							/>
						{:else if incident.content}
							<MarkdownPreview
								source={incident.content}
								class="mt-1 text-sm text-muted-foreground"
							/>
						{/if}
						{#if latestUpdate}
							<p class="mt-2 text-xs text-muted-foreground">
								<span class="font-medium capitalize text-foreground"
									>{latestUpdate.status}</span
								>
								<span class="mx-1">·</span>
								{new Date(latestUpdate.created_at).toLocaleString()}
							</p>
						{/if}
						{#if incident.updates?.length > 1}
							<details class="mt-3 border-t border-border/70 pt-3">
								<summary
									class="cursor-pointer text-xs font-medium text-muted-foreground transition-colors hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
								>
									View timeline ({incident.updates.length} updates)
								</summary>
								<ol class="mt-3 space-y-3">
									{#each incident.updates as update (update.id)}
										<li class="grid grid-cols-[auto_1fr] gap-3">
											<span
												class="mt-1.5 h-2 w-2 rounded-full bg-current opacity-70"
											></span>
											<div class="min-w-0">
												<div
													class="flex flex-wrap items-center gap-2 text-xs text-muted-foreground"
												>
													<span class="font-medium capitalize text-foreground">
														{update.status}
													</span>
													<span
														>{new Date(
															update.created_at,
														).toLocaleString()}</span
													>
												</div>
												<MarkdownPreview
													source={update.content}
													class="mt-1 text-sm text-muted-foreground"
												/>
											</div>
										</li>
									{/each}
								</ol>
							</details>
						{/if}
					</div>
				</div>
			</div>
		{/each}
	</section>
{/if}
