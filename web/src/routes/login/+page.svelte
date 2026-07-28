<script lang="ts">
	import { auth } from '$lib/stores/auth.svelte.ts';
	import { authApi } from '$lib/api/auth';
	import { goto } from '$app/navigation';
	import { page } from '$app/stores';
	import { onMount } from 'svelte';
	import BrandMark from '$lib/components/BrandMark.svelte';
	import Wordmark from '$lib/components/Wordmark.svelte';
	import * as m from '$lib/paraglide/messages.js';
	import { isWebAuthnSupported } from '$lib/api/webauthn';
	import { KeyRound, Building2 } from '@lucide/svelte';

	let username = $state('');
	let password = $state('');
	let otp = $state('');
	let requires2FA = $state(false);
	let ticket = $state('');
	let loading = $state(false);
	let error = $state('');
	let hasUsers = $state(true);
	let oidcEnabled = $state(false);

	function oidcErrorMessage(code: string): string {
		switch (code) {
			case 'access_denied':
				return m.login_sso_access_denied();
			case 'no_account':
				return m.login_sso_no_account();
			default:
				return m.login_sso_error();
		}
	}

	onMount(async () => {
		// OIDC callback handoff: success uses #oidc_token= (fragment); errors use ?oidc_error=
		const hash = typeof window !== 'undefined' ? window.location.hash.replace(/^#/, '') : '';
		const hashParams = new URLSearchParams(hash);
		const oidcToken = hashParams.get('oidc_token');
		const oidcErr = $page.url.searchParams.get('oidc_error');
		if (oidcToken) {
			loading = true;
			try {
				await auth.completeOIDC(oidcToken);
				// Strip token from the address bar before navigating.
				history.replaceState(null, '', '/login');
				goto('/dashboard', { replaceState: true });
				return;
			} catch (e: any) {
				error = e?.message || m.login_sso_error();
				history.replaceState(null, '', '/login');
			} finally {
				loading = false;
			}
		} else if (oidcErr) {
			error = oidcErrorMessage(oidcErr);
			history.replaceState(null, '', '/login');
		}

		if (auth.isAuthenticated) {
			goto('/dashboard', { replaceState: true });
			return;
		}
		try {
			const res = await authApi.hasUsers();
			hasUsers = res.has_users;
		} catch {
			// If the endpoint fails, assume users exist
		}
		try {
			const st = await authApi.oidcStatus();
			oidcEnabled = st.enabled;
		} catch {
			oidcEnabled = false;
		}
	});

	async function handleLogin() {
		if (!username || !password) {
			error = 'Username and password required';
			return;
		}
		loading = true;
		error = '';
		try {
			const res = await auth.login(username, password);
			if (res.requires_2fa) {
				requires2FA = true;
				ticket = res.ticket || '';
			} else {
				goto('/dashboard', { replaceState: true });
			}
		} catch (e: any) {
			error = e?.message || 'Login failed';
		} finally {
			loading = false;
		}
	}

	async function handleVerify2FA() {
		if (!otp || otp.length !== 6) {
			error = 'Enter 6-digit code';
			return;
		}
		loading = true;
		error = '';
		try {
			await auth.verify2FA(ticket, otp);
			goto('/dashboard', { replaceState: true });
		} catch (e: any) {
			error = e?.message || 'Verification failed';
		} finally {
			loading = false;
		}
	}

	async function handlePasskeyLogin() {
		if (!username) {
			error = 'Enter your username to use a passkey';
			return;
		}
		loading = true;
		error = '';
		try {
			await auth.passkeyLogin(username);
			goto('/dashboard', { replaceState: true });
		} catch (e: any) {
			error = e?.message || 'Passkey sign-in failed';
		} finally {
			loading = false;
		}
	}

	async function handleFirstUser() {
		if (!username || password.length < 8) {
			error = 'Password must be at least 8 characters';
			return;
		}
		loading = true;
		error = '';
		try {
			await auth.register(username, password);
			goto('/dashboard', { replaceState: true });
		} catch (e: any) {
			error = e?.message || 'Registration failed';
		} finally {
			loading = false;
		}
	}

	const inputClass =
		'w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm placeholder:text-faint focus:border-primary/40 focus:outline-none focus:ring-2 focus:ring-ring';
</script>

<svelte:head>
	<title>Phoenix · Sign in</title>
</svelte:head>

<div class="app-glow relative flex min-h-dvh items-center justify-center bg-background p-4">
	<div class="relative z-10 w-full max-w-md">
		<!-- Branding -->
		<div class="mb-8 text-center">
			<div class="mb-1 flex items-center justify-center">
				<BrandMark size={72} />
			</div>
			<div class="flex justify-center">
				<Wordmark size={46} />
			</div>
			<p class="mt-2 text-sm text-muted-foreground">Self-hosted monitoring</p>
		</div>

		<!-- Card -->
		<div class="rounded-xl border border-border bg-card p-8">
			{#if !requires2FA}
				{#if hasUsers}
					<div class="mb-6 border-b border-border pb-3">
						<h2 class="text-lg font-semibold tracking-tight">Sign in</h2>
						<p class="mt-1 text-sm text-muted-foreground">Welcome back.</p>
					</div>
				{:else}
					<div class="mb-6 border-b border-border pb-3">
						<h2 class="text-lg font-semibold tracking-tight">Create your account</h2>
						<p class="mt-1 text-sm text-muted-foreground">This is your first user — you'll be the admin.</p>
					</div>
				{/if}

				{#if error}
					<div class="mb-4 rounded-lg border border-danger/25 bg-danger/10 p-3 text-sm text-danger">
						{error}
					</div>
				{/if}
			{/if}

			{#if requires2FA}
				<!-- 2FA Step -->
				<div class="space-y-4">
					<div>
						<h3 class="text-lg font-semibold tracking-tight">Two-factor authentication</h3>
						<p class="mt-1 text-sm text-muted-foreground">Enter the 6-digit code from your authenticator app.</p>
					</div>
					<div>
						<label for="otp" class="text-sm font-medium">Verification code</label>
						<input
							id="otp"
							type="text"
							bind:value={otp}
							maxlength="6"
							inputmode="numeric"
							class="{inputClass} mt-1 font-mono text-lg tracking-[4px]"
							placeholder="000000"
							onkeydown={(e) => e.key === 'Enter' && handleVerify2FA()}
						/>
					</div>
					<button
						type="button"
						onclick={handleVerify2FA}
						disabled={loading}
						class="w-full rounded-lg bg-primary py-2.5 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60"
					>
						{loading ? 'Verifying…' : 'Verify & Sign in'}
					</button>
					<button
						type="button"
						onclick={() => { requires2FA = false; otp = ''; }}
						class="w-full text-sm text-muted-foreground transition-colors hover:text-foreground"
					>Back to login</button>
				</div>
			{:else if hasUsers}
				<!-- Login Form -->
				<div class="space-y-4">
					<div>
						<label for="login-username" class="text-sm font-medium">Username</label>
						<input
							id="login-username"
							type="text"
							bind:value={username}
							class="{inputClass} mt-1"
							placeholder="admin"
							onkeydown={(e) => e.key === 'Enter' && handleLogin()}
						/>
					</div>
					<div>
						<label for="login-password" class="text-sm font-medium">Password</label>
						<input
							id="login-password"
							type="password"
							bind:value={password}
							class="{inputClass} mt-1"
							placeholder="••••••••"
							onkeydown={(e) => e.key === 'Enter' && handleLogin()}
						/>
					</div>
					<button
						type="button"
						onclick={handleLogin}
						disabled={loading || !username || !password}
						class="w-full rounded-lg bg-primary py-2.5 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60"
					>
						{loading ? 'Signing in…' : 'Sign in'}
					</button>

					{#if oidcEnabled || isWebAuthnSupported()}
						<div class="flex items-center gap-3 text-xs text-muted-foreground">
							<span class="h-px flex-1 bg-border"></span>
							or
							<span class="h-px flex-1 bg-border"></span>
						</div>
					{/if}
					{#if oidcEnabled}
						<a
							href={authApi.oidcLoginURL()}
							class="flex w-full items-center justify-center gap-2 rounded-lg border border-border bg-surface py-2.5 text-sm font-medium transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
						>
							<Building2 class="h-4 w-4" />
							{m.login_sso()}
						</a>
					{/if}
					{#if isWebAuthnSupported()}
						<button
							type="button"
							onclick={handlePasskeyLogin}
							disabled={loading || !username}
							class="flex w-full items-center justify-center gap-2 rounded-lg border border-border bg-surface py-2.5 text-sm font-medium transition-colors hover:bg-accent hover:text-accent-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60"
						>
							<KeyRound class="h-4 w-4" />
							{m.login_passkey()}
						</button>
					{/if}
				</div>
			{:else}
				<!-- First-user bootstrap form -->
				<div class="space-y-4">
					<div>
						<label for="reg-username" class="text-sm font-medium">Username</label>
						<input
							id="reg-username"
							type="text"
							bind:value={username}
							class="{inputClass} mt-1"
							placeholder="admin"
						/>
					</div>
					<div>
						<label for="reg-password" class="text-sm font-medium">Password (min 8 chars)</label>
						<input
							id="reg-password"
							type="password"
							bind:value={password}
							class="{inputClass} mt-1"
							placeholder="••••••••"
						/>
					</div>
					<button
						type="button"
						onclick={handleFirstUser}
						disabled={loading || !username || password.length < 8}
						class="w-full rounded-lg bg-primary py-2.5 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60"
					>
						{loading ? 'Creating account…' : 'Create account'}
					</button>
					<p class="text-center text-xs text-muted-foreground">First user becomes admin</p>
				</div>
			{/if}
		</div>

		<p class="mt-6 text-center text-xs text-muted-foreground">
			Self-hosted • No telemetry
		</p>
	</div>
</div>
