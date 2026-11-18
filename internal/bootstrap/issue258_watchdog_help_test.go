package bootstrap

import (
	"bytes"
	"strings"
	"testing"
)

func TestIssue258WatchdogHelpExplainsHealthDeadline(t *testing.T) {
	var out, stderr bytes.Buffer
	if code := RunProbeAdmin(t.Context(), Config{}, []string{"--help"}, &out, &stderr); code != 0 {
		t.Fatalf("help exit = %d", code)
	}
	for _, want := range []string{"last healthy application-health frame, not disconnect", "75-95s"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("help missing %q", want)
		}
	}
}
