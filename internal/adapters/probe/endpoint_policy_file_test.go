package probe

import (
	"strings"
	"testing"
)

func TestEndpointPolicyDocumentBounds(t *testing.T) {
	for _, raw := range []string{`null`, `{"allowed_cidrs":["127.0.0.1/8"]}`, `{"allowed_cidrs":["invalid"]}`, `{"allowed_cidrs":[],"allowed_cidrs":["0.0.0.0/0"]}`, `{"unknown":true}`, `{} {}`, strings.Repeat(" ", 65537)} {
		if _, err := DecodeEndpointPolicy(strings.NewReader(raw)); err == nil {
			t.Fatalf("invalid policy accepted: %.80s", raw)
		}
	}
	p, err := DecodeEndpointPolicy(strings.NewReader(`{"allowed_cidrs":["127.0.0.1/32","10.0.0.0/8"]}`))
	if err != nil || len(p.AllowedCIDRs) != 2 {
		t.Fatal("valid private policy rejected")
	}
	if p, err := LoadEndpointPolicy(""); err != nil || len(p.AllowedCIDRs) != 0 {
		t.Fatal("public default changed")
	}
}
