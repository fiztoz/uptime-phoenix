//go:build noweb

// Package assets, noweb variant: compiles an empty WebAssets so binaries that
// never serve HTTP (the worker) don't embed the SPA. The router skips SPA
// registration when web/dist/index.html is absent, and worker mode disables
// the HTTP server entirely — so this is behavior-preserving for the worker.
package assets

import "embed"

// WebAssets is intentionally empty under the noweb build tag.
var WebAssets embed.FS
