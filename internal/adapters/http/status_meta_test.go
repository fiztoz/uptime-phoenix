package http

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type fakeMetaResolver struct {
	bySlug   map[string]*domain.StatusPage
	byDomain map[string]*domain.StatusPage
}

func (f *fakeMetaResolver) GetBySlug(_ context.Context, slug string) (*domain.StatusPage, error) {
	if sp, ok := f.bySlug[slug]; ok {
		return sp, nil
	}
	return nil, ports.ErrNotFound
}

func (f *fakeMetaResolver) ResolveDomain(_ context.Context, host string) (*domain.StatusPage, error) {
	if sp, ok := f.byDomain[host]; ok {
		return sp, nil
	}
	return nil, ports.ErrNotFound
}

func baseIndex() []byte {
	return []byte(`<!doctype html><html><head><meta charset="utf-8" /><title>Phoenix</title></head><body>app</body></html>`)
}

func TestInjectStatusPageMeta_SlugPublished(t *testing.T) {
	r := &fakeMetaResolver{bySlug: map[string]*domain.StatusPage{
		"acme": {Slug: "acme", Title: "Acme Status", Description: "All systems", Published: true},
	}}
	out := InjectStatusPageMeta(context.Background(), r, "/status/acme", "localhost:3000", "https://status.example.com", baseIndex())
	s := string(out)
	if !strings.Contains(s, `og:title" content="Acme Status"`) {
		t.Fatalf("missing og:title: %s", s)
	}
	if !strings.Contains(s, `og:description" content="All systems"`) {
		t.Fatalf("missing og:description")
	}
	if !strings.Contains(s, `og:url" content="https://status.example.com/status/acme"`) {
		t.Fatalf("missing og:url: %s", s)
	}
	if !strings.Contains(s, `twitter:card" content="summary"`) {
		t.Fatalf("missing twitter:card")
	}
	if !strings.Contains(s, `og:site_name" content="Phoenix"`) {
		t.Fatalf("missing og:site_name")
	}
	if !strings.Contains(s, `og:image" content="https://status.example.com/brand/phoenix-mascot.png"`) {
		t.Fatalf("empty icon must fall back to mascot: %s", s)
	}
}

func TestInjectStatusPageMeta_KumaDefaultIconFallsBackToMascot(t *testing.T) {
	r := &fakeMetaResolver{bySlug: map[string]*domain.StatusPage{
		"acme": {Slug: "acme", Title: "Acme", Published: true, Icon: "/icon.svg"},
	}}
	out := InjectStatusPageMeta(context.Background(), r, "/status/acme", "localhost", "https://status.example.com", baseIndex())
	s := string(out)
	if strings.Contains(s, "/icon.svg") {
		t.Fatalf("Kuma /icon.svg must not be used as og:image: %s", s)
	}
	if !strings.Contains(s, `og:image" content="https://status.example.com/brand/phoenix-mascot.png"`) {
		t.Fatalf("want mascot og:image, got: %s", s)
	}
}

func TestInjectStatusPageMeta_BareSlugPathIsNotAStatusPage(t *testing.T) {
	idx := baseIndex()
	r := &fakeMetaResolver{bySlug: map[string]*domain.StatusPage{
		"acme": {Slug: "acme", Title: "Acme", Published: true},
	}}
	out := InjectStatusPageMeta(context.Background(), r, "/acme", "localhost", "https://example.com", idx)
	if !bytes.Equal(out, idx) {
		t.Fatalf("bare /:slug must not be treated as a status page: %s", out)
	}
}

func TestInjectStatusPageMeta_CustomDomainRoot(t *testing.T) {
	r := &fakeMetaResolver{byDomain: map[string]*domain.StatusPage{
		"status.acme.com": {Slug: "acme", Title: "Acme CDN", Description: "CDN health", Published: true},
	}}
	out := InjectStatusPageMeta(context.Background(), r, "/", "status.acme.com:443", "https://status.acme.com", baseIndex())
	s := string(out)
	if !strings.Contains(s, `og:title" content="Acme CDN"`) {
		t.Fatalf("custom domain not resolved: %s", s)
	}
	if !strings.Contains(s, `og:description" content="CDN health"`) {
		t.Fatalf("missing description")
	}
}

func TestInjectStatusPageMeta_ProtectedPageStillHasMeta(t *testing.T) {
	r := &fakeMetaResolver{bySlug: map[string]*domain.StatusPage{
		"secret": {
			Slug:         "secret",
			Title:        "Secret Page",
			Description:  "Internal",
			Published:    true,
			PasswordHash: "$2a$10$notarealhashbutmustnotleak",
		},
	}}
	out := InjectStatusPageMeta(context.Background(), r, "/status/secret", "localhost", "https://example.com", baseIndex())
	s := string(out)
	if !strings.Contains(s, `og:title" content="Secret Page"`) {
		t.Fatalf("protected published page should still inject title")
	}
	if strings.Contains(s, "$2a$") || strings.Contains(s, "notarealhash") {
		t.Fatalf("password hash leaked into HTML")
	}
}

func TestInjectStatusPageMeta_UnpublishedFallsBack(t *testing.T) {
	idx := baseIndex()
	r := &fakeMetaResolver{bySlug: map[string]*domain.StatusPage{
		"draft": {Slug: "draft", Title: "Draft", Published: false},
	}}
	out := InjectStatusPageMeta(context.Background(), r, "/status/draft", "localhost", "https://example.com", idx)
	if !bytes.Equal(out, idx) {
		t.Fatalf("unpublished page must return original bytes")
	}
}

func TestInjectStatusPageMeta_NotFoundFallsBack(t *testing.T) {
	idx := baseIndex()
	r := &fakeMetaResolver{bySlug: map[string]*domain.StatusPage{}}
	out := InjectStatusPageMeta(context.Background(), r, "/status/missing", "localhost", "https://example.com", idx)
	if !bytes.Equal(out, idx) {
		t.Fatalf("unknown slug must return original bytes")
	}
}

func TestInjectStatusPageMeta_AdminAndAssetsUnchanged(t *testing.T) {
	idx := baseIndex()
	r := &fakeMetaResolver{bySlug: map[string]*domain.StatusPage{
		"acme": {Slug: "acme", Title: "Acme", Published: true},
	}}
	for _, path := range []string{
		"/dashboard",
		"/api/status/acme",
		"/assets/app.js",
		"/_app/immutable/entry.js",
		"/status-pages",
		"/favicon.svg",
		"/brand/phoenix-mascot.png",
	} {
		out := InjectStatusPageMeta(context.Background(), r, path, "localhost", "https://example.com", idx)
		if !bytes.Equal(out, idx) {
			t.Fatalf("path %s must be byte-identical to original index", path)
		}
	}
}

func TestInjectStatusPageMeta_XSSEscaping(t *testing.T) {
	r := &fakeMetaResolver{bySlug: map[string]*domain.StatusPage{
		"x": {
			Slug:        "x",
			Title:       `"><script>alert(1)</script>`,
			Description: `" onload="alert(2)`,
			Published:   true,
		},
	}}
	out := InjectStatusPageMeta(context.Background(), r, "/status/x", "localhost", "https://example.com", baseIndex())
	s := string(out)
	if strings.Contains(s, "<script>") {
		t.Fatalf("unescaped script in output")
	}
	if !strings.Contains(s, "&lt;script&gt;") && !strings.Contains(s, "&#34;") && !strings.Contains(s, "&quot;") {
		// html.EscapeString produces &#34; or &quot; depending on version; require some escaping.
		if !strings.Contains(s, "alert(1)") {
			// fully stripped somehow — also ok, but prefer escaped
		} else if !strings.Contains(s, "&lt;") && !strings.Contains(s, "&#") {
			t.Fatalf("expected HTML escaping of title/description: %s", s)
		}
	}
}

func TestInjectStatusPageMeta_NilResolver(t *testing.T) {
	idx := baseIndex()
	out := InjectStatusPageMeta(context.Background(), nil, "/status/x", "localhost", "https://example.com", idx)
	if !bytes.Equal(out, idx) {
		t.Fatalf("nil resolver must return original")
	}
}

func TestExtractStatusSlug(t *testing.T) {
	tests := []struct {
		path string
		want string
	}{
		{"/status/acme", "acme"},
		{"/status/Acme", "acme"},
		{"/status/acme/history", "acme"},
		{"/acme", ""},
		{"/", ""},
		{"/status", ""},
		{"/api/foo", ""}, // multi-segment non-status
	}
	for _, tt := range tests {
		// isNonStatusShellPath filters api; extract alone on /api/foo returns "".
		got := extractStatusSlug(tt.path)
		if tt.path == "/api/foo" {
			// extractStatusSlug returns "" for multi-segment non-status.
			if got != "" {
				t.Fatalf("extractStatusSlug(%q)=%q want empty", tt.path, got)
			}
			continue
		}
		if got != tt.want {
			t.Fatalf("extractStatusSlug(%q)=%q want %q", tt.path, got, tt.want)
		}
	}
}

// Ensure ports.ErrNotFound is still the typed miss used by ResolveDomain.
var _ = errors.Is
