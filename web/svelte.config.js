import adapter from "@sveltejs/adapter-static";
import { vitePreprocess } from "@sveltejs/vite-plugin-svelte";

/** @type {import('@sveltejs/kit').Config} */
const config = {
  preprocess: vitePreprocess(),
  kit: {
    adapter: adapter({
      pages: "dist",
      assets: "dist",
      fallback: "index.html", // SPA fallback for admin routes
      precompress: false,
      strict: true,
    }),
    csrf: {
      trustedOrigins: ["http://localhost:3000", "http://localhost:5173"],
    },
    prerender: {
      handleUnseenRoutes: "ignore",
    },
  },
};

export default config;
