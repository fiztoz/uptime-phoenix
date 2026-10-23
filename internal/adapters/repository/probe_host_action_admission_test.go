package repository_test

import (
	"context"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestProbeHostActionAdmissionStoreRefusals(t *testing.T) {
	signer := m8HostAuthority(t, 53)
	var missing *repository.ProbeHostActionAdmissionStore
	for _, kind := range m8HostKinds {
		r := m8HostRequest(signer, m8InstallOp1, m8InstallOwnerA, kind)
		for _, store := range []*repository.ProbeHostActionAdmissionStore{missing, repository.NewProbeHostActionAdmissionStore(nil)} {
			a, err := store.AdmitHostAction(t.Context(), r)
			if a != (domain.ProbeHostActionAdmission{}) || err != domain.ErrInternal {
				t.Fatalf("%s: missing DB returned admission", kind)
			}
			a, err = store.AdmitHostAction(nil, r)
			if a != (domain.ProbeHostActionAdmission{}) || err != domain.ErrValidation {
				t.Fatalf("%s: nil context accepted", kind)
			}
			var nilContext *struct{ context.Context }
			a, err = store.AdmitHostAction(nilContext, r)
			if a != (domain.ProbeHostActionAdmission{}) || err != domain.ErrValidation {
				t.Fatalf("%s: typed nil context accepted", kind)
			}
			a, err = store.AdmitHostAction(t.Context(), domain.ProbeHostActionAdmissionRequest{})
			if a != (domain.ProbeHostActionAdmission{}) || err != domain.ErrValidation {
				t.Fatalf("%s: invalid request accepted", kind)
			}
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			a, err = store.AdmitHostAction(ctx, r)
			if a != (domain.ProbeHostActionAdmission{}) || err != context.Canceled {
				t.Fatalf("%s: canceled request accepted", kind)
			}
		}
	}
}

// The artifact attempt belongs to ProbeBinaryStageAdmissionStore. This store
// must refuse it, and every other malformed kind or binding, from the domain
// contract alone, before it could ever reach a database.
func TestProbeHostActionAdmissionStoreRefusesForeignKinds(t *testing.T) {
	signer := m8HostAuthority(t, 53)
	store := repository.NewProbeHostActionAdmissionStore(nil)
	good := m8HostRequest(signer, m8InstallOp1, m8InstallOwnerA, domain.ProbeHostActionUnitFile)
	for name, change := range map[string]func(*domain.ProbeHostActionAdmissionRequest){
		"artifact_directory": func(r *domain.ProbeHostActionAdmissionRequest) { r.Intent.Kind = "artifact_directory" },
		"unknown_kind":       func(r *domain.ProbeHostActionAdmissionRequest) { r.Intent.Kind = "container" },
		"empty_kind":         func(r *domain.ProbeHostActionAdmissionRequest) { r.Intent.Kind = "" },
		"docker_identity": func(r *domain.ProbeHostActionAdmissionRequest) {
			r.Intent.Identity = m8InstallIdentity(domain.ProbeInstallDocker, m8InstallOp1)
		},
		"non_root_host": func(r *domain.ProbeHostActionAdmissionRequest) { r.Intent.Identity.Host.User = "phoenix" },
		"bad_attempt":   func(r *domain.ProbeHostActionAdmissionRequest) { r.Intent.AttemptID = "not-a-uuid" },
		"bad_owner":     func(r *domain.ProbeHostActionAdmissionRequest) { r.Intent.OwnerID = "" },
		"zero_nonce":    func(r *domain.ProbeHostActionAdmissionRequest) { r.Challenge.Nonce = [32]byte{} },
		// A marker for another kind (or any other bytes) is not this intent's marker.
		"foreign_marker": func(r *domain.ProbeHostActionAdmissionRequest) {
			other := r.Intent
			other.Kind = domain.ProbeHostActionService
			r.Challenge.MarkerSHA256 = other.MarkerSHA256()
		},
	} {
		bad := good
		change(&bad)
		a, err := store.AdmitHostAction(t.Context(), bad)
		if bad.Valid() || a != (domain.ProbeHostActionAdmission{}) || err != domain.ErrValidation {
			t.Fatalf("%s: store accepted a request outside the host-action contract", name)
		}
	}
	// The untouched request reaches the (absent) database and fails closed.
	if a, err := store.AdmitHostAction(t.Context(), good); a != (domain.ProbeHostActionAdmission{}) || err != domain.ErrInternal {
		t.Fatal("valid request must reach the database check")
	}
}

// Three kinds, three distinct markers: a grant for one attempt can never be
// replayed as another kind's, and each marker binds the exact derived unit bytes.
func TestProbeHostActionAdmissionRequestsAreKindBound(t *testing.T) {
	signer := m8HostAuthority(t, 53)
	seen := map[string]string{}
	for _, kind := range m8HostKinds {
		r := m8HostRequest(signer, m8InstallOp1, m8InstallOwnerA, kind)
		if !r.Valid() {
			t.Fatalf("%s: fixture request invalid", kind)
		}
		spec, ok := r.Intent.Resource()
		if !ok || spec.Kind != kind || spec.Ordinal != m8HostOrdinal(kind) || !spec.Owned {
			t.Fatalf("%s: intent derived the wrong layout resource: %+v", kind, spec)
		}
		if other, dup := seen[r.Challenge.MarkerSHA256]; dup {
			t.Fatalf("%s shares a marker with %s", kind, other)
		}
		seen[r.Challenge.MarkerSHA256] = kind
	}
}

func m8HostOrdinal(kind string) int {
	for i, k := range m8HostLayout {
		if k == kind {
			return i + 1
		}
	}
	return 0
}
