package services

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestValidateBrandAsset_AcceptsHTTPSAndDataURL(t *testing.T) {
	sp := &domain.StatusPage{
		Icon:    "https://cdn.example.com/logo.png",
		Favicon: "https://cdn.example.com/favicon.ico",
	}
	if err := validateStatusPageBranding(sp); err != nil {
		t.Fatal(err)
	}

	payload := base64.StdEncoding.EncodeToString([]byte("fakepng"))
	sp.Icon = "data:image/png;base64," + payload
	if err := validateStatusPageBranding(sp); err != nil {
		t.Fatal(err)
	}
}

func TestValidateBrandAsset_RejectsBadSchemeAndOversize(t *testing.T) {
	sp := &domain.StatusPage{Icon: "javascript:alert(1)"}
	if err := validateStatusPageBranding(sp); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("got %v", err)
	}

	sp.Icon = "ftp://x/y"
	if err := validateStatusPageBranding(sp); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("got %v", err)
	}

	sp.Icon = "data:text/plain;base64,abc"
	if err := validateStatusPageBranding(sp); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("got %v", err)
	}

	big := strings.Repeat("A", statusPageBrandAssetMaxDecoded+10)
	sp.Icon = "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(big))
	if err := validateStatusPageBranding(sp); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("got %v", err)
	}
}

func TestValidateBrandAsset_EmptyOK(t *testing.T) {
	if err := validateStatusPageBranding(&domain.StatusPage{}); err != nil {
		t.Fatal(err)
	}
}
