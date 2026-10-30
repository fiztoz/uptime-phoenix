package handlers

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func createStatusPageJSON(t *testing.T, repo *handlerStatusPageRepo, body string) *httptest.ResponseRecorder {
	t.Helper()
	svc := services.NewStatusPageService(repo, nil, nil, nil, nil, nil, handlerPasswordHasher{})
	h := NewStatusPageHandlers(svc)
	req := httptest.NewRequest(http.MethodPost, "/api/status-pages", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	if err := h.Create(echo.New().NewContext(req, rec)); err != nil {
		t.Fatalf("create status page: %v", err)
	}
	return rec
}

// GitHub #124: the slug becomes the public URL path, so it is held to the
// same URL-safe pattern the web form already enforces (^[a-z0-9-]+$).
func TestStatusPageSlugMustBeURLSafe(t *testing.T) {
	for _, bad := range []string{"Val Status!", "val status", "a/b", "ümlaut", "dot.page", "?"} {
		t.Run("create "+bad, func(t *testing.T) {
			repo := &handlerStatusPageRepo{}
			rec := createStatusPageJSON(t, repo, fmt.Sprintf(`{"slug":%q,"title":"T"}`, bad))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("POST slug %q = %d; want 400; body=%s", bad, rec.Code, rec.Body.String())
			}
			if repo.page != nil {
				t.Fatalf("slug %q was stored as %q", bad, repo.page.Slug)
			}
		})
		t.Run("update "+bad, func(t *testing.T) {
			repo := &handlerStatusPageRepo{page: &domain.StatusPage{ID: 1, Slug: "ok", Title: "T"}}
			rec := updateStatusPageJSON(t, repo, fmt.Sprintf(`{"slug":%q}`, bad), handlerPasswordHasher{})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT slug %q = %d; want 400; body=%s", bad, rec.Code, rec.Body.String())
			}
			if repo.page.Slug != "ok" {
				t.Fatalf("slug changed to %q", repo.page.Slug)
			}
		})
	}

	t.Run("good slugs are normalized and kept", func(t *testing.T) {
		repo := &handlerStatusPageRepo{}
		if rec := createStatusPageJSON(t, repo, `{"slug":"  Public-Status-2 ","title":"T"}`); rec.Code != http.StatusCreated {
			t.Fatalf("POST = %d; want 201; body=%s", rec.Code, rec.Body.String())
		}
		if repo.page.Slug != "public-status-2" {
			t.Fatalf("stored slug = %q; want public-status-2", repo.page.Slug)
		}
		repo.page.ID = 1
		if rec := updateStatusPageJSON(t, repo, `{"slug":"Renamed"}`, handlerPasswordHasher{}); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d; want 200; body=%s", rec.Code, rec.Body.String())
		}
		if repo.page.Slug != "renamed" {
			t.Fatalf("updated slug = %q; want renamed (lowercased like create)", repo.page.Slug)
		}
	})
}
