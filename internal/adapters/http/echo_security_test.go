package http_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	echomiddleware "github.com/labstack/echo/v4/middleware"

	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
)

// Exercise the real sign-in limiter, not just a non-nil configuration field.
// The legacy control deliberately re-enables the vulnerable upstream default.
func TestEchoCredentialIPWorkaround(t *testing.T) {
	for _, peer := range []string{"198.51.100.7", "10.1.2.3", "127.0.0.1", "2001:db8::7", "fd00::7", "fe80::7"} {
		for _, header := range []string{echo.HeaderXForwardedFor, echo.HeaderXRealIP} {
			for _, legacy := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/legacy=%t", peer, header, legacy), func(t *testing.T) {
					e := authRateLimitRouterWithOptions(t, httppkg.RouterOptions{})
					if e.IPExtractor == nil {
						t.Fatal("production hub must configure IPExtractor")
					}
					if legacy {
						e.IPExtractor = nil
					}
					for i := 0; i <= httppkg.SignInAttemptsPerMinute; i++ {
						req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{}`))
						req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
						req.Header.Set(header, fmt.Sprintf("203.0.113.%d", i+1))
						req.RemoteAddr = net.JoinHostPort(peer, "40000")
						rec := httptest.NewRecorder()
						e.ServeHTTP(rec, req)
						if i == httppkg.SignInAttemptsPerMinute && !legacy {
							if rec.Code != http.StatusTooManyRequests {
								t.Fatalf("rotating %s bypassed sign-in budget: status=%d", header, rec.Code)
							}
						} else if rec.Code < 400 || rec.Code >= 500 || rec.Code == http.StatusTooManyRequests {
							t.Fatalf("attempt %d: status=%d; want malformed-credential refusal without throttling", i+1, rec.Code)
						}
					}
				})
			}
		}
	}
}

// Phoenix does not install Echo's reverse-proxy middleware in production. Mount
// it only in this test to prove the configured extractor blocks the second
// advisory too, including its preservation of a spoofed X-Real-IP when unset.
func TestEchoProxyIPWorkaround(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(r.Header.Get(echo.HeaderXRealIP)))
	}))
	t.Cleanup(upstream.Close)
	target, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	for _, peer := range []string{"198.51.100.7", "10.1.2.3", "fd00::7"} {
		for _, legacy := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/legacy=%t", peer, legacy), func(t *testing.T) {
				e := authRateLimitRouterWithOptions(t, httppkg.RouterOptions{})
				if e.IPExtractor == nil {
					t.Fatal("production hub must configure IPExtractor")
				}
				if legacy {
					e.IPExtractor = nil
				}
				e.Group("/_echo_security_proxy", echomiddleware.Proxy(echomiddleware.NewRoundRobinBalancer([]*echomiddleware.ProxyTarget{{URL: target}})))
				req := httptest.NewRequest(http.MethodGet, "/_echo_security_proxy/check", nil)
				req.RemoteAddr = net.JoinHostPort(peer, "40000")
				req.Header.Set(echo.HeaderXForwardedFor, "203.0.113.99")
				req.Header.Set(echo.HeaderXRealIP, "203.0.113.100")
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, req)
				want := peer
				if legacy {
					want = "203.0.113.100"
				}
				if rec.Code != http.StatusOK || rec.Body.String() != want {
					t.Fatalf("upstream status=%d X-Real-IP=%q; want 200/%q", rec.Code, rec.Body.String(), want)
				}
			})
		}
	}
}

// The expiring scanner exceptions cover exactly these production constructors.
// A new constructor requires an explicit security review and behavioral tests;
// it must not silently inherit an application-wide exception for Echo defaults.
func TestEchoConstructorSecurityInventory(t *testing.T) {
	root := filepath.Clean("../../..")
	want := map[string]int{"internal/adapters/http/router.go": 1, "internal/adapters/probe/edge_server.go": 1}
	seen := make(map[string]int)
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") || strings.HasPrefix(entry.Name(), "_") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			aliases := make(map[string]bool)
			for _, imp := range file.Imports {
				importPath, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return err
				}
				if importPath != "github.com/labstack/echo/v4" {
					continue
				}
				alias := "echo"
				if imp.Name != nil {
					alias = imp.Name.Name
				}
				if alias == "." {
					return fmt.Errorf("Echo dot import requires security review: %s", path)
				}
				aliases[alias] = true
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			ast.Inspect(file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok || selector.Sel.Name != "New" {
					return true
				}
				identifier, ok := selector.X.(*ast.Ident)
				if ok && aliases[identifier.Name] {
					seen[rel]++
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for path, count := range seen {
		if want[path] != count {
			t.Errorf("unreviewed Echo constructor reference: %s count=%d", path, count)
		}
	}
	for path, count := range want {
		if seen[path] != count {
			t.Errorf("reviewed Echo constructor changed: %s count=%d; want %d", path, seen[path], count)
		}
	}
}
