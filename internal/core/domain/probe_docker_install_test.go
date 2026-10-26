package domain

import (
	"slices"
	"strings"
	"testing"
)

func TestProbeInstallLayoutByMode(t *testing.T) {
	for mode, want := range map[string][2]string{
		ProbeInstallSystemd: {"/var/lib/uptime-phoenix-probe-", ".service"},
		ProbeInstallDocker:  {"/var/lib/uptime-phoenix-probe-", ""},
	} {
		r := installTestIdentity(mode)
		data, service, ok := ProbeInstallLayout(r)
		if !ok || data.Reference != want[0]+r.OperationID || service.Reference != "uptime-phoenix-probe-"+r.OperationID+want[1] {
			t.Fatalf("%s layout: %+v %+v %v", mode, data, service, ok)
		}
	}
	if _, _, ok := ProbeInstallLayout(ProbeInstallIdentity{}); ok {
		t.Fatal("invalid identity has a layout")
	}
}

func TestProbeDockerArgsAreDerivedAndHardened(t *testing.T) {
	r := installTestIdentity(ProbeInstallDocker)
	tag := ProbeDockerImageTag(r)
	if tag != "uptime-phoenix-probe:"+r.Image.ManifestSHA256 {
		t.Fatalf("tag %q", tag)
	}
	create, init := ProbeDockerCreateArgs(r), ProbeDockerInitArgs(r)
	data := "/var/lib/uptime-phoenix-probe-" + r.OperationID
	for name, args := range map[string][]string{"create": create, "init": init} {
		joined := strings.Join(args, " ")
		for _, want := range []string{
			"--user 0:0", "--cap-drop ALL", "--cap-add NET_RAW", "--security-opt no-new-privileges", "--read-only",
			"--mount type=bind,src=" + data + ",dst=" + ProbeDockerDataMount,
			"--label " + ProbeDockerOperationLabel + "=" + r.OperationID, tag,
		} {
			if !strings.Contains(joined, want) {
				t.Fatalf("%s args %q lack %q", name, joined, want)
			}
		}
		if slices.Contains(args, "--privileged") || strings.Contains(joined, "docker.sock") {
			t.Fatalf("%s args widen privileges: %q", name, joined)
		}
	}
	if create[0] != "create" || !slices.Contains(create, "--publish") || create[len(create)-3] != "run" {
		t.Fatalf("create args %q", create)
	}
	if init[0] != "run" || !slices.Contains(init, "--rm") || !slices.Contains(init, "none") || init[len(init)-3] != "init" {
		t.Fatalf("init args %q", init)
	}
	systemd := installTestIdentity(ProbeInstallSystemd)
	if ProbeDockerCreateArgs(systemd) != nil || ProbeDockerInitArgs(systemd) != nil || ProbeDockerImageTag(systemd) != "" {
		t.Fatal("a systemd identity derived docker arguments")
	}
}

func TestProbeHostActionPayloadValid(t *testing.T) {
	good := ProbeHostActionPayload{SHA256: strings.Repeat("a", 64), Size: 1024, LayoutIndexSHA256: strings.Repeat("b", 64)}
	if !good.Valid() {
		t.Fatal("good payload refused")
	}
	for name, mutate := range map[string]func(*ProbeHostActionPayload){
		"upper hex":   func(p *ProbeHostActionPayload) { p.SHA256 = strings.Repeat("A", 64) },
		"short index": func(p *ProbeHostActionPayload) { p.LayoutIndexSHA256 = "ab" },
		"zero size":   func(p *ProbeHostActionPayload) { p.Size = 0 },
		"over limit":  func(p *ProbeHostActionPayload) { p.Size = MaxProbeImageArchiveBytes + 1 },
	} {
		p := good
		mutate(&p)
		if p.Valid() {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestProbeDockerPreflightAndLifecycleIdentities(t *testing.T) {
	r := installTestIdentity(ProbeInstallDocker)
	machine := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[r.Image.Pin.Architecture]
	report := ProbeInstallHostReport{Kernel: "Linux", Machine: machine, DockerCLI: true, Root: true, Authority: true}
	if code := report.Refusal(r); code != "" {
		t.Fatalf("clean docker host refused: %q", code)
	}
	report.DockerCLI = false
	if code := report.Refusal(r); code != ProbeInstallPreflightDocker {
		t.Fatalf("docker missing: %q", code)
	}
	report.DockerCLI, report.Systemd = true, false
	if code := report.Refusal(r); code != "" {
		t.Fatalf("docker mode needs no systemd: %q", code)
	}
	activation := ProbeActivationIdentity{Install: r, ProbeKey: "edge", Name: "Edge", ListenPort: ProbeActivationDefaultPort, ClientKeyFingerprint: r.ClientKeyFingerprint, RequestedBy: 1}
	steps := activation.Steps()
	if !activation.Valid() || len(steps) != 4 || steps[0].Reference != "/var/lib/uptime-phoenix-probe-"+r.OperationID || steps[2].Reference != "uptime-phoenix-probe-"+r.OperationID {
		t.Fatalf("docker activation %v steps %+v", activation.Valid(), steps)
	}
	cleanup := ProbeCleanupIdentity{CleanupID: "c1111111-1111-4111-8111-111111111111", Install: r, ClientKeyFingerprint: r.ClientKeyFingerprint, RequestedBy: 1}
	if !cleanup.Valid() {
		t.Fatal("docker cleanup identity refused")
	}
}
