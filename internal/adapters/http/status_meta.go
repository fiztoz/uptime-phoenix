package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"strings"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// StatusPageMetaResolver looks up published-page candidates for OG injection.
// StatusPageService satisfies this with GetBySlug and ResolveDomain.
type StatusPageMetaResolver interface {
	GetBySlug(ctx context.Context, slug string) (*domain.StatusPage, error)
	ResolveDomain(ctx context.Context, host string) (*domain.StatusPage, error)
}

// InjectStatusPageMeta injects escaped Open Graph / Twitter metadata into the
// SPA index.html shell for published public status pages only.
//
// path is the request path (e.g. "/status/my-slug" or "/"). host is the
// request Host (port stripped for custom-domain lookup). origin is the
// absolute public origin used for og:url (scheme+host, no trailing slash).
//
// Unpublished, admin, API, asset, and unknown paths return the original
// index bytes unchanged. Never exposes PasswordHash or other secrets.
func InjectStatusPageMeta(
	ctx context.Context,
	resolver StatusPageMetaResolver,
	path, host, origin string,
	indexHTML []byte,
) []byte {
	if resolver == nil || len(indexHTML) == 0 {
		return indexHTML
	}
	sp := resolveStatusPageForMeta(ctx, resolver, path, host)
	if sp == nil || !sp.Published {
		return indexHTML
	}
	return injectMetaTags(indexHTML, sp, path, origin)
}

func resolveStatusPageForMeta(
	ctx context.Context,
	resolver StatusPageMetaResolver,
	path, host string,
) *domain.StatusPage {
	path = strings.TrimSpace(path)
	if path == "" {
		path = "/"
	}
	// Never transform non-SPA / non-status paths.
	if isNonStatusShellPath(path) {
		return nil
	}

	// /status/:slug or bare /:slug style public routes.
	if slug := extractStatusSlug(path); slug != "" {
		sp, err := resolver.GetBySlug(ctx, slug)
		if err != nil {
			return nil
		}
		return sp
	}

	// Custom-domain root (path "/" or empty) — resolve by Host.
	if path == "/" {
		h := stripHostPort(host)
		if h == "" {
			return nil
		}
		sp, err := resolver.ResolveDomain(ctx, h)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) || errors.Is(err, ports.ErrNotFound) {
				return nil
			}
			return nil
		}
		return sp
	}
	return nil
}

func isNonStatusShellPath(path string) bool {
	p := strings.ToLower(path)
	switch {
	case strings.HasPrefix(p, "/api/"),
		strings.HasPrefix(p, "/ws"),
		strings.HasPrefix(p, "/assets/"),
		strings.HasPrefix(p, "/_app/"),
		strings.HasPrefix(p, "/favicon"),
		strings.HasPrefix(p, "/brand/"),
		strings.HasPrefix(p, "/fonts/"),
		strings.HasPrefix(p, "/dashboard"),
		strings.HasPrefix(p, "/monitors"),
		strings.HasPrefix(p, "/notifications"),
		strings.HasPrefix(p, "/settings"),
		strings.HasPrefix(p, "/status-pages"),
		strings.HasPrefix(p, "/maintenance"),
		strings.HasPrefix(p, "/incidents"),
		strings.HasPrefix(p, "/backup"),
		strings.HasPrefix(p, "/login"):
		return true
	}
	// Static asset extensions must stay byte-identical.
	for _, ext := range []string{".js", ".css", ".map", ".png", ".svg", ".woff2", ".ico", ".json", ".txt", ".webp"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return false
}

// extractStatusSlug returns the public status-page slug from paths like
// "/status/my-slug" or "/my-slug" (single segment). Multi-segment admin
// paths are rejected by isNonStatusShellPath first.
func extractStatusSlug(path string) string {
	path = strings.Trim(path, "/")
	if path == "" {
		return ""
	}
	parts := strings.Split(path, "/")
	if len(parts) == 2 && parts[0] == "status" && parts[1] != "" {
		return strings.ToLower(parts[1])
	}
	// Public SPA route is /(public)/[domain] → path "/:slug".
	if len(parts) == 1 && parts[0] != "" {
		slug := strings.ToLower(parts[0])
		// Reserved single segments that are not status pages.
		switch slug {
		case "status", "api", "ws", "health", "metrics", "assets":
			return ""
		}
		return slug
	}
	return ""
}

func stripHostPort(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	// net.SplitHostPort fails without a port; handle both forms.
	if h, _, err := net.SplitHostPort(host); err == nil {
		return strings.ToLower(h)
	}
	return strings.ToLower(host)
}

func injectMetaTags(indexHTML []byte, sp *domain.StatusPage, path, origin string) []byte {
	title := strings.TrimSpace(sp.Title)
	if title == "" {
		title = sp.Slug
	}
	desc := strings.TrimSpace(sp.Description)
	if desc == "" {
		desc = title + " — status page"
	}
	// Truncate description for social previews.
	if len(desc) > 300 {
		desc = desc[:297] + "..."
	}

	escTitle := html.EscapeString(title)
	escDesc := html.EscapeString(desc)
	escSite := html.EscapeString("Phoenix")

	ogURL := ""
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin != "" {
		p := path
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
		ogURL = origin + p
	} else if sp.Slug != "" {
		ogURL = "/status/" + sp.Slug
	}
	escURL := html.EscapeString(ogURL)

	// Prefer the page icon when it looks like an absolute/relative image URL;
	// otherwise fall back to the Phoenix brand mark.
	image := "/brand/phoenix-mascot.png"
	if icon := strings.TrimSpace(sp.Icon); icon != "" && (strings.HasPrefix(icon, "http://") || strings.HasPrefix(icon, "https://") || strings.HasPrefix(icon, "/")) {
		image = icon
	}
	if origin != "" && strings.HasPrefix(image, "/") {
		image = origin + image
	}
	escImage := html.EscapeString(image)

	block := fmt.Sprintf(`
    <!-- phoenix-status-meta -->
    <title>%s</title>
    <meta name="description" content="%s" />
    <meta property="og:type" content="website" />
    <meta property="og:site_name" content="%s" />
    <meta property="og:title" content="%s" />
    <meta property="og:description" content="%s" />
    <meta property="og:url" content="%s" />
    <meta property="og:image" content="%s" />
    <meta name="twitter:card" content="summary" />
    <meta name="twitter:title" content="%s" />
    <meta name="twitter:description" content="%s" />
`, escTitle, escDesc, escSite, escTitle, escDesc, escURL, escImage, escTitle, escDesc)

	// Insert before </head> if present; otherwise prepend.
	lower := bytes.ToLower(indexHTML)
	idx := bytes.Index(lower, []byte("</head>"))
	if idx < 0 {
		out := make([]byte, 0, len(block)+len(indexHTML))
		out = append(out, []byte(block)...)
		out = append(out, indexHTML...)
		return out
	}
	out := make([]byte, 0, len(indexHTML)+len(block))
	out = append(out, indexHTML[:idx]...)
	out = append(out, []byte(block)...)
	out = append(out, indexHTML[idx:]...)
	return out
}
