package handlers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/middleware"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/memory"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func serveExtensions(t *testing.T, raw string) *httptest.ResponseRecorder {
	t.Helper()
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	e.GET("/api/extensions", handlers.NewExtensionHandlers(raw).List)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/extensions", nil))
	return rec
}

func decodeExtensionList(t *testing.T, rec *httptest.ResponseRecorder) []handlers.ExtensionView {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/extensions = %d; want 200", rec.Code)
	}
	if rec.Body.String() == "null" {
		t.Fatal("response body is null; want a JSON array")
	}
	var got []handlers.ExtensionView
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal extensions: %v\nbody: %s", err, rec.Body.String())
	}
	if got == nil {
		t.Fatal("decoded list is nil; want empty slice")
	}
	return got
}

func TestExtensionCatalog_RequiresViewExtensionsCapability(t *testing.T) {
	ctx := context.Background()
	users := memory.NewUserRepo()
	apiKeys := memory.NewAPIKeyRepo()
	perms := memory.NewUserPermissionRepo()
	authenticator := auth.NewJWTAuthenticator("extension-test-signing-key", 24, users)
	authSvc := services.NewAuthService(users, apiKeys, authenticator, auth.NewTOTPProvider("Phoenix"))
	accessSvc := services.NewAccessService(users, perms, nil, nil)

	admin, err := authSvc.CreateUser(ctx, "extension-admin", "password123", true, true, "UTC", services.UserCapabilities{})
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	viewer, err := authSvc.CreateUser(ctx, "extension-viewer", "password123", true, false, "UTC", services.UserCapabilities{CanViewExtensions: true})
	if err != nil {
		t.Fatalf("create viewer: %v", err)
	}
	member, err := authSvc.CreateUser(ctx, "extension-member", "password123", true, false, "UTC", services.UserCapabilities{})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	e := echo.New()
	catalog := handlers.NewExtensionHandlers(`[{"id":"storage","title":"Storage","path":"/storage","uiToken":"launch-secret"}]`)
	e.GET(
		"/api/extensions",
		catalog.List,
		middleware.AuthMiddleware(authSvc),
		middleware.IssueSessionCookieOnBearer,
		middleware.RequireCapability(accessSvc, middleware.CapViewExtensions),
	)
	e.GET(
		"/api/extensions/:id/frame",
		catalog.Frame,
		middleware.BearerOrSessionCookie(authSvc),
		middleware.RequireCapability(accessSvc, middleware.CapViewExtensions),
	)

	for _, tc := range []struct {
		name     string
		username string
		userID   int64
		wantCode int
	}{
		{name: "admin is implicit", username: admin.Username, userID: admin.ID, wantCode: http.StatusOK},
		{name: "non-admin with flag", username: viewer.Username, userID: viewer.ID, wantCode: http.StatusOK},
		{name: "non-admin without flag", username: member.Username, userID: member.ID, wantCode: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, err := authSvc.Login(ctx, tc.username, "password123")
			if err != nil {
				t.Fatalf("login user %d: %v", tc.userID, err)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/extensions", nil)
			req.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("GET /api/extensions = %d; want %d (body: %s)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusOK && len(decodeExtensionList(t, rec)) != 1 {
				t.Fatal("authorized extension catalog did not return its registered item")
			}

			// A Bearer-authenticated catalog fetch issues the scoped session
			// cookie so the subsequent iframe navigation can reach /frame. The
			// recorder's Result().Cookies() is unreliable with Echo; assert the
			// raw Set-Cookie header, which is what a real browser stores.
			if tc.wantCode == http.StatusOK {
				sc := rec.Header().Get("Set-Cookie")
				if !strings.Contains(sc, "phoenix_session="+token) ||
					!strings.Contains(sc, "Path=/api/extensions") ||
					!strings.Contains(sc, "HttpOnly") {
					t.Errorf("catalog fetch did not issue the scoped phoenix_session cookie: %q", sc)
				}
			}

			// The iframe launch point carries the same gate. A real browser
			// iframe cannot send the Bearer header, so also cover the
			// cookie-only path the scoped session cookie enables.
			frameReq := httptest.NewRequest(http.MethodGet, "/api/extensions/storage/frame", nil)
			frameReq.Header.Set(echo.HeaderAuthorization, "Bearer "+token)
			frameRec := httptest.NewRecorder()
			e.ServeHTTP(frameRec, frameReq)
			wantFrame := tc.wantCode
			if wantFrame == http.StatusOK {
				wantFrame = http.StatusFound
			}
			if frameRec.Code != wantFrame {
				t.Errorf("GET frame with header = %d; want %d", frameRec.Code, wantFrame)
			}

			if tc.wantCode == http.StatusOK {
				cookieReq := httptest.NewRequest(http.MethodGet, "/api/extensions/storage/frame", nil)
				cookieReq.AddCookie(&http.Cookie{Name: middleware.SessionCookieName, Value: token})
				cookieRec := httptest.NewRecorder()
				e.ServeHTTP(cookieRec, cookieReq)
				if cookieRec.Code != http.StatusFound {
					t.Errorf("GET frame with cookie only = %d; want 302 (iframe launch)", cookieRec.Code)
				}
				if loc := cookieRec.Header().Get("Location"); loc != "/storage?ui_token=launch-secret" {
					t.Errorf("cookie frame Location = %q; want /storage?ui_token=launch-secret", loc)
				}
			}
		})
	}
}

// TestExtensionFrame_NavigationWithoutCredentialIsRejected pins the
// launch transport contract: GET /api/extensions/:id/frame is the surface
// that releases an entry's ui_token credential, so it must never answer a
// request with neither the Authorization header nor the scoped
// phoenix_session cookie. The cookie is the iframe's transport; a
// header-less, cookie-less request is exactly what an attacker probing the
// endpoint would send, and the only safe answer is 401, never a redirect
// carrying the token.
func TestExtensionFrame_NavigationWithoutCredentialIsRejected(t *testing.T) {
	ctx := context.Background()
	users := memory.NewUserRepo()
	apiKeys := memory.NewAPIKeyRepo()
	perms := memory.NewUserPermissionRepo()
	authenticator := auth.NewJWTAuthenticator("extension-frame-nav-key", 24, users)
	authSvc := services.NewAuthService(users, apiKeys, authenticator, auth.NewTOTPProvider("Phoenix"))
	accessSvc := services.NewAccessService(users, perms, nil, nil)

	if _, err := authSvc.CreateUser(ctx, "frame-admin", "password123", true, true, "UTC", services.UserCapabilities{}); err != nil {
		t.Fatalf("create admin: %v", err)
	}

	e := echo.New()
	e.GET(
		"/api/extensions/:id/frame",
		handlers.NewExtensionHandlers(`[{"id":"storage","title":"ECS Usage","path":"/storage","uiToken":"tok"}]`).Frame,
		middleware.BearerOrSessionCookie(authSvc),
		middleware.RequireCapability(accessSvc, middleware.CapViewExtensions),
	)

	// An iframe navigation carrying neither a Bearer header (impossible for
	// <iframe src>) nor the scoped session cookie: must be 401, never a
	// redirect that would leak the ui_token in the Location.
	req := httptest.NewRequest(http.MethodGet, "/api/extensions/storage/frame", nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("header-less frame navigation = %d; want 401", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Error("header-less frame navigation must not redirect (credential leak)")
	}
	if body := rec.Body.String(); !strings.Contains(body, "missing authorization header") {
		t.Errorf("body = %q; want the missing-authorization-header error", body)
	}
}

func TestExtensionHandlers_FrameRedirectsIntoExtensionPath(t *testing.T) {
	e := echo.New()
	h := handlers.NewExtensionHandlers(`[{"id":"storage","title":"Storage","path":"/storage"}]`)
	e.GET("/api/extensions/:id/frame", h.Frame)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/extensions/storage/frame", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("frame = %d; want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/storage" {
		t.Errorf("Location = %q; want /storage", loc)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q; want no-store", rec.Header().Get("Cache-Control"))
	}
}

func TestExtensionHandlers_FrameHandsOverLaunchCredential(t *testing.T) {
	e := echo.New()
	h := handlers.NewExtensionHandlers(`[{"id":"storage","title":"Storage","path":"/storage","uiToken":"s3cr3t-launch"}]`)
	e.GET("/api/extensions/:id/frame", h.Frame)
	e.GET("/api/extensions", h.List)

	// The redirect carries the credential as the ui_token query parameter the
	// extension accepts.
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/extensions/storage/frame", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("frame = %d; want 302", rec.Code)
	}
	want := "/storage?ui_token=s3cr3t-launch"
	if loc := rec.Header().Get("Location"); loc != want {
		t.Errorf("Location = %q; want %q", loc, want)
	}

	// The catalog response must never carry it.
	listRec := httptest.NewRecorder()
	e.ServeHTTP(listRec, httptest.NewRequest(http.MethodGet, "/api/extensions", nil))
	if body := listRec.Body.String(); strings.Contains(body, "s3cr3t-launch") || strings.Contains(body, "uiToken") {
		t.Errorf("catalog leaked the launch credential: %s", body)
	}
	got := decodeExtensionList(t, listRec)
	if len(got) != 1 || got[0].Path != "/storage" {
		t.Errorf("catalog = %#v; want the one storage entry", got)
	}
}

func TestExtensionHandlers_FrameUnknownIDIs404(t *testing.T) {
	e := echo.New()
	h := handlers.NewExtensionHandlers(`[{"id":"storage","title":"Storage","path":"/storage"}]`)
	e.GET("/api/extensions/:id/frame", h.Frame)

	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/extensions/nope/frame", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("frame for unknown id = %d; want 404", rec.Code)
	}
}

func TestExtensionHandlers_EmptyEnvReturnsEmptyArray(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n"} {
		rec := serveExtensions(t, raw)
		got := decodeExtensionList(t, rec)
		if len(got) != 0 {
			t.Errorf("PHOENIX_EXTENSIONS %q → %#v; want []", raw, got)
		}
		if rec.Body.String() != "[]\n" && rec.Body.String() != "[]" {
			t.Errorf("empty catalog body = %q; want []", rec.Body.String())
		}
	}
}

func TestExtensionHandlers_ValidJSONReturnsViews(t *testing.T) {
	raw := `[{"id":"ecs-usage","title":"Storage","path":"/storage"}]`
	got := decodeExtensionList(t, serveExtensions(t, raw))
	want := []handlers.ExtensionView{
		{ID: "ecs-usage", Title: "Storage", Path: "/storage", Icon: "/storage/icon.svg"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v; want %#v", got, want)
	}
}

func TestExtensionHandlers_ExplicitIconKept(t *testing.T) {
	raw := `[{"id":"ecs-usage","title":"Storage","path":"/storage","icon":"/storage/favicon.ico"}]`
	got := decodeExtensionList(t, serveExtensions(t, raw))
	if len(got) != 1 || got[0].Icon != "/storage/favicon.ico" {
		t.Errorf("icon = %#v; want /storage/favicon.ico", got)
	}
}

func TestExtensionHandlers_RejectsUnsafeIcon(t *testing.T) {
	for _, icon := range []string{
		"https://evil.example/x.png",
		"//evil.example/x.png",
		"javascript:alert(1)",
		"/storage/../secret.svg",
	} {
		raw := `[{"id":"ecs-usage","title":"Storage","path":"/storage","icon":"` + icon + `"}]`
		got := decodeExtensionList(t, serveExtensions(t, raw))
		if len(got) != 1 || got[0].Icon != "/storage/icon.svg" {
			t.Errorf("unsafe icon %q → %#v; want fallback /storage/icon.svg", icon, got)
		}
	}
}

func TestExtensionHandlers_ExtraKeysStripped(t *testing.T) {
	raw := `[{
		"id":"ecs-usage",
		"title":"Storage",
		"path":"/storage",
		"image":"ghcr.io/example/ecs-usage:1",
		"secretName":"ecs-usage-db",
		"credentials":{"password":"nope"}
	}]`
	rec := serveExtensions(t, raw)
	got := decodeExtensionList(t, rec)
	want := []handlers.ExtensionView{
		{ID: "ecs-usage", Title: "Storage", Path: "/storage", Icon: "/storage/icon.svg"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v; want %#v", got, want)
	}

	var asMaps []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &asMaps); err != nil {
		t.Fatalf("unmarshal as maps: %v", err)
	}
	if len(asMaps) != 1 {
		t.Fatalf("got %d objects; want 1", len(asMaps))
	}
	for key := range asMaps[0] {
		switch key {
		case "id", "title", "path", "icon":
		default:
			t.Errorf("unexpected wire key %q (image, secretName, credentials must not leak)", key)
		}
	}
	body := rec.Body.String()
	for _, forbidden := range []string{"image", "secretName", "secret_name", "credentials", "ghcr.io", "password"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("response leaked %q: %s", forbidden, body)
		}
	}
}

func TestExtensionHandlers_MalformedJSONServesEmptyList(t *testing.T) {
	for _, raw := range []string{
		`{not json`,
		`{"id":"ecs-usage"}`,
		`not-an-array`,
		`[{"id":1}]`,
	} {
		got := decodeExtensionList(t, serveExtensions(t, raw))
		if len(got) != 0 {
			t.Errorf("malformed %q → %#v; want []", raw, got)
		}
	}
}

func TestExtensionHandlers_NullJSONServesEmptyList(t *testing.T) {
	got := decodeExtensionList(t, serveExtensions(t, "null"))
	if len(got) != 0 {
		t.Errorf("null → %#v; want []", got)
	}
}

func TestExtensionHandlers_SkipsIncompleteRows(t *testing.T) {
	raw := `[{"id":"ok","title":"Ok","path":"/ok"},{"id":"","title":"X","path":"/x"},{"id":"y","title":"  ","path":"/y"},{"id":"z","title":"Z","path":""}]`
	got := decodeExtensionList(t, serveExtensions(t, raw))
	want := []handlers.ExtensionView{{ID: "ok", Title: "Ok", Path: "/ok", Icon: "/ok/icon.svg"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v; want %#v", got, want)
	}
}

func TestExtensionView_JSONTagsSnakeCase(t *testing.T) {
	typ := reflect.TypeOf(handlers.ExtensionView{})
	want := map[string]string{
		"ID":    "id",
		"Title": "title",
		"Path":  "path",
		"Icon":  "icon",
	}
	if typ.NumField() != len(want) {
		t.Fatalf("ExtensionView has %d fields; want only id, title, path, icon", typ.NumField())
	}
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		tag := f.Tag.Get("json")
		name, _, _ := strings.Cut(tag, ",")
		if name != want[f.Name] {
			t.Errorf("field %s json tag = %q; want %q", f.Name, name, want[f.Name])
		}
		if name == "" || name != strings.ToLower(name) || strings.ContainsAny(name, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") {
			t.Errorf("field %s json name %q is not snake_case", f.Name, name)
		}
	}
}
