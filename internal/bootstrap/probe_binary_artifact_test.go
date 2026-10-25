package bootstrap

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestProbeBinaryVerificationCLIRefusals(t *testing.T) {
	for _, args := range [][]string{nil, {"--file", "sensitive-path-canary"}, {"--unknown-sensitive-canary"}, {"--size", "sensitive-number-canary"}, {"--version", "latest"}} {
		var out, stderr bytes.Buffer
		if RunProbeBinaryVerification(t.Context(), args, &out, &stderr) != 2 || out.Len() != 0 || strings.Contains(stderr.String(), "canary") {
			t.Fatal("unsafe malformed response")
		}
	}
	var out, stderr bytes.Buffer
	if RunProbeBinaryVerification(t.Context(), []string{"--help"}, &out, &stderr) != 0 || stderr.Len() != 0 || !strings.Contains(out.String(), "NOT host compatibility") {
		t.Fatal("help failed")
	}
	args := []string{"--file", "sensitive-path-canary", "--version", "0.4.5", "--architecture", "amd64", "--sha256", strings.Repeat("a", 64), "--size", "1"}
	out.Reset()
	stderr.Reset()
	if RunProbeBinaryVerification(t.Context(), args, &out, &stderr) != 1 || out.Len() != 0 || strings.Contains(stderr.String(), "canary") {
		t.Fatal("source error escaped")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out.Reset()
	stderr.Reset()
	if RunProbeBinaryVerification(ctx, args, &out, &stderr) != 1 || !strings.Contains(stderr.String(), "canceled") {
		t.Fatal("cancellation ignored")
	}
}
