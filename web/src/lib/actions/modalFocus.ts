export interface ModalFocusOptions {
  onClose: () => void;
  initialFocus?: string;
}

const focusableSelector = [
  "a[href]",
  'button:not([disabled]):not([tabindex="-1"])',
  'input:not([disabled]):not([type="hidden"]):not([tabindex="-1"])',
  'select:not([disabled]):not([tabindex="-1"])',
  'textarea:not([disabled]):not([tabindex="-1"])',
  '[tabindex]:not([tabindex="-1"])',
].join(",");

/** Keeps keyboard focus inside an open modal and restores it when the modal closes. */
export function modalFocus(node: HTMLElement, options: ModalFocusOptions) {
  const returnFocus =
    document.activeElement instanceof HTMLElement
      ? document.activeElement
      : null;
  let currentOptions = options;

  function focusableElements(): HTMLElement[] {
    return Array.from(
      node.querySelectorAll<HTMLElement>(focusableSelector),
    ).filter((element) => element.getClientRects().length > 0);
  }

  function focusInitial() {
    const preferred = currentOptions.initialFocus
      ? node.querySelector<HTMLElement>(currentOptions.initialFocus)
      : node.querySelector<HTMLElement>("[data-modal-autofocus]");
    (preferred ?? focusableElements()[0] ?? node).focus();
  }

  function handleKeydown(event: KeyboardEvent) {
    if (event.key === "Escape") {
      event.preventDefault();
      currentOptions.onClose();
      return;
    }

    if (event.key !== "Tab") return;
    const focusable = focusableElements();
    if (focusable.length === 0) {
      event.preventDefault();
      node.focus();
      return;
    }

    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (
      event.shiftKey &&
      (document.activeElement === first || document.activeElement === node)
    ) {
      event.preventDefault();
      last.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  }

  node.addEventListener("keydown", handleKeydown);
  queueMicrotask(focusInitial);

  return {
    update(nextOptions: ModalFocusOptions) {
      currentOptions = nextOptions;
    },
    destroy() {
      node.removeEventListener("keydown", handleKeydown);
      queueMicrotask(() => {
        if (returnFocus?.isConnected) returnFocus.focus();
      });
    },
  };
}
