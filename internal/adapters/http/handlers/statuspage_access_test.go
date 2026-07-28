package handlers

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type handlerStatusPageRepo struct {
	page        *domain.StatusPage
	updateCalls int
}

func (r *handlerStatusPageRepo) Create(_ context.Context, sp *domain.StatusPage) error {
	r.page = cloneHandlerStatusPage(sp)
	return nil
}

func (r *handlerStatusPageRepo) GetByID(_ context.Context, id int64) (*domain.StatusPage, error) {
	if r.page == nil || r.page.ID != id {
		return nil, ports.ErrNotFound
	}
	return cloneHandlerStatusPage(r.page), nil
}

func (r *handlerStatusPageRepo) GetBySlug(_ context.Context, slug string) (*domain.StatusPage, error) {
	if r.page == nil || r.page.Slug != slug {
		return nil, ports.ErrNotFound
	}
	return cloneHandlerStatusPage(r.page), nil
}

func (r *handlerStatusPageRepo) List(context.Context) ([]*domain.StatusPage, error) {
	if r.page == nil {
		return []*domain.StatusPage{}, nil
	}
	return []*domain.StatusPage{cloneHandlerStatusPage(r.page)}, nil
}

func (r *handlerStatusPageRepo) Update(_ context.Context, sp *domain.StatusPage) error {
	r.updateCalls++
	r.page = cloneHandlerStatusPage(sp)
	return nil
}

func (r *handlerStatusPageRepo) Delete(context.Context, int64) error { return nil }

func cloneHandlerStatusPage(sp *domain.StatusPage) *domain.StatusPage {
	if sp == nil {
		return nil
	}
	cp := *sp
	return &cp
}

func updateStatusPageAccessCode(t *testing.T, repo *handlerStatusPageRepo, accessCode string) *httptest.ResponseRecorder {
	t.Helper()
	return updateStatusPageAccessCodeWithHasher(t, repo, accessCode, handlerPasswordHasher{})
}

func updateStatusPageAccessCodeWithHasher(
	t *testing.T,
	repo *handlerStatusPageRepo,
	accessCode string,
	passwords ports.PasswordHasher,
) *httptest.ResponseRecorder {
	t.Helper()
	return updateStatusPageJSON(t, repo, fmt.Sprintf(`{"access_code":%q}`, accessCode), passwords)
}

func updateStatusPageJSON(
	t *testing.T,
	repo *handlerStatusPageRepo,
	body string,
	passwords ports.PasswordHasher,
) *httptest.ResponseRecorder {
	t.Helper()

	svc := services.NewStatusPageService(repo, nil, nil, nil, nil, nil, passwords)
	h := NewStatusPageHandlers(svc)
	e := echo.New()
	req := httptest.NewRequest(http.MethodPut, "/api/status-pages/1", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPath("/api/status-pages/:id")
	c.SetParamNames("id")
	c.SetParamValues("1")

	if err := h.Update(c); err != nil {
		t.Fatalf("update status page: %v", err)
	}
	return rec
}

type handlerPasswordHasher struct {
	hashErr error
}

func (h handlerPasswordHasher) Hash(password string) (string, error) {
	if h.hashErr != nil {
		return "", h.hashErr
	}
	return "hashed:" + password, nil
}

func (handlerPasswordHasher) Verify(hashed, password string) error {
	if hashed != "hashed:"+password {
		return fmt.Errorf("password mismatch")
	}
	return nil
}

func TestUpdateStatusPage_InvalidAccessCodeDoesNotReturnSuccess(t *testing.T) {
	repo := &handlerStatusPageRepo{page: &domain.StatusPage{
		ID:           1,
		Slug:         "protected",
		Title:        "Protected",
		PasswordHash: "existing-hash",
	}}

	rec := updateStatusPageAccessCode(t, repo, strings.Repeat("x", 73))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if repo.updateCalls != 0 {
		t.Fatalf("repository updated %d times after hashing failure; want 0", repo.updateCalls)
	}
}

func TestUpdateStatusPage_AccessCodeHashFailureDoesNotReturnSuccess(t *testing.T) {
	repo := &handlerStatusPageRepo{page: &domain.StatusPage{
		ID:           1,
		Slug:         "protected",
		Title:        "Protected",
		PasswordHash: "existing-hash",
	}}
	boom := errors.New("hasher unavailable")

	rec := updateStatusPageAccessCodeWithHasher(
		t,
		repo,
		"valid password",
		handlerPasswordHasher{hashErr: boom},
	)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 (body: %s)", rec.Code, rec.Body.String())
	}
	if repo.updateCalls != 0 {
		t.Fatalf("repository updated %d times after hashing failure; want 0", repo.updateCalls)
	}
}

func TestUpdateStatusPage_EmptyAccessCodeClearsProtection(t *testing.T) {
	repo := &handlerStatusPageRepo{page: &domain.StatusPage{
		ID:           1,
		Slug:         "protected",
		Title:        "Protected",
		PasswordHash: "existing-hash",
	}}

	rec := updateStatusPageAccessCode(t, repo, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if repo.page.PasswordHash != "" {
		t.Fatalf("password hash = %q; want cleared", repo.page.PasswordHash)
	}
}

func TestUpdateStatusPage_InvalidSLATargetDoesNotPersist(t *testing.T) {
	repo := &handlerStatusPageRepo{page: &domain.StatusPage{ID: 1, Slug: "public", Title: "Public"}}

	rec := updateStatusPageJSON(t, repo, `{"sla_target":100.001}`, handlerPasswordHasher{})

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if repo.updateCalls != 0 {
		t.Fatalf("repository updated %d times after SLA validation failure; want 0", repo.updateCalls)
	}
}

func TestUpdateStatusPage_ZeroSLATargetClearsPublicDisplay(t *testing.T) {
	target := 99.9
	repo := &handlerStatusPageRepo{page: &domain.StatusPage{
		ID: 1, Slug: "public", Title: "Public", SLATarget: &target,
	}}

	rec := updateStatusPageJSON(t, repo, `{"sla_target":0}`, handlerPasswordHasher{})

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	if repo.page.SLATarget != nil {
		t.Fatalf("SLA target = %v; want cleared", *repo.page.SLATarget)
	}
}
