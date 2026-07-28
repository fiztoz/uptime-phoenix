/**
 * Theme store (runes) — light/dark toggle persisted in localStorage.
 * Applies 'dark' class to documentElement.
 */
function createThemeStore() {
  const saved =
    typeof window !== "undefined"
      ? (localStorage.getItem("phoenix_theme") as "light" | "dark" | null)
      : null;
  const initialTheme: "light" | "dark" =
    typeof window === "undefined"
      ? "dark"
      : (saved ??
        (window.matchMedia("(prefers-color-scheme: dark)").matches
          ? "dark"
          : "light"));
  let theme = $state<"light" | "dark">(initialTheme);

  // Load on init
  if (typeof window !== "undefined") {
    applyTheme(initialTheme);
  }

  function applyTheme(t: "light" | "dark") {
    if (typeof document !== "undefined") {
      if (t === "dark") {
        document.documentElement.classList.add("dark");
      } else {
        document.documentElement.classList.remove("dark");
      }
    }
  }

  function toggle() {
    theme = theme === "dark" ? "light" : "dark";
    localStorage.setItem("phoenix_theme", theme);
    applyTheme(theme);
  }

  function setTheme(t: "light" | "dark") {
    theme = t;
    localStorage.setItem("phoenix_theme", theme);
    applyTheme(theme);
  }

  return {
    get theme() {
      return theme;
    },
    toggle,
    setTheme,
  };
}

export const themeStore = createThemeStore();
