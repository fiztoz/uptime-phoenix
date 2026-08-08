<script lang="ts">
	import { backupApi, type BackupDocument, type ImportSummary } from '$lib/api/backup';
	import { Download, Upload, AlertTriangle, CheckCircle2, FileJson } from '@lucide/svelte';
	import { toast } from 'svelte-sonner';
	import * as m from '$lib/paraglide/messages.js';

	let exporting = $state(false);
	let importing = $state(false);
	let pasteJSON = $state('');
	let confirmImport = $state(false);
	let pendingDoc = $state<BackupDocument | Record<string, unknown> | null>(null);
	let summary = $state<ImportSummary | null>(null);
	let parseError = $state('');

	async function downloadBackup() {
		exporting = true;
		summary = null;
		try {
			const doc = await backupApi.export();
			const blob = new Blob([JSON.stringify(doc, null, 2)], { type: 'application/json' });
			const url = URL.createObjectURL(blob);
			const a = document.createElement('a');
			const stamp = new Date().toISOString().slice(0, 19).replace(/[:T]/g, '-');
			a.href = url;
			a.download = `phoenix-backup-${stamp}.json`;
			a.click();
			URL.revokeObjectURL(url);
			toast.success(m.backup_page_downloaded_toast());
		} catch (e: unknown) {
			const msg =
				e && typeof e === 'object' && 'message' in e
					? String((e as { message: string }).message)
					: m.backup_page_export_failed();
			toast.error(msg);
		} finally {
			exporting = false;
		}
	}

	function parseDoc(raw: string): BackupDocument | Record<string, unknown> {
		const parsed = JSON.parse(raw) as unknown;
		if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
			throw new Error(m.backup_page_must_be_object());
		}
		return parsed as BackupDocument;
	}

	function prepareImportFromText() {
		parseError = '';
		summary = null;
		try {
			pendingDoc = parseDoc(pasteJSON);
			confirmImport = true;
		} catch (e: unknown) {
			parseError = e instanceof Error ? e.message : m.backup_page_invalid_json();
			pendingDoc = null;
			confirmImport = false;
		}
	}

	async function onFileSelected(ev: Event) {
		parseError = '';
		summary = null;
		const input = ev.target as HTMLInputElement;
		const file = input.files?.[0];
		if (!file) return;
		try {
			const text = await file.text();
			pasteJSON = text;
			pendingDoc = parseDoc(text);
			confirmImport = true;
		} catch (e: unknown) {
			parseError = e instanceof Error ? e.message : m.backup_page_read_file_failed();
			pendingDoc = null;
			confirmImport = false;
		} finally {
			input.value = '';
		}
	}

	async function runImport() {
		if (!pendingDoc) return;
		importing = true;
		try {
			summary = await backupApi.import(pendingDoc);
			confirmImport = false;
			pendingDoc = null;
			const skips = summary.skipped?.length ?? 0;
			if (skips > 0) {
				toast.success(m.backup_page_import_finished_skipped({ count: skips }));
			} else {
				toast.success(m.backup_page_import_success());
			}
		} catch (e: unknown) {
			const msg =
				e && typeof e === 'object' && 'message' in e
					? String((e as { message: string }).message)
					: m.backup_page_import_failed();
			toast.error(msg);
		} finally {
			importing = false;
		}
	}

	function cancelImport() {
		confirmImport = false;
		pendingDoc = null;
	}

	const summaryRows = $derived(
		summary
			? [
					{ label: m.backup_page_row_proxies(), value: summary.proxies_created },
					{ label: m.backup_page_row_notification_templates(), value: summary.notification_templates_created },
					{ label: m.nav_notifications(), value: summary.notifications_created },
					{ label: m.backup_page_row_tags_created(), value: summary.tags_created },
					{ label: m.backup_page_row_tags_reused(), value: summary.tags_reused },
					{ label: m.nav_monitors(), value: summary.monitors_created },
					{ label: m.backup_page_row_monitor_tags(), value: summary.monitor_tags_created },
					{ label: m.backup_page_row_monitor_notifications(), value: summary.monitor_notifications_created },
					{ label: m.nav_status_pages(), value: summary.status_pages_created },
					{ label: m.backup_page_row_status_page_monitors(), value: summary.status_page_monitors_created },
					{ label: m.backup_page_row_cnames(), value: summary.status_page_cnames_created },
					{ label: m.nav_incidents(), value: summary.incidents_created },
					{ label: m.backup_page_row_maintenance_windows(), value: summary.maintenance_windows_created },
					{ label: m.backup_page_row_maintenance_monitors(), value: summary.maintenance_monitors_created }
				]
			: []
	);

	const primaryBtn =
		'inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60';
	const secondaryBtn =
		'inline-flex items-center gap-2 rounded-lg border border-border bg-surface px-4 py-2 text-sm font-medium text-foreground transition-colors hover:bg-accent focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60';
</script>

<svelte:head>
	<title>{m.app_name()} · {m.nav_backup()}</title>
</svelte:head>

<div class="space-y-6">
	<div>
		<h1 class="text-2xl font-semibold tracking-tight">{m.nav_backup()}</h1>
		<p class="mt-1 text-sm text-muted-foreground">
			{m.backup_page_subtitle()}
		</p>
	</div>

	<!-- Secrets warning -->
	<div
		class="flex gap-3 rounded-xl border border-warning/30 bg-warning/10 px-4 py-3 text-sm text-foreground"
		role="alert"
	>
		<AlertTriangle class="mt-0.5 h-5 w-5 shrink-0 text-warning" />
		<div>
			<p class="font-medium">{m.backup_page_secrets_warning_title()}</p>
			<p class="mt-1 text-muted-foreground">
				{m.backup_page_secrets_warning_body()}
			</p>
		</div>
	</div>

	<!-- Export -->
	<section class="rounded-xl border border-border bg-surface/60 p-5 shadow-sm">
		<div class="flex items-start gap-3">
			<div class="grid h-10 w-10 place-items-center rounded-lg border border-border bg-elevated">
				<Download class="h-5 w-5 text-primary" />
			</div>
			<div class="min-w-0 flex-1">
				<h2 class="font-semibold">{m.backup_page_export_heading()}</h2>
				<p class="mt-1 text-sm text-muted-foreground">
					{m.backup_page_export_description()}
				</p>
				<button
					type="button"
					class="{primaryBtn} mt-4"
					disabled={exporting}
					onclick={downloadBackup}
				>
					<Download class="h-4 w-4" />
					{exporting ? m.backup_page_exporting() : m.backup_page_download_backup()}
				</button>
			</div>
		</div>
	</section>

	<!-- Import -->
	<section class="rounded-xl border border-border bg-surface/60 p-5 shadow-sm">
		<div class="flex items-start gap-3">
			<div class="grid h-10 w-10 place-items-center rounded-lg border border-border bg-elevated">
				<Upload class="h-5 w-5 text-primary" />
			</div>
			<div class="min-w-0 flex-1 space-y-4">
				<div>
					<h2 class="font-semibold">{m.backup_page_import_heading()}</h2>
					<p class="mt-1 text-sm text-muted-foreground">
						{m.backup_page_import_description()}
					</p>
				</div>

				<label class="flex cursor-pointer flex-col items-center justify-center gap-2 rounded-lg border border-dashed border-border bg-background/40 px-4 py-6 text-sm text-muted-foreground transition-colors hover:border-primary/40 hover:bg-primary/5">
					<FileJson class="h-6 w-6" />
					<span>{m.backup_page_choose_file_pre()} <code class="text-xs">.json</code> {m.backup_page_choose_file_post()}</span>
					<input type="file" accept="application/json,.json" class="hidden" onchange={onFileSelected} />
				</label>

				<div>
					<label for="backup-json" class="mb-1.5 block text-xs font-medium text-muted-foreground">
						{m.backup_page_or_paste_json()}
					</label>
					<textarea
						id="backup-json"
						class="min-h-[140px] w-full rounded-lg border border-border bg-background px-3 py-2 font-mono text-xs text-foreground placeholder:text-muted-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
						placeholder={'{\n  "version": 1,\n  ...\n}'}
						bind:value={pasteJSON}
					></textarea>
					{#if parseError}
						<p class="mt-1.5 text-sm text-destructive">{parseError}</p>
					{/if}
					<button
						type="button"
						class="{secondaryBtn} mt-3"
						disabled={!pasteJSON.trim() || importing}
						onclick={prepareImportFromText}
					>
						<Upload class="h-4 w-4" />
						{m.backup_page_review_import()}
					</button>
				</div>
			</div>
		</div>
	</section>

	<!-- Confirm step -->
	{#if confirmImport && pendingDoc}
		<section class="rounded-xl border border-primary/30 bg-primary/5 p-5 shadow-sm">
			<h2 class="font-semibold">{m.backup_page_confirm_import_heading()}</h2>
			<p class="mt-1 text-sm text-muted-foreground">
				{m.backup_page_confirm_import_body()}
			</p>
			<div class="mt-4 flex flex-wrap gap-2">
				<button type="button" class={primaryBtn} disabled={importing} onclick={runImport}>
					{importing ? m.backup_page_importing() : m.backup_page_confirm_import_button()}
				</button>
				<button type="button" class={secondaryBtn} disabled={importing} onclick={cancelImport}>
					{m.btn_cancel()}
				</button>
			</div>
		</section>
	{/if}

	<!-- Result summary -->
	{#if summary}
		<section class="rounded-xl border border-border bg-surface/60 p-5 shadow-sm">
			<div class="mb-3 flex items-center gap-2">
				<CheckCircle2 class="h-5 w-5 text-success" />
				<h2 class="font-semibold">{m.backup_page_import_result_heading()}</h2>
			</div>
			<div class="grid grid-cols-2 gap-2 sm:grid-cols-3">
				{#each summaryRows as row}
					<div class="rounded-lg border border-border bg-background/50 px-3 py-2">
						<div class="text-xs text-muted-foreground">{row.label}</div>
						<div class="text-lg font-semibold tabular-nums">{row.value}</div>
					</div>
				{/each}
			</div>
			{#if summary.skipped?.length}
				<div class="mt-4">
					<h3 class="text-sm font-medium">{m.backup_page_skipped_heading({ count: summary.skipped.length })}</h3>
					<ul class="mt-2 max-h-48 space-y-1 overflow-y-auto text-xs text-muted-foreground">
						{#each summary.skipped as s}
							<li class="rounded border border-border/60 px-2 py-1">
								<span class="font-medium text-foreground">{s.kind}</span>
								{#if s.name}
									· {s.name}
								{/if}
								— {s.reason}
							</li>
						{/each}
					</ul>
				</div>
			{/if}
		</section>
	{/if}
</div>
