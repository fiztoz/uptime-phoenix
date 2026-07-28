// Package assets provides embedded frontend static assets for the Phoenix
// binary. The web/dist directory is populated by `npm run build` in the
// web/ subdirectory and embedded at compile time via go:embed.
//
// This file lives at the module root so the embed pattern "web/dist" does
// not require ".." (forbidden by Go embed rules).
package assets

import "embed"

// WebAssets contains the built SvelteKit SPA files (index.html, _app/*, etc).
//
//go:embed all:web/dist
var WebAssets embed.FS
