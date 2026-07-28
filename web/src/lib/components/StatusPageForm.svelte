<script lang="ts">
	import { statusPagesApi, type CreateStatusPageInput, type StatusPage } from '$lib/api/statuspages';
	import { toast } from 'svelte-sonner';
	import { X } from '@lucide/svelte';
	import Select from '$lib/components/Select.svelte';
	import { modalFocus } from '$lib/actions/modalFocus';
	import { untrack } from 'svelte';
	import * as m from '$lib/paraglide/messages.js';

	interface Props {
		statusPage?: StatusPage;
		onSaved?: (statusPage: StatusPage) => void | Promise<void>;
		onClose?: () => void;
	}

	let { statusPage, onSaved, onClose }: Props = $props();
	const initialStatusPage = untrack(() => statusPage);

	let open = $state(true);
	let loading = $state(false);
	let removeAccess = $state(false);
	let slaTargetEnabled = $state(initialStatusPage?.sla_target != null);
	let formData = $state({
		title: initialStatusPage?.title || '',
		slug: initialStatusPage?.slug || '',
		description: initialStatusPage?.description || '',
		icon: initialStatusPage?.icon || '',
		favicon: initialStatusPage?.favicon || '',
		theme: initialStatusPage?.theme || 'auto',
		published: initialStatusPage?.published ?? true,
		// Write-only: never prefilled from an existing page (the API never
		// returns it). Blank means "leave unchanged" on update / "no password"
		// on create.
		password: '',
		footer_text: initialStatusPage?.footer_text || '',
		custom_css: initialStatusPage?.custom_css || '',
		dashboard_style: initialStatusPage?.dashboard_style ?? 'full',
		show_tags: initialStatusPage?.show_tags ?? false,
		auto_resolve_incidents: initialStatusPage?.auto_resolve_incidents ?? false,
		// F3.5: default branded (true). Explicit false hides Phoenix chrome on public pages.
		show_powered_by: initialStatusPage?.show_powered_by ?? true,
		sla_target: initialStatusPage?.sla_target?.toString() ?? '99.9',
	});

	const brandAssetMaxBytes = 256 * 1024;

	async function readImageAsDataURL(file: File): Promise<string> {
		if (!file.type.startsWith('image/')) {
			throw new Error('Choose an image file (PNG, JPEG, SVG, or WebP).');
		}
		if (file.size > brandAssetMaxBytes) {
			throw new Error('Image must be at most 256 KiB.');
		}
		return await new Promise((resolve, reject) => {
			const reader = new FileReader();
			reader.onload = () => {
				const result = reader.result;
				if (typeof result === 'string') resolve(result);
				else reject(new Error('Could not read image'));
			};
			reader.onerror = () => reject(new Error('Could not read image'));
			reader.readAsDataURL(file);
		});
	}

	async function onLogoFile(event: Event) {
		const input = event.currentTarget as HTMLInputElement;
		const file = input.files?.[0];
		input.value = '';
		if (!file) return;
		try {
			formData.icon = await readImageAsDataURL(file);
			toast.success('Logo ready — save the page to apply');
		} catch (e: any) {
			toast.error(e?.message || 'Logo upload failed');
		}
	}

	async function onFaviconFile(event: Event) {
		const input = event.currentTarget as HTMLInputElement;
		const file = input.files?.[0];
		input.value = '';
		if (!file) return;
		try {
			formData.favicon = await readImageAsDataURL(file);
			toast.success('Favicon ready — save the page to apply');
		} catch (e: any) {
			toast.error(e?.message || 'Favicon upload failed');
		}
	}

	async function handleSubmit() {
		if (!formData.title.trim() || !formData.slug.trim()) {
			toast.error(m.status_page_form_title_slug_required());
			return;
		}
		if (!/^[a-z0-9-]+$/.test(formData.slug)) {
			toast.error(m.status_page_form_slug_invalid());
			return;
		}
		let slaTarget = 99.9;
		if (slaTargetEnabled) {
			slaTarget = Number(formData.sla_target);
			if (!Number.isFinite(slaTarget) || slaTarget <= 0 || slaTarget > 100) {
				toast.error(m.status_page_form_sla_target_invalid());
				return;
			}
		}
		if (formData.password) {
			if (Array.from(formData.password).length < 8) {
				toast.error(m.status_page_form_password_too_short());
				return;
			}
			if (new TextEncoder().encode(formData.password).length > 72) {
				toast.error(m.status_page_form_password_too_long());
				return;
			}
		}

		loading = true;
		try {
			const input: CreateStatusPageInput = {
				title: formData.title.trim(),
				slug: formData.slug.trim(),
				description: formData.description.trim() || undefined,
				// Always send icon/favicon so clear-to-empty works on update.
				icon: formData.icon.trim(),
				favicon: formData.favicon.trim(),
				theme: formData.theme as any,
				published: formData.published,
				footer_text: formData.footer_text.trim() || undefined,
				custom_css: formData.custom_css.trim() || undefined,
				dashboard_style: formData.dashboard_style,
				show_tags: formData.show_tags,
				auto_resolve_incidents: formData.auto_resolve_incidents,
				show_powered_by: formData.show_powered_by,
				...(slaTargetEnabled
					? { sla_target: slaTarget }
					: statusPage
						? { sla_target: 0 }
						: {}),
			};
			// The write-only field is omitted to preserve protection, populated to
			// replace it, and sent empty only when removal is explicitly selected.
			if (statusPage?.has_access && removeAccess) {
				input.access_code = '';
			} else if (formData.password) {
				input.access_code = formData.password;
			}

			let saved: StatusPage;
			if (statusPage) {
				saved = await statusPagesApi.update(statusPage.id, input);
				toast.success(m.status_page_form_updated_toast());
			} else {
				saved = await statusPagesApi.create(input);
				toast.success(m.status_page_form_created_toast());
			}
			await onSaved?.(saved);
			close();
		} catch (err: any) {
			toast.error(err?.message || m.monitor_form_save_failed());
		} finally {
			loading = false;
		}
	}

	function close() {
		open = false;
		onClose?.();
	}

	// Shared, token-consistent class strings.
	const inputClass =
		'w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring';
	const primaryBtn =
		'inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60';
	const ghostBtn =
		'inline-flex items-center gap-2 rounded-lg border border-border px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground';
</script>

{#if open}
	<div
		class="fixed inset-0 z-50 flex items-end justify-center bg-black/60 p-0 backdrop-blur-sm sm:items-center sm:p-4"
	>
		<button
			type="button"
			tabindex="-1"
			class="absolute inset-0 cursor-default"
			onclick={close}
			aria-label={m.btn_close()}
		></button>
		<div
			use:modalFocus={{ onClose: close, initialFocus: '#sp-title' }}
			class="relative z-10 max-h-[90dvh] w-full max-w-2xl overflow-y-auto rounded-t-xl border border-border bg-card p-4 shadow-xl sm:rounded-xl sm:p-6"
			role="dialog"
			aria-modal="true"
			aria-labelledby="status-page-form-title"
			tabindex="-1"
		>
			<div class="flex items-center justify-between border-b border-border pb-4">
				<h2 id="status-page-form-title" class="text-lg font-semibold tracking-tight">{statusPage ? m.status_page_form_edit_title() : m.status_pages_create()}</h2>
				<button type="button" onclick={close} class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground" aria-label={m.btn_close()}>
					<X class="h-5 w-5" />
				</button>
			</div>

			<div class="mt-6 grid grid-cols-1 gap-4 md:grid-cols-2">
				<div class="md:col-span-2">
					<label for="sp-title" class="text-sm font-medium">{m.status_page_form_title_label()}</label>
					<input
						id="sp-title"
						type="text"
						bind:value={formData.title}
						class="{inputClass} mt-1"
						placeholder={m.status_page_form_title_placeholder()}
					/>
				</div>

				<div>
					<label for="sp-slug" class="text-sm font-medium">{m.status_page_form_slug_label()}</label>
					<input
						id="sp-slug"
						type="text"
						bind:value={formData.slug}
						class="{inputClass} mt-1 font-mono"
						placeholder="mycompany"
						disabled={!!statusPage}
					/>
					<p class="mt-1 text-xs text-muted-foreground">{m.status_page_form_slug_help({ slug: formData.slug || 'slug' })}</p>
				</div>

				<div>
					<label for="sp-theme" class="text-sm font-medium">{m.status_page_form_theme_label()}</label>
					<div class="mt-1">
						<Select
							id="sp-theme"
							options={[
								{ value: 'auto', label: m.status_page_form_theme_auto() },
								{ value: 'light', label: m.status_page_form_theme_light() },
								{ value: 'dark', label: m.status_page_form_theme_dark() },
							]}
							value={formData.theme}
							onValueChange={(v) => { formData.theme = v as typeof formData.theme; }}
							class="w-full"
						/>
					</div>
				</div>

				<div class="md:col-span-2">
					<label for="sp-desc" class="text-sm font-medium">{m.monitor_form_description_label()}</label>
					<textarea
						id="sp-desc"
						bind:value={formData.description}
						class="{inputClass} mt-1"
						rows="2"
						placeholder={m.status_page_form_description_placeholder()}
					></textarea>
				</div>

				<div class="md:col-span-2">
					<label for="sp-icon" class="text-sm font-medium">{m.status_page_form_icon_label()}</label>
					<input
						id="sp-icon"
						type="text"
						bind:value={formData.icon}
						class="{inputClass} mt-1"
						placeholder="https://example.com/logo.png"
					/>
					<div class="mt-2 flex flex-wrap items-center gap-3">
						<label class="inline-flex cursor-pointer items-center gap-2 text-xs font-medium text-primary hover:underline">
							<input type="file" accept="image/*" class="sr-only" onchange={onLogoFile} />
							Upload logo
						</label>
						{#if formData.icon}
							<button
								type="button"
								class="text-xs text-muted-foreground hover:text-danger"
								onclick={() => (formData.icon = '')}
							>
								Clear logo
							</button>
							<img src={formData.icon} alt="" class="h-8 w-8 rounded border border-border object-contain" />
						{/if}
					</div>
					<p class="mt-1 text-xs text-muted-foreground">{m.status_page_form_icon_help()}</p>
				</div>

				<div class="md:col-span-2">
					<label for="sp-favicon" class="text-sm font-medium">Favicon URL</label>
					<input
						id="sp-favicon"
						type="text"
						bind:value={formData.favicon}
						class="{inputClass} mt-1"
						placeholder="https://example.com/favicon.png"
					/>
					<div class="mt-2 flex flex-wrap items-center gap-3">
						<label class="inline-flex cursor-pointer items-center gap-2 text-xs font-medium text-primary hover:underline">
							<input type="file" accept="image/*" class="sr-only" onchange={onFaviconFile} />
							Upload favicon
						</label>
						{#if formData.favicon}
							<button
								type="button"
								class="text-xs text-muted-foreground hover:text-danger"
								onclick={() => (formData.favicon = '')}
							>
								Clear favicon
							</button>
							<img src={formData.favicon} alt="" class="h-6 w-6 rounded border border-border object-contain" />
						{/if}
					</div>
					<p class="mt-1 text-xs text-muted-foreground">
						Browser tab icon. HTTPS URL or upload (max 256 KiB). Leave empty for the default.
					</p>
				</div>

				<div class="md:col-span-2">
					<label for="sp-footer" class="text-sm font-medium">{m.status_page_form_footer_label()}</label>
					<input
						id="sp-footer"
						type="text"
						bind:value={formData.footer_text}
						class="{inputClass} mt-1"
						placeholder={m.status_page_form_footer_placeholder()}
					/>
				</div>

				<div class="md:col-span-2">
					<label for="sp-dashboard-style" class="text-sm font-medium">{m.status_page_form_dashboard_style_label()}</label>
					<div class="mt-1">
						<Select
							id="sp-dashboard-style"
							options={[
								{ value: 'full', label: m.status_page_form_style_full() },
								{ value: 'grid', label: m.status_page_form_style_grid() },
								{ value: 'pills', label: m.status_page_form_style_pills() },
							]}
							value={formData.dashboard_style}
							onValueChange={(v) => { formData.dashboard_style = v as typeof formData.dashboard_style; }}
							class="w-full"
						/>
					</div>
					<p class="mt-1 text-xs text-muted-foreground">{m.status_page_form_dashboard_style_help()}</p>
				</div>

				<div class="md:col-span-2">
					<label for="sp-password" class="text-sm font-medium">{m.status_page_form_password_label()}</label>
					<input
						id="sp-password"
						type="password"
						bind:value={formData.password}
						class="{inputClass} mt-1"
						placeholder="••••••••"
						autocomplete="new-password"
						disabled={removeAccess}
					/>
					<p class="mt-1 text-xs text-muted-foreground">
						{m.status_page_form_password_help()}
						{statusPage?.has_access ? m.status_page_form_password_help_keep() : m.status_page_form_password_help_none()}
					</p>
					{#if statusPage?.has_access}
						<label class="mt-3 flex items-center gap-2 text-sm text-muted-foreground">
							<input
								type="checkbox"
								checked={removeAccess}
								onchange={(event) => {
									removeAccess = event.currentTarget.checked;
									if (removeAccess) formData.password = '';
								}}
								class="h-4 w-4 rounded border-border accent-primary"
							/>
							{m.status_page_form_remove_password()}
						</label>
					{/if}
				</div>

				<div class="md:col-span-2">
					<label for="sp-css" class="text-sm font-medium">{m.status_page_form_custom_css_label()}</label>
					<textarea
						id="sp-css"
						bind:value={formData.custom_css}
						class="{inputClass} mt-1 font-mono text-xs"
						rows="4"
						placeholder={'.status-page { --color-primary: #6366f1; }'}
					></textarea>
				</div>

				<div class="flex flex-wrap items-center gap-4 pt-2 md:col-span-2">
					<div class="flex items-center gap-2">
						<input type="checkbox" bind:checked={formData.published} id="published" class="h-4 w-4 rounded border-border accent-primary" />
						<label for="published" class="text-sm text-muted-foreground">{m.status_page_form_published_label()}</label>
					</div>
					<div class="flex items-center gap-2">
						<input type="checkbox" bind:checked={formData.show_tags} id="show-tags" class="h-4 w-4 rounded border-border accent-primary" />
						<label for="show-tags" class="text-sm text-muted-foreground">{m.status_page_form_show_tags_label()}</label>
					</div>
					<div class="flex items-center gap-2">
						<input type="checkbox" bind:checked={formData.auto_resolve_incidents} id="auto-resolve" class="h-4 w-4 rounded border-border accent-primary" />
						<label for="auto-resolve" class="text-sm text-muted-foreground">{m.status_page_form_auto_resolve_label()}</label>
					</div>
					<div class="flex items-center gap-2">
						<input type="checkbox" bind:checked={formData.show_powered_by} id="show-powered-by" class="h-4 w-4 rounded border-border accent-primary" />
						<label for="show-powered-by" class="text-sm text-muted-foreground">Show “Powered by Phoenix”</label>
					</div>
				</div>

				<div class="rounded-lg border border-border bg-surface/60 p-4 md:col-span-2">
					<label class="flex items-start gap-3" for="show-sla-target">
						<input
							id="show-sla-target"
							type="checkbox"
							bind:checked={slaTargetEnabled}
							class="mt-0.5 h-4 w-4 rounded border-border accent-primary"
						/>
						<span>
							<span class="block text-sm font-medium">{m.status_page_form_sla_target_label()}</span>
							<span class="mt-0.5 block text-xs text-muted-foreground">{m.status_page_form_sla_target_help()}</span>
						</span>
					</label>
					{#if slaTargetEnabled}
						<div class="mt-3 max-w-xs">
							<label for="sla-target" class="text-xs font-medium text-muted-foreground">{m.status_page_form_sla_target_value_label()}</label>
							<div class="relative mt-1">
								<input
									id="sla-target"
									type="number"
									min="0.001"
									max="100"
									step="0.001"
									bind:value={formData.sla_target}
									class="{inputClass} pr-8 font-mono"
								/>
								<span class="pointer-events-none absolute inset-y-0 right-3 flex items-center text-sm text-muted-foreground">%</span>
							</div>
						</div>
					{/if}
				</div>
			</div>

			<div class="mt-8 flex justify-end gap-3 border-t border-border pt-4">
				<button type="button" onclick={close} class={ghostBtn}>{m.btn_cancel()}</button>
				<button
					type="button"
					onclick={handleSubmit}
					disabled={loading}
					class={primaryBtn}
				>
					{loading ? m.btn_saving() : statusPage ? m.btn_update() : m.btn_create()}
				</button>
			</div>
		</div>
	</div>
{/if}
