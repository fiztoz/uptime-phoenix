import { recoverFromPreloadError } from "$lib/deployment-recovery";

window.addEventListener("vite:preloadError", (event) => {
  recoverFromPreloadError(event, {
    storage: sessionStorage,
    reload: () => location.reload(),
  });
});
