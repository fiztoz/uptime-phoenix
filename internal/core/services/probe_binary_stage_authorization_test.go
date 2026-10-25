package services

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type stageAuthRepoFunc func(context.Context, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error)

func (f stageAuthRepoFunc) AdmitBinaryStage(ctx context.Context, r domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
	return f(ctx, r)
}

type stageAuthSigner struct {
	key   ed25519.PrivateKey
	pub   [32]byte
	calls int
	sign  func(context.Context, domain.ProbeBinaryStageAdmission) (domain.ProbeBinaryStageGrant, error)
}

func (s *stageAuthSigner) PublicKey() [32]byte { return s.pub }
func (s *stageAuthSigner) SignStageAdmission(ctx context.Context, a domain.ProbeBinaryStageAdmission) (domain.ProbeBinaryStageGrant, error) {
	s.calls++
	if s.sign != nil {
		return s.sign(ctx, a)
	}
	g := domain.ProbeBinaryStageGrant{Request: a.Request}
	copy(g.Signature[:], ed25519.Sign(s.key, a.Request.SigningMessage()))
	return g, nil
}

func stageAuthFixture() (domain.ProbeBinaryStageAdmissionRequest, *stageAuthSigner) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{29}, 32))
	signer := &stageAuthSigner{key: key}
	copy(signer.pub[:], key[32:])
	intent := domain.ProbeBinaryStageIntent{Identity: installJournalIdentity(domain.ProbeInstallSystemd, installJournalOpID), OwnerID: installJournalOwner, AttemptID: installJournalAttempt}
	return domain.ProbeBinaryStageAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
		AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(signer.pub), Nonce: [32]byte{1}, MarkerSHA256: intent.MarkerSHA256(),
	}}, signer
}

func stageAuthAck(r domain.ProbeBinaryStageAdmissionRequest) domain.ProbeBinaryStageAdmission {
	return domain.ProbeBinaryStageAdmission{Request: r, AdmittedAt: installJournalBaseTime()}
}

func stageAuthService(t *testing.T, repo ports.ProbeBinaryStageAdmissionRepository, signer ports.ProbeBinaryStageGrantSigner) *ProbeBinaryStageAuthorizationService {
	t.Helper()
	s, err := NewProbeBinaryStageAuthorizationService(repo, signer)
	if err != nil {
		t.Fatal("construct stage authorization service")
	}
	return s
}

func stageAuthRefused(t *testing.T, grant domain.ProbeBinaryStageGrant, err, want error) {
	t.Helper()
	if grant != (domain.ProbeBinaryStageGrant{}) || err != want {
		t.Fatalf("expected zero grant and fixed refusal; error match=%v", err == want)
	}
}

func TestProbeBinaryStageAuthorizationConstructionAndInput(t *testing.T) {
	r, signer := stageAuthFixture()
	calls := 0
	repo := stageAuthRepoFunc(func(context.Context, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
		calls++
		return stageAuthAck(r), nil
	})
	var nilRepo stageAuthRepoFunc
	var nilSigner *stageAuthSigner
	for _, pair := range []struct {
		repo   ports.ProbeBinaryStageAdmissionRepository
		signer ports.ProbeBinaryStageGrantSigner
	}{{nil, signer}, {nilRepo, signer}, {repo, nil}, {repo, nilSigner}, {repo, &stageAuthSigner{}}} {
		if s, err := NewProbeBinaryStageAuthorizationService(pair.repo, pair.signer); s != nil || err != domain.ErrValidation {
			t.Fatal("invalid dependencies accepted")
		}
	}
	svc := stageAuthService(t, repo, signer)
	bad := r
	bad.Challenge.Nonce = [32]byte{}
	g, err := svc.Authorize(t.Context(), bad)
	stageAuthRefused(t, g, err, domain.ErrValidation)
	bad = r
	bad.Challenge.AuthorityKeyID = domain.ProbeBinaryStageAuthorityKeyID([32]byte{7})
	g, err = svc.Authorize(t.Context(), bad)
	stageAuthRefused(t, g, err, domain.ErrValidation)
	var bareNilCtx context.Context
	g, err = svc.Authorize(bareNilCtx, r)
	stageAuthRefused(t, g, err, domain.ErrValidation)
	var nilContext *stageAuthNilContext
	g, err = svc.Authorize(nilContext, r)
	stageAuthRefused(t, g, err, domain.ErrValidation)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	g, err = svc.Authorize(ctx, r)
	stageAuthRefused(t, g, err, context.Canceled)
	var missing *ProbeBinaryStageAuthorizationService
	g, err = missing.Authorize(t.Context(), r)
	stageAuthRefused(t, g, err, domain.ErrInternal)
	if calls != 0 || signer.calls != 0 {
		t.Fatal("input refusal performed authority work")
	}
}

type stageAuthNilContext struct{ context.Context }

func TestProbeBinaryStageAuthorizationFreshCommitAndSharedBudget(t *testing.T) {
	r, signer := stageAuthFixture()
	var repoCtx context.Context
	consumed, calls := false, 0
	repo := stageAuthRepoFunc(func(ctx context.Context, got domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
		calls++
		if got != r {
			t.Fatal("request changed")
		}
		if consumed {
			return domain.ProbeBinaryStageAdmission{}, ports.ErrConflict
		}
		consumed, repoCtx = true, ctx
		return stageAuthAck(got), nil
	})
	signer.sign = func(ctx context.Context, a domain.ProbeBinaryStageAdmission) (domain.ProbeBinaryStageGrant, error) {
		if !consumed || ctx != repoCtx {
			t.Fatal("signing before commit or reset context")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("missing shared bounded budget")
		}
		g := domain.ProbeBinaryStageGrant{Request: a.Request}
		copy(g.Signature[:], ed25519.Sign(signer.key, a.Request.SigningMessage()))
		return g, nil
	}
	svc := stageAuthService(t, repo, signer)
	g, err := svc.Authorize(t.Context(), r)
	if err != nil || g.Request != r || !g.ValidFor(signer.pub) {
		t.Fatal("fresh admission not verified")
	}
	g, err = svc.Authorize(t.Context(), r)
	stageAuthRefused(t, g, err, ports.ErrConflict)
	if calls != 2 || signer.calls != 1 {
		t.Fatal("replay signed again")
	}
}

func TestProbeBinaryStageAuthorizationConsumeReducesRemainingBudget(t *testing.T) {
	r, signer := stageAuthFixture()
	var admissionContext context.Context
	var deadline time.Time
	var initialRemaining time.Duration
	repo := stageAuthRepoFunc(func(ctx context.Context, got domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
		var ok bool
		deadline, ok = ctx.Deadline()
		if !ok {
			t.Fatal("admission lacks bounded context")
		}
		admissionContext, initialRemaining = ctx, time.Until(deadline)
		timer := time.NewTimer(25 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			t.Fatal("fixture budget exhausted before admission completed")
		}
		return stageAuthAck(got), nil
	})
	signer.sign = func(ctx context.Context, admission domain.ProbeBinaryStageAdmission) (domain.ProbeBinaryStageGrant, error) {
		signingDeadline, ok := ctx.Deadline()
		if !ok || ctx != admissionContext || !signingDeadline.Equal(deadline) || time.Until(signingDeadline) > initialRemaining-20*time.Millisecond {
			t.Fatal("admission time did not reduce the original signing budget")
		}
		grant := domain.ProbeBinaryStageGrant{Request: admission.Request}
		copy(grant.Signature[:], ed25519.Sign(signer.key, admission.Request.SigningMessage()))
		return grant, nil
	}
	grant, err := stageAuthService(t, repo, signer).Authorize(t.Context(), r)
	if err != nil || grant.Request != r || !grant.ValidFor(signer.pub) || signer.calls != 1 {
		t.Fatal("elapsed-budget control did not produce one verified grant")
	}
}

func TestProbeBinaryStageAuthorizationDeadlineExhaustion(t *testing.T) {
	for _, phase := range []string{"admission", "signing"} {
		t.Run(phase, func(t *testing.T) {
			r, signer := stageAuthFixture()
			var admissionContext context.Context
			calls := 0
			repo := stageAuthRepoFunc(func(ctx context.Context, got domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
				calls++
				admissionContext = ctx
				if phase == "admission" {
					<-ctx.Done()
				}
				// Deliberately return a valid receipt even after expiry: the
				// service must independently refuse it, without signing.
				return stageAuthAck(got), nil
			})
			signer.sign = func(ctx context.Context, admission domain.ProbeBinaryStageAdmission) (domain.ProbeBinaryStageGrant, error) {
				if ctx != admissionContext {
					t.Fatal("signer received a replenished context")
				}
				grant := domain.ProbeBinaryStageGrant{Request: admission.Request}
				copy(grant.Signature[:], ed25519.Sign(signer.key, admission.Request.SigningMessage()))
				<-ctx.Done()
				// A cryptographically valid late result is still unusable.
				return grant, nil
			}
			svc := stageAuthService(t, repo, signer)
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			grant, err := svc.Authorize(ctx, r)
			stageAuthRefused(t, grant, err, context.DeadlineExceeded)
			wantSigning := 0
			if phase == "signing" {
				wantSigning = 1
			}
			if calls != 1 || signer.calls != wantSigning {
				t.Fatal("expired budget signed or retried an admission")
			}
		})
	}
}

func TestProbeBinaryStageAuthorizationMalformedAcknowledgment(t *testing.T) {
	for name, change := range map[string]func(*domain.ProbeBinaryStageAdmission){
		"zero":           func(a *domain.ProbeBinaryStageAdmission) { *a = domain.ProbeBinaryStageAdmission{} },
		"nonce":          func(a *domain.ProbeBinaryStageAdmission) { a.Request.Challenge.Nonce[1]++ },
		"time precision": func(a *domain.ProbeBinaryStageAdmission) { a.AdmittedAt = a.AdmittedAt.Add(time.Nanosecond) },
		"time zone": func(a *domain.ProbeBinaryStageAdmission) {
			a.AdmittedAt = a.AdmittedAt.In(time.FixedZone("local", 3600))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, signer := stageAuthFixture()
			repo := stageAuthRepoFunc(func(context.Context, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
				a := stageAuthAck(r)
				change(&a)
				return a, nil
			})
			g, err := stageAuthService(t, repo, signer).Authorize(t.Context(), r)
			stageAuthRefused(t, g, err, domain.ErrInternal)
			if signer.calls != 0 {
				t.Fatal("malformed acknowledgment signed")
			}
		})
	}
}

func TestProbeBinaryStageAuthorizationBadSignatures(t *testing.T) {
	for _, mode := range []string{"zero", "different request", "different key", "corrupt signature"} {
		t.Run(mode, func(t *testing.T) {
			r, signer := stageAuthFixture()
			repo := stageAuthRepoFunc(func(context.Context, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
				return stageAuthAck(r), nil
			})
			signer.sign = func(_ context.Context, a domain.ProbeBinaryStageAdmission) (domain.ProbeBinaryStageGrant, error) {
				if mode == "zero" {
					return domain.ProbeBinaryStageGrant{}, nil
				}
				g := domain.ProbeBinaryStageGrant{Request: a.Request}
				if mode == "different request" {
					g.Request.Challenge.Nonce[2]++
				}
				key := signer.key
				if mode == "different key" {
					key = ed25519.NewKeyFromSeed(bytes.Repeat([]byte{30}, 32))
				}
				copy(g.Signature[:], ed25519.Sign(key, g.Request.SigningMessage()))
				if mode == "corrupt signature" {
					g.Signature[3] ^= 1
				}
				return g, nil
			}
			g, err := stageAuthService(t, repo, signer).Authorize(t.Context(), r)
			stageAuthRefused(t, g, err, domain.ErrInternal)
		})
	}
}

func TestProbeBinaryStageAuthorizationCancellationAndSecrecy(t *testing.T) {
	for _, phase := range []string{"after commit", "after sign"} {
		t.Run(phase, func(t *testing.T) {
			r, signer := stageAuthFixture()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			repo := stageAuthRepoFunc(func(context.Context, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
				if phase == "after commit" {
					cancel()
				}
				return stageAuthAck(r), nil
			})
			signer.sign = func(_ context.Context, a domain.ProbeBinaryStageAdmission) (domain.ProbeBinaryStageGrant, error) {
				g := domain.ProbeBinaryStageGrant{Request: a.Request}
				copy(g.Signature[:], ed25519.Sign(signer.key, a.Request.SigningMessage()))
				cancel()
				return g, nil
			}
			g, err := stageAuthService(t, repo, signer).Authorize(ctx, r)
			stageAuthRefused(t, g, err, context.Canceled)
			if phase == "after commit" && signer.calls != 0 {
				t.Fatal("canceled commit signed")
			}
		})
	}
	for _, safe := range []error{domain.ErrValidation, ports.ErrConflict, ports.ErrNotFound, context.Canceled, context.DeadlineExceeded, domain.ErrInternal} {
		for _, phase := range []string{"repository", "signer"} {
			r, signer := stageAuthFixture()
			private := fmt.Errorf("private fixture diagnostic: %w", safe)
			repo := stageAuthRepoFunc(func(context.Context, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
				if phase == "repository" {
					return stageAuthAck(r), private
				}
				return stageAuthAck(r), nil
			})
			if phase == "signer" {
				signer.sign = func(context.Context, domain.ProbeBinaryStageAdmission) (domain.ProbeBinaryStageGrant, error) {
					return domain.ProbeBinaryStageGrant{}, private
				}
			}
			g, err := stageAuthService(t, repo, signer).Authorize(t.Context(), r)
			want := safe
			// A port's validation error describes corruption of an already
			// validated request/receipt, not caller input; keep the journal's
			// existing fixed internal-error contract.
			if safe == domain.ErrValidation {
				want = domain.ErrInternal
			}
			stageAuthRefused(t, g, err, want)
		}
	}
	r, signer := stageAuthFixture()
	repo := stageAuthRepoFunc(func(context.Context, domain.ProbeBinaryStageAdmissionRequest) (domain.ProbeBinaryStageAdmission, error) {
		return stageAuthAck(r), errors.New("private fixture diagnostic")
	})
	g, err := stageAuthService(t, repo, signer).Authorize(t.Context(), r)
	stageAuthRefused(t, g, err, domain.ErrInternal)
}
