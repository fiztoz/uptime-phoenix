<script lang="ts">
	import { auth } from '$lib/stores/auth.svelte.ts';
	import { themeStore } from '$lib/stores/theme.svelte.ts';
	import { toast } from 'svelte-sonner';
	import { Shield, User, Palette, Key, KeyRound, Bell, Plus, Edit2, Trash2, Send, Tag, Copy, Sun, Moon, Fingerprint, UserCog, Route, ChevronDown, SlidersHorizontal } from '@lucide/svelte';
	import NotificationForm from '$lib/components/NotificationForm.svelte';
	import UserPermissionEditor from '$lib/components/settings/UserPermissionEditor.svelte';
	import EmptyState from '$lib/components/EmptyState.svelte';
	import Skeleton from '$lib/components/Skeleton.svelte';
	import { confirmAction } from '$lib/stores/confirm.svelte';
	import { tagsApi, type Tag as TagType } from '$lib/api/tags';
	import { apiKeysApi, type ApiKey, type ApiKeyScope } from '$lib/api/apikeys';
	import { webauthnApi, isWebAuthnSupported, type Passkey } from '$lib/api/webauthn';
	import {
		usersApi,
		type GroupGrant,
		type User as UserAccount,
		type UserPermissions,
	} from '$lib/api/users';
	import { monitorsApi, type MonitorWithGroup } from '$lib/api/monitors';
	import { monitorGroupsApi, type MonitorGroupView } from '$lib/api/monitorGroups';
	import type { Monitor } from '$lib/stores/ws.svelte.js';
	import { proxiesApi, type Proxy, type ProxyProtocol } from '$lib/api/proxies';
	import Select from '$lib/components/Select.svelte';
	import ColorPicker from '$lib/components/ColorPicker.svelte';
	import DateTimePicker from '$lib/components/DateTimePicker.svelte';

	let show2FASetup = $state(false);
	let qrUrl = $state('');
	let secret = $state('');
	let otpCode = $state('');
	let disablePassword = $state('');
	let loading = $state(false);

	// Notifications state (loaded on mount)
	let notifications = $state<any[]>([]);
	let notificationsLoading = $state(true);
	let notificationsError = $state<string | null>(null);
	let showNotificationForm = $state(false);
	let editingNotification = $state<any>(null);

	// Tags
	let tags = $state<TagType[]>([]);
	let tagsLoading = $state(true);
	let tagsError = $state<string | null>(null);
	let tagName = $state('');
	let tagColor = $state('#666666');
	let editingTagId = $state<number | null>(null);

	// Proxies
	let proxies = $state<Proxy[]>([]);
	let proxiesLoading = $state(true);
	let proxiesError = $state<string | null>(null);
	let proxyProtocol = $state<ProxyProtocol>('http');
	let proxyHost = $state('');
	let proxyPort = $state(8080);
	let proxyAuthEnabled = $state(false);
	let proxyUsername = $state('');
	let proxyPassword = $state('');
	let proxyActive = $state(true);
	let proxyIsDefault = $state(false);
	let editingProxyId = $state<number | null>(null);

	// API keys
	let apiKeys = $state<ApiKey[]>([]);
	let apiKeysLoading = $state(true);
	let apiKeysError = $state<string | null>(null);
	let newKeyName = $state('');
	let newKeyExpiresAt = $state('');
	let newKeyScopes = $state<Set<ApiKeyScope>>(new Set(['read']));
	let createdKeyPlain = $state<string | null>(null);

	// Passkeys (WebAuthn)
	let passkeys = $state<Passkey[]>([]);
	let passkeysLoading = $state(true);
	let passkeysError = $state<string | null>(null);
	let newPasskeyName = $state('');
	let passkeyLoading = $state(false);
	const passkeysSupported = isWebAuthnSupported();

	// Users (admin)
	let users = $state<UserAccount[]>([]);
	let usersLoading = $state(true);
	let usersError = $state<string | null>(null);
	let newUsername = $state('');
	let newUserPassword = $state('');
	let newUserActive = $state(true);
	let newUserIsAdmin = $state(false);
	let newUserCanManageNotifications = $state(false);
	let newUserCanManageMaintenance = $state(false);
	let userLoading = $state(false);
	let permissionEditorUserId = $state<number | null>(null);
	let permissionLoadingUserId = $state<number | null>(null);
	let permissionSavingUserId = $state<number | null>(null);
	let capabilitySavingUserId = $state<number | null>(null);
	/** Working drafts — what "Save access" would send for each user. */
	let userPermissions = $state<Map<number, UserPermissions>>(new Map());
	/** The last set the server acknowledged, per user. Drives the dirty check
	 *  and "Discard" — `PUT /permissions` is a REPLACE-SET, so an accidental
	 *  save of a half-built draft would silently revoke the rest. */
	let savedPermissions = $state<Map<number, UserPermissions>>(new Map());
	let permissionMonitors = $state<MonitorWithGroup[]>([]);
	let permissionGroups = $state<MonitorGroupView[]>([]);
	let permissionTargetsLoading = $state(false);
	let permissionTargetsError = $state<string | null>(null);
	let permissionTargetsLoaded = $state(false);

	const user = $derived(auth.user);
	const twoFactorEnabled = $derived(user?.two_factor_enabled ?? false);
	const isAdmin = $derived(user?.is_admin ?? false);

	// Anchors for the jump nav. Section ids double as scroll targets.
	const navSections = $derived(
		[
			{ id: 'profile', label: 'Profile' },
			{ id: 'security', label: 'Security' },
			{ id: 'passkeys', label: 'Passkeys' },
			{ id: 'appearance', label: 'Appearance' },
			{ id: 'tags', label: 'Tags' },
			{ id: 'proxies', label: 'Proxies' },
			{ id: 'notifications', label: 'Notifications' },
			...(isAdmin ? [{ id: 'users', label: 'Users' }] : []),
			{ id: 'api-keys', label: 'API Keys' },
		]
	);

	async function start2FASetup() {
		try {
			loading = true;
			const res = await auth.setup2FA();
			qrUrl = res.qr_url;
			secret = res.secret;
			show2FASetup = true;
			otpCode = '';
		} catch (e: any) {
			toast.error(e?.message || 'Failed to start 2FA setup');
		} finally {
			loading = false;
		}
	}

	async function enable2FA() {
		if (!otpCode || otpCode.length !== 6) {
			toast.error('Enter 6-digit code');
			return;
		}
		try {
			loading = true;
			await auth.enable2FA(otpCode);
			show2FASetup = false;
			qrUrl = '';
			secret = '';
			otpCode = '';
		} catch (e: any) {
			toast.error(e?.message || 'Enable 2FA failed');
		} finally {
			loading = false;
		}
	}

	async function disable2FA() {
		if (!disablePassword) {
			toast.error('Enter your password');
			return;
		}
		const ok = await confirmAction({
			title: 'Disable two-factor authentication?',
			message:
				'Your account will be protected by your password alone. Anyone who learns it can sign in as you. You can re-enable 2FA later, but you will have to re-scan the QR code.',
			confirmLabel: 'Disable 2FA',
			destructive: true,
		});
		if (!ok) return;

		try {
			loading = true;
			await auth.disable2FA(disablePassword);
			disablePassword = '';
		} catch (e: any) {
			toast.error(e?.message || 'Disable failed');
		} finally {
			loading = false;
		}
	}

	function cancel2FASetup() {
		show2FASetup = false;
		qrUrl = '';
		secret = '';
		otpCode = '';
	}

	// Notification handlers
	async function loadNotifications() {
		notificationsLoading = true;
		notificationsError = null;
		try {
			// Dynamic import to avoid circular if needed, but direct
			const { notificationsApi } = await import('$lib/api/notifications');
			notifications = await notificationsApi.list();
		} catch (e: any) {
			const message = e?.message || 'Failed to load notifications';
			notificationsError = message;
			toast.error(message);
		} finally {
			notificationsLoading = false;
		}
	}

	function openCreateNotification() {
		editingNotification = null;
		showNotificationForm = true;
	}

	function openEditNotification(n: any) {
		editingNotification = n;
		showNotificationForm = true;
	}

	async function handleDeleteNotification(id: number, name: string) {
		const ok = await confirmAction({
			title: `Delete notification provider "${name}"?`,
			message:
				'Every monitor that alerts through it stops sending alerts immediately — they will go down silently. Monitors themselves are not affected.',
			confirmLabel: 'Delete provider',
			destructive: true,
		});
		if (!ok) return;
		try {
			const { notificationsApi } = await import('$lib/api/notifications');
			await notificationsApi.remove(id);
			toast.success('Notification deleted');
			await loadNotifications();
		} catch (e: any) {
			toast.error(e?.message || 'Delete failed');
		}
	}

	async function handleTestNotification(id: number, name: string) {
		try {
			const { notificationsApi } = await import('$lib/api/notifications');
			await notificationsApi.test(id);
			toast.success(`Test alert sent via ${name}`);
		} catch (e: any) {
			toast.error(e?.message || 'Test send failed');
		}
	}

	async function loadTags() {
		tagsLoading = true;
		tagsError = null;
		try {
			tags = await tagsApi.list();
		} catch (error: unknown) {
			tags = [];
			tagsError = error && typeof error === 'object' && 'message' in error
				? String((error as { message: string }).message)
				: 'Failed to load tags';
		} finally {
			tagsLoading = false;
		}
	}

	async function saveTag() {
		if (!tagName.trim()) {
			toast.error('Tag name is required');
			return;
		}
		try {
			if (editingTagId != null) {
				await tagsApi.update(editingTagId, { name: tagName.trim(), color: tagColor });
				toast.success('Tag updated');
			} else {
				await tagsApi.create({ name: tagName.trim(), color: tagColor });
				toast.success('Tag created');
			}
			tagName = '';
			tagColor = '#666666';
			editingTagId = null;
			await loadTags();
		} catch (e: any) {
			toast.error(e?.message || 'Save tag failed');
		}
	}

	function startEditTag(t: TagType) {
		editingTagId = t.id;
		tagName = t.name;
		tagColor = t.color;
	}

	function cancelEditTag() {
		editingTagId = null;
		tagName = '';
		tagColor = '#666666';
	}

	async function deleteTag(id: number, name: string) {
		const ok = await confirmAction({
			title: `Delete tag "${name}"?`,
			message:
				'It is removed from every monitor carrying it, and any dashboard filter saved against it stops matching. The monitors themselves are untouched.',
			confirmLabel: 'Delete tag',
			destructive: true,
		});
		if (!ok) return;
		try {
			await tagsApi.remove(id);
			toast.success('Tag deleted');
			await loadTags();
		} catch (e: any) {
			toast.error(e?.message || 'Delete failed');
		}
	}

	async function loadProxies() {
		proxiesLoading = true;
		proxiesError = null;
		try {
			proxies = await proxiesApi.list();
		} catch (error: unknown) {
			proxies = [];
			proxiesError = error && typeof error === 'object' && 'message' in error
				? String((error as { message: string }).message)
				: 'Failed to load proxies';
		} finally {
			proxiesLoading = false;
		}
	}

	function resetProxyForm() {
		editingProxyId = null;
		proxyProtocol = 'http';
		proxyHost = '';
		proxyPort = 8080;
		proxyAuthEnabled = false;
		proxyUsername = '';
		proxyPassword = '';
		proxyActive = true;
		proxyIsDefault = false;
	}

	async function saveProxy() {
		if (!proxyHost.trim()) {
			toast.error('Host is required');
			return;
		}
		if (proxyPort < 1 || proxyPort > 65535) {
			toast.error('Port must be between 1 and 65535');
			return;
		}
		try {
			const input = {
				protocol: proxyProtocol,
				host: proxyHost.trim(),
				port: Number(proxyPort),
				auth: proxyAuthEnabled,
				username: proxyAuthEnabled ? proxyUsername.trim() : '',
				// Only send a password when the caller actually typed one — an
				// empty string on edit would overwrite the stored credential.
				...(proxyAuthEnabled && proxyPassword ? { password: proxyPassword } : {}),
				active: proxyActive,
				is_default: proxyIsDefault,
			};
			if (editingProxyId != null) {
				await proxiesApi.update(editingProxyId, input);
				toast.success('Proxy updated');
			} else {
				await proxiesApi.create(input);
				toast.success('Proxy created');
			}
			resetProxyForm();
			await loadProxies();
		} catch (e: any) {
			toast.error(e?.message || 'Save proxy failed');
		}
	}

	function startEditProxy(p: Proxy) {
		editingProxyId = p.id;
		proxyProtocol = p.protocol;
		proxyHost = p.host;
		proxyPort = p.port;
		proxyAuthEnabled = p.auth;
		proxyUsername = p.username;
		proxyPassword = '';
		proxyActive = p.active;
		proxyIsDefault = p.is_default;
	}

	async function deleteProxy(id: number, label: string) {
		const ok = await confirmAction({
			title: `Delete proxy "${label}"?`,
			message:
				'Monitors currently routed through it fall back to a direct connection on their next check — which may reach a different host, or none at all.',
			confirmLabel: 'Delete proxy',
			destructive: true,
		});
		if (!ok) return;
		try {
			await proxiesApi.remove(id);
			toast.success('Proxy deleted');
			if (editingProxyId === id) resetProxyForm();
			await loadProxies();
		} catch (e: any) {
			toast.error(e?.message || 'Delete failed');
		}
	}

	async function loadApiKeys() {
		apiKeysLoading = true;
		apiKeysError = null;
		try {
			apiKeys = await apiKeysApi.list();
		} catch (error: unknown) {
			apiKeys = [];
			apiKeysError = error && typeof error === 'object' && 'message' in error
				? String((error as { message: string }).message)
				: 'Failed to load API keys';
		} finally {
			apiKeysLoading = false;
		}
	}

	function toggleScope(scope: ApiKeyScope) {
		const next = new Set(newKeyScopes);
		if (next.has(scope)) next.delete(scope);
		else next.add(scope);
		if (next.size === 0) next.add('read');
		newKeyScopes = next;
	}

	async function createApiKey() {
		if (!newKeyName.trim()) {
			toast.error('Key name is required');
			return;
		}
		try {
			const res = await apiKeysApi.create({
				name: newKeyName.trim(),
				scopes: [...newKeyScopes],
				expires_at: newKeyExpiresAt.trim() ? new Date(newKeyExpiresAt).toISOString() : null,
			});
			createdKeyPlain = res.key;
			newKeyName = '';
			newKeyExpiresAt = '';
			newKeyScopes = new Set(['read']);
			await loadApiKeys();
			toast.success('API key created — copy it now');
		} catch (e: any) {
			toast.error(e?.message || 'Create failed');
		}
	}

	async function revokeApiKey(id: number, name: string) {
		const ok = await confirmAction({
			title: `Revoke API key "${name}"?`,
			message:
				'Any script, dashboard or integration still using this key starts failing on its next request. This cannot be undone — you would have to issue a new key and update every caller.',
			confirmLabel: 'Revoke key',
			destructive: true,
		});
		if (!ok) return;
		try {
			await apiKeysApi.revoke(id);
			toast.success('API key revoked');
			await loadApiKeys();
		} catch (e: any) {
			toast.error(e?.message || 'Revoke failed');
		}
	}

	async function copyKey() {
		if (!createdKeyPlain) return;
		try {
			await navigator.clipboard.writeText(createdKeyPlain);
			toast.success('Copied to clipboard');
		} catch {
			toast.error('Copy failed');
		}
	}

	async function loadPasskeys() {
		if (!passkeysSupported) {
			passkeysLoading = false;
			passkeysError = null;
			return;
		}
		passkeysLoading = true;
		passkeysError = null;
		try {
			passkeys = await webauthnApi.list();
		} catch (error: unknown) {
			passkeys = [];
			passkeysError = error && typeof error === 'object' && 'message' in error
				? String((error as { message: string }).message)
				: 'Failed to load passkeys';
		} finally {
			passkeysLoading = false;
		}
	}

	async function registerPasskey() {
		try {
			passkeyLoading = true;
			const name = newPasskeyName.trim() || 'Passkey';
			await webauthnApi.register(name);
			newPasskeyName = '';
			await loadPasskeys();
			toast.success('Passkey registered');
		} catch (e: any) {
			// Cancelled prompts surface as DOMException; keep the message tidy.
			toast.error(e?.message || 'Passkey registration failed');
		} finally {
			passkeyLoading = false;
		}
	}

	async function deletePasskey(id: number, name: string) {
		const ok = await confirmAction({
			title: `Delete passkey "${name}"?`,
			message:
				'That device can no longer sign in without a password. Your password still works, and you can register the device again later.',
			confirmLabel: 'Delete passkey',
			destructive: true,
		});
		if (!ok) return;
		try {
			await webauthnApi.remove(id);
			toast.success('Passkey deleted');
			await loadPasskeys();
		} catch (e: any) {
			toast.error(e?.message || 'Delete failed');
		}
	}

	async function loadUsers() {
		if (!isAdmin) {
			users = [];
			usersLoading = false;
			usersError = null;
			return;
		}
		usersLoading = true;
		usersError = null;
		try {
			users = await usersApi.list();
		} catch (error: unknown) {
			users = [];
			usersError = error && typeof error === 'object' && 'message' in error
				? String((error as { message: string }).message)
				: 'Failed to load users';
		} finally {
			usersLoading = false;
		}
	}

	async function loadPermissionTargets() {
		if (!isAdmin) return;
		permissionTargetsLoading = true;
		permissionTargetsError = null;
		try {
			const [monitors, groups] = await Promise.all([
				monitorsApi.list(),
				monitorGroupsApi.list(),
			]);
			// The wire carries group_id on every monitor even though the shared
			// Monitor type does not declare it — see MonitorWithGroup. The grant
			// summary needs it to work out what a group grant actually covers.
			permissionMonitors = monitors as MonitorWithGroup[];
			permissionGroups = groups;
			permissionTargetsLoaded = true;
		} catch (e: any) {
			permissionTargetsError = e?.message || 'Failed to load grant targets';
			permissionMonitors = [];
			permissionGroups = [];
			permissionTargetsLoaded = false;
		} finally {
			permissionTargetsLoading = false;
		}
	}

	function permissionsFor(userId: number): UserPermissions {
		return userPermissions.get(userId) ?? { monitor_ids: [], groups: [] };
	}

	function savedPermissionsFor(userId: number): UserPermissions {
		return savedPermissions.get(userId) ?? { monitor_ids: [], groups: [] };
	}

	/**
	 * Copies a permission set, deduping both halves.
	 *
	 * The group grants are cloned per-entry rather than shallow-copied: draft and
	 * saved must not share grant objects, or toggling "include subfolders" on the
	 * draft would mutate the saved set too and the dirty check would never fire.
	 */
	function clonePermissions(permissions: UserPermissions): UserPermissions {
		const seen = new Set<number>();
		const groups: GroupGrant[] = [];
		for (const g of permissions.groups) {
			if (seen.has(g.group_id)) continue;
			seen.add(g.group_id);
			groups.push({ group_id: g.group_id, include_descendants: g.include_descendants });
		}
		return { monitor_ids: [...new Set(permissions.monitor_ids)], groups };
	}

	function setPermissionsFor(userId: number, permissions: UserPermissions) {
		userPermissions.set(userId, clonePermissions(permissions));
		userPermissions = new Map(userPermissions);
	}

	/** Records what the server has, and resets the draft to match it. */
	function commitPermissionsFor(userId: number, permissions: UserPermissions) {
		savedPermissions.set(userId, clonePermissions(permissions));
		savedPermissions = new Map(savedPermissions);
		setPermissionsFor(userId, permissions);
	}

	/** Throw away an unsaved draft and go back to the server's set. */
	function resetPermissions(userId: number) {
		setPermissionsFor(userId, savedPermissionsFor(userId));
	}

	async function togglePermissionEditor(targetUser: UserAccount) {
		if (permissionEditorUserId === targetUser.id) {
			permissionEditorUserId = null;
			return;
		}
		permissionEditorUserId = targetUser.id;

		// An admin is unrestricted — their stored grants constrain nothing, so
		// there is nothing to fetch and nothing to pick from.
		if (targetUser.is_admin) return;

		if (!userPermissions.has(targetUser.id)) {
			permissionLoadingUserId = targetUser.id;
			try {
				commitPermissionsFor(targetUser.id, await usersApi.getPermissions(targetUser.id));
			} catch (e: any) {
				toast.error(e?.message || 'Load permissions failed');
				permissionEditorUserId = null;
				return;
			} finally {
				permissionLoadingUserId = null;
			}
		}
		// Retry on a previous failure, but do not refetch on every open just
		// because the instance genuinely has zero monitors and zero groups.
		if (!permissionTargetsLoaded && !permissionTargetsLoading) {
			await loadPermissionTargets();
		}
	}

	function addPermissionMonitor(targetUserId: number, monitor: Monitor) {
		const current = permissionsFor(targetUserId);
		setPermissionsFor(targetUserId, {
			...current,
			monitor_ids: [...current.monitor_ids, monitor.id],
		});
	}

	function removePermissionMonitor(targetUserId: number, monitorId: number) {
		const current = permissionsFor(targetUserId);
		setPermissionsFor(targetUserId, {
			...current,
			monitor_ids: current.monitor_ids.filter((id) => id !== monitorId),
		});
	}

	function togglePermissionGroup(targetUserId: number, groupId: number) {
		const current = permissionsFor(targetUserId);
		const hasGroup = current.groups.some((g) => g.group_id === groupId);
		setPermissionsFor(targetUserId, {
			...current,
			groups: hasGroup
				? current.groups.filter((g) => g.group_id !== groupId)
				: // New grants default to DEEP, matching the server's column default and
					// the behavior every group grant had before the toggle existed. An
					// admin ticking a folder means "give them this folder", and the
					// narrow reading of that is the surprising one.
					[...current.groups, { group_id: groupId, include_descendants: true }],
		});
	}

	function togglePermissionGroupDescendants(
		targetUserId: number,
		groupId: number,
		includeDescendants: boolean
	) {
		const current = permissionsFor(targetUserId);
		setPermissionsFor(targetUserId, {
			...current,
			groups: current.groups.map((g) =>
				g.group_id === groupId ? { ...g, include_descendants: includeDescendants } : g
			),
		});
	}

	async function saveCapabilities(targetUser: UserAccount) {
		try {
			// Deliberately NOT `userLoading` — that flag gates the "Create user"
			// button, and flipping a capability has nothing to do with it.
			capabilitySavingUserId = targetUser.id;
			const res = await usersApi.update(targetUser.id, {
				can_manage_notifications: targetUser.can_manage_notifications,
				can_manage_maintenance: targetUser.can_manage_maintenance,
				can_create_monitors: targetUser.can_create_monitors,
				can_create_top_level_monitors: targetUser.can_create_top_level_monitors,
				can_create_groups: targetUser.can_create_groups,
				can_edit_group_metadata: targetUser.can_edit_group_metadata,
			});
			users = users.map((u) => (u.id === targetUser.id ? res.user : u));
			toast.success('Capabilities saved');
		} catch (e: any) {
			toast.error(e?.message || 'Save capabilities failed');
			// The optimistic flip did not stick — resync with the server.
			await loadUsers();
		} finally {
			capabilitySavingUserId = null;
		}
	}

	/**
	 * Flip one capability flag and persist immediately. `users` is deep-reactive
	 * $state, so mutating the member updates the toggle optimistically; a failed
	 * save reloads the list and puts it back.
	 */
	async function toggleCapability(
		targetUser: UserAccount,
		key:
			| 'can_manage_notifications'
			| 'can_manage_maintenance'
			| 'can_create_monitors'
			| 'can_create_top_level_monitors'
			| 'can_create_groups'
			| 'can_edit_group_metadata',
		value: boolean
	) {
		targetUser[key] = value;
		await saveCapabilities(targetUser);
	}

	async function saveUserPermissions(targetUser: UserAccount) {
		permissionSavingUserId = targetUser.id;
		try {
			// PUT replaces the whole set — we always send the complete draft.
			const saved = await usersApi.updatePermissions(targetUser.id, permissionsFor(targetUser.id));
			commitPermissionsFor(targetUser.id, saved);
			toast.success('Permissions saved');
		} catch (e: any) {
			toast.error(e?.message || 'Save permissions failed');
		} finally {
			permissionSavingUserId = null;
		}
	}

	async function createUser() {
		if (!newUsername.trim()) {
			toast.error('Username is required');
			return;
		}
		if (newUserPassword.length < 8) {
			toast.error('Password must be at least 8 characters');
			return;
		}
		try {
			userLoading = true;
			await usersApi.create({
				username: newUsername.trim(),
				password: newUserPassword,
				active: newUserActive,
				is_admin: newUserIsAdmin,
				can_manage_notifications: newUserCanManageNotifications,
				can_manage_maintenance: newUserCanManageMaintenance,
			});
			newUsername = '';
			newUserPassword = '';
			newUserActive = true;
			newUserIsAdmin = false;
			newUserCanManageNotifications = false;
			newUserCanManageMaintenance = false;
			await loadUsers();
			toast.success('User created');
		} catch (e: any) {
			toast.error(e?.message || 'Create user failed');
		} finally {
			userLoading = false;
		}
	}

	async function deleteUser(id: number, username: string) {
		const ok = await confirmAction({
			title: `Delete user "${username}"?`,
			message:
				'They lose access immediately, and every monitor and group granted to them is revoked. Their monitors, notifications and maintenance windows stay — only the account goes.',
			confirmLabel: 'Delete user',
			destructive: true,
		});
		if (!ok) return;
		try {
			await usersApi.remove(id);
			userPermissions.delete(id);
			userPermissions = new Map(userPermissions);
			savedPermissions.delete(id);
			savedPermissions = new Map(savedPermissions);
			if (permissionEditorUserId === id) permissionEditorUserId = null;
			toast.success('User deleted');
			await loadUsers();
		} catch (e: any) {
			toast.error(e?.message || 'Delete failed');
		}
	}

	$effect(() => {
		loadNotifications();
		loadTags();
		loadProxies();
		loadApiKeys();
		loadPasskeys();
		loadUsers();
	});

	// Shared class strings — keep markup DRY and token-consistent.
	const inputClass =
		'w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring';
	const sectionIcon = 'h-5 w-5 text-muted-foreground';
	const ghostBtn =
		'inline-flex items-center gap-2 rounded-lg px-3 py-2 text-sm font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground';
	const primaryBtn =
		'inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60';
</script>

<svelte:head>
	<title>Phoenix · Settings</title>
</svelte:head>

<div class="space-y-6">
	<!-- Page heading -->
	<div>
		<h1 class="text-2xl font-semibold tracking-tight">Settings</h1>
		<p class="mt-1 text-sm text-muted-foreground">Manage your account, security, and integrations.</p>
	</div>

	<!--
		Jump nav. The page is long; scrolling past six sections to revoke an API key
		is not a feature. Scrolls horizontally inside itself on narrow screens so it
		never widens the page.
	-->
	<nav aria-label="Settings sections" class="-mx-1 overflow-x-auto px-1 pb-1">
		<ul class="flex w-max gap-1.5">
			{#each navSections as s (s.id)}
				<li>
					<a
						href="#{s.id}"
						class="inline-flex whitespace-nowrap rounded-full border border-border bg-surface/60 px-3 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
					>{s.label}</a>
				</li>
			{/each}
		</ul>
	</nav>

	<!-- Profile -->
	<section id="profile" class="scroll-mt-20 rounded-xl border border-border bg-card p-4 sm:p-6">
		<div class="flex items-center gap-3">
			<span class="grid h-9 w-9 shrink-0 place-items-center rounded-lg bg-elevated"><User class={sectionIcon} /></span>
			<div class="min-w-0">
				<h2 class="text-sm font-semibold tracking-tight">Profile</h2>
				<p class="text-xs text-muted-foreground">Your account identity.</p>
			</div>
		</div>
		<div class="mt-4 space-y-0 overflow-hidden rounded-lg border border-border">
			<div class="flex items-center justify-between gap-4 border-b border-border px-4 py-3 text-sm last:border-0">
				<span class="shrink-0 text-muted-foreground">Username</span>
				<span class="min-w-0 truncate font-medium">{user?.username || '—'}</span>
			</div>
			<div class="flex items-center justify-between gap-4 border-b border-border px-4 py-3 text-sm last:border-0">
				<span class="shrink-0 text-muted-foreground">User ID</span>
				<span class="font-mono text-xs text-muted-foreground">{user?.id || '—'}</span>
			</div>
			<div class="px-4 py-3 text-xs text-muted-foreground last:border-0">
				Password change available in a future release.
			</div>
		</div>
	</section>

	<!-- Security / 2FA -->
	<section id="security" class="scroll-mt-20 rounded-xl border border-border bg-card p-4 sm:p-6">
		<div class="flex items-center gap-3">
			<span class="grid h-9 w-9 shrink-0 place-items-center rounded-lg bg-elevated"><Shield class={sectionIcon} /></span>
			<div class="min-w-0">
				<h2 class="text-sm font-semibold tracking-tight">Security</h2>
				<p class="text-xs text-muted-foreground">Two-factor authentication and account protection.</p>
			</div>
		</div>

		{#if show2FASetup}
			<div class="mt-6 rounded-lg border border-border bg-surface/60 p-4 sm:p-5">
				<h3 class="text-sm font-semibold">Scan QR Code</h3>
				<p class="mt-1 text-xs text-muted-foreground">Use Google Authenticator, Authy or similar.</p>

				{#if qrUrl}
					<div class="my-4 flex justify-center">
						<img src={qrUrl} alt="2FA QR Code" class="h-40 w-40 max-w-full rounded-lg border border-border bg-white p-2 sm:h-48 sm:w-48" />
					</div>
				{/if}

				<div class="mt-3">
					<label for="secret" class="text-xs text-muted-foreground">Secret (manual entry)</label>
					<div id="secret" class="mt-1 break-all rounded-lg border border-border bg-background p-2 font-mono text-xs">{secret}</div>
				</div>

				<div class="mt-4">
					<label for="otp2fa" class="text-sm font-medium">Enter 6-digit code</label>
					<input
						id="otp2fa"
						type="text"
						bind:value={otpCode}
						maxlength="6"
						class="{inputClass} mt-1 font-mono tracking-widest"
						placeholder="123456"
					/>
				</div>

				<div class="mt-4 flex gap-2">
					<button onclick={enable2FA} disabled={loading} class={primaryBtn}>Enable 2FA</button>
					<button onclick={cancel2FASetup} class={ghostBtn}>Cancel</button>
				</div>
			</div>
		{:else if twoFactorEnabled}
			<div class="mt-4 rounded-lg border border-success/25 bg-success/10 p-4">
				<div class="flex items-center gap-2">
					<span class="dot dot-up"></span>
					<p class="font-medium text-success">Two-factor authentication is enabled</p>
				</div>
				<p class="mt-1 text-xs text-muted-foreground">Your account is protected.</p>
				<div class="mt-3 flex flex-col gap-2 sm:flex-row sm:items-center">
					<input
						type="password"
						bind:value={disablePassword}
						placeholder="Current password"
						class="{inputClass} flex-1 sm:w-48"
					/>
					<button
						onclick={disable2FA}
						disabled={loading || !disablePassword}
						class="inline-flex items-center gap-2 rounded-lg border border-destructive/25 px-3 py-2 text-sm font-medium text-destructive transition-colors hover:bg-destructive/10 disabled:opacity-50"
					>Disable 2FA</button>
				</div>
			</div>
		{:else}
			<div class="mt-4">
				<p class="text-sm text-muted-foreground">Add an extra layer of security with TOTP 2FA.</p>
				<button onclick={start2FASetup} disabled={loading} class="{primaryBtn} mt-3">Enable 2FA</button>
			</div>
		{/if}
	</section>

	<!-- Passkeys (WebAuthn) -->
	<section id="passkeys" class="scroll-mt-20 rounded-xl border border-border bg-card p-4 sm:p-6">
		<div class="flex items-center gap-3">
			<span class="grid h-9 w-9 shrink-0 place-items-center rounded-lg bg-elevated"><Fingerprint class={sectionIcon} /></span>
			<div class="min-w-0">
				<h2 class="text-sm font-semibold tracking-tight">Passkeys</h2>
				<p class="text-xs text-muted-foreground">Sign in without a password using a hardware key, Touch ID, or your phone.</p>
			</div>
		</div>

		{#if !passkeysSupported}
			<p class="mt-4 text-sm text-muted-foreground">This browser does not support passkeys.</p>
		{:else}
			<div class="mt-4 flex flex-col gap-2 sm:flex-row sm:items-end">
				<div class="min-w-0 flex-1">
					<label for="passkey-name" class="text-xs text-muted-foreground">Passkey name</label>
					<input id="passkey-name" type="text" bind:value={newPasskeyName} placeholder="e.g. MacBook Touch ID" class="{inputClass} mt-1" />
				</div>
				<button type="button" onclick={registerPasskey} disabled={passkeyLoading} class="{primaryBtn} w-full shrink-0 justify-center whitespace-nowrap sm:w-auto sm:self-end">
					<Plus class="h-4 w-4" />
					{passkeyLoading ? 'Waiting…' : 'Register a passkey'}
				</button>
			</div>

			{#if passkeysLoading}
				<div class="mt-4 space-y-2" role="status">
					<span class="sr-only">Loading passkeys…</span>
					{#each Array(2) as _}
						<div class="rounded-lg border border-border p-4">
							<Skeleton class="h-4 w-40" />
							<Skeleton class="mt-2 h-3 w-24" />
						</div>
					{/each}
				</div>
			{:else if passkeysError}
				<div class="mt-4 flex flex-col gap-3 rounded-lg border border-danger/25 bg-danger/10 p-4 text-sm text-danger sm:flex-row sm:items-center sm:justify-between">
					<span class="min-w-0 break-words">{passkeysError}</span>
					<button type="button" onclick={loadPasskeys} class="shrink-0 rounded-lg border border-danger/30 px-3 py-1.5 font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">Retry</button>
				</div>
			{:else if passkeys.length === 0}
				<div class="mt-4"><EmptyState icon={Fingerprint} title="No passkeys registered yet." description="Register a passkey to sign in without a password." /></div>
			{:else}
				<div class="mt-4 overflow-hidden rounded-lg border border-border">
					{#each passkeys as pk, i (pk.id)}
						<div
							class="flex items-center justify-between gap-3 px-4 py-3 text-sm transition-colors hover:bg-accent/40 {i !==
							passkeys.length - 1
								? 'border-b border-border'
								: ''}"
						>
							<div class="min-w-0">
								<div class="truncate font-medium">{pk.name || 'Passkey'}</div>
								<div class="text-xs text-muted-foreground">
									added {new Date(pk.created_at).toLocaleDateString()}{#if pk.last_used_at} · last used {new Date(pk.last_used_at).toLocaleDateString()}{/if}
								</div>
							</div>
							<button
								type="button"
								onclick={() => deletePasskey(pk.id, pk.name || 'Passkey')}
								class="grid h-8 w-8 shrink-0 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-danger"
								aria-label="Delete passkey"
							><Trash2 class="h-3.5 w-3.5" /></button>
						</div>
					{/each}
				</div>
			{/if}
		{/if}
	</section>

	<!-- Appearance -->
	<section id="appearance" class="scroll-mt-20 rounded-xl border border-border bg-card p-4 sm:p-6">
		<div class="flex items-center gap-3">
			<span class="grid h-9 w-9 shrink-0 place-items-center rounded-lg bg-elevated"><Palette class={sectionIcon} /></span>
			<div class="min-w-0">
				<h2 class="text-sm font-semibold tracking-tight">Appearance</h2>
				<p class="text-xs text-muted-foreground">Switch between the dark console and light theme.</p>
			</div>
		</div>
		<div class="mt-4 flex flex-wrap items-center justify-between gap-3">
			<div class="min-w-0">
				<p class="text-sm font-medium">Theme</p>
				<p class="text-xs text-muted-foreground">Current: {themeStore.theme}</p>
			</div>
			<button onclick={themeStore.toggle} class="{ghostBtn} shrink-0 border border-border">
				{#if themeStore.theme === 'dark'}
					<Sun class="h-4 w-4" />
					Toggle Light
				{:else}
					<Moon class="h-4 w-4" />
					Toggle Dark
				{/if}
			</button>
		</div>
	</section>

	<!-- Tags -->
	<section id="tags" class="scroll-mt-20 rounded-xl border border-border bg-card p-4 sm:p-6">
		<div class="flex items-center gap-3">
			<span class="grid h-9 w-9 shrink-0 place-items-center rounded-lg bg-elevated"><Tag class={sectionIcon} /></span>
			<div class="min-w-0">
				<h2 class="text-sm font-semibold tracking-tight">Tags</h2>
				<p class="text-xs text-muted-foreground">Organize monitors with colored tags.</p>
			</div>
		</div>

		<!--
			Name + swatch share the first row (the swatch is a fixed 56px, so it never
			squeezes the name field below usable width); the buttons wrap to their own
			row on narrow screens rather than pushing the row wider than the viewport.
		-->
		<div class="mt-4 flex flex-col gap-3 sm:flex-row sm:items-end">
			<div class="flex min-w-0 flex-1 items-end gap-2">
				<div class="min-w-0 flex-1">
					<label for="tag-name" class="text-xs text-muted-foreground">Name</label>
					<input id="tag-name" type="text" bind:value={tagName} class="{inputClass} mt-1" />
				</div>
				<div class="shrink-0">
					<label for="tag-color" class="text-xs text-muted-foreground">Color</label>
					<ColorPicker
						id="tag-color"
						bind:value={tagColor}
						class="mt-1"
					/>
				</div>
			</div>
			<div class="flex shrink-0 gap-2">
				<button type="button" onclick={saveTag} class="{primaryBtn} flex-1 justify-center whitespace-nowrap sm:flex-none">
					<Plus class="h-4 w-4" />
					{editingTagId != null ? 'Update' : 'Add'} tag
				</button>
				{#if editingTagId != null}
					<button type="button" onclick={cancelEditTag} class="{ghostBtn} shrink-0 border border-border">Cancel</button>
				{/if}
			</div>
		</div>

		{#if tagsLoading}
			<div class="mt-4 space-y-2" role="status"><span class="sr-only">Loading tags…</span><Skeleton class="h-12 w-full" /><Skeleton class="h-12 w-full" /></div>
		{:else if tagsError}
			<div class="mt-4 flex flex-col gap-3 rounded-lg border border-danger/25 bg-danger/10 p-4 text-sm text-danger sm:flex-row sm:items-center sm:justify-between"><span class="min-w-0 break-words">{tagsError}</span><button type="button" onclick={loadTags} class="shrink-0 rounded-lg border border-danger/30 px-3 py-1.5 font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">Retry</button></div>
		{:else if tags.length === 0}
			<p class="mt-4 text-sm text-muted-foreground">No tags yet.</p>
		{:else}
			<div class="mt-4 overflow-hidden rounded-lg border border-border">
				{#each tags as t, i (t.id)}
					<div
						class="flex items-center justify-between gap-3 px-4 py-3 text-sm transition-colors hover:bg-accent/40 {i !==
						tags.length - 1
							? 'border-b border-border'
							: ''}"
					>
						<div class="flex min-w-0 items-center gap-3">
							<span class="h-4 w-4 shrink-0 rounded-full border border-border" style="background-color: {t.color}"></span>
							<span class="truncate font-medium">{t.name}</span>
							<!-- The swatch already says this; drop the hex before it squeezes the name. -->
							<span class="hidden shrink-0 font-mono text-xs text-muted-foreground sm:inline">{t.color}</span>
						</div>
						<div class="flex shrink-0 gap-1">
							<button
								type="button"
								onclick={() => startEditTag(t)}
								class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
								aria-label="Edit tag"
							><Edit2 class="h-3.5 w-3.5" /></button>
							<button
								type="button"
								onclick={() => deleteTag(t.id, t.name)}
								class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-danger"
								aria-label="Delete tag"
							><Trash2 class="h-3.5 w-3.5" /></button>
						</div>
					</div>
				{/each}
			</div>
		{/if}
	</section>

	<!-- Proxies -->
	<section id="proxies" class="scroll-mt-20 rounded-xl border border-border bg-card p-4 sm:p-6" data-testid="proxies-section">
		<div class="flex items-center gap-3">
			<span class="grid h-9 w-9 shrink-0 place-items-center rounded-lg bg-elevated"><Route class={sectionIcon} /></span>
			<div class="min-w-0">
				<h2 class="text-sm font-semibold tracking-tight">Proxies</h2>
				<p class="text-xs text-muted-foreground">Route monitor checks through an outbound HTTP/HTTPS/SOCKS5 proxy.</p>
			</div>
		</div>

		<div class="mt-4 grid grid-cols-1 gap-3 sm:grid-cols-2">
			<div class="min-w-0">
				<label for="proxy-protocol" class="text-xs text-muted-foreground">Protocol</label>
				<div class="mt-1">
					<Select
						id="proxy-protocol"
						options={[
							{ value: 'http', label: 'http' },
							{ value: 'https', label: 'https' },
							{ value: 'socks5', label: 'socks5' },
						]}
						value={proxyProtocol}
						onValueChange={(v) => (proxyProtocol = v as ProxyProtocol)}
						class="w-full"
					/>
				</div>
			</div>
			<div class="min-w-0">
				<label for="proxy-host" class="text-xs text-muted-foreground">Host</label>
				<input id="proxy-host" type="text" bind:value={proxyHost} placeholder="proxy.example.com" class="{inputClass} mt-1" />
			</div>
			<div class="min-w-0">
				<label for="proxy-port" class="text-xs text-muted-foreground">Port</label>
				<input id="proxy-port" type="number" bind:value={proxyPort} min="1" max="65535" class="{inputClass} mt-1" />
			</div>
			<!-- Three checkboxes do not fit one 360px row: wrap instead of overflowing. -->
			<div class="flex flex-wrap items-center gap-x-4 gap-y-2 sm:items-end sm:pb-2.5">
				<label class="flex items-center gap-1.5 whitespace-nowrap text-sm text-muted-foreground">
					<input type="checkbox" class="h-4 w-4 rounded border-border accent-primary" bind:checked={proxyAuthEnabled} />
					Requires auth
				</label>
				<label class="flex items-center gap-1.5 whitespace-nowrap text-sm text-muted-foreground">
					<input type="checkbox" class="h-4 w-4 rounded border-border accent-primary" bind:checked={proxyActive} />
					Active
				</label>
				<label class="flex items-center gap-1.5 whitespace-nowrap text-sm text-muted-foreground">
					<input type="checkbox" class="h-4 w-4 rounded border-border accent-primary" bind:checked={proxyIsDefault} />
					Default
				</label>
			</div>
			{#if proxyAuthEnabled}
				<div class="min-w-0">
					<label for="proxy-username" class="text-xs text-muted-foreground">Username</label>
					<input id="proxy-username" type="text" bind:value={proxyUsername} class="{inputClass} mt-1" />
				</div>
				<div class="min-w-0">
					<label for="proxy-password" class="text-xs text-muted-foreground">
						Password{editingProxyId != null ? ' (leave blank to keep existing)' : ''}
					</label>
					<input id="proxy-password" type="password" bind:value={proxyPassword} class="{inputClass} mt-1" />
				</div>
			{/if}
		</div>

		<div class="mt-4 flex flex-wrap gap-2">
			<button type="button" data-testid="proxy-save-btn" onclick={saveProxy} class="{primaryBtn} flex-1 justify-center whitespace-nowrap sm:flex-none">
				<Plus class="h-4 w-4" />
				{editingProxyId != null ? 'Update' : 'Add'} proxy
			</button>
			{#if editingProxyId != null}
				<button type="button" onclick={resetProxyForm} class="{ghostBtn} shrink-0 border border-border">Cancel</button>
			{/if}
		</div>

		{#if proxiesLoading}
			<div class="mt-4 space-y-2" role="status"><span class="sr-only">Loading proxies…</span><Skeleton class="h-14 w-full" /><Skeleton class="h-14 w-full" /></div>
		{:else if proxiesError}
			<div class="mt-4 flex flex-col gap-3 rounded-lg border border-danger/25 bg-danger/10 p-4 text-sm text-danger sm:flex-row sm:items-center sm:justify-between"><span class="min-w-0 break-words">{proxiesError}</span><button type="button" onclick={loadProxies} class="shrink-0 rounded-lg border border-danger/30 px-3 py-1.5 font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">Retry</button></div>
		{:else if proxies.length === 0}
			<p class="mt-4 text-sm text-muted-foreground" data-testid="proxies-empty">No proxies configured yet.</p>
		{:else}
			<div class="mt-4 overflow-hidden rounded-lg border border-border" data-testid="proxies-list">
				{#each proxies as p, i (p.id)}
					<div
						data-testid="proxy-row"
						class="flex items-center justify-between gap-3 px-4 py-3 text-sm transition-colors hover:bg-accent/40 {i !==
						proxies.length - 1
							? 'border-b border-border'
							: ''}"
					>
						<div class="min-w-0">
							<div class="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
								<span class="min-w-0 truncate font-mono text-xs font-medium">{p.protocol}://{p.host}:{p.port}</span>
								{#if p.is_default}
									<span class="inline-flex shrink-0 items-center gap-1 rounded-full border border-primary/25 bg-primary/10 px-2 py-0.5 text-[11px] font-medium text-primary">default</span>
								{/if}
								{#if !p.active}
									<span class="inline-flex shrink-0 items-center gap-1 rounded-full border border-border bg-muted/40 px-2 py-0.5 text-[11px] font-medium text-muted-foreground">inactive</span>
								{/if}
							</div>
							{#if p.auth}
								<div class="truncate text-xs text-muted-foreground">auth as {p.username || '—'}</div>
							{/if}
						</div>
						<div class="flex shrink-0 gap-1">
							<button
								type="button"
								onclick={() => startEditProxy(p)}
								class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
								aria-label="Edit proxy"
							><Edit2 class="h-3.5 w-3.5" /></button>
							<button
								type="button"
								onclick={() => deleteProxy(p.id, `${p.protocol}://${p.host}:${p.port}`)}
								class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-danger"
								aria-label="Delete proxy"
							><Trash2 class="h-3.5 w-3.5" /></button>
						</div>
					</div>
				{/each}
			</div>
		{/if}
	</section>

	<!-- Notifications -->
	<section id="notifications" class="scroll-mt-20 rounded-xl border border-border bg-card p-4 sm:p-6">
		<div class="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
			<div class="flex items-center gap-3">
				<span class="grid h-9 w-9 shrink-0 place-items-center rounded-lg bg-elevated"><Bell class={sectionIcon} /></span>
				<div class="min-w-0">
					<h2 class="text-sm font-semibold tracking-tight">Notifications</h2>
					<p class="text-xs text-muted-foreground">Alert providers configured for this instance.</p>
				</div>
			</div>
			<button onclick={openCreateNotification} class="{primaryBtn} shrink-0 self-start px-3 py-1.5 sm:self-auto">
				<Plus class="h-4 w-4" /> Add
			</button>
		</div>

		{#if notificationsLoading}
			<div class="mt-4 space-y-2" role="status"><span class="sr-only">Loading notifications…</span><Skeleton class="h-14 w-full" /><Skeleton class="h-14 w-full" /></div>
		{:else if notificationsError}
			<div class="mt-4 flex flex-col gap-3 rounded-lg border border-danger/25 bg-danger/10 p-4 text-sm text-danger sm:flex-row sm:items-center sm:justify-between"><span class="min-w-0 break-words">{notificationsError}</span><button type="button" onclick={loadNotifications} class="shrink-0 rounded-lg border border-danger/30 px-3 py-1.5 font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">Retry</button></div>
		{:else if notifications.length === 0}
			<p class="mt-4 text-sm text-muted-foreground">No notification providers configured yet. Add one to receive alerts on monitor failures.</p>
		{:else}
			<div class="mt-4 overflow-hidden rounded-lg border border-border">
				{#each notifications as n, i (n.id)}
					<div
						class="flex flex-col gap-2 px-4 py-3 text-sm transition-colors hover:bg-accent/40 sm:flex-row sm:items-center sm:justify-between sm:gap-3 {i !==
						notifications.length - 1
							? 'border-b border-border'
							: ''}"
					>
						<div class="flex min-w-0 items-center gap-3">
							<div class="min-w-0">
								<div class="truncate font-medium">{n.name}</div>
								<div class="text-xs text-muted-foreground">{n.type}</div>
							</div>
							{#if !n.active}
								<span
									class="inline-flex shrink-0 items-center gap-1.5 rounded-full border border-border bg-muted/40 px-2.5 py-0.5 text-xs font-medium text-muted-foreground"
								>
									<span class="dot dot-muted"></span>disabled
								</span>
							{/if}
						</div>
						<div class="flex shrink-0 items-center gap-1">
							<button
								onclick={() => handleTestNotification(n.id, n.name)}
								class="inline-flex items-center gap-1 rounded-lg border border-border px-2 py-1 text-xs text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground"
								title="Send test alert"
							>
								<Send class="h-3.5 w-3.5" /> Test
							</button>
							<button
								onclick={() => openEditNotification(n)}
								class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
								aria-label="Edit notification"
							><Edit2 class="h-3.5 w-3.5" /></button>
							<button
								onclick={() => handleDeleteNotification(n.id, n.name)}
								class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-danger"
								aria-label="Delete notification"
							><Trash2 class="h-3.5 w-3.5" /></button>
						</div>
					</div>
				{/each}
			</div>
		{/if}
	</section>

	<!-- Users (admin only) -->
	{#if isAdmin}
		<section id="users" class="scroll-mt-20 rounded-xl border border-border bg-card p-4 sm:p-6" data-testid="users-section">
			<div class="flex items-center gap-3">
				<span class="grid h-9 w-9 shrink-0 place-items-center rounded-lg bg-elevated"><UserCog class={sectionIcon} /></span>
				<div class="min-w-0">
					<h2 class="text-sm font-semibold tracking-tight">Users</h2>
					<p class="text-xs text-muted-foreground">Accounts that can sign in to this instance.</p>
				</div>
			</div>

			<!--
				Create-user form. The credentials and the four flags are separate rows:
				cramming two inputs, four checkboxes and a button onto one line overflowed
				every phone-width viewport. The flags wrap into two columns from `sm` up.
			-->
			<div class="mt-4 rounded-lg border border-border bg-surface/40 p-3 sm:p-4">
				<div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
					<div class="min-w-0">
						<label for="new-user-username" class="text-xs text-muted-foreground">Username</label>
						<input id="new-user-username" type="text" bind:value={newUsername} placeholder="newuser" class="{inputClass} mt-1" />
					</div>
					<div class="min-w-0">
						<label for="new-user-password" class="text-xs text-muted-foreground">Password (min 8 chars)</label>
						<input
							id="new-user-password"
							type="password"
							bind:value={newUserPassword}
							placeholder="••••••••"
							class="{inputClass} mt-1"
							onkeydown={(e) => e.key === 'Enter' && createUser()}
						/>
					</div>
				</div>

				<div class="mt-3 grid grid-cols-1 gap-2 sm:grid-cols-2">
					<label class="flex items-center gap-2 text-sm text-muted-foreground">
						<input type="checkbox" class="h-4 w-4 shrink-0 rounded border-border accent-primary" bind:checked={newUserActive} />
						Active
					</label>
					<label class="flex items-center gap-2 text-sm text-muted-foreground">
						<input type="checkbox" class="h-4 w-4 shrink-0 rounded border-border accent-primary" bind:checked={newUserIsAdmin} />
						Admin <span class="text-xs text-faint">(sees everything)</span>
					</label>
					<label class="flex items-center gap-2 text-sm text-muted-foreground" title="Allow this user to create and manage notification providers">
						<input type="checkbox" class="h-4 w-4 shrink-0 rounded border-border accent-primary" bind:checked={newUserCanManageNotifications} />
						Manage notifications
					</label>
					<label class="flex items-center gap-2 text-sm text-muted-foreground" title="Allow this user to create and manage maintenance windows">
						<input type="checkbox" class="h-4 w-4 shrink-0 rounded border-border accent-primary" bind:checked={newUserCanManageMaintenance} />
						Manage maintenance
					</label>
				</div>

				<button type="button" onclick={createUser} disabled={userLoading} class="{primaryBtn} mt-3 w-full justify-center sm:w-auto">
					<Plus class="h-4 w-4" /> Create user
				</button>
				<p class="mt-2 text-xs text-muted-foreground">
					A new non-admin sees <strong class="font-medium text-foreground">no monitors at all</strong> until you grant some below.
				</p>
			</div>

			{#if usersLoading}
				<div class="mt-4 space-y-2" role="status">
					<span class="sr-only">Loading users…</span>
					{#each Array(3) as _}
						<div class="rounded-lg border border-border p-4">
							<Skeleton class="h-4 w-36" />
							<Skeleton class="mt-2 h-3 w-28" />
						</div>
					{/each}
				</div>
			{:else if usersError}
				<div class="mt-4 flex flex-col gap-3 rounded-lg border border-danger/25 bg-danger/10 p-4 text-sm text-danger sm:flex-row sm:items-center sm:justify-between"><span class="min-w-0 break-words">{usersError}</span><button type="button" onclick={loadUsers} class="shrink-0 rounded-lg border border-danger/30 px-3 py-1.5 font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">Retry</button></div>
			{:else if users.length === 0}
				<div class="mt-4" data-testid="users-empty"><EmptyState icon={UserCog} title="No users yet." description="Create an account to grant access to this instance." /></div>
			{:else}
				<div class="mt-4 overflow-hidden rounded-lg border border-border" data-testid="users-list">
					{#each users as u, i (u.id)}
						{@const editorOpen = permissionEditorUserId === u.id}
						<div
							data-testid="user-row"
							class="text-sm {i !== users.length - 1 ? 'border-b border-border' : ''}"
						>
							<div class="flex flex-col gap-2 px-4 py-3 transition-colors hover:bg-accent/40 sm:flex-row sm:items-center sm:justify-between sm:gap-3">
								<div class="min-w-0">
									<div class="flex flex-wrap items-center gap-2">
										<span class="truncate font-medium">{u.username}</span>
										{#if u.active}
											<span class="inline-flex items-center gap-1 rounded-full border border-success/25 bg-success/10 px-2 py-0.5 text-[11px] font-medium text-success">active</span>
										{:else}
											<span class="inline-flex items-center gap-1 rounded-full border border-border bg-muted/40 px-2 py-0.5 text-[11px] font-medium text-muted-foreground">disabled</span>
										{/if}
										{#if u.is_admin}
											<span class="inline-flex items-center gap-1 rounded-full border border-primary/25 bg-primary/10 px-2 py-0.5 text-[11px] font-medium text-primary">admin</span>
										{/if}
									</div>
									<div class="text-xs text-muted-foreground">created {new Date(u.created_at).toLocaleDateString()}</div>
								</div>
								<div class="flex shrink-0 items-center gap-1 self-start sm:self-auto">
									<button
										type="button"
										data-testid="user-access-btn"
										onclick={() => togglePermissionEditor(u)}
										aria-expanded={editorOpen}
										aria-controls="user-access-{u.id}"
										class="inline-flex items-center gap-1.5 rounded-lg border px-2.5 py-1 text-xs font-medium transition-colors {editorOpen
											? 'border-primary/40 bg-primary/10 text-primary'
											: 'border-border text-muted-foreground hover:bg-accent hover:text-accent-foreground'}"
									>
										<KeyRound class="h-3.5 w-3.5" />
										Access
									</button>
									{#if user?.id !== u.id}
										<button
											type="button"
											onclick={() => deleteUser(u.id, u.username)}
											class="grid h-8 w-8 shrink-0 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-danger"
											aria-label="Delete user"
										><Trash2 class="h-3.5 w-3.5" /></button>
									{/if}
								</div>
							</div>

							{#if editorOpen}
								<div id="user-access-{u.id}">
									<UserPermissionEditor
										user={u}
										draft={permissionsFor(u.id)}
										saved={savedPermissionsFor(u.id)}
										monitors={permissionMonitors}
										groups={permissionGroups}
										loading={permissionLoadingUserId === u.id}
										saving={permissionSavingUserId === u.id}
										capabilitySaving={capabilitySavingUserId === u.id}
										targetsLoading={permissionTargetsLoading}
										targetsError={permissionTargetsError}
										onAddMonitor={(m) => addPermissionMonitor(u.id, m)}
										onRemoveMonitor={(id) => removePermissionMonitor(u.id, id)}
										onToggleGroup={(id) => togglePermissionGroup(u.id, id)}
										onToggleGroupDescendants={(id, deep) =>
											togglePermissionGroupDescendants(u.id, id, deep)}
										onToggleCapability={(key, value) => toggleCapability(u, key, value)}
										onSave={() => saveUserPermissions(u)}
										onReset={() => resetPermissions(u.id)}
										onRetryTargets={loadPermissionTargets}
									/>
								</div>
							{/if}
						</div>
					{/each}
				</div>
			{/if}
		</section>
	{/if}

	<!-- API Keys -->
	<section id="api-keys" class="scroll-mt-20 rounded-xl border border-border bg-card p-4 sm:p-6" data-testid="api-keys-section">
		<div class="flex items-center gap-3">
			<span class="grid h-9 w-9 shrink-0 place-items-center rounded-lg bg-elevated"><Key class={sectionIcon} /></span>
			<div class="min-w-0">
				<h2 class="text-sm font-semibold tracking-tight">API Keys</h2>
				<p class="text-xs text-muted-foreground">Programmatic access for metrics and automation.</p>
			</div>
		</div>
		<p class="mt-2 text-xs text-muted-foreground">The secret is shown only once when you create a key.</p>

		{#if createdKeyPlain}
			<div class="mt-4 rounded-lg border border-warning/25 bg-warning/10 p-4">
				<div class="flex items-center gap-2">
					<span class="dot dot-warn"></span>
					<p class="text-sm font-medium text-warning">Copy your new API key now</p>
				</div>
				<p class="mt-1 text-xs text-muted-foreground">You will not be able to see it again.</p>
				<div class="mt-3 flex flex-col gap-2 sm:flex-row">
					<code class="min-w-0 flex-1 break-all rounded-lg border border-border bg-background p-2 text-xs">{createdKeyPlain}</code>
					<button type="button" onclick={copyKey} class="{ghostBtn} shrink-0 justify-center">
						<Copy class="h-4 w-4" /> Copy
					</button>
				</div>
				<button
					type="button"
					class="mt-3 text-xs text-muted-foreground underline-offset-2 transition-colors hover:text-foreground hover:underline"
					onclick={() => (createdKeyPlain = null)}
				>
					Dismiss
				</button>
			</div>
		{/if}

		<!--
			Name + expiry side by side from `sm` up, scopes on their own row. Packing
			all four controls into one flex row pushed the button off-screen at 360px.
		-->
		<div class="mt-4 rounded-lg border border-border bg-surface/40 p-3 sm:p-4">
			<div class="grid grid-cols-1 gap-3 sm:grid-cols-2">
				<div class="min-w-0">
					<label for="key-name" class="text-xs text-muted-foreground">Name</label>
					<input id="key-name" type="text" bind:value={newKeyName} placeholder="CI metrics" class="{inputClass} mt-1" />
				</div>
				<div class="min-w-0">
					<label for="key-expires" class="text-xs text-muted-foreground">Expires (optional)</label>
					<DateTimePicker id="key-expires" bind:value={newKeyExpiresAt} placeholder="No expiry" class="mt-1" />
				</div>
			</div>

			<div class="mt-3">
				<span class="text-xs text-muted-foreground">Scopes</span>
				<div class="mt-1 flex flex-wrap gap-x-4 gap-y-2">
					{#each ['read', 'write', 'metrics'] as scope}
						<label class="flex items-center gap-2 text-sm text-muted-foreground">
							<input
								type="checkbox"
								class="h-4 w-4 shrink-0 rounded border-border accent-primary"
								checked={newKeyScopes.has(scope as ApiKeyScope)}
								onchange={() => toggleScope(scope as ApiKeyScope)}
							/>
							{scope}
						</label>
					{/each}
				</div>
			</div>

			<button type="button" data-testid="api-key-create-btn" onclick={createApiKey} class="{primaryBtn} mt-3 w-full justify-center sm:w-auto">Create key</button>
		</div>

		{#if apiKeysLoading}
			<div class="mt-4 space-y-2" role="status">
				<span class="sr-only">Loading API keys…</span>
				{#each Array(3) as _}
					<div class="rounded-lg border border-border p-4">
						<Skeleton class="h-4 w-40" />
						<Skeleton class="mt-2 h-3 w-52 max-w-full" />
					</div>
				{/each}
			</div>
		{:else if apiKeysError}
			<div class="mt-4 flex flex-col gap-3 rounded-lg border border-danger/25 bg-danger/10 p-4 text-sm text-danger sm:flex-row sm:items-center sm:justify-between"><span class="min-w-0 break-words">{apiKeysError}</span><button type="button" onclick={loadApiKeys} class="shrink-0 rounded-lg border border-danger/30 px-3 py-1.5 font-medium focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">Retry</button></div>
		{:else if apiKeys.length === 0}
			<div class="mt-4" data-testid="api-keys-empty"><EmptyState icon={Key} title="No API keys yet." description="Create a scoped key for automation or metrics access." /></div>
		{:else}
			<div class="mt-4 overflow-hidden rounded-lg border border-border" data-testid="api-keys-list">
				{#each apiKeys as k, i (k.id)}
					<div
						data-testid="api-key-row"
						data-key-name={k.name}
						class="flex flex-col gap-2 px-4 py-3 text-sm transition-colors hover:bg-accent/40 sm:flex-row sm:items-center sm:justify-between sm:gap-3 {i !==
						apiKeys.length - 1
							? 'border-b border-border'
							: ''}"
					>
						<div class="min-w-0">
							<div class="flex items-center gap-2">
								<span class="truncate font-medium">{k.name}</span>
								{#if !k.active}
									<span class="inline-flex shrink-0 items-center gap-1 rounded-full border border-danger/25 bg-danger/10 px-2 py-0.5 text-[11px] font-medium text-danger">revoked</span>
								{/if}
							</div>
							<div class="text-xs text-muted-foreground">
								{k.scopes?.join(', ') || 'read'} · created {new Date(k.created_at).toLocaleDateString()}
								{#if k.expires_at}
									· expires {new Date(k.expires_at).toLocaleString()}
								{/if}
							</div>
						</div>
						{#if k.active}
							<button
								type="button"
								onclick={() => revokeApiKey(k.id, k.name)}
								class="shrink-0 self-start rounded-lg border border-destructive/25 px-2 py-1 text-xs font-medium text-destructive transition-colors hover:bg-destructive/10 sm:self-auto"
							>Revoke</button>
						{/if}
					</div>
				{/each}
			</div>
		{/if}
	</section>
</div>

{#if showNotificationForm}
	<NotificationForm
		notification={editingNotification}
		onSaved={async () => { showNotificationForm = false; await loadNotifications(); }}
		onClose={() => (showNotificationForm = false)}
	/>
{/if}
