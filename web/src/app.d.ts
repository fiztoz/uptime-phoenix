// See https://svelte.dev/docs/kit/types#app.d.ts
// for information about these interfaces
declare global {
  namespace App {
    // interface Error {}
    // interface Locals {}
    // interface PageData {}
    // interface PageState {}
    // interface Platform {}
  }

  interface Window {
    /**
     * E2E hook: append-only log of WS events observed by the realtime store
     * (see lib/stores/ws.svelte.ts `appendWsDebugEvent`). Tests reset this via
     * `page.addInitScript` before asserting on WS activity.
     */
    __phoenixWsEventLog?: Array<{
      type: string;
      monitorId?: number;
      at: string;
    }>;
    /** E2E hook: direct handle to the realtime WS store singleton. */
    __phoenixRealtime?: unknown;
  }
}

export {};
