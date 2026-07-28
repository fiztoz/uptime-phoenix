<script lang="ts">
	import { monitorsApi, type CreateMonitorInput, type MonitorWithGroup } from '$lib/api/monitors';
	import { monitorGroupsApi, buildGroupOptions, type MonitorGroupView } from '$lib/api/monitorGroups';
	import { proxiesApi, type Proxy } from '$lib/api/proxies';
	import { escalationApi, type EscalationPolicy } from '$lib/api/escalation';
	import {
		buildMonitorTypeSelectGroups,
		monitorTypeConfig,
		normalizeHttpUrl,
		parseHeadersInput,
		stringifyHeaders,
		type MonitorTypeGroupId,
	} from '$lib/monitor-types';
	import { realtime, type Monitor } from '$lib/stores/ws.svelte.js';
	import { toast } from 'svelte-sonner';
	import { AlertTriangle, BookOpen, Download, X } from '@lucide/svelte';
	import Select from '$lib/components/Select.svelte';
	import DockerSetupGuide from '$lib/components/DockerSetupGuide.svelte';
	import MqttSetupGuide from '$lib/components/MqttSetupGuide.svelte';
	import DatabaseSetupGuide from '$lib/components/DatabaseSetupGuide.svelte';
	import RabbitmqSetupGuide from '$lib/components/RabbitmqSetupGuide.svelte';
	import { modalFocus } from '$lib/actions/modalFocus';
	import { untrack } from 'svelte';
	import * as m from '$lib/paraglide/messages.js';

	/** Static guide paths (web/static/docs/*.md). */
	const DOCKER_GUIDE_PATH = '/docs/docker-remote-setup.md';
	const DOCKER_GUIDE_FILENAME = 'phoenix-docker-remote-setup.md';
	const MQTT_GUIDE_PATH = '/docs/mqtt-setup.md';
	const MQTT_GUIDE_FILENAME = 'phoenix-mqtt-setup.md';
	const DATABASE_GUIDE_PATH = '/docs/database-monitor-setup.md';
	const DATABASE_GUIDE_FILENAME = 'phoenix-database-monitor-setup.md';
	const RABBITMQ_GUIDE_PATH = '/docs/rabbitmq-setup.md';
	const RABBITMQ_GUIDE_FILENAME = 'phoenix-rabbitmq-setup.md';

	interface Props {
		monitor?: Monitor;
		onSaved?: () => void;
		onClose?: () => void;
	}

	let { monitor, onSaved, onClose }: Props = $props();
	const initialMonitor = untrack(() => monitor);
	// Narrow the incoming monitor to read group_id, which isn't on the
	// shared WS Monitor type yet (see MonitorWithGroup in $lib/api/monitors).
	const editingMonitor = initialMonitor as MonitorWithGroup | undefined;

	let open = $state(true);
	let loading = $state(false);
	let selectedType = $state(initialMonitor?.type || 'http');
	let dockerGuideOpen = $state(false);
	let mqttGuideOpen = $state(false);
	let databaseGuideOpen = $state(false);
	let rabbitmqGuideOpen = $state(false);

	function downloadDockerGuide() {
		const a = document.createElement('a');
		a.href = DOCKER_GUIDE_PATH;
		a.download = DOCKER_GUIDE_FILENAME;
		a.target = '_blank';
		a.rel = 'noopener';
		a.click();
	}

	function downloadMqttGuide() {
		const a = document.createElement('a');
		a.href = MQTT_GUIDE_PATH;
		a.download = MQTT_GUIDE_FILENAME;
		a.target = '_blank';
		a.rel = 'noopener';
		a.click();
	}

	function downloadDatabaseGuide() {
		const a = document.createElement('a');
		a.href = DATABASE_GUIDE_PATH;
		a.download = DATABASE_GUIDE_FILENAME;
		a.target = '_blank';
		a.rel = 'noopener';
		a.click();
	}

	function downloadRabbitmqGuide() {
		const a = document.createElement('a');
		a.href = RABBITMQ_GUIDE_PATH;
		a.download = RABBITMQ_GUIDE_FILENAME;
		a.target = '_blank';
		a.rel = 'noopener';
		a.click();
	}

	// --- Monitor group (folder) ------------------------------------------
	let groups = $state<MonitorGroupView[]>([]);
	let groupOptions = $derived(buildGroupOptions(groups));
	let selectedGroupId = $state<string>(
		editingMonitor?.group_id != null ? String(editingMonitor.group_id) : ''
	);

	$effect(() => {
		monitorGroupsApi
			.list()
			.then((list) => {
				groups = list;
			})
			.catch(() => {
				groups = [];
			});
	});

	// --- Proxy (outbound proxy for this monitor's checks) ----------------
	let proxies = $state<Proxy[]>([]);
	let selectedProxyId = $state<string>(
		editingMonitor?.proxy_id != null ? String(editingMonitor.proxy_id) : ''
	);

	$effect(() => {
		proxiesApi
			.list()
			.then((list) => {
				proxies = list;
			})
			.catch(() => {
				proxies = [];
			});
	});

	// --- Escalation policy (F2.3) ----------------------------------------
	// This is the monitor's DIRECT assignment only. An inherited folder policy is
	// deliberately not shown here: rendering it would make saving this form
	// silently convert inheritance into a direct assignment, and the monitor
	// would then stop following its folder.
	let escalationPolicies = $state<EscalationPolicy[]>([]);
	let escalationPolicyId = $state('');
	let escalationBaselineId = $state('');

	$effect(() => {
		const monitorId = editingMonitor?.id;
		Promise.all([
			escalationApi.list(),
			monitorId ? escalationApi.getForMonitor(monitorId) : Promise.resolve(null),
		])
			.then(([list, assignment]) => {
				escalationPolicies = list;
				escalationBaselineId = assignment && assignment.policy_id ? String(assignment.policy_id) : '';
				escalationPolicyId = escalationBaselineId;
			})
			.catch(() => {
				// A non-admin (or a caller without can_manage_notifications) gets 403
				// here. The picker stays empty and the stored assignment is left
				// alone — never cleared as a side effect of an unrelated save.
				escalationPolicies = [];
			});
	});

	/**
	 * Seed the editable config from the monitor's config, minus fields that live
	 * top-level on the backend (accepted_statuscodes), and with object-valued
	 * fields (e.g. HTTP headers) rendered as editable text.
	 */
	function buildInitialConfig(): Record<string, unknown> {
		const cfg: Record<string, unknown> = { ...(initialMonitor?.config || {}) };
		delete cfg.accepted_statuscodes;
		if (cfg.headers && typeof cfg.headers === 'object') {
			cfg.headers = stringifyHeaders(cfg.headers);
		}
		// Older MQTT forms stored the broker as `url`; the checker key is `broker`.
		if (
			(initialMonitor?.type === 'mqtt' || !initialMonitor) &&
			(cfg.broker === undefined || cfg.broker === '') &&
			typeof cfg.url === 'string' &&
			cfg.url
		) {
			cfg.broker = cfg.url;
		}
		// Older database forms used `dsn`; checker + form use `connection_string`.
		if (
			(initialMonitor?.type === 'database' || !initialMonitor) &&
			(cfg.connection_string === undefined || cfg.connection_string === '') &&
			typeof cfg.dsn === 'string' &&
			cfg.dsn
		) {
			cfg.connection_string = cfg.dsn;
		}
		return cfg;
	}

	let formData = $state({
		name: initialMonitor?.name || '',
		description: initialMonitor?.description || '',
		// interval/timeout are top-level monitor fields (not inside config).
		interval: initialMonitor?.interval || 60,
		timeout: initialMonitor?.timeout || 30,
		retryInterval: initialMonitor?.retry_interval ?? 60,
		maxRetries: initialMonitor?.max_retries ?? 0,
		resendInterval: initialMonitor?.resend_interval ?? 0,
		// Manual display order (lower first). Default matches schema/API (2000).
		weight: initialMonitor?.weight ?? 2000,
		upsideDown: initialMonitor?.upside_down ?? false,
		tlsIgnore: initialMonitor?.tls_ignore ?? false,
		certExpiryNotify: initialMonitor?.cert_expiry_notify ?? false,
		// Edited as a comma-separated string; sent as a top-level string[] on save
		// (see handleSubmit) — the backend reads accepted_statuscodes as a sibling
		// of config, not a field inside it.
		acceptedStatusCodes: (initialMonitor?.accepted_statuscodes ?? []).join(', '),
		config: buildInitialConfig(),
	});

	// Ensure config has defaults for selected type
	$effect(() => {
		const cfg = monitorTypeConfig[selectedType];
		if (cfg) {
			cfg.fields.forEach((f) => {
				if (formData.config[f.key] === undefined && f.default !== undefined) {
					formData.config[f.key] = f.default;
				}
			});
		}
	});

	function updateConfigField(key: string, value: unknown) {
		formData.config = { ...formData.config, [key]: value };
	}

	function getFieldValue(key: string): unknown {
		return formData.config[key] ?? '';
	}

	/** Whether a config field should render given showWhen dependencies. */
	function isFieldVisible(field: { showWhen?: { key: string; values: string[] } }): boolean {
		if (!field.showWhen) return true;
		const current = String(getFieldValue(field.showWhen.key) ?? '');
		return field.showWhen.values.includes(current);
	}

	let visibleFields = $derived(
		(monitorTypeConfig[selectedType]?.fields || []).filter((f) => isFieldVisible(f))
	);

	async function handleSubmit() {
		if (!formData.name.trim()) {
			toast.error(m.monitor_form_name_required());
			return;
		}
		if (!selectedType) {
			toast.error(m.monitor_form_type_required());
			return;
		}

		const cfg = monitorTypeConfig[selectedType];
		for (const field of cfg?.fields || []) {
			if (!field.required || field.key === 'accepted_statuscodes') continue;
			if (!isFieldVisible(field)) continue;
			const value = formData.config[field.key];
			if (value === undefined || value === null || String(value).trim() === '') {
				toast.error(m.monitor_form_field_required({ label: field.label }));
				return;
			}
		}
		// HTTP auth sub-fields: required only when their method is selected.
		if (selectedType === 'http') {
			const method = String(formData.config.auth_method || 'none');
			const need = (key: string, label: string) => {
				const v = formData.config[key];
				if (v === undefined || v === null || String(v).trim() === '') {
					toast.error(m.monitor_form_field_required({ label }));
					return false;
				}
				return true;
			};
			if (method === 'basic') {
				if (!need('auth_username', 'Username') || !need('auth_password', 'Password')) return;
			} else if (method === 'bearer') {
				if (!need('auth_bearer_token', 'Bearer Token')) return;
			} else if (method === 'oauth2_cc') {
				if (
					!need('oauth2_token_url', 'Token URL') ||
					!need('oauth2_client_id', 'Client ID') ||
					!need('oauth2_client_secret', 'Client Secret')
				) {
					return;
				}
			}
		}

		loading = true;
		try {
			// accepted_statuscodes is a top-level backend field, not part of config.
			const config: Record<string, unknown> = { ...formData.config };
			delete config.accepted_statuscodes;
			if (selectedType === 'http') {
				config.url = normalizeHttpUrl(config.url);
				formData.config = { ...formData.config, url: config.url };
			}
			// The HTTP checker (internal/adapters/checker/http.go) reads config.headers
			// as a JSON object; the form edits it as text, so parse it back here.
			if (typeof config.headers === 'string') {
				const parsedHeaders = parseHeadersInput(config.headers);
				if (parsedHeaders) {
					config.headers = parsedHeaders;
				} else {
					delete config.headers;
				}
			}
			const acceptedStatusCodes = formData.acceptedStatusCodes
				.split(',')
				.map((s) => s.trim())
				.filter(Boolean);

			const input: CreateMonitorInput = {
				name: formData.name.trim(),
				description: formData.description.trim(),
				type: selectedType,
				interval: Number(formData.interval),
				timeout: Number(formData.timeout),
				retry_interval: Number(formData.retryInterval),
				max_retries: Number(formData.maxRetries),
				resend_interval: Number(formData.resendInterval),
				// Always send weight on update so the backend does not zero it
				// (Update always applies the field).
				weight: Number(formData.weight),
				upside_down: formData.upsideDown,
				// Only meaningful for HTTP checks; never carry a stale toggle onto
				// another type when the user switches type before saving.
				tls_ignore: selectedType === 'http' ? formData.tlsIgnore : false,
				cert_expiry_notify: selectedType === 'http' ? formData.certExpiryNotify : false,
				config,
				accepted_statuscodes: acceptedStatusCodes,
				active: true,
				group_id: selectedGroupId === '' ? null : Number(selectedGroupId),
				proxy_id: selectedProxyId === '' ? null : Number(selectedProxyId),
			};

			if (monitor) {
				const updated = await monitorsApi.update(monitor.id, input);
				// Optimistically patch WS store so interval/timeout show immediately.
			// Clear `target` (a WS-only computed field) so the detail page's
			// targetUrl derived falls through to config.url, which reflects the
			// updated endpoint. The server will re-populate target on the next
			// monitor.update WS event.
			const { target: _stale, ...patch } = {
				...monitor,
				...updated,
				interval: updated.interval ?? input.interval,
				timeout: updated.timeout ?? input.timeout,
				status: monitor.status,
			};
			realtime.patchMonitor(patch);
				await syncEscalationAssignment(monitor.id);
				toast.success(m.monitor_form_updated_toast());
			} else {
				const created = await monitorsApi.create(input);
				await syncEscalationAssignment(created.id);
				toast.success(m.monitor_form_created_toast());
			}
			onSaved?.();
			close();
		} catch (err: any) {
			toast.error(err?.message || m.monitor_form_save_failed());
		} finally {
			loading = false;
		}
	}

	/**
	 * Write the escalation assignment, but only when it actually changed. A user
	 * whose picker never loaded (403) has baseline === current === '', so an
	 * ordinary save can never clear someone else's assignment.
	 */
	async function syncEscalationAssignment(monitorId: number) {
		if (escalationPolicyId === escalationBaselineId) return;
		try {
			await escalationApi.setForMonitor(monitorId, escalationPolicyId === '' ? 0 : Number(escalationPolicyId));
			escalationBaselineId = escalationPolicyId;
		} catch (err: any) {
			// The monitor itself saved. Say exactly what did not.
			toast.error(err?.message || m.escalation_assign_failed());
		}
	}

	function close() {
		open = false;
		onClose?.();
	}

	/** Localized section headings for the type picker (General / Passive / …). */
	const typeSelectGroups = $derived(
		buildMonitorTypeSelectGroups((id: MonitorTypeGroupId) => {
			switch (id) {
				case 'general':
					return m.monitor_type_group_general();
				case 'passive':
					return m.monitor_type_group_passive();
				case 'infrastructure':
					return m.monitor_type_group_infrastructure();
				case 'protocol':
					return m.monitor_type_group_protocol();
			}
		}),
	);

	// Shared, token-consistent class strings.
	const inputClass =
		'w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring';
	const primaryBtn =
		'inline-flex items-center gap-2 rounded-lg bg-primary px-5 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60';
	const ghostBtn =
		'inline-flex items-center gap-2 rounded-lg border border-border px-4 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground';
</script>

{#if open}
	<!-- Modal overlay -->
	<div class="fixed inset-0 z-50 flex items-end justify-center bg-black/60 p-0 backdrop-blur-sm sm:items-center sm:p-4">
		<button
			type="button"
			tabindex="-1"
			class="absolute inset-0 cursor-default"
			onclick={close}
			aria-label={m.btn_close()}
		></button>
		<div
			use:modalFocus={{ onClose: close, initialFocus: '#monitor-name' }}
			class="relative z-10 max-h-[90dvh] w-full max-w-2xl overflow-y-auto rounded-t-xl border border-border bg-card p-4 shadow-xl sm:rounded-xl sm:p-6"
			role="dialog"
			aria-modal="true"
			aria-labelledby="monitor-form-title"
			tabindex="-1"
		>
			<div class="flex items-center justify-between border-b border-border pb-4">
				<h3 id="monitor-form-title" class="text-lg font-semibold tracking-tight">{monitor ? m.monitor_form_edit_title() : m.monitors_create()}</h3>
				<button type="button" onclick={close} class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground" aria-label={m.btn_close()}><X class="h-5 w-5" /></button>
			</div>

			<div class="mt-6 space-y-6">
				<!-- Common fields -->
				<div class="grid grid-cols-1 gap-4 md:grid-cols-2">
					<div>
						<label for="monitor-name" class="text-sm font-medium">{m.monitor_form_name_label()}</label>
						<input
							id="monitor-name"
							type="text"
							bind:value={formData.name}
							class="{inputClass} mt-1"
							placeholder={m.monitor_form_name_placeholder()}
						/>
					</div>

					<div>
						<label for="monitor-type" class="text-sm font-medium">{m.monitor_form_type_label()}</label>
						<div class="mt-1">
							<Select
								id="monitor-type"
								groups={typeSelectGroups}
								value={selectedType}
								onValueChange={(v) => (selectedType = v)}
								disabled={!!monitor}
								placeholder={m.monitor_form_type_placeholder()}
								class="w-full disabled:opacity-60"
							/>
						</div>
					</div>

					<div>
						<label for="monitor-interval" class="text-sm font-medium">{m.monitor_form_interval_label()}</label>
						<input
							id="monitor-interval"
							type="number"
							bind:value={formData.interval}
							min="10"
							class="{inputClass} mt-1"
						/>
					</div>

					<div>
						<label for="monitor-timeout" class="text-sm font-medium">{m.monitor_form_timeout_label()}</label>
						<input
							id="monitor-timeout"
							type="number"
							bind:value={formData.timeout}
							min="1"
							step="0.5"
							class="{inputClass} mt-1"
						/>
					</div>
				</div>

				<!-- Per-type config -->
				<div class="border-t border-border pt-6">
					<h4 class="mb-3 text-sm font-semibold text-muted-foreground">{m.monitor_form_type_config_heading()}</h4>

					{#if selectedType === 'docker'}
						<div
							class="mb-4 rounded-lg border border-warning/40 bg-warning/10 px-3 py-3 text-sm"
							role="note"
						>
							<div class="flex gap-2">
								<AlertTriangle class="mt-0.5 h-4 w-4 shrink-0 text-warning" aria-hidden="true" />
								<div class="min-w-0 space-y-2">
									<p class="font-medium text-foreground">{m.docker_monitor_warning_title()}</p>
									<p class="text-xs leading-relaxed text-muted-foreground">
										{m.docker_monitor_warning_body()}
									</p>
									<div class="flex flex-wrap gap-2 pt-0.5">
										<button
											type="button"
											onclick={() => (dockerGuideOpen = true)}
											class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs font-medium text-foreground transition-colors hover:bg-accent"
										>
											<BookOpen class="h-3.5 w-3.5" />
											{m.docker_monitor_warning_view_guide()}
										</button>
										<button
											type="button"
											onclick={downloadDockerGuide}
											class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs font-medium text-foreground transition-colors hover:bg-accent"
										>
											<Download class="h-3.5 w-3.5" />
											{m.docker_monitor_warning_download_guide()}
										</button>
									</div>
								</div>
							</div>
						</div>
					{/if}

					{#if selectedType === 'mqtt'}
						<div
							class="mb-4 rounded-lg border border-border bg-surface/60 px-3 py-3 text-sm"
							role="note"
						>
							<div class="flex gap-2">
								<BookOpen class="mt-0.5 h-4 w-4 shrink-0 text-primary" aria-hidden="true" />
								<div class="min-w-0 space-y-2">
									<p class="font-medium text-foreground">{m.mqtt_monitor_help_title()}</p>
									<p class="text-xs leading-relaxed text-muted-foreground">
										{m.mqtt_monitor_help_body()}
									</p>
									<div class="flex flex-wrap gap-2 pt-0.5">
										<button
											type="button"
											onclick={() => (mqttGuideOpen = true)}
											class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs font-medium text-foreground transition-colors hover:bg-accent"
										>
											<BookOpen class="h-3.5 w-3.5" />
											{m.mqtt_monitor_help_view_guide()}
										</button>
										<button
											type="button"
											onclick={downloadMqttGuide}
											class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs font-medium text-foreground transition-colors hover:bg-accent"
										>
											<Download class="h-3.5 w-3.5" />
											{m.mqtt_monitor_help_download_guide()}
										</button>
									</div>
								</div>
							</div>
						</div>
					{/if}

					{#if selectedType === 'database'}
						<div
							class="mb-4 rounded-lg border border-border bg-surface/60 px-3 py-3 text-sm"
							role="note"
						>
							<div class="flex gap-2">
								<BookOpen class="mt-0.5 h-4 w-4 shrink-0 text-primary" aria-hidden="true" />
								<div class="min-w-0 space-y-2">
									<p class="font-medium text-foreground">{m.database_monitor_help_title()}</p>
									<p class="text-xs leading-relaxed text-muted-foreground">
										{m.database_monitor_help_body()}
									</p>
									<div class="flex flex-wrap gap-2 pt-0.5">
										<button
											type="button"
											onclick={() => (databaseGuideOpen = true)}
											class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs font-medium text-foreground transition-colors hover:bg-accent"
										>
											<BookOpen class="h-3.5 w-3.5" />
											{m.database_monitor_help_view_guide()}
										</button>
										<button
											type="button"
											onclick={downloadDatabaseGuide}
											class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs font-medium text-foreground transition-colors hover:bg-accent"
										>
											<Download class="h-3.5 w-3.5" />
											{m.database_monitor_help_download_guide()}
										</button>
									</div>
								</div>
							</div>
						</div>
					{/if}

					{#if selectedType === 'rabbitmq'}
						<div
							class="mb-4 rounded-lg border border-border bg-surface/60 px-3 py-3 text-sm"
							role="note"
						>
							<div class="flex gap-2">
								<BookOpen class="mt-0.5 h-4 w-4 shrink-0 text-primary" aria-hidden="true" />
								<div class="min-w-0 space-y-2">
									<p class="font-medium text-foreground">{m.rabbitmq_monitor_help_title()}</p>
									<p class="text-xs leading-relaxed text-muted-foreground">
										{m.rabbitmq_monitor_help_body()}
									</p>
									<div class="flex flex-wrap gap-2 pt-0.5">
										<button
											type="button"
											onclick={() => (rabbitmqGuideOpen = true)}
											class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs font-medium text-foreground transition-colors hover:bg-accent"
										>
											<BookOpen class="h-3.5 w-3.5" />
											{m.rabbitmq_monitor_help_view_guide()}
										</button>
										<button
											type="button"
											onclick={downloadRabbitmqGuide}
											class="inline-flex items-center gap-1.5 rounded-md border border-border bg-surface px-2.5 py-1.5 text-xs font-medium text-foreground transition-colors hover:bg-accent"
										>
											<Download class="h-3.5 w-3.5" />
											{m.rabbitmq_monitor_help_download_guide()}
										</button>
									</div>
								</div>
							</div>
						</div>
					{/if}

					<div class="grid grid-cols-1 gap-x-4 gap-y-4 md:grid-cols-2">
						{#each visibleFields as field (field.key)}
							{#if field.type === 'checkbox'}
								<div class="md:col-span-2">
									<div class="flex items-center gap-2">
										<input
											id="cfg-{field.key}"
											type="checkbox"
											checked={Boolean(getFieldValue(field.key))}
											onchange={(e) => updateConfigField(field.key, (e.target as HTMLInputElement).checked)}
											class="h-4 w-4 rounded border-border"
										/>
										<label for="cfg-{field.key}" class="text-sm font-medium">{field.label}</label>
									</div>
									{#if field.help}
										<p class="mt-1 text-xs text-muted-foreground">{field.help}</p>
									{/if}
								</div>
							{:else}
								<div class={field.type === 'textarea' ? 'md:col-span-2' : ''}>
									<label for="cfg-{field.key}" class="text-sm font-medium">
										{field.label}{field.required ? ' *' : ''}
									</label>
									{#if field.key === 'accepted_statuscodes'}
										<input
											id="cfg-{field.key}"
											type="text"
											bind:value={formData.acceptedStatusCodes}
											placeholder={field.placeholder}
											class="{inputClass} mt-1"
										/>
									{:else if field.type === 'select'}
										<div class="mt-1">
											<Select
												id="cfg-{field.key}"
												options={(field.options || []).map((opt) => ({ value: opt.value, label: opt.label }))}
												value={String(getFieldValue(field.key) || field.default || '')}
												onValueChange={(v) => updateConfigField(field.key, v)}
												class="w-full"
											/>
										</div>
									{:else if field.type === 'number'}
										<input
											id="cfg-{field.key}"
											type="number"
											value={getFieldValue(field.key) as number}
											oninput={(e) => updateConfigField(field.key, parseFloat((e.target as HTMLInputElement).value) || 0)}
											class="{inputClass} mt-1"
										/>
									{:else if field.type === 'textarea'}
										<textarea
											id="cfg-{field.key}"
											value={getFieldValue(field.key) as string}
											oninput={(e) => updateConfigField(field.key, (e.target as HTMLTextAreaElement).value)}
											rows="4"
											placeholder={field.placeholder}
											class="{inputClass} mt-1 font-mono text-xs"
										></textarea>
									{:else if field.type === 'password'}
										<input
											id="cfg-{field.key}"
											type="password"
											value={getFieldValue(field.key) as string}
											oninput={(e) => updateConfigField(field.key, (e.target as HTMLInputElement).value)}
											placeholder={field.placeholder}
											autocomplete="off"
											class="{inputClass} mt-1 font-mono text-xs"
										/>
									{:else}
										<input
											id="cfg-{field.key}"
											type="text"
											value={getFieldValue(field.key) as string}
											oninput={(e) => updateConfigField(field.key, (e.target as HTMLInputElement).value)}
											onblur={() => field.key === 'url' && selectedType === 'http' && updateConfigField(field.key, normalizeHttpUrl(getFieldValue(field.key)))}
											placeholder={field.placeholder}
											class="{inputClass} mt-1"
										/>
									{/if}
									{#if field.help}
										<p class="mt-1 text-xs text-muted-foreground">{field.help}</p>
									{/if}
								</div>
							{/if}
						{/each}
					</div>
				</div>

				<!-- Advanced settings (applies to all monitor types) -->
				<div class="border-t border-border pt-6">
					<h4 class="mb-3 text-sm font-semibold text-muted-foreground">{m.monitor_form_advanced_heading()}</h4>
					<div class="grid grid-cols-1 gap-x-4 gap-y-4 md:grid-cols-2">
						<div class="md:col-span-2">
							<label class="text-sm font-medium" for="monitor-description">{m.monitor_form_description_label()}</label>
							<textarea
								id="monitor-description"
								bind:value={formData.description}
								rows="2"
								placeholder={m.monitor_form_description_placeholder()}
								class="{inputClass} mt-1"
							></textarea>
						</div>

						<div>
							<label class="text-sm font-medium" for="monitor-retry-interval">{m.monitor_form_retry_interval_label()}</label>
							<input
								id="monitor-retry-interval"
								type="number"
								bind:value={formData.retryInterval}
								min="0"
								class="{inputClass} mt-1"
							/>
						</div>

						<div>
							<label class="text-sm font-medium" for="monitor-max-retries">{m.monitor_form_max_retries_label()}</label>
							<input
								id="monitor-max-retries"
								type="number"
								bind:value={formData.maxRetries}
								min="0"
								class="{inputClass} mt-1"
							/>
							<p class="mt-1 text-xs text-muted-foreground">{m.monitor_form_max_retries_help()}</p>
						</div>

						<div>
							<label class="text-sm font-medium" for="monitor-resend-interval">{m.monitor_form_resend_interval_label()}</label>
							<input
								id="monitor-resend-interval"
								type="number"
								bind:value={formData.resendInterval}
								min="0"
								class="{inputClass} mt-1"
							/>
							<p class="mt-1 text-xs text-muted-foreground">{m.monitor_form_resend_interval_help()}</p>
						</div>

						<div>
							<label class="text-sm font-medium" for="monitor-weight">{m.monitor_form_weight_label()}</label>
							<input
								id="monitor-weight"
								type="number"
								bind:value={formData.weight}
								min="0"
								step="1"
								class="{inputClass} mt-1"
							/>
							<p class="mt-1 text-xs text-muted-foreground">{m.monitor_form_weight_help()}</p>
						</div>

						<div>
							<label class="text-sm font-medium" for="monitor-group">{m.monitor_form_group_label()}</label>
							<div class="mt-1">
								<Select
									id="monitor-group"
									options={[
										{ value: '', label: m.option_none() },
										...groupOptions.map((opt) => ({
											value: String(opt.id),
											label: '\u00A0\u00A0'.repeat(opt.depth) + (opt.depth > 0 ? '↳ ' : '') + opt.name,
										})),
									]}
									value={selectedGroupId}
									onValueChange={(v) => (selectedGroupId = v)}
									class="w-full"
								/>
							</div>
							<p class="mt-1 text-xs text-muted-foreground">
								{m.monitor_form_group_help()}
							</p>
						</div>

						<div>
							<label class="text-sm font-medium" for="monitor-proxy">{m.monitor_form_proxy_label()}</label>
							<div class="mt-1">
								<Select
									id="monitor-proxy"
									options={[
										{ value: '', label: m.option_none() },
										...proxies.map((p) => ({
											value: String(p.id),
											label: `${p.protocol}://${p.host}:${p.port}${p.is_default ? m.monitor_form_proxy_default() : ''}`,
										})),
									]}
									value={selectedProxyId}
									onValueChange={(v) => (selectedProxyId = v)}
									class="w-full"
								/>
							</div>
							<p class="mt-1 text-xs text-muted-foreground">
								{m.monitor_form_proxy_help()}
							</p>
						</div>

						<div>
							<label class="text-sm font-medium" for="monitor-escalation">{m.escalation_assign_label()}</label>
							<div class="mt-1">
								<Select
									id="monitor-escalation"
									options={[
										{ value: '', label: m.escalation_assign_none() },
										...escalationPolicies.map((p) => ({
											value: String(p.id),
											label: p.enabled ? p.name : `${p.name} (${m.escalation_disabled()})`,
										})),
									]}
									value={escalationPolicyId}
									onValueChange={(v) => (escalationPolicyId = v)}
									class="w-full"
								/>
							</div>
							<p class="mt-1 text-xs text-muted-foreground">
								{m.escalation_assign_help_monitor()}
							</p>
						</div>

						<div class="flex items-center gap-2 pt-6">
							<input
								id="monitor-upside-down"
								type="checkbox"
								bind:checked={formData.upsideDown}
								class="h-4 w-4 rounded border-border"
							/>
							<label for="monitor-upside-down" class="text-sm font-medium">{m.monitor_form_upside_down_label()}</label>
						</div>

						{#if selectedType === 'http'}
							<div>
								<div class="flex items-center gap-2">
									<input
										id="monitor-tls-ignore"
										type="checkbox"
										bind:checked={formData.tlsIgnore}
										class="h-4 w-4 rounded border-border"
									/>
									<label for="monitor-tls-ignore" class="text-sm font-medium">{m.monitor_form_tls_ignore_label()}</label>
								</div>
								<p class="mt-1 text-xs text-warning">{m.monitor_form_tls_ignore_warning()}</p>
							</div>
							<div>
								<div class="flex items-center gap-2">
									<input
										id="monitor-cert-expiry-notify"
										type="checkbox"
										bind:checked={formData.certExpiryNotify}
										class="h-4 w-4 rounded border-border"
									/>
									<label for="monitor-cert-expiry-notify" class="text-sm font-medium"
										>{m.monitor_form_cert_expiry_notify_label()}</label
									>
								</div>
								<p class="mt-1 text-xs text-muted-foreground">
									{m.monitor_form_cert_expiry_notify_help()}
								</p>
							</div>
						{/if}
					</div>
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
					{loading ? m.btn_saving() : monitor ? m.monitor_form_save_changes() : m.monitors_create()}
				</button>
			</div>
		</div>
	</div>

	<DockerSetupGuide bind:open={dockerGuideOpen} />
	<MqttSetupGuide bind:open={mqttGuideOpen} />
	<DatabaseSetupGuide bind:open={databaseGuideOpen} />
	<RabbitmqSetupGuide bind:open={rabbitmqGuideOpen} />
{/if}
