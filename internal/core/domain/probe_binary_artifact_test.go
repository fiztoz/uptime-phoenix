package domain

import (
	"strings"
	"testing"
)

func TestProbeBinaryPin(t *testing.T) {
	p := ProbeBinaryPin{Version: "0.4.5-rc.1", Architecture: "arm64", SHA256: strings.Repeat("a", 64), Size: 123}
	if !p.Valid() || p.ReleaseName() != "uptime-phoenix-probe_0.4.5-rc.1_linux_arm64" {
		t.Fatal("release grammar mismatch")
	}
	for _, change := range []func(*ProbeBinaryPin){
		func(p *ProbeBinaryPin) { p.Version = "latest" }, func(p *ProbeBinaryPin) { p.Version = "v0.4.5" }, func(p *ProbeBinaryPin) { p.Version = "../0.4.5" },
		func(p *ProbeBinaryPin) { p.Version = strings.Repeat("1", 65) + ".1.1" }, func(p *ProbeBinaryPin) { p.Architecture = "386" },
		func(p *ProbeBinaryPin) { p.SHA256 = strings.Repeat("A", 64) }, func(p *ProbeBinaryPin) { p.SHA256 = strings.Repeat("z", 64) },
		func(p *ProbeBinaryPin) { p.SHA256 = "a" }, func(p *ProbeBinaryPin) { p.Size = 0 }, func(p *ProbeBinaryPin) { p.Size = -1 },
		func(p *ProbeBinaryPin) { p.Size = MaxProbeBinarySize + 1 },
	} {
		bad := p
		change(&bad)
		if bad.Valid() || bad.ReleaseName() != "" {
			t.Fatal("invalid pin accepted")
		}
	}
}
