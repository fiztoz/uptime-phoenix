package auth

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestStreamResetProtectionBindsProvenanceAndArchive(t *testing.T) {
	p, err := NewProbeConfigProtector(bytes.Repeat([]byte{43}, 32))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	plan := domain.ProbeStreamResetPlan{ResetID: uuid.NewString(), HubID: uuid.NewString(), ProbeID: uuid.NewString(), EnrollmentID: uuid.NewString(), PreviousStreamID: uuid.NewString(), StreamID: uuid.NewString(), Fingerprint: strings.Repeat("a", 64), CredentialVersion: 1, CertificateVersion: 1, HubCommittedSeq: 11, ConnectionGeneration: 9, PreparedAt: now}
	r := domain.EdgeStreamResetRecord{Plan: plan, InitialStreamID: plan.PreviousStreamID, Source: domain.EdgeIdentity{ProbeID: plan.ProbeID, HubID: plan.HubID, StreamID: plan.PreviousStreamID, Fingerprint: plan.Fingerprint, LastCreatedSeq: 8, CommittedSeq: 3, ConnectionGeneration: 2, ConfigRevision: 1}, State: "applied", ReservedAt: now, AppliedAt: now.Add(time.Second), ArchiveBytes: 4096, ArchiveSHA256: strings.Repeat("b", 64)}
	proof, err := p.SealStreamReset(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.VerifyStreamReset(t.Context(), r, proof); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.EdgeStreamResetRecord){
		"operation":    func(r *domain.EdgeStreamResetRecord) { r.Plan.ResetID = uuid.NewString() },
		"hub":          func(r *domain.EdgeStreamResetRecord) { r.Plan.HubID = uuid.NewString(); r.Source.HubID = r.Plan.HubID },
		"enrollment":   func(r *domain.EdgeStreamResetRecord) { r.Plan.EnrollmentID = uuid.NewString() },
		"pin":          func(r *domain.EdgeStreamResetRecord) { r.Plan.Fingerprint = strings.Repeat("c", 64) },
		"new epoch":    func(r *domain.EdgeStreamResetRecord) { r.Plan.StreamID = uuid.NewString() },
		"anchor":       func(r *domain.EdgeStreamResetRecord) { r.InitialStreamID = uuid.NewString() },
		"source bound": func(r *domain.EdgeStreamResetRecord) { r.Source.LastCreatedSeq++ },
		"hub bound":    func(r *domain.EdgeStreamResetRecord) { r.Plan.HubCommittedSeq++ },
		"fence":        func(r *domain.EdgeStreamResetRecord) { r.Plan.ConnectionGeneration++ },
		"config":       func(r *domain.EdgeStreamResetRecord) { r.Source.ConfigRevision++ },
		"credential":   func(r *domain.EdgeStreamResetRecord) { r.Plan.CredentialVersion++ },
		"certificate":  func(r *domain.EdgeStreamResetRecord) { r.Plan.CertificateVersion++ },
		"digest":       func(r *domain.EdgeStreamResetRecord) { r.ArchiveSHA256 = strings.Repeat("d", 64) },
		"size":         func(r *domain.EdgeStreamResetRecord) { r.ArchiveBytes++ },
		"phase":        func(r *domain.EdgeStreamResetRecord) { r.State = "archived"; r.AppliedAt = time.Time{} },
		"time":         func(r *domain.EdgeStreamResetRecord) { r.ReservedAt = r.ReservedAt.Add(time.Microsecond) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := r
			mutate(&changed)
			if err := p.VerifyStreamReset(t.Context(), changed, proof); err == nil {
				t.Fatal("changed record authenticated")
			}
		})
	}
	wrong, err := NewProbeConfigProtector(bytes.Repeat([]byte{44}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.VerifyStreamReset(t.Context(), r, proof); err == nil {
		t.Fatal("wrong key accepted")
	}
	changed := bytes.Clone(proof)
	changed[len(changed)-1] ^= 1
	if err := p.VerifyStreamReset(t.Context(), r, changed); err == nil {
		t.Fatal("changed proof accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := p.VerifyStreamReset(ctx, r, proof); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := p.SealStreamReset(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
