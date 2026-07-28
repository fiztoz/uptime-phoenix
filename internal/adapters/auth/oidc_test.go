package auth

import (
	"reflect"
	"testing"
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
