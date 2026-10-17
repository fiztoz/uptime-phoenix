package domain

import (
	"strings"
	"testing"
)

func TestProbeImagePinValidation(t *testing.T) {
	base := ProbeImagePin{Repository: "ghcr.io/example/uptime-phoenix-probe", Version: "1.2.3-rc.1", Architecture: "amd64", IndexSHA256: strings.Repeat("a", 64), IndexSize: 100}
	if !base.Valid() {
		t.Fatal("valid pin refused")
	}
	for _, repo := range []string{"localhost/probe", "localhost:5000/probe", "127.0.0.1:5000/org/probe", "[::1]:5000/org/probe", "registry.example/org/a__b-c.d"} {
		p := base
		p.Repository = repo
		if !p.Valid() {
			t.Fatalf("qualified repository refused: %s", repo)
		}
	}
	for _, repo := range []string{"probe", "org/probe", "registry/probe", "https://ghcr.io/org/probe", "GHCR.IO/org/probe", "ghcr.io/Org/probe", "ghcr.io/org/probe:latest", "ghcr.io/org/probe@sha256:" + base.IndexSHA256, "ghcr.io//probe", "ghcr.io/../probe", "ghcr.io/-probe", "ghcr.io/_probe", "localhost:/probe", "localhost:0/probe", "localhost:05000/probe", "ghcr.io:65536/probe", "user@ghcr.io/probe", "ghcr.io/probe?x", "ghcr.io/probe#x", "ghcr.io/probe\n", "[fe80::1%25en0]/probe"} {
		t.Run(repo, func(t *testing.T) {
			p := base
			p.Repository = repo
			if p.Valid() {
				t.Fatal("mutable/ambiguous repository accepted")
			}
		})
	}
	for name, change := range map[string]func(*ProbeImagePin){"version": func(p *ProbeImagePin) { p.Version = "v1.2.3" }, "version_length": func(p *ProbeImagePin) { p.Version = "1.2.3-" + strings.Repeat("a", 60) }, "architecture": func(p *ProbeImagePin) { p.Architecture = "x86_64" }, "uppercase_digest": func(p *ProbeImagePin) { p.IndexSHA256 = strings.Repeat("A", 64) }, "bad_digest": func(p *ProbeImagePin) { p.IndexSHA256 = strings.Repeat("z", 64) }, "zero_size": func(p *ProbeImagePin) { p.IndexSize = 0 }, "limit": func(p *ProbeImagePin) { p.IndexSize = MaxProbeImageMetadataSize + 1 }} {
		t.Run(name, func(t *testing.T) {
			p := base
			change(&p)
			if p.Valid() {
				t.Fatal("invalid pin accepted")
			}
		})
	}
	s := ProbeImageSelection{Pin: base, ManifestSHA256: strings.Repeat("b", 64), ConfigSHA256: strings.Repeat("c", 64), LayerCount: 1}
	if s.Reference() != base.Repository+"@sha256:"+s.ManifestSHA256 {
		t.Fatal("not exact child reference")
	}
	s.LayerCount = 0
	if s.Valid() || s.Reference() != "" {
		t.Fatal("invalid output formed a reference")
	}
	s.LayerCount = MaxProbeImageLayers + 1
	if s.Valid() {
		t.Fatal("layer count unbounded")
	}
	s.LayerCount = 1
	s.ConfigSHA256 = "bad"
	if s.Valid() {
		t.Fatal("config digest unchecked")
	}
	s.ConfigSHA256 = strings.Repeat("c", 64)
	s.ManifestSHA256 = "bad"
	if s.Valid() {
		t.Fatal("manifest digest unchecked")
	}
}
