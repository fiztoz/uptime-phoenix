<script lang="ts">
  import {
    probesApi,
    errorMessage,
    type ProbeHostBootstrapView,
  } from "$lib/api/probes";
  import * as m from "$lib/paraglide/messages.js";

  let { onclose }: { onclose: () => void } = $props();

  const inputClass =
    "w-full rounded-lg border border-border bg-surface px-3 py-2 text-sm focus:outline-none focus:ring-2 focus:ring-ring";
  const buttonClass =
    "inline-flex items-center gap-2 rounded-lg border border-border px-4 py-2 text-sm hover:bg-accent disabled:opacity-60";
  const primaryButtonClass =
    "inline-flex items-center gap-2 rounded-lg bg-primary px-4 py-2 text-sm font-medium text-primary-foreground transition-colors hover:bg-primary/90 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:opacity-60";

  let host = $state("");
  let port = $state(22);
  let user = $state("root");
  // The password and key live only in this form until submit, and are
  // cleared as soon as the request settles, whatever the outcome.
  let password = $state("");
  let privateKey = $state("");
  let fingerprint = $state("");
  let helperVersion = $state("");
  let helperArchitecture = $state<"amd64" | "arm64">("amd64");
  let helperSHA256 = $state("");
  let helperSize = $state<number | undefined>(undefined);
  let running = $state(false);
  let error = $state("");
  let result = $state<ProbeHostBootstrapView | null>(null);

  /** host:port, bracketing a bare IPv6 literal. */
  function address(): string {
    const h = host.trim();
    return `${h.includes(":") && !h.startsWith("[") ? `[${h}]` : h}:${port}`;
  }

  async function submit(event: SubmitEvent) {
    event.preventDefault();
    running = true;
    error = "";
    result = null;
    try {
      result = await probesApi.bootstrapHost({
        ssh_address: address(),
        ssh_user: user.trim(),
        ssh_password: password,
        host_key_fingerprint: fingerprint.trim(),
        private_key: privateKey,
        helper_version: helperVersion.trim(),
        helper_architecture: helperArchitecture,
        helper_sha256: helperSHA256.trim().toLowerCase(),
        helper_size: helperSize ?? 0,
      });
    } catch (e) {
      error = errorMessage(e, m.probes_bootstrap_failed());
    } finally {
      password = "";
      privateKey = "";
      running = false;
    }
  }
</script>

<form
  onsubmit={submit}
  class="space-y-4 rounded-xl border border-border bg-card p-5"
  autocomplete="off"
>
  <h2 class="text-sm font-semibold">{m.probes_bootstrap_title()}</h2>
  <p class="text-sm text-muted-foreground">{m.probes_bootstrap_help()}</p>
  <p
    class="rounded-lg border border-border bg-surface px-3 py-2 text-sm"
    role="note"
  >
    {m.probes_bootstrap_password_note()}
  </p>
  <div class="grid gap-4 sm:grid-cols-2">
    <label class="space-y-1 text-sm"
      >{m.probes_bootstrap_host()}<input
        class={inputClass}
        bind:value={host}
        required
        maxlength="253"
        placeholder="probe-host.example.com"
        spellcheck={false}
      /></label
    >
    <label class="space-y-1 text-sm"
      >{m.probes_bootstrap_port()}<input
        class={inputClass}
        type="number"
        bind:value={port}
        required
        min="1"
        max="65535"
      /></label
    >
    <label class="space-y-1 text-sm"
      >{m.probes_bootstrap_user()}<input
        class={inputClass}
        bind:value={user}
        required
        maxlength="64"
        pattern="[A-Za-z0-9_\-]+"
        spellcheck={false}
      /></label
    >
    <label class="space-y-1 text-sm"
      >{m.probes_bootstrap_password()}<input
        class={inputClass}
        type="password"
        bind:value={password}
        required
        maxlength="1024"
        autocomplete="new-password"
      /></label
    >
    <div class="space-y-1 text-sm sm:col-span-2">
      <label for="probe-bootstrap-fingerprint"
        >{m.probes_bootstrap_fingerprint()}</label
      >
      <input
        id="probe-bootstrap-fingerprint"
        class="{inputClass} font-mono"
        bind:value={fingerprint}
        required
        pattern={"SHA256:[A-Za-z0-9+/]{43}"}
        placeholder="SHA256:…"
        aria-describedby="probe-bootstrap-fingerprint-help"
        spellcheck={false}
      />
      <span
        id="probe-bootstrap-fingerprint-help"
        class="block text-xs text-muted-foreground"
        >{m.probes_bootstrap_fingerprint_help()}</span
      >
    </div>
    <div class="space-y-1 text-sm sm:col-span-2">
      <label for="probe-bootstrap-key">{m.probes_bootstrap_private_key()}</label
      >
      <textarea
        id="probe-bootstrap-key"
        class="{inputClass} h-28 font-mono text-xs"
        bind:value={privateKey}
        required
        maxlength="65536"
        aria-describedby="probe-bootstrap-key-help"
        spellcheck={false}
      ></textarea>
      <span
        id="probe-bootstrap-key-help"
        class="block text-xs text-muted-foreground"
        >{m.probes_bootstrap_private_key_help()}</span
      >
    </div>
    <label class="space-y-1 text-sm"
      >{m.probes_bootstrap_helper_version()}<input
        class={inputClass}
        bind:value={helperVersion}
        required
        maxlength="64"
        placeholder="0.6.0"
        spellcheck={false}
      /></label
    >
    <label class="space-y-1 text-sm"
      >{m.probes_bootstrap_helper_architecture()}<select
        class={inputClass}
        bind:value={helperArchitecture}
      >
        <option value="amd64">amd64</option>
        <option value="arm64">arm64</option>
      </select></label
    >
    <label class="space-y-1 text-sm"
      >{m.probes_bootstrap_helper_sha256()}<input
        class="{inputClass} font-mono"
        bind:value={helperSHA256}
        required
        pattern={"[a-fA-F0-9]{64}"}
        maxlength="64"
        spellcheck={false}
      /></label
    >
    <label class="space-y-1 text-sm"
      >{m.probes_bootstrap_helper_size()}<input
        class={inputClass}
        type="number"
        bind:value={helperSize}
        required
        min="1"
      /></label
    >
    <p class="text-xs text-muted-foreground sm:col-span-2">
      {m.probes_bootstrap_helper_help()}
    </p>
  </div>
  {#if error}<p
      class="rounded-lg border border-danger/25 bg-danger/10 p-3 text-sm"
      role="alert"
    >
      {error}
    </p>{/if}
  {#if result}<div
      class="space-y-1 rounded-lg border border-border bg-surface p-3 text-sm"
      role="status"
    >
      <p class="font-medium">{m.probes_bootstrap_done()}</p>
      <p>
        {m.probes_bootstrap_result_helper({
          version: result.helper.version,
          architecture: result.helper.architecture,
          state: result.helper.state,
        })}
      </p>
      <p>
        {m.probes_bootstrap_result_authority({
          state: result.stage_authority,
        })}
      </p>
      <p>{m.probes_bootstrap_result_key({ state: result.authorized_key })}</p>
    </div>{/if}
  <div class="flex gap-3">
    <button type="submit" class={primaryButtonClass} disabled={running}
      >{running
        ? m.probes_bootstrap_running()
        : m.probes_bootstrap_submit()}</button
    ><button type="button" class={buttonClass} onclick={onclose}
      >{m.probes_cancel()}</button
    >
  </div>
</form>
