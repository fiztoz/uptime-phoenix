<script lang="ts">
  import {
    buildEmailPreviewDocument,
    resolveEmailPreviewSubject,
  } from "$lib/utils/email-preview";
  import * as m from "$lib/paraglide/messages.js";

  type EmailPreviewView = "desktop" | "mobile" | "plain";

  interface Props {
    subject: string;
    htmlBody: string;
    plainBody: string;
    view: EmailPreviewView;
    alertName: string;
    status: string;
  }

  let { subject, htmlBody, plainBody, view, alertName, status }: Props =
    $props();

  const previewDocument = $derived(buildEmailPreviewDocument(htmlBody));
  const frameWidth = $derived(view === "mobile" ? 375 : 600);
  const displaySubject = $derived(
    resolveEmailPreviewSubject(subject, alertName, status),
  );
</script>

<div
  class="email-preview overflow-hidden rounded-xl"
  aria-label={m.notification_template_email_preview_label()}
>
  <div
    class="email-client-bar flex items-center justify-between gap-4 px-4 py-3"
  >
    <div class="min-w-0">
      <p class="truncate text-sm font-semibold">
        {m.notification_template_email_inbox()}
      </p>
      <p class="mt-0.5 truncate text-[11px] text-[var(--email-muted)]">
        {m.notification_template_email_preview_approximate()}
      </p>
    </div>
    <span
      class="shrink-0 rounded bg-[var(--email-chip)] px-2 py-1 text-[10px] font-semibold uppercase tracking-wide text-[var(--email-muted)]"
    >
      {view === "plain"
        ? m.notification_template_email_format_plain()
        : view === "mobile"
          ? m.notification_template_email_preview_mobile()
          : m.notification_template_email_preview_desktop()}
    </span>
  </div>

  <div
    class="email-envelope border-b border-[var(--email-border)] px-4 py-4 sm:px-5"
  >
    <h4
      class="break-words text-base font-semibold leading-6 text-[var(--email-heading)]"
    >
      {displaySubject}
    </h4>
    <dl
      class="mt-3 grid grid-cols-[auto_minmax(0,1fr)] gap-x-3 gap-y-1 text-xs leading-5"
    >
      <dt class="text-[var(--email-muted)]">
        {m.notification_template_email_from()}
      </dt>
      <dd class="truncate text-[var(--email-text)]">
        Phoenix Monitor &lt;alerts@phoenix.local&gt;
      </dd>
      <dt class="text-[var(--email-muted)]">
        {m.notification_template_email_to()}
      </dt>
      <dd class="truncate text-[var(--email-text)]">on-call@example.com</dd>
    </dl>
  </div>

  <div class="email-stage overflow-x-auto p-3 sm:p-4">
    {#if view === "plain"}
      <div
        class="mx-auto min-h-80 w-full max-w-[600px] bg-[var(--email-message)] px-5 py-6 shadow-sm sm:px-8"
      >
        <pre
          class="whitespace-pre-wrap break-words font-sans text-sm leading-6 text-[var(--email-text)]">{plainBody ||
            "—"}</pre>
      </div>
    {:else}
      <div class="mx-auto" style={`width: min(${frameWidth}px, 100%);`}>
        <iframe
          title={m.notification_template_email_iframe_title()}
          sandbox=""
          referrerpolicy="no-referrer"
          srcdoc={previewDocument}
          class="block h-[32rem] w-full border-0 bg-[var(--email-message)] shadow-sm"
        ></iframe>
      </div>
    {/if}
  </div>
</div>

<style>
  .email-preview {
    background: var(--email-canvas);
    color: var(--email-text);
  }

  .email-client-bar {
    background: var(--email-toolbar);
    border-bottom: 1px solid var(--email-border);
    color: var(--email-heading);
  }

  .email-envelope {
    background: var(--email-message);
  }

  .email-stage {
    background: var(--email-canvas);
  }
</style>
