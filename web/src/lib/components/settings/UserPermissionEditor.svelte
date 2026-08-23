<script lang="ts">
	/**
	 * Per-user access editor: which monitors and groups a user may see, plus the
	 * capability flags.
	 *
	 * Three things this component exists to communicate honestly, because getting
	 * any of them wrong is a security bug the tests would not catch:
	 *
	 * 1. **An admin is unrestricted.** It does not matter what is stored in their
	 *    grant set or their capability flags — the server ORs `is_admin` over
	 *    everything (docs/HANDOFF-NEXT.md §3.1). So for an admin we do NOT render
	 *    a grant editor that pretends to constrain them. We say so instead.
	 * 2. **A group grant reaches as far as its toggle says.** Deep (the default)
	 *    hands over every monitor inside the folder and inside every subfolder
	 *    below it, forever, including monitors added later. Shallow stops at the
	 *    folder's own monitors. The summary line spells out the real number.
	 * 3. **Grants are about SEEING, never changing.** Nothing on this screen can
	 *    give someone the right to edit a monitor. That is ownership — you may
	 *    change what you created — and it is not editable here. The "can create"
	 *    capabilities below let a user make new monitors and folders, which they
	 *    then own; they confer nothing over anyone else's.
	 *
	 * State lives in the parent page (which owns the API calls); this component is
	 * presentation plus callbacks.
	 */
	import MonitorPicker from '$lib/components/MonitorPicker.svelte';
	import EmptyState from '$lib/components/EmptyState.svelte';
	import Skeleton from '$lib/components/Skeleton.svelte';
	import {
		AlertCircle,
		Bell,
		CalendarClock,
		FolderPlus,
		FolderTree,
		KeyRound,
		PlusCircle,
		Puzzle,
		ShieldCheck,
		X,
	} from '@lucide/svelte';
	import type { Monitor } from '$lib/stores/ws.svelte.js';
	import type { MonitorWithGroup } from '$lib/api/monitors';
	import type { MonitorGroupView } from '$lib/api/monitorGroups';
	import type { User as UserAccount, UserPermissions } from '$lib/api/users';
	import { grantSummary, groupOptions, permissionsEqual } from '$lib/user-permissions';

	interface Props {
		user: UserAccount;
		/** Working draft — what Save would send. */
		draft: UserPermissions;
		/** Last set the server acknowledged, for the dirty check. */
		saved: UserPermissions;
		monitors: MonitorWithGroup[];
		groups: MonitorGroupView[];
		/** Fetching this user's existing grants. */
		loading?: boolean;
		/** A save is in flight. */
		saving?: boolean;
		/** Fetching the monitor/group lists to pick from. */
		targetsLoading?: boolean;
		targetsError?: string | null;
		/** A capability-flag write is in flight. */
		capabilitySaving?: boolean;
		onAddMonitor: (monitor: Monitor) => void;
		onRemoveMonitor: (monitorId: number) => void;
		onToggleGroup: (groupId: number) => void;
		/** Flips one granted group between deep and shallow. */
		onToggleGroupDescendants: (groupId: number, includeDescendants: boolean) => void;
		onToggleCapability: (key: CapabilityKey, value: boolean) => void;
		onSave: () => void;
		onReset: () => void;
		onRetryTargets: () => void;
	}

	type CapabilityKey =
		| 'can_manage_notifications'
		| 'can_manage_maintenance'
		| 'can_view_extensions'
		| 'can_create_monitors'
		| 'can_create_top_level_monitors'
		| 'can_create_groups'
		| 'can_edit_group_metadata';

	let {
		user,
		draft,
		saved,
		monitors,
		groups,
		loading = false,
		saving = false,
		targetsLoading = false,
		targetsError = null,
		capabilitySaving = false,
		onAddMonitor,
		onRemoveMonitor,
		onToggleGroup,
		onToggleGroupDescendants,
		onToggleCapability,
		onSave,
		onReset,
		onRetryTargets,
	}: Props = $props();

	const grantedMonitors = $derived(
		draft.monitor_ids
			.map((id) => monitors.find((m) => m.id === id))
			.filter((m): m is MonitorWithGroup => m !== undefined)
	);
	const options = $derived(groupOptions(groups));
	const summary = $derived(grantSummary(groups, monitors, draft));
	const dirty = $derived(!permissionsEqual(draft, saved));
	const noGrants = $derived(draft.monitor_ids.length === 0 && draft.groups.length === 0);
	/** Granted group id -> its reach, so the row can render its own toggle. */
	const grantByGroupId = $derived(new Map(draft.groups.map((g) => [g.group_id, g])));

	const capabilities = [
		{
			key: 'can_manage_notifications' as const,
			label: 'Manage notifications',
			help: 'Create, edit and delete notification providers.',
			icon: Bell,
		},
		{
			key: 'can_manage_maintenance' as const,
			label: 'Manage maintenance',
			help: 'Schedule and cancel maintenance windows.',
			icon: CalendarClock,
		},
		{
			key: 'can_view_extensions' as const,
			label: 'View extensions',
			help: 'Discover and open registered extension pages in Phoenix. An extension may still require its own sign-in.',
			icon: Puzzle,
		},
		{
			key: 'can_create_monitors' as const,
			label: 'Create monitors',
			help: 'Add new monitors inside groups they are granted, and edit or delete the ones they created. Not anyone else’s.',
			icon: PlusCircle,
		},
		{
			key: 'can_create_top_level_monitors' as const,
			label: 'Create top-level monitors',
			help: 'Also allow creating monitors outside any group. Requires “Create monitors”. Does not expand group access.',
			icon: PlusCircle,
		},
		{
			key: 'can_create_groups' as const,
			label: 'Create groups',
			help: 'Add new folders, and edit or delete the ones they created. Not anyone else’s.',
			icon: FolderPlus,
		},
		{
			key: 'can_edit_group_metadata' as const,
			label: 'Edit group metadata',
			help: 'On groups they can see: edit contact, description, condition, and display options. Cannot rename, move, or delete folders.',
			icon: FolderTree,
		},
	];

	const primaryBtn =
		'inline-flex items-center justify-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50';
	const ghostBtn =
		'inline-flex items-center justify-center gap-2 rounded-lg border border-border px-3 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground disabled:opacity-50';
</script>

<div
	class="border-t border-border bg-surface/40 px-4 py-4 sm:px-5"
	data-testid="user-permission-editor"
	data-user-id={user.id}
>
	{#if user.is_admin}
		<!--
			An admin's grants are not a constraint, so we must not draw a box that
			implies they are. State the truth and stop.
		-->
		<div
			class="flex flex-col gap-3 rounded-lg border border-primary/25 bg-primary/5 p-4 sm:flex-row"
			data-testid="user-permission-unrestricted"
		>
			<span class="grid h-8 w-8 shrink-0 place-items-center rounded-lg bg-primary/10 text-primary">
				<ShieldCheck class="h-4 w-4" />
			</span>
			<div class="min-w-0 text-sm">
				<p class="font-medium text-foreground">This account is unrestricted.</p>
				<p class="mt-1 text-xs leading-relaxed text-muted-foreground">
					Admins see every monitor, group and registered extension, and can manage notifications
					and maintenance — regardless of the grants or capability flags stored against them. To
					limit what <span class="font-medium text-foreground">{user.username}</span> can reach,
					remove their admin role first.
				</p>
				<dl class="mt-3 flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
					{#each capabilities as cap (cap.key)}
						<div class="flex items-center gap-1.5">
							<dt>{cap.label} flag:</dt>
							<dd class="font-mono text-foreground">{user[cap.key] ? 'true' : 'false'}</dd>
						</div>
					{/each}
				</dl>
				<p class="mt-1.5 text-[11px] leading-relaxed text-faint">
					Stored but not consulted while the account is an admin. They take effect if the admin role
					is removed.
				</p>
			</div>
		</div>
	{:else}
		<div class="space-y-5">
			<!-- Capability flags -->
			<section>
				<div class="flex items-center gap-2">
					<KeyRound class="h-4 w-4 shrink-0 text-muted-foreground" />
					<h3 class="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
						Capabilities
					</h3>
				</div>
				<p class="mt-1 text-xs leading-relaxed text-muted-foreground">
					Account-level permissions are independent of the monitor access granted below. Create
					permissions give {user.username} control of what
					<em class="not-italic font-medium text-foreground">they</em> make — never of monitors or
					folders someone else created, however many they can see.
				</p>
				<div class="mt-3 grid gap-2 sm:grid-cols-2">
					{#each capabilities as cap (cap.key)}
						{@const on = user[cap.key]}
						<div class="flex items-start gap-3 rounded-lg border border-border bg-card p-3">
							<cap.icon class="mt-0.5 h-4 w-4 shrink-0 text-muted-foreground" />
							<div class="min-w-0 flex-1">
								<p class="text-sm font-medium">{cap.label}</p>
								<p class="mt-0.5 text-xs leading-relaxed text-muted-foreground">{cap.help}</p>
							</div>
							<button
								type="button"
								role="switch"
								aria-checked={on}
								aria-label="{cap.label} for {user.username}"
								disabled={capabilitySaving}
								data-testid="user-capability-toggle"
								data-capability={cap.key}
								onclick={() => onToggleCapability(cap.key, !on)}
								class="relative mt-0.5 h-5 w-9 shrink-0 rounded-full transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-50 {on
									? 'bg-primary'
									: 'bg-muted'}"
							>
								<span
									class="absolute left-0 top-0.5 h-4 w-4 rounded-full bg-white shadow transition-transform {on
										? 'translate-x-[18px]'
										: 'translate-x-0.5'}"
								></span>
							</button>
						</div>
					{/each}
				</div>
			</section>

			<!-- Grants -->
			<section>
				<div class="flex items-center gap-2">
					<FolderTree class="h-4 w-4 shrink-0 text-muted-foreground" />
					<h3 class="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
						Monitor access
					</h3>
				</div>
				<p class="mt-1 text-xs leading-relaxed text-muted-foreground">
					A non-admin with no grants sees nothing. Granting a group grants the monitors inside it,
					including ones added later — and, unless you turn off <span class="font-medium text-foreground"
						>Include subfolders</span
					>, everything in every folder nested below it too.
				</p>

				{#if loading}
					<div class="mt-3 space-y-2" role="status">
						<span class="sr-only">Loading permissions…</span>
						<Skeleton class="h-9 w-full" />
						<Skeleton class="h-6 w-2/3" />
						<Skeleton class="h-20 w-full" />
					</div>
				{:else if targetsError}
					<div
						class="mt-3 flex flex-col gap-3 rounded-lg border border-danger/25 bg-danger/10 p-4 sm:flex-row sm:items-center sm:justify-between"
						data-testid="user-permission-error"
					>
						<div class="flex min-w-0 items-start gap-2 text-sm text-danger">
							<AlertCircle class="mt-0.5 h-4 w-4 shrink-0" />
							<span class="min-w-0 break-words">{targetsError}</span>
						</div>
						<button type="button" onclick={onRetryTargets} class="{ghostBtn} shrink-0">Retry</button>
					</div>
				{:else}
					<!-- Individual monitors -->
					<div class="mt-3">
						<span class="text-xs font-medium text-muted-foreground">Individual monitors</span>
						<div class="mt-1.5">
							<MonitorPicker
								monitors={monitors as Monitor[]}
								exclude={draft.monitor_ids}
								loading={targetsLoading}
								placeholder="Search monitors to grant…"
								emptyText={monitors.length === 0
									? 'No monitors exist yet.'
									: 'Every monitor is already granted.'}
								onSelect={onAddMonitor}
							/>
						</div>

						{#if grantedMonitors.length > 0}
							<ul class="mt-2.5 flex flex-wrap gap-1.5" data-testid="user-granted-monitors">
								{#each grantedMonitors as m (m.id)}
									<li
										class="inline-flex max-w-full items-center gap-1.5 rounded-full border border-border bg-card py-1 pl-2.5 pr-1 text-xs"
									>
										<span class="truncate font-medium">{m.name}</span>
										<button
											type="button"
											onclick={() => onRemoveMonitor(m.id)}
											aria-label="Revoke access to {m.name}"
											class="grid h-4 w-4 shrink-0 place-items-center rounded-full text-muted-foreground transition-colors hover:bg-accent hover:text-danger"
										><X class="h-3 w-3" /></button>
									</li>
								{/each}
							</ul>
						{/if}
					</div>

					<!-- Groups -->
					<div class="mt-4">
						<span class="text-xs font-medium text-muted-foreground">Groups</span>
						{#if targetsLoading}
							<div class="mt-1.5 space-y-1.5" role="status">
								<span class="sr-only">Loading groups…</span>
								<Skeleton class="h-8 w-full" />
								<Skeleton class="h-8 w-full" />
							</div>
						{:else if options.length === 0}
							<p class="mt-1.5 text-xs text-muted-foreground">
								No groups exist yet. Create one on the Monitors page to grant access in bulk.
							</p>
						{:else}
							<div
								class="mt-1.5 max-h-56 space-y-0.5 overflow-y-auto rounded-lg border border-border bg-card p-1.5"
								data-testid="user-group-grants"
							>
								{#each options as opt (opt.id)}
									{@const grant = grantByGroupId.get(opt.id)}
									{@const checked = grant !== undefined}
									<div class="rounded-md transition-colors hover:bg-accent/50">
										<label
											class="flex cursor-pointer items-center gap-2.5 px-2 py-1.5 text-sm"
										>
											<input
												type="checkbox"
												class="h-4 w-4 shrink-0 rounded border-border accent-primary"
												{checked}
												data-testid="user-group-checkbox"
												data-group-id={opt.id}
												onchange={() => onToggleGroup(opt.id)}
											/>
											<span class="min-w-0 flex-1 truncate" title={opt.path}>{opt.path}</span>
										</label>
										<!--
											The reach control only exists once the folder is actually granted:
											an "include subfolders" checkbox on an ungranted folder would be
											inert, and offering a choice that decides nothing is how an admin
											ends up believing they restricted something.
										-->
										{#if grant}
											<label
												class="ml-9 flex cursor-pointer items-center gap-2 pb-1.5 pr-2 text-xs text-muted-foreground"
											>
												<input
													type="checkbox"
													class="h-3.5 w-3.5 shrink-0 rounded border-border accent-primary"
													checked={grant.include_descendants}
													data-testid="user-group-descendants-checkbox"
													data-group-id={opt.id}
													onchange={() =>
														onToggleGroupDescendants(opt.id, !grant.include_descendants)}
												/>
												<span>Include subfolders</span>
											</label>
										{/if}
									</div>
								{/each}
							</div>
						{/if}
					</div>

					<!-- What this actually adds up to -->
					<p class="mt-3 text-xs text-muted-foreground" data-testid="user-grant-summary">
						{#if noGrants}
							<span class="font-medium text-warning">{user.username} currently sees no monitors.</span>
						{:else}
							<span class="font-medium text-foreground"
								>{user.username} sees {summary.total}
								{summary.total === 1 ? 'monitor' : 'monitors'}</span
							>
							— {summary.direct} granted directly, {summary.viaGroups} via
							{summary.groups}
							{summary.groups === 1 ? 'group' : 'groups'}.
						{/if}
						{#if summary.redundantDirect > 0}
							<span class="mt-1 block text-faint">
								{summary.redundantDirect}
								{summary.redundantDirect === 1 ? 'monitor is' : 'monitors are'} granted both directly
								and via a group — removing the group would not revoke {summary.redundantDirect === 1
									? 'it'
									: 'them'}.
							</span>
						{/if}
					</p>

					{#if noGrants && !dirty}
						<div class="mt-3">
							<EmptyState
								icon={FolderTree}
								title="No access granted yet."
								description="Pick a monitor or tick a group above, then save."
							/>
						</div>
					{/if}

					<div class="mt-4 flex flex-col-reverse gap-2 sm:flex-row sm:items-center sm:justify-end">
						{#if dirty}
							<span class="text-xs text-warning sm:mr-auto">Unsaved changes.</span>
							<button type="button" onclick={onReset} disabled={saving} class={ghostBtn}>
								Discard
							</button>
						{/if}
						<button
							type="button"
							onclick={onSave}
							disabled={saving || !dirty}
							data-testid="user-permission-save-btn"
							class={primaryBtn}
						>
							{saving ? 'Saving…' : 'Save access'}
						</button>
					</div>
				{/if}
			</section>
		</div>
	{/if}
</div>
