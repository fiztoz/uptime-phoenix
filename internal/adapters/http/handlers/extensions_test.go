package handlers_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
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

func TestExtensionHandlers_EmptyEnvReturnsEmptyArray(t *testing.T) {
	for _, raw := range []string{"", "   ", "\n"} {
		rec := serveExtensions(t, raw)
		got := decodeExtensionList(t, rec)
		if len(got) != 0 {
			t.Errorf("PHOENIX_EXTENSIONS %q → %#v; want []", raw, got)
		}
		if rec.Body.String() != "[]\n" && rec.Body.String() != "[]" {
			t.Errorf("empty catalogue body = %q; want []", rec.Body.String())
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
