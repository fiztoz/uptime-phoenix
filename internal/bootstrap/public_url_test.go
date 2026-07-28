package bootstrap

import "testing"

func TestValidatePublicURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		wantErr bool
	}{
		{"", false},
		{"https://status.example.com", false},
		{"http://localhost:3000", false},
		{"https://status.example.com/", false},
		{"ftp://x", true},
		{"not-a-url", true},
		{"//no-scheme", true},
	}
	for _, tc := range cases {
		err := validatePublicURL(tc.in)
		if tc.wantErr && err == nil {
			t.Errorf("validatePublicURL(%q) want error", tc.in)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("validatePublicURL(%q) unexpected error: %v", tc.in, err)
		}
	}
}
