package domain

import (
	"encoding/base64"
	"strings"
	"testing"
)

func binaryStageDomainIntent() ProbeBinaryStageIntent {
	fp := "SHA256:" + base64.RawStdEncoding.EncodeToString(make([]byte, 32))
	return ProbeBinaryStageIntent{Identity: ProbeInstallIdentity{OperationID: "11111111-1111-4111-8111-111111111111", Host: ProbeSSHHost{Address: "example.test:22", User: "deploy"}, HostKeyFingerprint: fp, ClientKeyFingerprint: fp, RequestedBy: 1, LayoutVersion: 1, Mode: ProbeInstallSystemd, Binary: ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: strings.Repeat("a", 64), Size: 17}}, OwnerID: "22222222-2222-4222-8222-222222222222", AttemptID: "33333333-3333-4333-8333-333333333333"}
}

func TestProbeBinaryStageDomain(t *testing.T) {
	i := binaryStageDomainIntent()
	if !i.Valid() || i.Directory() != "/opt/uptime-phoenix-probe-"+i.Identity.OperationID {
		t.Fatal("canonical staging intent refused")
	}
	const want = "phoenix-binary-stage-v1\noperation_id=11111111-1111-4111-8111-111111111111\nowner_id=22222222-2222-4222-8222-222222222222\nattempt_id=33333333-3333-4333-8333-333333333333\nssh_address=example.test:22\nssh_user=deploy\nhost_key=SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\nclient_key=SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA\nrequested_by=1\nlayout_version=1\nmode=systemd\nversion=1.2.3\narchitecture=amd64\nsha256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\nsize=17\n"
	if i.Marker() != want || len(i.MarkerSHA256()) != 64 {
		t.Fatal("marker wire bytes changed")
	}
	for _, tc := range []struct {
		name   string
		change func(*ProbeBinaryStageIntent)
	}{
		{"owner", func(i *ProbeBinaryStageIntent) { i.OwnerID = "../escape" }},
		{"attempt", func(i *ProbeBinaryStageIntent) { i.AttemptID = "" }},
		{"operation", func(i *ProbeBinaryStageIntent) { i.Identity.OperationID = "BAD" }},
		{"mode", func(i *ProbeBinaryStageIntent) { i.Identity.Mode = ProbeInstallDocker }},
		{"hidden_image", func(i *ProbeBinaryStageIntent) { i.Identity.Image.Pin.Version = "1.2.3" }},
		{"host", func(i *ProbeBinaryStageIntent) { i.Identity.Host.Address = "example.test:22\npath=x" }},
		{"version", func(i *ProbeBinaryStageIntent) { i.Identity.Binary.Version = "../../escape" }},
		{"layout", func(i *ProbeBinaryStageIntent) { i.Identity.LayoutVersion = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := i
			tc.change(&bad)
			if bad.Valid() || bad.Marker() != "" || bad.Directory() != "" || bad.MarkerSHA256() != "" {
				t.Fatal("invalid intent produced a path/marker")
			}
		})
	}
	for _, change := range []func(*ProbeBinaryStageIntent){
		func(i *ProbeBinaryStageIntent) { i.OwnerID = "44444444-4444-4444-8444-444444444444" },
		func(i *ProbeBinaryStageIntent) { i.AttemptID = "55555555-5555-4555-8555-555555555555" },
		func(i *ProbeBinaryStageIntent) { i.Identity.Host.Address = "other.test:2222" },
		func(i *ProbeBinaryStageIntent) { i.Identity.Host.User = "another" },
		func(i *ProbeBinaryStageIntent) { i.Identity.RequestedBy = 2 },
		func(i *ProbeBinaryStageIntent) { i.Identity.Binary.Architecture = "arm64" },
		func(i *ProbeBinaryStageIntent) { i.Identity.Binary.Size++ },
		func(i *ProbeBinaryStageIntent) { i.Identity.Binary.SHA256 = strings.Repeat("b", 64) },
	} {
		changed := i
		change(&changed)
		if !changed.Valid() || changed.MarkerSHA256() == i.MarkerSHA256() {
			t.Fatal("marker lost immutable tuple field")
		}
	}
	max := i
	max.Identity.Host.Address = strings.Repeat(strings.Repeat("a", 63)+".", 3) + strings.Repeat("b", 61) + ":65535"
	max.Identity.Host.User = strings.Repeat("a", 64)
	max.Identity.Binary.Version = "1.2.3-" + strings.Repeat("a", 58)
	max.Identity.Binary.Size = MaxProbeBinarySize
	if !max.Valid() || len(max.Marker()) > MaxProbeBinaryStageMarkerSize {
		t.Fatal("maximum valid intent exceeded marker budget")
	}
	for _, state := range []string{ProbeBinaryStageAbsent, ProbeBinaryStageConflict, ProbeBinaryStageContentMatches} {
		if !(ProbeBinaryStageObservation{Intent: i, State: state}).Valid() {
			t.Fatal("valid observation refused")
		}
	}
	for _, state := range []string{"", "applied", "owned", "installed", ProbeInstallResourcesReady} {
		if (ProbeBinaryStageObservation{Intent: i, State: state}).Valid() {
			t.Fatal("observation invented authority")
		}
	}
}
