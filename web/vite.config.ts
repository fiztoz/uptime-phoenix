import adapter from "@sveltejs/adapter-static";
import { sveltekit } from "@sveltejs/kit/vite";
import { vitePreprocess } from "@sveltejs/vite-plugin-svelte";
import { defineConfig } from "vite";
import tailwindcss from "@tailwindcss/vite";
import { paraglideVitePlugin } from "@inlang/paraglide-js";

export default defineConfig({
  server: {
    // Proxy API and WebSocket traffic to the Go backend during local dev.
    proxy: {
      "/api": {
        target: "http://localhost:3000",
        changeOrigin: true,
      },
      "/ws": {
        target: "ws://localhost:3000",
        ws: true,
      },
    },
  },
  plugins: [
    paraglideVitePlugin({
      project: "./project.inlang",
      outdir: "./src/lib/paraglide",
    }),
    tailwindcss(),
    // SvelteKit 3: project config lives here (svelte.config.js is no longer
    // read). Former `kit.*` options are top-level plugin options.
    sveltekit({
      preprocess: vitePreprocess(),
      // The Go binary embeds web/dist (//go:embed all:web/dist in assets.go)
      // and serves index.html as the SPA fallback.
      adapter: adapter({
        pages: "dist",
        assets: "dist",
        fallback: "index.html", // SPA fallback for admin routes
        precompress: false,
        strict: true,
      }),
      // Nested SPA routes like /status/:slug must use root-absolute asset
      // URLs. Relative paths resolve against /status/ and break the favicon
      // and /_app bundle when the fallback index.html is served there.
      paths: {
        relative: false,
      },
      csrf: {
        trustedOrigins: ["http://localhost:3000", "http://localhost:5173"],
      },
      prerender: {
        handleUnseenRoutes: "ignore",
      },
    }),
  ],
});
