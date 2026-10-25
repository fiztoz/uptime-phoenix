package domain

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Independent literals: a changed constant, signing domain or layout path must
// not silently change what these tests accept.
const (
	hostActDomainOperationID = "11111111-1111-4111-8111-111111111111"
	hostActDomainOwnerID     = "22222222-2222-4222-8222-222222222222"
	hostActDomainAttemptID   = "33333333-3333-4333-8333-333333333333"
	hostActDomainOtherID     = "44444444-4444-4444-8444-444444444444"

	hostActDomainSigningDomain = "phoenix-host-action-admission-v1\x00"
	hostActDomainStageDomain   = "phoenix-stage-admission-v1\x00"

	hostActDomainDataDir  = "/var/lib/uptime-phoenix-probe-" + hostActDomainOperationID
	hostActDomainArtifact = "/opt/uptime-phoenix-probe-" + hostActDomainOperationID
	hostActDomainUnitPath = "/etc/systemd/system/uptime-phoenix-probe-" + hostActDomainOperationID + ".service"
	hostActDomainUnitName = "uptime-phoenix-probe-" + hostActDomainOperationID + ".service"
	hostActDomainRelease  = "uptime-phoenix-probe_1.2.3_linux_amd64"
)

func hostActDomainFingerprint(b byte) string {
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(bytes.Repeat([]byte{b}, 32))
}

func hostActDomainIdentity() ProbeInstallIdentity {
	return ProbeInstallIdentity{
		OperationID:          hostActDomainOperationID,
		Host:                 ProbeSSHHost{Address: "example.test:22", User: "root"},
		HostKeyFingerprint:   hostActDomainFingerprint(0),
		ClientKeyFingerprint: hostActDomainFingerprint(1),
		RequestedBy:          7,
		LayoutVersion:        ProbeInstallLayoutVersion,
		Mode:                 ProbeInstallSystemd,
		Binary:               ProbeBinaryPin{Version: "1.2.3", Architecture: "amd64", SHA256: strings.Repeat("a", 64), Size: 17},
	}
}

func hostActDomainIntent(kind string) ProbeHostActionIntent {
	return ProbeHostActionIntent{Identity: hostActDomainIdentity(), OwnerID: hostActDomainOwnerID, AttemptID: hostActDomainAttemptID, Kind: kind}
}

var hostActDomainKinds = []string{ProbeHostActionDataDirectory, ProbeHostActionUnitFile, ProbeHostActionService}

type hostActDomainAuthority struct {
	priv  ed25519.PrivateKey
	raw   [32]byte
	keyID string
}

func hostActDomainNewAuthority(t *testing.T) hostActDomainAuthority {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	a := hostActDomainAuthority{priv: priv}
	copy(a.raw[:], pub)
	a.keyID = ProbeBinaryStageAuthorityKeyID(a.raw)
	return a
}

func (a hostActDomainAuthority) challenge(marker string) ProbeBinaryStageChallenge {
	return ProbeBinaryStageChallenge{AuthorityKeyID: a.keyID, Nonce: [32]byte{1, 2, 3}, MarkerSHA256: marker}
}

func (a hostActDomainAuthority) request(intent ProbeHostActionIntent) ProbeHostActionAdmissionRequest {
	return ProbeHostActionAdmissionRequest{Intent: intent, Challenge: a.challenge(intent.MarkerSHA256())}
}

func (a hostActDomainAuthority) grant(request ProbeHostActionAdmissionRequest) ProbeHostActionGrant {
	grant := ProbeHostActionGrant{Request: request}
	copy(grant.Signature[:], ed25519.Sign(a.priv, request.SigningMessage()))
	return grant
}

func TestProbeHostActionIntentValidAcceptsOnlyRootSystemdAndThreeKinds(t *testing.T) {
	for _, kind := range hostActDomainKinds {
		if !hostActDomainIntent(kind).Valid() {
			t.Fatalf("canonical %s intent refused", kind)
		}
	}
	if got := []string{ProbeHostActionDataDirectory, ProbeHostActionUnitFile, ProbeHostActionService}; !reflect.DeepEqual(got, []string{"data_directory", "unit_file", "service"}) {
		t.Fatalf("kind constants drifted: %v", got)
	}

	// artifact_directory is a stage grant, never a host action; docker kinds,
	// near-miss spellings and empty kinds are refused.
	for _, kind := range []string{
		"artifact_directory", "image_cache", "container", "", "Data_Directory", "DATA_DIRECTORY", " data_directory",
		"data_directory ", "service\n", "unit_file\x00", "unit-file", "services", "systemd",
	} {
		if (hostActDomainIntent(kind)).Valid() {
			t.Fatalf("kind %q accepted", kind)
		}
	}

	cases := map[string]func(*ProbeHostActionIntent){
		"docker mode, systemd binary retained": func(i *ProbeHostActionIntent) { i.Identity.Mode = ProbeInstallDocker },
		"empty mode":                           func(i *ProbeHostActionIntent) { i.Identity.Mode = "" },
		"non-root deploy":                      func(i *ProbeHostActionIntent) { i.Identity.Host.User = "deploy" },
		"non-root capitalized":                 func(i *ProbeHostActionIntent) { i.Identity.Host.User = "Root" },
		"non-root trailing space":              func(i *ProbeHostActionIntent) { i.Identity.Host.User = "root " },
		"empty ssh user":                       func(i *ProbeHostActionIntent) { i.Identity.Host.User = "" },
		"empty owner":                          func(i *ProbeHostActionIntent) { i.OwnerID = "" },
		"malformed owner":                      func(i *ProbeHostActionIntent) { i.OwnerID = "../escape" },
		"uppercase owner":                      func(i *ProbeHostActionIntent) { i.OwnerID = "AAAAAAAA-AAAA-4AAA-8AAA-AAAAAAAAAAAA" },
		"nil uuid owner":                       func(i *ProbeHostActionIntent) { i.OwnerID = "00000000-0000-0000-0000-000000000000" },
		"empty attempt":                        func(i *ProbeHostActionIntent) { i.AttemptID = "" },
		"malformed attempt":                    func(i *ProbeHostActionIntent) { i.AttemptID = "not-an-id" },
		"bad operation":                        func(i *ProbeHostActionIntent) { i.Identity.OperationID = "BAD" },
		"bad layout":                           func(i *ProbeHostActionIntent) { i.Identity.LayoutVersion = ProbeInstallLayoutVersion + 1 },
		"zero actor":                           func(i *ProbeHostActionIntent) { i.Identity.RequestedBy = 0 },
		"bad host key pin":                     func(i *ProbeHostActionIntent) { i.Identity.HostKeyFingerprint = "SHA256:short" },
		"bad client key pin":                   func(i *ProbeHostActionIntent) { i.Identity.ClientKeyFingerprint = "" },
		"address with newline":                 func(i *ProbeHostActionIntent) { i.Identity.Host.Address = "example.test:22\npath=x" },
		"invalid binary pin":                   func(i *ProbeHostActionIntent) { i.Identity.Binary.Size = 0 },
		"path in binary version":               func(i *ProbeHostActionIntent) { i.Identity.Binary.Version = "../../escape" },
		"hidden image on a systemd identity":   func(i *ProbeHostActionIntent) { i.Identity.Image.Pin.Version = "1.2.3" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			bad := hostActDomainIntent(ProbeHostActionService)
			mutate(&bad)
			if bad.Valid() {
				t.Fatal("invalid intent accepted")
			}
			if _, ok := bad.Resource(); ok || bad.Marker() != "" || bad.MarkerSHA256() != "" {
				t.Fatal("invalid intent produced a resource, marker or marker digest")
			}
			if req := (ProbeHostActionAdmissionRequest{Intent: bad, Challenge: ProbeBinaryStageChallenge{AuthorityKeyID: strings.Repeat("a", 64), Nonce: [32]byte{1}, MarkerSHA256: strings.Repeat("b", 64)}}); req.Valid() || req.SigningMessage() != nil {
				t.Fatal("invalid intent produced an admissible request")
			}
		})
	}

	t.Run("docker identities accept only their own kinds", func(t *testing.T) {
		docker := installTestIdentity(ProbeInstallDocker)
		if !docker.Valid() || docker.Host.User != "root" {
			t.Fatalf("fixture docker identity must be valid and root: %+v", docker)
		}
		payload := ProbeHostActionPayload{SHA256: strings.Repeat("a", 64), Size: 4096, LayoutIndexSHA256: strings.Repeat("b", 64)}
		intent := func(kind string, p ProbeHostActionPayload) ProbeHostActionIntent {
			return ProbeHostActionIntent{Identity: docker, OwnerID: hostActDomainOwnerID, AttemptID: hostActDomainAttemptID, Kind: kind, Payload: p}
		}
		for _, kind := range []string{ProbeHostActionDataDirectory, ProbeHostActionContainer, ProbeActivationProbeInit, ProbeActivationServiceStart, ProbeHostActionCleanup} {
			if i := intent(kind, ProbeHostActionPayload{}); !i.Valid() || i.Marker() == "" {
				t.Fatalf("docker %q refused", kind)
			}
			if intent(kind, payload).Valid() {
				t.Fatalf("docker %q accepted a payload", kind)
			}
		}
		if i := intent(ProbeHostActionImageCache, payload); !i.Valid() || !strings.Contains(i.Marker(), "payload_sha256="+payload.SHA256+"\n") {
			t.Fatalf("image_cache with payload refused or unbound: %q", i.Marker())
		}
		if intent(ProbeHostActionImageCache, ProbeHostActionPayload{}).Valid() {
			t.Fatal("image_cache without a payload accepted")
		}
		for _, kind := range []string{ProbeHostActionUnitFile, ProbeHostActionService} {
			if i := intent(kind, ProbeHostActionPayload{}); i.Valid() || i.Marker() != "" {
				t.Fatalf("systemd-only %q accepted for docker", kind)
			}
		}
		// The systemd identity refuses the docker-only kinds.
		systemd := hostActDomainIntent(ProbeHostActionService)
		for _, kind := range []string{ProbeHostActionImageCache, ProbeHostActionContainer} {
			systemd.Kind, systemd.Payload = kind, payload
			if systemd.Valid() {
				t.Fatalf("systemd %q accepted", kind)
			}
		}
		// The container marker binds the exact derived docker arguments.
		c := intent(ProbeHostActionContainer, ProbeHostActionPayload{})
		other := c
		other.Identity.Image.ManifestSHA256 = strings.Repeat("c", 64)
		if !other.Valid() {
			t.Fatalf("fixture mutation must stay valid: %+v", other.Identity.Image)
		}
		if c.MarkerSHA256() == other.MarkerSHA256() || !strings.Contains(c.Marker(), "container_sha256=") {
			t.Fatal("container marker does not bind the image selection and arguments")
		}
	})

	t.Run("zero intent", func(t *testing.T) {
		var zero ProbeHostActionIntent
		if zero.Valid() || zero.Marker() != "" || zero.MarkerSHA256() != "" {
			t.Fatal("zero intent accepted")
		}
	})
}

func TestProbeHostActionIntentResourceIsIdentityDerived(t *testing.T) {
	identity := hostActDomainIdentity()
	layout := identity.Resources()
	wantRef := map[string]string{
		ProbeHostActionDataDirectory: hostActDomainDataDir,
		ProbeHostActionUnitFile:      hostActDomainUnitPath,
		ProbeHostActionService:       hostActDomainUnitName,
	}
	wantOrdinal := map[string]int{ProbeHostActionDataDirectory: 1, ProbeHostActionUnitFile: 3, ProbeHostActionService: 4}
	for _, kind := range hostActDomainKinds {
		spec, ok := hostActDomainIntent(kind).Resource()
		if !ok || spec.Kind != kind || spec.Reference != wantRef[kind] || spec.Ordinal != wantOrdinal[kind] || !spec.Owned {
			t.Fatalf("%s resource %+v ok=%v", kind, spec, ok)
		}
		if spec != layout[spec.Ordinal-1] {
			t.Fatalf("%s resource is not the identity-derived layout entry", kind)
		}
	}
	// The artifact directory sits between the data directory and the unit file
	// in the layout; no host action may resolve to it.
	if layout[1].Kind != "artifact_directory" || layout[1].Reference != hostActDomainArtifact {
		t.Fatalf("fixture layout drifted: %+v", layout)
	}
}

func TestProbeHostActionMarkerCanonicalBytes(t *testing.T) {
	common := func(kind, reference string) []string {
		return []string{
			"phoenix-host-action-v1",
			"kind=" + kind,
			"reference=" + reference,
			"operation_id=" + hostActDomainOperationID,
			"owner_id=" + hostActDomainOwnerID,
			"attempt_id=" + hostActDomainAttemptID,
			"ssh_address=example.test:22",
			"ssh_user=root",
			"host_key=" + hostActDomainFingerprint(0),
			"client_key=" + hostActDomainFingerprint(1),
			"requested_by=7",
			"layout_version=1",
			"mode=systemd",
			"version=1.2.3",
			"architecture=amd64",
			"sha256=" + strings.Repeat("a", 64),
			"size=17",
		}
	}
	unitDigest := sha256.Sum256([]byte(ProbeSystemdUnit(hostActDomainIdentity())))
	want := map[string]string{
		ProbeHostActionDataDirectory: strings.Join(append(common(ProbeHostActionDataDirectory, hostActDomainDataDir), ""), "\n"),
		ProbeHostActionService:       strings.Join(append(common(ProbeHostActionService, hostActDomainUnitName), ""), "\n"),
		ProbeHostActionUnitFile: strings.Join(append(common(ProbeHostActionUnitFile, hostActDomainUnitPath),
			"unit_sha256="+hex.EncodeToString(unitDigest[:]), ""), "\n"),
	}
	seen := map[string]string{}
	for _, kind := range hostActDomainKinds {
		intent := hostActDomainIntent(kind)
		marker := intent.Marker()
		if marker != want[kind] {
			t.Fatalf("%s marker bytes changed:\n%s\nwant:\n%s", kind, marker, want[kind])
		}
		if !strings.HasSuffix(marker, "\n") || strings.HasSuffix(marker, "\n\n") || strings.Contains(marker, "\r") {
			t.Fatalf("%s marker is not LF-terminated canonical text", kind)
		}
		digest := sha256.Sum256([]byte(marker))
		if got := intent.MarkerSHA256(); got != hex.EncodeToString(digest[:]) || len(got) != 64 || got != strings.ToLower(got) {
			t.Fatalf("%s marker digest %q", kind, got)
		}
		if other, dup := seen[intent.MarkerSHA256()]; dup {
			t.Fatalf("%s and %s share a marker digest", kind, other)
		}
		seen[intent.MarkerSHA256()] = kind
	}
	for kind, marker := range want {
		hasUnitDigest := strings.Contains(marker, "unit_sha256=")
		if hasUnitDigest != (kind == ProbeHostActionUnitFile) {
			t.Fatalf("%s: unit_sha256 present=%v", kind, hasUnitDigest)
		}
	}
}

func TestProbeHostActionMarkerDiffersFromBinaryStageMarker(t *testing.T) {
	stage := ProbeBinaryStageIntent{Identity: hostActDomainIdentity(), OwnerID: hostActDomainOwnerID, AttemptID: hostActDomainAttemptID}
	if !stage.Valid() || stage.MarkerSHA256() == "" {
		t.Fatal("fixture stage intent invalid")
	}
	for _, kind := range hostActDomainKinds {
		intent := hostActDomainIntent(kind)
		if intent.Marker() == stage.Marker() || intent.MarkerSHA256() == stage.MarkerSHA256() {
			t.Fatalf("%s marker equals the binary-stage marker for the same identity", kind)
		}
		if strings.HasPrefix(intent.Marker(), "phoenix-binary-stage-v1") || !strings.HasPrefix(intent.Marker(), "phoenix-host-action-v1\n") {
			t.Fatalf("%s marker version line %q", kind, strings.SplitN(intent.Marker(), "\n", 2)[0])
		}
	}
}

func TestProbeHostActionMarkerBindsEveryField(t *testing.T) {
	mutations := map[string]func(*ProbeHostActionIntent){
		"operation":   func(i *ProbeHostActionIntent) { i.Identity.OperationID = hostActDomainOtherID },
		"owner":       func(i *ProbeHostActionIntent) { i.OwnerID = hostActDomainOtherID },
		"attempt":     func(i *ProbeHostActionIntent) { i.AttemptID = hostActDomainOtherID },
		"address":     func(i *ProbeHostActionIntent) { i.Identity.Host.Address = "other.test:22" },
		"host key":    func(i *ProbeHostActionIntent) { i.Identity.HostKeyFingerprint = hostActDomainFingerprint(9) },
		"client key":  func(i *ProbeHostActionIntent) { i.Identity.ClientKeyFingerprint = hostActDomainFingerprint(9) },
		"actor":       func(i *ProbeHostActionIntent) { i.Identity.RequestedBy++ },
		"version":     func(i *ProbeHostActionIntent) { i.Identity.Binary.Version = "1.2.4" },
		"arch":        func(i *ProbeHostActionIntent) { i.Identity.Binary.Architecture = "arm64" },
		"binary hash": func(i *ProbeHostActionIntent) { i.Identity.Binary.SHA256 = strings.Repeat("b", 64) },
		"binary size": func(i *ProbeHostActionIntent) { i.Identity.Binary.Size++ },
	}
	for _, kind := range hostActDomainKinds {
		base := hostActDomainIntent(kind)
		for name, mutate := range mutations {
			changed := base
			mutate(&changed)
			if !changed.Valid() {
				t.Fatalf("%s/%s: mutation made the intent invalid; fixture is inert", kind, name)
			}
			if changed.Marker() == base.Marker() || changed.MarkerSHA256() == base.MarkerSHA256() {
				t.Fatalf("%s: marker ignores %s", kind, name)
			}
		}
	}
}

func TestProbeHostActionUnitFileMarkerBindsDerivedUnitDigest(t *testing.T) {
	unitSHALine := func(intent ProbeHostActionIntent) string {
		for _, line := range strings.Split(intent.Marker(), "\n") {
			if strings.HasPrefix(line, "unit_sha256=") {
				return line
			}
		}
		return ""
	}
	base := hostActDomainIntent(ProbeHostActionUnitFile)
	baseLine := unitSHALine(base)
	digest := sha256.Sum256([]byte(ProbeSystemdUnit(base.Identity)))
	if baseLine != "unit_sha256="+hex.EncodeToString(digest[:]) {
		t.Fatalf("unit_sha256 line %q does not match the derived unit", baseLine)
	}

	// The unit text names the release, so the digest follows the binary pin's
	// version and architecture and the operation id.
	for name, mutate := range map[string]func(*ProbeHostActionIntent){
		"version":   func(i *ProbeHostActionIntent) { i.Identity.Binary.Version = "1.2.4" },
		"arch":      func(i *ProbeHostActionIntent) { i.Identity.Binary.Architecture = "arm64" },
		"operation": func(i *ProbeHostActionIntent) { i.Identity.OperationID = hostActDomainOtherID },
	} {
		changed := base
		mutate(&changed)
		if got := unitSHALine(changed); got == "" || got == baseLine {
			t.Fatalf("unit_sha256 does not follow the binary pin's %s: %q", name, got)
		}
		if changed.MarkerSHA256() == base.MarkerSHA256() {
			t.Fatalf("marker digest ignores %s", name)
		}
	}

	// Bytes the unit text does not contain cannot move unit_sha256, but the
	// marker still binds them through its own lines.
	for name, mutate := range map[string]func(*ProbeHostActionIntent){
		"binary hash": func(i *ProbeHostActionIntent) { i.Identity.Binary.SHA256 = strings.Repeat("b", 64) },
		"binary size": func(i *ProbeHostActionIntent) { i.Identity.Binary.Size++ },
		"owner":       func(i *ProbeHostActionIntent) { i.OwnerID = hostActDomainOtherID },
	} {
		changed := base
		mutate(&changed)
		if unitSHALine(changed) != baseLine {
			t.Fatalf("unit_sha256 moved with %s although the unit text does not contain it", name)
		}
		if changed.MarkerSHA256() == base.MarkerSHA256() {
			t.Fatalf("marker ignores %s", name)
		}
	}
}

func TestProbeHostActionAdmissionRequestBindsMarkerAndKind(t *testing.T) {
	authority := hostActDomainNewAuthority(t)
	for _, kind := range hostActDomainKinds {
		request := authority.request(hostActDomainIntent(kind))
		if !request.Valid() || len(request.SigningMessage()) == 0 {
			t.Fatalf("%s: canonical request refused", kind)
		}
	}
	request := authority.request(hostActDomainIntent(ProbeHostActionService))
	for name, mutate := range map[string]func(*ProbeHostActionAdmissionRequest){
		"zero nonce": func(r *ProbeHostActionAdmissionRequest) { r.Challenge.Nonce = [32]byte{} },
		"upper authority": func(r *ProbeHostActionAdmissionRequest) {
			r.Challenge.AuthorityKeyID = strings.ToUpper(r.Challenge.AuthorityKeyID)
		},
		"short authority":   func(r *ProbeHostActionAdmissionRequest) { r.Challenge.AuthorityKeyID = "a" },
		"non-hex authority": func(r *ProbeHostActionAdmissionRequest) { r.Challenge.AuthorityKeyID = strings.Repeat("g", 64) },
		"zero marker":       func(r *ProbeHostActionAdmissionRequest) { r.Challenge.MarkerSHA256 = strings.Repeat("0", 64) },
		"empty marker":      func(r *ProbeHostActionAdmissionRequest) { r.Challenge.MarkerSHA256 = "" },
		"other kind's marker": func(r *ProbeHostActionAdmissionRequest) {
			r.Challenge.MarkerSHA256 = hostActDomainIntent(ProbeHostActionDataDirectory).MarkerSHA256()
		},
		"binary-stage marker": func(r *ProbeHostActionAdmissionRequest) {
			r.Challenge.MarkerSHA256 = ProbeBinaryStageIntent{Identity: r.Intent.Identity, OwnerID: r.Intent.OwnerID, AttemptID: r.Intent.AttemptID}.MarkerSHA256()
		},
		"kind changed after marker": func(r *ProbeHostActionAdmissionRequest) { r.Intent.Kind = ProbeHostActionDataDirectory },
		"artifact_directory kind":   func(r *ProbeHostActionAdmissionRequest) { r.Intent.Kind = "artifact_directory" },
		"non-root":                  func(r *ProbeHostActionAdmissionRequest) { r.Intent.Identity.Host.User = "deploy" },
		"docker":                    func(r *ProbeHostActionAdmissionRequest) { r.Intent.Identity.Mode = ProbeInstallDocker },
	} {
		t.Run(name, func(t *testing.T) {
			bad := request
			mutate(&bad)
			if bad.Valid() || bad.SigningMessage() != nil {
				t.Fatal("invalid request is admissible or produced signing bytes")
			}
		})
	}
	if (ProbeHostActionAdmissionRequest{}).Valid() || (ProbeHostActionAdmissionRequest{}).SigningMessage() != nil {
		t.Fatal("zero request admissible")
	}
}

func TestProbeHostActionSigningMessageCanonicalAndDomainSeparated(t *testing.T) {
	authority := hostActDomainNewAuthority(t)
	for _, kind := range hostActDomainKinds {
		intent := hostActDomainIntent(kind)
		request := authority.request(intent)
		key := sha256.Sum256(authority.raw[:])
		marker := sha256.Sum256([]byte(intent.Marker()))
		want := append([]byte(hostActDomainSigningDomain), key[:]...)
		want = append(want, request.Challenge.Nonce[:]...)
		want = append(want, marker[:]...)
		if got := request.SigningMessage(); !bytes.Equal(got, want) || len(got) != len(hostActDomainSigningDomain)+96 {
			t.Fatalf("%s: signing message bytes changed (%d bytes)", kind, len(got))
		}
		// The caller may scribble on the message without affecting the next one.
		message := request.SigningMessage()
		message[0] ^= 1
		if !bytes.Equal(request.SigningMessage(), want) {
			t.Fatalf("%s: signing message storage was shared", kind)
		}
	}

	// The host-action message never equals the binary-stage message, even for
	// the same identity, owner, attempt, authority and nonce.
	stage := ProbeBinaryStageAdmissionRequest{
		Intent:    ProbeBinaryStageIntent{Identity: hostActDomainIdentity(), OwnerID: hostActDomainOwnerID, AttemptID: hostActDomainAttemptID},
		Challenge: authority.challenge(""),
	}
	stage.Challenge.MarkerSHA256 = stage.Intent.MarkerSHA256()
	if !stage.Valid() {
		t.Fatal("comparable binary-stage request invalid")
	}
	stageMessage := stage.SigningMessage()
	if !bytes.HasPrefix(stageMessage, []byte(hostActDomainStageDomain)) || bytes.HasPrefix(stageMessage, []byte(hostActDomainSigningDomain)) {
		t.Fatal("binary-stage signing domain drifted")
	}
	for _, kind := range hostActDomainKinds {
		hostMessage := authority.request(hostActDomainIntent(kind)).SigningMessage()
		if bytes.Equal(hostMessage, stageMessage) || bytes.HasPrefix(hostMessage, []byte(hostActDomainStageDomain)) {
			t.Fatalf("%s host-action message collides with the binary-stage message", kind)
		}
	}
	// The kinds are distinct messages too.
	seen := map[string]string{}
	for _, kind := range hostActDomainKinds {
		message := string(authority.request(hostActDomainIntent(kind)).SigningMessage())
		if other, dup := seen[message]; dup {
			t.Fatalf("%s and %s sign the same message", kind, other)
		}
		seen[message] = kind
	}
}

func TestProbeHostActionGrantVerifiesOnlyItsExactRequest(t *testing.T) {
	authority := hostActDomainNewAuthority(t)
	other := hostActDomainNewAuthority(t)
	for _, kind := range hostActDomainKinds {
		request := authority.request(hostActDomainIntent(kind))
		grant := authority.grant(request)
		if !grant.ValidFor(authority.raw) {
			t.Fatalf("%s: genuine grant refused", kind)
		}
		if grant.ValidFor([32]byte{}) {
			t.Fatalf("%s: zero public key verified", kind)
		}
		if grant.ValidFor(other.raw) {
			t.Fatalf("%s: grant verified under another authority", kind)
		}
		forged := other.grant(request)
		if forged.ValidFor(authority.raw) {
			t.Fatalf("%s: another authority's signature verified", kind)
		}
		flipped := grant
		flipped.Signature[0] ^= 1
		if flipped.ValidFor(authority.raw) {
			t.Fatalf("%s: corrupted signature verified", kind)
		}
		if (ProbeHostActionGrant{Request: request}).ValidFor(authority.raw) {
			t.Fatalf("%s: zero signature verified", kind)
		}
		if (ProbeHostActionGrant{Signature: grant.Signature}).ValidFor(authority.raw) {
			t.Fatalf("%s: signature without a request verified", kind)
		}
	}

	// A request that names another authority's key id, signed by this
	// authority's key, verifies cryptographically but is not this authority's
	// grant: the key id the request names must be the verifying key's.
	for _, kind := range hostActDomainKinds {
		misnamed := authority.request(hostActDomainIntent(kind))
		misnamed.Challenge.AuthorityKeyID = other.keyID
		grant := authority.grant(misnamed)
		if !ed25519.Verify(ed25519.PublicKey(authority.raw[:]), misnamed.SigningMessage(), grant.Signature[:]) {
			t.Fatalf("%s: fixture signature does not verify; the key id check is not isolated", kind)
		}
		if grant.ValidFor(authority.raw) {
			t.Fatalf("%s: a grant naming another authority's key id verified under the signing key", kind)
		}
	}

	base := authority.grant(authority.request(hostActDomainIntent(ProbeHostActionUnitFile)))
	for name, mutate := range map[string]func(*ProbeHostActionAdmissionRequest){
		"operation": func(r *ProbeHostActionAdmissionRequest) { r.Intent.Identity.OperationID = hostActDomainOtherID },
		"owner":     func(r *ProbeHostActionAdmissionRequest) { r.Intent.OwnerID = hostActDomainOtherID },
		"attempt":   func(r *ProbeHostActionAdmissionRequest) { r.Intent.AttemptID = hostActDomainOtherID },
		"kind":      func(r *ProbeHostActionAdmissionRequest) { r.Intent.Kind = ProbeHostActionService },
		"address":   func(r *ProbeHostActionAdmissionRequest) { r.Intent.Identity.Host.Address = "other.test:22" },
		"host key": func(r *ProbeHostActionAdmissionRequest) {
			r.Intent.Identity.HostKeyFingerprint = hostActDomainFingerprint(9)
		},
		"client key": func(r *ProbeHostActionAdmissionRequest) {
			r.Intent.Identity.ClientKeyFingerprint = hostActDomainFingerprint(9)
		},
		"actor":     func(r *ProbeHostActionAdmissionRequest) { r.Intent.Identity.RequestedBy++ },
		"version":   func(r *ProbeHostActionAdmissionRequest) { r.Intent.Identity.Binary.Version = "1.2.4" },
		"arch":      func(r *ProbeHostActionAdmissionRequest) { r.Intent.Identity.Binary.Architecture = "arm64" },
		"digest":    func(r *ProbeHostActionAdmissionRequest) { r.Intent.Identity.Binary.SHA256 = strings.Repeat("b", 64) },
		"size":      func(r *ProbeHostActionAdmissionRequest) { r.Intent.Identity.Binary.Size++ },
		"nonce":     func(r *ProbeHostActionAdmissionRequest) { r.Challenge.Nonce[31]++ },
		"authority": func(r *ProbeHostActionAdmissionRequest) { r.Challenge.AuthorityKeyID = strings.Repeat("c", 64) },
	} {
		t.Run("changed "+name, func(t *testing.T) {
			bad := base
			mutate(&bad.Request)
			// Keep the request internally consistent so only the signature binds it.
			bad.Request.Challenge.MarkerSHA256 = bad.Request.Intent.MarkerSHA256()
			if bad.ValidFor(authority.raw) {
				t.Fatal("changed tuple retained authorization")
			}
		})
	}
}

// A grant signed for one signing domain must never verify in the other, in
// either direction, whatever the signed message bytes are.
func TestProbeHostActionAndBinaryStageGrantsNeverCrossVerify(t *testing.T) {
	authority := hostActDomainNewAuthority(t)

	stageRequest := ProbeBinaryStageAdmissionRequest{
		Intent:    ProbeBinaryStageIntent{Identity: hostActDomainIdentity(), OwnerID: hostActDomainOwnerID, AttemptID: hostActDomainAttemptID},
		Challenge: authority.challenge(""),
	}
	stageRequest.Challenge.MarkerSHA256 = stageRequest.Intent.MarkerSHA256()
	stageGrant := ProbeBinaryStageGrant{Request: stageRequest}
	copy(stageGrant.Signature[:], ed25519.Sign(authority.priv, stageRequest.SigningMessage()))
	if !stageGrant.ValidFor(authority.raw) {
		t.Fatal("genuine binary-stage grant refused")
	}

	for _, kind := range hostActDomainKinds {
		hostRequest := authority.request(hostActDomainIntent(kind))
		hostGrant := authority.grant(hostRequest)
		if !hostGrant.ValidFor(authority.raw) {
			t.Fatalf("%s: genuine host-action grant refused", kind)
		}

		// A binary-stage signature presented under a host-action request.
		crossed := ProbeHostActionGrant{Request: hostRequest, Signature: stageGrant.Signature}
		if crossed.ValidFor(authority.raw) {
			t.Fatalf("%s: a binary-stage signature verified as a host-action grant", kind)
		}
		// A host-action signature presented under the binary-stage request.
		reverse := ProbeBinaryStageGrant{Request: stageRequest, Signature: hostGrant.Signature}
		if reverse.ValidFor(authority.raw) {
			t.Fatalf("%s: a host-action signature verified as a binary-stage grant", kind)
		}

		// Isolate the domain prefix: sign the binary-stage domain over exactly
		// the host-action request's key, nonce and marker digest. Only the
		// prefix differs from the host-action message, and it must not verify.
		key, _ := hex.DecodeString(hostRequest.Challenge.AuthorityKeyID)
		marker, _ := hex.DecodeString(hostRequest.Challenge.MarkerSHA256)
		stageDomain := append([]byte(hostActDomainStageDomain), key...)
		stageDomain = append(stageDomain, hostRequest.Challenge.Nonce[:]...)
		stageDomain = append(stageDomain, marker...)
		if len(stageDomain)-len(hostActDomainStageDomain) != len(hostRequest.SigningMessage())-len(hostActDomainSigningDomain) {
			t.Fatal("fixture bodies differ in length; the prefix isolation is inert")
		}
		onlyPrefix := ProbeHostActionGrant{Request: hostRequest}
		copy(onlyPrefix.Signature[:], ed25519.Sign(authority.priv, stageDomain))
		if onlyPrefix.ValidFor(authority.raw) {
			t.Fatalf("%s: a signature over the binary-stage domain verified as a host-action grant", kind)
		}
		// And the mirror: host-action domain over a real binary-stage body.
		skey, _ := hex.DecodeString(stageRequest.Challenge.AuthorityKeyID)
		smarker, _ := hex.DecodeString(stageRequest.Challenge.MarkerSHA256)
		hostDomain := append([]byte(hostActDomainSigningDomain), skey...)
		hostDomain = append(hostDomain, stageRequest.Challenge.Nonce[:]...)
		hostDomain = append(hostDomain, smarker...)
		mirror := ProbeBinaryStageGrant{Request: stageRequest}
		copy(mirror.Signature[:], ed25519.Sign(authority.priv, hostDomain))
		if mirror.ValidFor(authority.raw) {
			t.Fatalf("%s: a signature over the host-action domain verified as a binary-stage grant", kind)
		}
	}

	// A grant for one kind never authorizes another kind of the same operation.
	for _, from := range hostActDomainKinds {
		grant := authority.grant(authority.request(hostActDomainIntent(from)))
		for _, to := range hostActDomainKinds {
			if from == to {
				continue
			}
			swapped := ProbeHostActionGrant{Request: authority.request(hostActDomainIntent(to)), Signature: grant.Signature}
			if swapped.ValidFor(authority.raw) {
				t.Fatalf("a %s grant verified as %s", from, to)
			}
		}
	}
}

func TestProbeHostActionAdmissionStorageContractAndNoWireTags(t *testing.T) {
	authority := hostActDomainNewAuthority(t)
	request := authority.request(hostActDomainIntent(ProbeHostActionDataDirectory))
	stamp := time.Date(2026, 10, 23, 1, 2, 3, 123456000, time.UTC)
	if !(ProbeHostActionAdmission{Request: request, AdmittedAt: stamp}).Valid() {
		t.Fatal("UTC microsecond stamp refused")
	}
	for name, bad := range map[string]time.Time{
		"zero":       {},
		"nanosecond": stamp.Add(time.Nanosecond),
		"local zone": stamp.In(time.FixedZone("offset", 7*3600)),
	} {
		if (ProbeHostActionAdmission{Request: request, AdmittedAt: bad}).Valid() {
			t.Fatalf("storage timestamp contract not enforced for %s", name)
		}
	}
	invalid := request
	invalid.Challenge.MarkerSHA256 = strings.Repeat("0", 64)
	if (ProbeHostActionAdmission{Request: invalid, AdmittedAt: stamp}).Valid() {
		t.Fatal("admission of an invalid request accepted")
	}

	// Domain types are pure data: a json tag here would let a handler marshal
	// one and leak its capitalized fields.
	for _, value := range []any{
		ProbeHostActionIntent{}, request, ProbeHostActionAdmission{}, ProbeHostActionGrant{},
	} {
		typ := reflect.TypeOf(value)
		for i := range typ.NumField() {
			if typ.Field(i).Tag.Get("json") != "" {
				t.Fatalf("%s.%s gained a wire tag", typ.Name(), typ.Field(i).Name)
			}
		}
	}
}

func hostActDomainUnitLines(t *testing.T, unit string) []string {
	t.Helper()
	if !strings.HasSuffix(unit, "\n") || strings.HasSuffix(unit, "\n\n") {
		t.Fatalf("unit must end with exactly one newline: %q", unit[max(0, len(unit)-8):])
	}
	return strings.Split(strings.TrimSuffix(unit, "\n"), "\n")
}

func hostActDomainCount(lines []string, want string) int {
	n := 0
	for _, line := range lines {
		if line == want {
			n++
		}
	}
	return n
}

func TestProbeSystemdUnitContent(t *testing.T) {
	identity := hostActDomainIdentity()
	unit := ProbeSystemdUnit(identity)
	lines := hostActDomainUnitLines(t, unit)

	// Exactly the artifact directory + release name, started with `run
	// --data-dir <data dir>`; no shell, no PATH lookup, no extra arguments.
	execStart := "ExecStart=" + hostActDomainArtifact + "/" + hostActDomainRelease + " run --data-dir " + hostActDomainDataDir
	if hostActDomainCount(lines, execStart) != 1 {
		t.Fatalf("ExecStart line missing or duplicated; want %q in\n%s", execStart, unit)
	}
	execLines := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "ExecStart") || strings.HasPrefix(line, "ExecStartPre") || strings.HasPrefix(line, "ExecStartPost") ||
			strings.HasPrefix(line, "ExecStop") || strings.HasPrefix(line, "ExecReload") || strings.HasPrefix(line, "ExecCondition") {
			execLines++
		}
	}
	if execLines != 1 {
		t.Fatalf("unit has %d Exec* lines, want exactly the one ExecStart", execLines)
	}
	if !strings.HasPrefix(strings.TrimPrefix(execStart, "ExecStart="), "/") {
		t.Fatal("ExecStart is not an absolute path")
	}

	for _, want := range []string{
		"ReadWritePaths=" + hostActDomainDataDir,
		"CapabilityBoundingSet=CAP_NET_RAW",
		"AmbientCapabilities=",
		"NoNewPrivileges=yes",
		"ProtectSystem=strict",
		"ProtectHome=yes",
		"PrivateTmp=yes",
		"PrivateDevices=yes",
		"ProtectKernelTunables=yes",
		"ProtectKernelModules=yes",
		"ProtectKernelLogs=yes",
		"ProtectControlGroups=yes",
		"ProtectClock=yes",
		"ProtectHostname=yes",
		"RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK",
		"RestrictNamespaces=yes",
		"MemoryDenyWriteExecute=yes",
		"SystemCallArchitectures=native",
		"UMask=0077",
		"Restart=on-failure",
		"RestartSec=10s",
		"WantedBy=multi-user.target",
		"Description=Uptime Phoenix probe " + hostActDomainOperationID,
		"# operation_id=" + hostActDomainOperationID,
	} {
		if got := hostActDomainCount(lines, want); got != 1 {
			t.Fatalf("line %q appears %d times, want exactly once", want, got)
		}
	}

	// The unit must grant writes to the data directory only and must not widen
	// the capability set, run unconfined or name another path.
	for _, line := range lines {
		for _, banned := range []string{"User=", "DynamicUser", "AmbientCapabilities=CAP", "CapabilityBoundingSet=~", "CAP_SYS_ADMIN", "ProtectSystem=no", "ProtectSystem=false", "NoNewPrivileges=no", "ReadWritePaths=/ ", "/tmp", "sh -c", "bash", "$"} {
			if strings.Contains(line, banned) {
				t.Fatalf("unit line %q contains %q", line, banned)
			}
		}
	}
	rw := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "ReadWritePaths=") {
			rw++
		}
	}
	if rw != 1 {
		t.Fatalf("%d ReadWritePaths lines, want exactly one", rw)
	}

	// Section layout: header comments, then [Unit], [Service], [Install] once
	// each and in that order; every other non-empty line is key=value or a
	// comment; no line has surrounding whitespace or a carriage return.
	var sections []string
	for _, line := range lines {
		if line != strings.TrimSpace(line) || strings.Contains(line, "\r") || strings.Contains(line, "\x00") {
			t.Fatalf("unit line %q has stray whitespace or control bytes", line)
		}
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]"):
			sections = append(sections, line)
		case !strings.Contains(line, "="):
			t.Fatalf("unit line %q is neither a section, comment nor assignment", line)
		}
	}
	if !reflect.DeepEqual(sections, []string{"[Unit]", "[Service]", "[Install]"}) {
		t.Fatalf("unit sections %v", sections)
	}
	if !strings.HasPrefix(lines[0], "# Managed by Uptime Phoenix (phoenix-systemd-unit-v1)") {
		t.Fatalf("unit header %q", lines[0])
	}
}

func TestProbeSystemdUnitIsDeterministicAndPure(t *testing.T) {
	identity := hostActDomainIdentity()
	first := ProbeSystemdUnit(identity)
	for range 5 {
		if got := ProbeSystemdUnit(identity); got != first {
			t.Fatal("unit differs between calls")
		}
	}
	// Equal for an equal identity regardless of how it was built, and unchanged
	// by fields the unit does not name.
	rebuilt := hostActDomainIdentity()
	if ProbeSystemdUnit(rebuilt) != first {
		t.Fatal("unit differs for an equal identity")
	}
	for name, mutate := range map[string]func(*ProbeInstallIdentity){
		"ssh address": func(i *ProbeInstallIdentity) { i.Host.Address = "other.test:2222" },
		"host key":    func(i *ProbeInstallIdentity) { i.HostKeyFingerprint = hostActDomainFingerprint(9) },
		"client key":  func(i *ProbeInstallIdentity) { i.ClientKeyFingerprint = hostActDomainFingerprint(9) },
		"actor":       func(i *ProbeInstallIdentity) { i.RequestedBy++ },
		"binary hash": func(i *ProbeInstallIdentity) { i.Binary.SHA256 = strings.Repeat("b", 64) },
		"binary size": func(i *ProbeInstallIdentity) { i.Binary.Size++ },
	} {
		changed := hostActDomainIdentity()
		mutate(&changed)
		if ProbeSystemdUnit(changed) != first {
			t.Fatalf("unit changed with %s, which it does not name", name)
		}
	}
	// The release, architecture and operation are named by the unit.
	for name, mutate := range map[string]func(*ProbeInstallIdentity){
		"version":   func(i *ProbeInstallIdentity) { i.Binary.Version = "1.2.4" },
		"arch":      func(i *ProbeInstallIdentity) { i.Binary.Architecture = "arm64" },
		"operation": func(i *ProbeInstallIdentity) { i.OperationID = hostActDomainOtherID },
	} {
		changed := hostActDomainIdentity()
		mutate(&changed)
		if got := ProbeSystemdUnit(changed); got == "" || got == first {
			t.Fatalf("unit ignores the %s", name)
		}
	}
	arm := hostActDomainIdentity()
	arm.Binary.Architecture = "arm64"
	if !strings.Contains(ProbeSystemdUnit(arm), "/uptime-phoenix-probe_1.2.3_linux_arm64 run --data-dir ") {
		t.Fatal("arm64 unit does not name the arm64 release")
	}
}

func TestProbeSystemdUnitEmptyForInvalidIdentity(t *testing.T) {
	if ProbeSystemdUnit(ProbeInstallIdentity{}) != "" {
		t.Fatal("zero identity produced a unit")
	}
	cases := map[string]func(*ProbeInstallIdentity){
		"docker mode":             func(i *ProbeInstallIdentity) { i.Mode = ProbeInstallDocker },
		"empty mode":              func(i *ProbeInstallIdentity) { i.Mode = "" },
		"unknown mode":            func(i *ProbeInstallIdentity) { i.Mode = "podman" },
		"layout version":          func(i *ProbeInstallIdentity) { i.LayoutVersion = 2 },
		"zero actor":              func(i *ProbeInstallIdentity) { i.RequestedBy = 0 },
		"bad operation":           func(i *ProbeInstallIdentity) { i.OperationID = "BAD" },
		"operation id newline":    func(i *ProbeInstallIdentity) { i.OperationID = hostActDomainOperationID + "\nExecStartPre=/bin/evil" },
		"operation id path":       func(i *ProbeInstallIdentity) { i.OperationID = "../../../../etc/cron.d" },
		"bad host pin":            func(i *ProbeInstallIdentity) { i.HostKeyFingerprint = "SHA256:short" },
		"invalid pin size":        func(i *ProbeInstallIdentity) { i.Binary.Size = 0 },
		"version newline inject":  func(i *ProbeInstallIdentity) { i.Binary.Version = "1.2.3\nExecStartPre=/bin/evil" },
		"version path":            func(i *ProbeInstallIdentity) { i.Binary.Version = "../../escape" },
		"version space inject":    func(i *ProbeInstallIdentity) { i.Binary.Version = "1.2.3 --evil" },
		"arch other":              func(i *ProbeInstallIdentity) { i.Binary.Architecture = "amd64\nUser=evil" },
		"hidden image on systemd": func(i *ProbeInstallIdentity) { i.Image.Pin.Version = "1.2.3" },
		"address newline inject":  func(i *ProbeInstallIdentity) { i.Host.Address = "example.test:22\nExecStartPre=/bin/evil" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			identity := hostActDomainIdentity()
			mutate(&identity)
			if got := ProbeSystemdUnit(identity); got != "" {
				t.Fatalf("invalid identity produced a unit:\n%s", got)
			}
		})
	}
	// A valid docker identity has no systemd unit either.
	if docker := installTestIdentity(ProbeInstallDocker); !docker.Valid() || ProbeSystemdUnit(docker) != "" {
		t.Fatalf("docker identity valid=%v unit=%q", docker.Valid(), ProbeSystemdUnit(docker))
	}
	// A non-root systemd identity is still a valid systemd identity: the unit is
	// derived from the identity, while the host-action intent is what pins root.
	deploy := hostActDomainIdentity()
	deploy.Host.User = "deploy"
	if ProbeSystemdUnit(deploy) == "" {
		t.Fatal("unit is not derived for a valid non-root systemd identity")
	}
	if (ProbeHostActionIntent{Identity: deploy, OwnerID: hostActDomainOwnerID, AttemptID: hostActDomainAttemptID, Kind: ProbeHostActionUnitFile}).Valid() {
		t.Fatal("non-root intent accepted")
	}
}
