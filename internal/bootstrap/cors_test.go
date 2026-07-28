package bootstrap

import "testing"

// TestCORSConfigFromEnv_ProductionNeverShipsWildcard is the R2.5 assertion:
// whatever the env looks like, the production path must never fall back to
// AllowOrigins ["*"]. Before this gate existed, an unset CORS_ALLOW_ORIGINS
// silently shipped the dev wildcard to production.
func TestCORSConfigFromEnv_ProductionNeverShipsWildcard(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no origins configured", Config{Production: true}},
		{"whitespace-only origins", Config{Production: true, CORSAllowOrigins: " ,  , "}},
		{"explicit origins", Config{Production: true, CORSAllowOrigins: "https://phoenix.example.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := corsConfigFromEnv(tc.cfg)
			for _, o := range got.AllowOrigins {
				if o == "*" {
					t.Fatalf("production CORS config ships the wildcard origin: %v", got.AllowOrigins)
				}
			}
		})
	}
}

// TestCORSConfigFromEnv_ProductionDefaultDenies pins the deny-by-default
// shape: production with no configured origins must disable cross-origin
// entirely, not merely narrow the allow-list.
func TestCORSConfigFromEnv_ProductionDefaultDenies(t *testing.T) {
	got := corsConfigFromEnv(Config{Production: true})
	if !got.DisableCrossOrigin {
		t.Error("production with no CORS_ALLOW_ORIGINS must deny cross-origin by default")
	}
	if len(got.AllowOrigins) != 0 {
		t.Errorf("deny-by-default config carries AllowOrigins %v; want none", got.AllowOrigins)
	}
}

// TestCORSConfigFromEnv_ProductionExplicitOrigins ensures the operator
// override still works: configured origins pass through verbatim and the
// deny switch stays off.
func TestCORSConfigFromEnv_ProductionExplicitOrigins(t *testing.T) {
	got := corsConfigFromEnv(Config{
		Production:       true,
		CORSAllowOrigins: "https://a.example.com, https://b.example.com",
	})
	if got.DisableCrossOrigin {
		t.Error("explicitly configured origins must not disable cross-origin")
	}
	want := []string{"https://a.example.com", "https://b.example.com"}
	if len(got.AllowOrigins) != len(want) {
		t.Fatalf("AllowOrigins = %v; want %v", got.AllowOrigins, want)
	}
	for i := range want {
		if got.AllowOrigins[i] != want[i] {
			t.Errorf("AllowOrigins[%d] = %q; want %q", i, got.AllowOrigins[i], want[i])
		}
	}
}

// TestCORSConfigFromEnv_DevDefaultStaysPermissive documents that dev keeps
// the wildcard so local tooling (Vite on :5173) is unaffected.
func TestCORSConfigFromEnv_DevDefaultStaysPermissive(t *testing.T) {
	got := corsConfigFromEnv(Config{Production: false})
	if got.DisableCrossOrigin {
		t.Error("dev default must not disable cross-origin")
	}
	if len(got.AllowOrigins) != 1 || got.AllowOrigins[0] != "*" {
		t.Errorf("dev AllowOrigins = %v; want [*]", got.AllowOrigins)
	}
}
