/**
 * Bun test plugin: compile `.svelte.ts` rune modules before importing them.
 *
 * Runes are compiler syntax, not runtime functions — `bun test` importing a
 * `.svelte.ts` module raw dies with "ReferenceError: $state is not defined".
 * Vite handles this in dev/build via the Svelte plugin; `bun test` does not go
 * through Vite, so it needs the same transform.
 *
 * Preloaded for `bun test` only (see bunfig.toml); the Vite build is unaffected.
 */
import { plugin, Transpiler } from "bun";
import { readFileSync } from "node:fs";
import { compileModule } from "svelte/compiler";

const ts = new Transpiler({ loader: "ts" });

plugin({
  name: "svelte-runes-modules",
  setup(build) {
    build.onLoad({ filter: /\.svelte\.ts$/ }, ({ path }) => {
      // Strip TS types first: compileModule parses JS, so a generic call
      // like `$state<Foo | null>(null)` would be a syntax error to it.
      const js = ts.transformSync(readFileSync(path, "utf8"));
      const compiled = compileModule(js, {
        filename: path,
        generate: "client",
      });
      return { contents: compiled.js.code, loader: "js" };
    });
  },
});
