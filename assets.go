//go:build !noweb

// Package assets provides embedded frontend static assets for the Phoenix
// binary. The web/dist directory is populated by `bun run build` in the
// web/ subdirectory and embedded at compile time via go:embed.
//
// This file lives at the module root so the embed pattern "web/dist" does
// not require ".." (forbidden by Go embed rules).
//
// Build tag `noweb` (see assets_noweb.go) compiles an empty WebAssets instead.
// The worker binary is built with `-tags noweb` so it does not carry the SPA
// (~3 MB of dead weight it never serves — worker mode disables HTTP). The
// router probes for web/dist/index.html at setup and skips SPA registration
// when absent, so no other code changes are needed.
package assets

import "embed"

// WebAssets contains the built SvelteKit SPA files (index.html, _app/*, etc).
//
//go:embed all:web/dist
var WebAssets embed.FS
