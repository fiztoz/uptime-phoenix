<script lang="ts">
	import { realtime } from '$lib/stores/ws.svelte.js';
	import { auth } from '$lib/stores/auth.svelte.ts';
	import { themeStore } from '$lib/stores/theme.svelte.ts';
	import { page } from '$app/stores';
	import { goto } from '$app/navigation';
	import { onMount } from 'svelte';
	import type { Snippet } from 'svelte';
	import { updateFaviconBadge } from '$lib/utils/favicon-badge.js';
	import { healthApi } from '$lib/api/health';
	import BrandMark from '$lib/components/BrandMark.svelte';
	import Wordmark from '$lib/components/Wordmark.svelte';
	import * as m from '$lib/paraglide/messages.js';

	let { children }: { children: Snippet } = $props();
	import {
		LayoutDashboard,
		Activity,
		Settings,
		AlertTriangle,
		BellRing,
		ListOrdered,
		Globe,
		CalendarClock,
		Bell,
		ChevronLeft,
		ChevronRight,
		Sun,
		Moon,
		Menu,
		X,
		LogOut,
		Archive,
	} from '@lucide/svelte';

	let sidebarCollapsed = $state(false);
	let mobileOpen = $state(false);
	let authChecked = $state(false);
	// Server build version for the sidebar footer. Fetched once; stays null
	// (and the footer line stays hidden) when the backend omits `version` or
	// the fetch fails.
	let serverVersion = $state<string | null>(null);

	onMount(async () => {
		await auth.loadUser();
		authChecked = true;
		if (!auth.isAuthenticated) {
			goto('/login', { replaceState: true });
			return;
		}
		realtime.connect();
		healthApi
			.live()
			.then((live) => {
				serverVersion = live.version?.trim() || null;
			})
			.catch(() => {
				serverVersion = null;
			});
	});

	$effect(() => {
		if (authChecked && !auth.isAuthenticated) {
			goto('/login', { replaceState: true });
		}
	});

	// Update favicon badge when monitors change.
	$effect(() => {
		const monitors = realtime.monitors;
		if (typeof document !== 'undefined' && monitors.length > 0) {
			updateFaviconBadge(monitors);
		}
	});

	const allNavItems = [
		{ href: '/dashboard', label: () => m.nav_dashboard(), icon: LayoutDashboard },
		{ href: '/monitors', label: () => m.nav_monitors(), icon: Activity },
		{ href: '/alerts', label: () => m.nav_alerts(), icon: BellRing },
		{ href: '/notifications', label: () => m.nav_notifications(), icon: Bell, gate: 'notifications' as const },
		{ href: '/escalation-policies', label: () => m.nav_escalation(), icon: ListOrdered, gate: 'notifications' as const },
		{ href: '/maintenance', label: () => m.nav_maintenance(), icon: CalendarClock, gate: 'maintenance' as const },
		{ href: '/status-pages', label: () => m.nav_status_pages(), icon: Globe, gate: 'admin' as const },
		{ href: '/incidents', label: () => m.nav_incidents(), icon: AlertTriangle },
		{ href: '/backup', label: () => m.nav_backup(), icon: Archive, gate: 'admin' as const },
		{ href: '/settings', label: () => m.nav_settings(), icon: Settings },
	];

	let isUserAdmin = $derived(auth.user?.is_admin ?? false);
	let canManageNotifications = $derived(isUserAdmin || (auth.user?.can_manage_notifications ?? false));
	let canManageMaintenance = $derived(isUserAdmin || (auth.user?.can_manage_maintenance ?? false));

	const navItems = $derived(
		allNavItems.filter((item) => {
			if (!item.gate) return true;
			if (item.gate === 'admin') return isUserAdmin;
			if (item.gate === 'notifications') return canManageNotifications;
			if (item.gate === 'maintenance') return canManageMaintenance;
			return true;
		})
	);

	// Active nav + current page title.
	let activeItem = $derived(
		navItems.find((i) => $page.url.pathname.startsWith(i.href)) ?? navItems[0]
	);

	// Global health summary for the top bar.
	let monitors = $derived(realtime.monitors);
	let downCount = $derived(monitors.filter((mo) => mo.status === 'down').length);
	let overall = $derived(
		monitors.length === 0 ? 'empty' : downCount > 0 ? 'down' : 'up'
	);

	let initials = $derived((auth.user?.username ?? 'PH').slice(0, 2).toUpperCase());
</script>

{#if !authChecked}
	<div class="flex h-dvh items-center justify-center bg-background">
		<div class="flex items-center gap-3 text-sm text-muted-foreground">
			<BrandMark size={32} />
			<span class="animate-pulse-dot">{m.loading()}</span>
		</div>
	</div>
{:else}
	<div class="app-glow relative flex h-dvh overflow-hidden">
		<!-- Mobile overlay -->
		{#if mobileOpen}
			<button
				type="button"
				class="fixed inset-0 z-30 bg-black/60 backdrop-blur-sm md:hidden"
				onclick={() => (mobileOpen = false)}
				aria-label={m.btn_close()}
			></button>
		{/if}

		<!-- Mobile hamburger -->
		<button
			onclick={() => (mobileOpen = !mobileOpen)}
			class="fixed top-3 left-3 z-50 rounded-lg border border-border bg-surface/80 p-2 text-foreground shadow-lg backdrop-blur md:hidden"
			aria-label={m.layout_toggle_menu()}
		>
			{#if mobileOpen}<X class="h-5 w-5" />{:else}<Menu class="h-5 w-5" />{/if}
		</button>

		<!-- Sidebar -->
		<aside
			class="z-10 flex flex-col border-r border-border bg-sidebar transition-all duration-200
			fixed inset-y-0 left-0 z-40 md:static md:z-10
			{mobileOpen ? 'translate-x-0' : '-translate-x-full md:translate-x-0'}
			{sidebarCollapsed ? 'w-[4.5rem]' : 'w-64'}"
		>
			<!-- Brand lockup -->
			<div class="flex h-16 items-center gap-3 border-b border-border px-4">
				<div
					class="grid h-11 w-11 shrink-0 place-items-center rounded-xl border border-border bg-elevated shadow-lg shadow-primary/20"
				>
					<BrandMark size={28} />
				</div>
				{#if !sidebarCollapsed}
					<div class="min-w-0 flex-1 leading-tight">
						<Wordmark size={30} class="-ml-0.5 block" />
						<div class="truncate text-[11px] text-muted-foreground">{m.layout_tagline()}</div>
					</div>
				{/if}
			</div>

			<!-- Nav -->
			<nav class="flex-1 space-y-1 overflow-y-auto p-3">
				{#if !sidebarCollapsed}
					<div class="eyebrow px-2 pt-1 pb-2">{m.layout_overview()}</div>
				{/if}
				{#each navItems as item}
					{@const active = $page.url.pathname.startsWith(item.href)}
					<a
						href={item.href}
						onclick={() => (mobileOpen = false)}
						title={sidebarCollapsed ? item.label() : undefined}
						class="group relative flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-colors
						{active
							? 'bg-primary/10 text-primary'
							: 'text-muted-foreground hover:bg-accent hover:text-accent-foreground'}
						{sidebarCollapsed ? 'justify-center' : ''}"
					>
						{#if active}
							<span
								class="absolute left-0 top-1/2 h-5 w-[3px] -translate-y-1/2 rounded-r-full bg-primary"
							></span>
						{/if}
						<item.icon class="h-[1.1rem] w-[1.1rem] shrink-0" />
						{#if !sidebarCollapsed}<span class="truncate">{item.label()}</span>{/if}
					</a>
				{/each}
			</nav>

			<!-- Footer controls -->
			<div class="space-y-1 border-t border-border p-3">
				{#if !sidebarCollapsed}
					<div
						class="mb-2 flex items-center gap-2 rounded-lg border border-border bg-surface/60 px-3 py-2 text-xs"
					>
						<span
							class="dot {overall === 'up' ? 'dot-up' : overall === 'down' ? 'dot-down' : 'dot-muted'} {realtime.isConnected &&
							overall === 'up'
								? 'animate-pulse-dot'
								: ''}"
						></span>
						<span class="truncate text-muted-foreground">
							{#if overall === 'empty'}{m.layout_no_monitors_yet()}{:else if overall === 'down'}{m.layout_down_count({ count: downCount })}{:else}{m.layout_all_operational()}{/if}
						</span>
					</div>
				{/if}

				<button
					onclick={themeStore.toggle}
					class="flex w-full items-center gap-3 rounded-lg px-3 py-2 text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground {sidebarCollapsed
						? 'justify-center'
						: ''}"
				>
					{#if themeStore.theme === 'dark'}
						<Sun class="h-[1.1rem] w-[1.1rem]" />{#if !sidebarCollapsed}<span>{m.nav_light_mode()}</span>{/if}
					{:else}
						<Moon class="h-[1.1rem] w-[1.1rem]" />{#if !sidebarCollapsed}<span>{m.nav_dark_mode()}</span>{/if}
					{/if}
				</button>

				<button
					onclick={() => (sidebarCollapsed = !sidebarCollapsed)}
					class="hidden w-full items-center gap-3 rounded-lg px-3 py-2 text-sm text-muted-foreground transition-colors hover:bg-accent hover:text-accent-foreground md:flex {sidebarCollapsed
						? 'justify-center'
						: ''}"
				>
					{#if sidebarCollapsed}
						<ChevronRight class="h-[1.1rem] w-[1.1rem]" />
					{:else}
						<ChevronLeft class="h-[1.1rem] w-[1.1rem]" /><span>{m.nav_collapse()}</span>
					{/if}
				</button>

				{#if !sidebarCollapsed && serverVersion}
					<div class="truncate px-3 pt-1 text-[11px] text-faint">Phoenix v{serverVersion}</div>
				{/if}
			</div>
		</aside>

		<!-- Main column -->
		<div class="relative z-10 flex min-w-0 flex-1 flex-col">
			<!-- Top bar -->
			<header
				class="sticky top-0 z-20 flex h-16 items-center gap-4 border-b border-border bg-background/70 px-4 backdrop-blur-xl md:px-6"
			>
				<div class="flex min-w-0 items-center gap-2 pl-10 md:pl-0">
					<h1 class="truncate text-base font-semibold tracking-tight md:text-lg">
						{activeItem.label()}
					</h1>
				</div>

				<div class="ml-auto flex items-center gap-2 md:gap-3">
					<!-- Global status chip -->
					<div
						class="hidden items-center gap-2 rounded-full border border-border bg-surface/70 px-3 py-1.5 text-xs sm:flex"
					>
						<span
							class="dot {overall === 'up'
								? 'dot-up'
								: overall === 'down'
									? 'dot-down'
									: 'dot-muted'}"
						></span>
						<span class="text-muted-foreground">
							{#if overall === 'empty'}{m.layout_no_monitors()}{:else if overall === 'down'}{m.layout_down_count({ count: downCount })}{:else}{m.layout_all_systems_operational()}{/if}
						</span>
					</div>

					<div class="hidden h-6 w-px bg-border sm:block"></div>

					<!-- User -->
					<div
						class="grid h-8 w-8 place-items-center rounded-full bg-primary/15 text-xs font-semibold text-primary ring-1 ring-primary/25"
						title={auth.user?.username ?? ''}
					>
						{initials}
					</div>
					<button
						onclick={() => auth.logout()}
						class="grid h-8 w-8 place-items-center rounded-lg text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
						title={m.layout_sign_out()}
						aria-label={m.layout_sign_out()}
					>
						<LogOut class="h-4 w-4" />
					</button>
				</div>
			</header>

			<!-- Reconnecting banner -->
			{#if !realtime.isConnected}
				<div
					class="flex items-center gap-2 border-b border-warning/20 bg-warning/10 px-4 py-2 text-sm text-warning md:px-6"
				>
					<span class="dot dot-warn animate-pulse-dot"></span>
					{m.reconnecting()}
				</div>
			{/if}

			<!-- Routed content -->
			<main class="flex-1 overflow-auto">
				<div class="mx-auto max-w-7xl p-4 md:p-6 lg:p-8">
					{@render children()}
				</div>
			</main>
		</div>
	</div>
{/if}
