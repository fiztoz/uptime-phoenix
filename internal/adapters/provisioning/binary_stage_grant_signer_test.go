package provisioning

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func grantSignerFixture(t *testing.T) (*Ed25519ProbeStageGrantSigner, domain.ProbeBinaryStageAdmission, []byte) {
	t.Helper()
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{41}, 32))
	signer, err := NewEd25519ProbeStageGrantSigner(key)
	if err != nil {
		t.Fatal("fixture signer construction")
	}
	intent := stageHeaderFixture(t, false, "amd64")
	request := domain.ProbeBinaryStageAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
		AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(signer.PublicKey()), Nonce: [32]byte{1}, MarkerSHA256: intent.MarkerSHA256(),
	}}
	return signer, domain.ProbeBinaryStageAdmission{Request: request, AdmittedAt: time.Date(2026, 10, 23, 1, 2, 3, 0, time.UTC)}, key
}

func TestEd25519ProbeStageGrantSignerKeyValidation(t *testing.T) {
	_, _, valid := grantSignerFixture(t)
	for _, n := range []int{0, 1, 32, 63, 65, 128} {
		if signer, err := NewEd25519ProbeStageGrantSigner(make([]byte, n)); signer != nil || err != domain.ErrValidation {
			t.Fatal("invalid key length accepted")
		}
	}
	for _, offset := range []int{0, 31, 32, 63} {
		bad := append([]byte(nil), valid...)
		bad[offset] ^= 1
		if signer, err := NewEd25519ProbeStageGrantSigner(bad); signer != nil || err != domain.ErrValidation {
			t.Fatal("inconsistent expanded private key accepted")
		}
	}
}

func TestEd25519ProbeStageGrantSignerCopiesAndVerifies(t *testing.T) {
	signer, admission, original := grantSignerFixture(t)
	public := signer.PublicKey()
	for i := range original {
		original[i] = 0
	}
	copyOfPublic := signer.PublicKey()
	copyOfPublic[0] ^= 1
	if signer.PublicKey() != public {
		t.Fatal("public key was shared")
	}
	grant, err := signer.SignStageAdmission(t.Context(), admission)
	if err != nil || grant.Request != admission.Request || !grant.ValidFor(public) {
		t.Fatal("injected key copy did not verify fixed admission")
	}
	if ed25519.Verify(public[:], append(admission.Request.SigningMessage(), 0), grant.Signature[:]) {
		t.Fatal("signature accepted alternate message")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			g, err := signer.SignStageAdmission(t.Context(), admission)
			if err != nil || !g.ValidFor(public) {
				t.Error("concurrent immutable signer failed")
			}
		})
	}
	wg.Wait()
}

func TestEd25519ProbeStageGrantSignerRefusals(t *testing.T) {
	signer, admission, _ := grantSignerFixture(t)
	for name, change := range map[string]func(*domain.ProbeBinaryStageAdmission){
		"zero": func(a *domain.ProbeBinaryStageAdmission) { *a = domain.ProbeBinaryStageAdmission{} },
		"authority": func(a *domain.ProbeBinaryStageAdmission) {
			a.Request.Challenge.AuthorityKeyID = domain.ProbeBinaryStageAuthorityKeyID([32]byte{4})
		},
		"nonce":     func(a *domain.ProbeBinaryStageAdmission) { a.Request.Challenge.Nonce = [32]byte{} },
		"precision": func(a *domain.ProbeBinaryStageAdmission) { a.AdmittedAt = a.AdmittedAt.Add(time.Nanosecond) },
		"root":      func(a *domain.ProbeBinaryStageAdmission) { a.Request.Intent.Identity.Host.User = "deploy" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := admission
			change(&bad)
			g, err := signer.SignStageAdmission(t.Context(), bad)
			if g != (domain.ProbeBinaryStageGrant{}) || err != domain.ErrValidation {
				t.Fatal("invalid admission signed")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	g, err := signer.SignStageAdmission(ctx, admission)
	if g != (domain.ProbeBinaryStageGrant{}) || err != context.Canceled {
		t.Fatal("canceled signing returned grant")
	}
	var nilCtx context.Context // deliberately nil: the refusal under test
	g, err = signer.SignStageAdmission(nilCtx, admission)
	if g != (domain.ProbeBinaryStageGrant{}) || err != domain.ErrValidation {
		t.Fatal("nil context accepted")
	}
	var nilContext *binaryStageNilContext
	g, err = signer.SignStageAdmission(nilContext, admission)
	if g != (domain.ProbeBinaryStageGrant{}) || err != domain.ErrValidation {
		t.Fatal("typed nil context accepted")
	}
	var missing *Ed25519ProbeStageGrantSigner
	g, err = missing.SignStageAdmission(t.Context(), admission)
	if g != (domain.ProbeBinaryStageGrant{}) || err != domain.ErrInternal || missing.PublicKey() != ([32]byte{}) {
		t.Fatal("nil signer accepted")
	}
}
