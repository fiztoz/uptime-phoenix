/**
 * Resolves public status page theme (light / dark / auto) with matchMedia for auto.
 */
export type PublicThemeSetting = "light" | "dark" | "auto";

function createPublicThemeStore() {
  let resolved = $state<"light" | "dark">("light");
  let media: MediaQueryList | null = null;
  let listener: ((e: MediaQueryListEvent) => void) | null = null;

  function applyResolved(t: "light" | "dark") {
    resolved = t;
    if (typeof document !== "undefined") {
      document.documentElement.classList.toggle("dark", t === "dark");
    }
  }

  function syncFromSetting(setting: PublicThemeSetting | undefined) {
    const s = setting ?? "auto";
    if (s === "dark") {
      applyResolved("dark");
      return;
    }
    if (s === "light") {
      applyResolved("light");
      return;
    }
    if (typeof window !== "undefined") {
      media = window.matchMedia("(prefers-color-scheme: dark)");
      applyResolved(media.matches ? "dark" : "light");
      if (!listener) {
        listener = (e: MediaQueryListEvent) =>
          applyResolved(e.matches ? "dark" : "light");
        media.addEventListener("change", listener);
      }
    }
  }

  function teardown() {
    if (media && listener) {
      media.removeEventListener("change", listener);
    }
    media = null;
    listener = null;
  }

  return {
    get resolved() {
      return resolved;
    },
    syncFromSetting,
    teardown,
  };
}

export const publicTheme = createPublicThemeStore();
