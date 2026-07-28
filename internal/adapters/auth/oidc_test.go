package auth

import (
	"net/url"
	"reflect"
	"testing"

	"golang.org/x/oauth2"
)

func TestExtractStringSlice(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
	}{
		{"nil", nil, nil},
		{"string slice", []string{"a", " b "}, []string{"a", "b"}},
		{"any slice", []any{"x", 1, "y"}, []string{"x", "y"}},
		{"csv string", "a, b;c d", []string{"a", "b", "c", "d"}},
		{"empty string", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractStringSlice(tc.in)
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("got %v", got)
				}
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v want %v", got, tc.want)
			}
		})
	}
}

func TestOIDCProvider_AuthCodeURL_IncludesPKCES256(t *testing.T) {
	// Build a provider without discovery so we can assert authorize query params.
	p := &OIDCProvider{
		cfg: OIDCConfig{ClientID: "client", Issuer: "https://idp.example.com"},
		oauth2: oauth2.Config{
			ClientID:    "client",
			RedirectURL: "https://app.example.com/api/auth/oidc/callback",
			Endpoint: oauth2.Endpoint{
				AuthURL:  "https://idp.example.com/authorize",
				TokenURL: "https://idp.example.com/token",
			},
			Scopes: []string{"openid"},
		},
	}

	challenge := "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" // example S256 challenge
	raw := p.AuthCodeURL("signed.state", "nonce-value", challenge)
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	q := u.Query()
	if got := q.Get("code_challenge"); got != challenge {
		t.Fatalf("code_challenge = %q, want %q", got, challenge)
	}
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Fatalf("code_challenge_method = %q, want S256", got)
	}
	if got := q.Get("state"); got != "signed.state" {
		t.Fatalf("state = %q", got)
	}
	if got := q.Get("nonce"); got != "nonce-value" {
		t.Fatalf("nonce = %q", got)
	}
}
