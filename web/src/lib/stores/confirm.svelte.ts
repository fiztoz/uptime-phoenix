/**
 * Promise-based confirmation dialog.
 *
 * Replaces `window.confirm()`, which is unstyled, blocks the event loop, and is
 * suppressible by the browser ("prevent this page from creating more dialogs")
 * — a suppressed confirm returns false, so a user who ticked that box silently
 * loses the ability to delete anything.
 *
 * Usage — the call site reads the same as the confirm() it replaces:
 *
 *   if (!(await confirmAction({ title: 'Delete monitor "api"?', destructive: true }))) return;
 *
 * <ConfirmDialog /> is mounted once in the root layout and renders whatever is
 * pending here. Requests that arrive while another is open are queued, so two
 * overlapping asks can never resolve the wrong promise.
 */

export interface ConfirmOptions {
  /** Question the user is answering. Required — this is what they read. */
  title: string;
  /** Consequences that are not obvious from the title. Optional. */
  message?: string;
  confirmLabel?: string;
  cancelLabel?: string;
  /** Styles confirm as destructive and focuses Cancel instead of Confirm. */
  destructive?: boolean;
  /**
   * Gate confirm behind typing this exact string. For actions with a blast
   * radius beyond the thing named in the title — use sparingly; a type-to-
   * confirm on every delete just trains people to type without reading.
   */
  requireText?: string;
}

export interface PendingConfirm extends ConfirmOptions {
  resolve: (confirmed: boolean) => void;
}

class ConfirmController {
  /** The request currently on screen; null when no dialog is open. */
  current = $state<PendingConfirm | null>(null);
  /** Requests that arrived while `current` was open, oldest first. */
  #queue: PendingConfirm[] = [];

  ask(options: ConfirmOptions): Promise<boolean> {
    return new Promise<boolean>((resolve) => {
      const pending: PendingConfirm = { ...options, resolve };
      if (this.current) this.#queue.push(pending);
      else this.current = pending;
    });
  }

  /** Answer the open request and promote the next queued one, if any. */
  #settle(confirmed: boolean) {
    const pending = this.current;
    if (!pending) return;
    this.current = this.#queue.shift() ?? null;
    pending.resolve(confirmed);
  }

  confirm() {
    this.#settle(true);
  }

  cancel() {
    this.#settle(false);
  }
}

export const confirmController = new ConfirmController();

/** Ask the user to confirm. Resolves true on confirm, false on cancel/dismiss. */
export function confirmAction(options: ConfirmOptions): Promise<boolean> {
  return confirmController.ask(options);
}
