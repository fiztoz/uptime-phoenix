package repository_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// m8DockerWidening is the migration that lets the 086 admission table hold the
// Docker kinds. It is the last migration a rollback of 086 must undo first.
const m8DockerWidening = "089_probe_docker_host_actions"

// m8DockerForeignAttempt is an attempt UUID no fixture journal ever reserves.
const m8DockerForeignAttempt = "d0c09999-d0c0-4d0c-8d0c-d0c0d0c0d0c0"

// m8DockerLayout is the frozen root/Docker resource order. A host action's
// layout index is its position here; its ordinal is index+1.
var m8DockerLayout = []string{domain.ProbeHostActionImageCache, domain.ProbeHostActionDataDirectory, domain.ProbeHostActionContainer}

// m8DockerIdentity is a valid root Docker install identity.
func m8DockerIdentity(operationID string) domain.ProbeInstallIdentity {
	identity := m8InstallIdentity(domain.ProbeInstallDocker, operationID)
	identity.Host.User = "root"
	return identity
}

// m8DockerAttempt returns a journal-unique attempt UUID per operation and kind.
func m8DockerAttempt(operationID, kind string) string {
	return fmt.Sprintf("d0c0%02d%02d-d0c0-4d0c-8d0c-d0c0d0c0d0c0", m8ActOpIndex(operationID), slices.Index(m8DockerLayout, kind)+1)
}

// m8DockerPayload is a canonical closed-archive descriptor, distinct per seed.
func m8DockerPayload(seed byte) domain.ProbeHostActionPayload {
	return domain.ProbeHostActionPayload{
		SHA256:            hex.EncodeToString(bytes.Repeat([]byte{seed}, 32)),
		Size:              4096 + int64(seed),
		LayoutIndexSHA256: hex.EncodeToString(bytes.Repeat([]byte{seed + 1}, 32)),
	}
}

func m8DockerChallenge(signer ports.ProbeHostActionGrantSigner, intent domain.ProbeHostActionIntent) domain.ProbeBinaryStageChallenge {
	return domain.ProbeBinaryStageChallenge{
		AuthorityKeyID: domain.ProbeBinaryStageAuthorityKeyID(signer.PublicKey()), Nonce: [32]byte{1}, MarkerSHA256: intent.MarkerSHA256(),
	}
}

// m8DockerRequest is the admission request for one Docker install resource,
// owned by owner A like every journal fixture here.
// image_cache carries the payload; every other kind must leave it zero.
func m8DockerRequest(signer ports.ProbeHostActionGrantSigner, operationID, kind string) domain.ProbeHostActionAdmissionRequest {
	intent := domain.ProbeHostActionIntent{Identity: m8DockerIdentity(operationID), OwnerID: m8InstallOwnerA, AttemptID: m8DockerAttempt(operationID, kind), Kind: kind}
	if kind == domain.ProbeHostActionImageCache {
		intent.Payload = m8DockerPayload(1)
	}
	return domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: m8DockerChallenge(signer, intent)}
}

// m8DockerActivationRequest is the admission request for one Docker activation
// host step (probe_init or service_start).
func m8DockerActivationRequest(signer ports.ProbeHostActionGrantSigner, operationID, ownerID, kind string) domain.ProbeHostActionAdmissionRequest {
	intent := domain.ProbeHostActionIntent{Identity: m8DockerIdentity(operationID), OwnerID: ownerID, AttemptID: m8ActAttempt(operationID, kind), Kind: kind}
	return domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: m8DockerChallenge(signer, intent)}
}

// m8DockerActivationIdentity is a valid activation request over a root Docker install.
func m8DockerActivationIdentity(operationID string) domain.ProbeActivationIdentity {
	install := m8DockerIdentity(operationID)
	return domain.ProbeActivationIdentity{
		Install: install, ProbeKey: "edge-eu-1", Name: "Edge EU 1", Location: "Frankfurt",
		ListenPort: domain.ProbeActivationDefaultPort, ClientKeyFingerprint: install.ClientKeyFingerprint,
		RequestedBy: install.RequestedBy,
	}
}

// m8DockerInstallReady drives a Docker install to resources_ready through the
// production journal store: image_cache, data_directory and container reserved
// and recorded applied in order.
func m8DockerInstallReady(t *testing.T, f probeRegistryFixture, operationID string) {
	t.Helper()
	store := m8InstallStore(f)
	m8InstallCreate(t, store, m8DockerIdentity(operationID), m8InstallOwnerA)
	m8InstallBeginOK(t, store, operationID, m8InstallOwnerA)
	var last *domain.ProbeInstallOperation
	for _, kind := range m8DockerLayout {
		attempt := m8DockerAttempt(operationID, kind)
		m8InstallReserveOK(t, store, operationID, m8InstallOwnerA, kind, attempt)
		last = m8InstallRecordOK(t, store, operationID, kind, attempt, m8InstallApplied())
	}
	if last.State != domain.ProbeInstallResourcesReady {
		t.Fatalf("docker install did not reach resources_ready: %+v", last)
	}
}

const (
	// m8DockerInFlight leaves every predecessor applied and the target reserved.
	m8DockerInFlight = iota
	// m8DockerPlanned leaves every predecessor applied and the target planned.
	m8DockerPlanned
	// m8DockerPredecessorOpen reserves, but never records, the immediate predecessor.
	m8DockerPredecessorOpen
)

// m8DockerEnv is one isolated engine fixture for the Docker host-action tests.
type m8DockerEnv struct {
	t      *testing.T
	ctx    context.Context
	f      probeRegistryFixture
	engine string
	signer *m8HostSigner
	store  *repository.ProbeHostActionAdmissionStore
	svc    *services.ProbeHostActionAuthorizationService
}

func newM8DockerEnv(t *testing.T, engine string) *m8DockerEnv {
	t.Helper()
	f := newProbeRegistryFixture(t, engine)
	signer := m8HostAuthority(t, 53)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	store := repository.NewProbeHostActionAdmissionStore(f.db)
	return &m8DockerEnv{t: t, ctx: ctx, f: f, engine: engine, signer: signer, store: store, svc: m8HostService(t, store, signer)}
}

// prepare reaches the requested journal state exclusively through production
// store entry points: every predecessor is reserved and then recorded applied.
func (e *m8DockerEnv) prepare(r domain.ProbeHostActionAdmissionRequest, stage int) {
	e.t.Helper()
	store := m8InstallStore(e.f)
	id, owner := r.Intent.Identity, r.Intent.OwnerID
	m8InstallCreate(e.t, store, id, owner)
	m8InstallBeginOK(e.t, store, id.OperationID, owner)
	target := slices.Index(m8DockerLayout, r.Intent.Kind)
	for i := range target {
		kind, attempt := m8DockerLayout[i], m8DockerAttempt(id.OperationID, m8DockerLayout[i])
		m8InstallReserveOK(e.t, store, id.OperationID, owner, kind, attempt)
		if stage == m8DockerPredecessorOpen && i == target-1 {
			break
		}
		m8InstallRecordOK(e.t, store, id.OperationID, kind, attempt, m8InstallApplied())
	}
	if stage == m8DockerInFlight {
		m8InstallReserveOK(e.t, store, id.OperationID, owner, r.Intent.Kind, r.Intent.AttemptID)
	}
}

func (e *m8DockerEnv) rows(want int64) {
	e.t.Helper()
	count, err := m8HostCount(e.t.Context(), e.f.db)
	if err != nil || count != want {
		e.t.Fatalf("admission row count: got %d want %d (err=%v)", count, want, err)
	}
}

func (e *m8DockerEnv) refused(grant domain.ProbeHostActionGrant, err, want error) {
	e.t.Helper()
	if grant != (domain.ProbeHostActionGrant{}) || !errors.Is(err, want) {
		e.t.Fatalf("zero grant/fixed refusal missing (match=%v, err=%v)", errors.Is(err, want), err)
	}
}

func (e *m8DockerEnv) granted(grant domain.ProbeHostActionGrant, err error, r domain.ProbeHostActionAdmissionRequest) {
	e.t.Helper()
	if err != nil || grant.Request != r || !grant.ValidFor(e.signer.PublicKey()) {
		e.t.Fatalf("expected exact cryptographically verified host action grant: %v", err)
	}
}

func (e *m8DockerEnv) noSigning() {
	e.t.Helper()
	if e.signer.calls.Load() != 0 {
		e.t.Fatal("refused admission was signed")
	}
}

func (e *m8DockerEnv) rawAdmissions() []m8HostRawAdmission {
	e.t.Helper()
	var rows []m8HostRawAdmission
	if err := e.f.db.NewSelect().Model(&rows).Order("operation_id ASC", "kind ASC").Scan(e.t.Context()); err != nil {
		e.t.Fatalf("read admissions: %v", err)
	}
	return rows
}

// insertRaw writes one admission row straight into the table so the schema, not
// the store, is the only thing deciding whether it is accepted.
func (e *m8DockerEnv) insertRaw(operationID, kind, attemptID string, nonceSeed byte) error {
	return m8DockerInsertRaw(e.ctx, e.f, operationID, kind, attemptID, nonceSeed)
}

func m8DockerInsertRaw(ctx context.Context, f probeRegistryFixture, operationID, kind, attemptID string, nonceSeed byte) error {
	_, err := f.db.ExecContext(ctx,
		"INSERT INTO probe_host_action_admissions (operation_id, kind, owner_id, attempt_id, authority_key_id, nonce_hex, marker_sha256, admitted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		operationID, kind, m8InstallOwnerA, attemptID, strings.Repeat("a", 64), hex.EncodeToString(bytes.Repeat([]byte{nonceSeed}, 32)),
		strings.Repeat("b", 64), time.Now().UTC().Truncate(time.Microsecond))
	return err
}

// m8DockerCheckRefused reports a CHECK-constraint refusal on the kind column:
// SQLite says "CHECK constraint failed: kind IN (...)" and MariaDB says
// "CONSTRAINT `probe_host_action_admissions.kind` failed for ...". A foreign-key
// or key refusal reads differently, so this cannot pass for the wrong reason.
func m8DockerCheckRefused(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "kind") && (strings.Contains(msg, "check constraint failed") || strings.Contains(msg, "failed for"))
}

type m8DockerKindLeaf struct {
	name  string
	stage int
	// skip returns a non-empty reason for kinds where the scenario has no
	// meaning; such a leaf is not generated for that kind at all.
	skip func(kind string) string
	run  func(e *m8DockerEnv, r domain.ProbeHostActionAdmissionRequest)
}

func m8DockerKindLeaves() []m8DockerKindLeaf {
	return []m8DockerKindLeaf{
		{name: "AdmitsAndGrantsVerifiedSignature", stage: m8DockerInFlight, run: func(e *m8DockerEnv, r domain.ProbeHostActionAdmissionRequest) {
			op := r.Intent.Identity.OperationID
			index := slices.Index(m8DockerLayout, r.Intent.Kind)
			before := m8InstallRawResources(e.t, e.f, op)
			g, err := e.svc.Authorize(e.ctx, r)
			e.granted(g, err, r)
			if g.ValidFor(m8HostAuthority(e.t, 54).PublicKey()) {
				e.t.Fatal("grant verified under a foreign authority")
			}
			if g.Request.Intent.Payload != r.Intent.Payload {
				e.t.Fatal("grant lost the image_cache payload")
			}
			e.rows(1)
			after := m8InstallRawResources(e.t, e.f, op)
			if len(after) != len(before) || len(after) != len(m8DockerLayout) ||
				after[index].State != domain.ProbeInstallResourceInFlight || after[index].FinishedAt != nil ||
				!after[index].StartedAt.Equal(*before[index].StartedAt) {
				e.t.Fatal("admission fabricated an applied resource")
			}
			for i := range index {
				if after[i].State != domain.ProbeInstallResourceApplied {
					e.t.Fatal("admission disturbed an applied predecessor")
				}
			}
			if e.signer.calls.Load() != 1 {
				e.t.Fatal("exactly one signature expected")
			}
		}},
		{name: "StoreRecordsExactRow", stage: m8DockerInFlight, run: func(e *m8DockerEnv, r domain.ProbeHostActionAdmissionRequest) {
			a, err := e.store.AdmitHostAction(e.ctx, r)
			if err != nil || a.Request != r || !a.Valid() {
				e.t.Fatalf("direct admission refused: %v", err)
			}
			if a.AdmittedAt.Location() != time.UTC || a.AdmittedAt.Nanosecond()%1000 != 0 {
				e.t.Fatal("admission stamp is not UTC microseconds")
			}
			rows := e.rawAdmissions()
			if len(rows) != 1 {
				e.t.Fatalf("expected one admission row, got %d", len(rows))
			}
			row, intent := rows[0], r.Intent
			if row.OperationID != intent.Identity.OperationID || row.Kind != intent.Kind || row.OwnerID != intent.OwnerID ||
				row.AttemptID != intent.AttemptID || row.AuthorityKeyID != r.Challenge.AuthorityKeyID ||
				row.NonceHex != hex.EncodeToString(r.Challenge.Nonce[:]) || row.MarkerSHA256 != intent.MarkerSHA256() ||
				!row.AdmittedAt.Equal(a.AdmittedAt) {
				e.t.Fatal("stored admission differs from the returned admission")
			}
			// The marker the row stores is the Docker marker: it names the image
			// selection, never the systemd binary, and binds the kind's own bytes.
			marker := intent.Marker()
			for _, want := range []string{
				"mode=docker\n", "kind=" + intent.Kind + "\n",
				"image_manifest_sha256=" + intent.Identity.Image.ManifestSHA256 + "\n",
				"image_config_sha256=" + intent.Identity.Image.ConfigSHA256 + "\n",
			} {
				if !strings.Contains(marker, want) {
					e.t.Fatalf("docker marker does not bind %q", want)
				}
			}
			if strings.Contains(marker, "\nsha256=") || strings.Contains(marker, "\nsize=") {
				e.t.Fatal("docker marker still lists systemd binary fields")
			}
			switch intent.Kind {
			case domain.ProbeHostActionImageCache:
				if !strings.Contains(marker, "payload_sha256="+intent.Payload.SHA256+"\n") {
					e.t.Fatal("image_cache marker does not bind its payload")
				}
			case domain.ProbeHostActionContainer:
				if !strings.Contains(marker, "container_sha256=") {
					e.t.Fatal("container marker does not bind the derived create arguments")
				}
			}
			if op := m8InstallRawOp(e.t, e.f, intent.Identity.OperationID); !op.UpdatedAt.Equal(a.AdmittedAt) || op.State != domain.ProbeInstallRunning {
				e.t.Fatal("operation causal stamp was not advanced to the admission stamp")
			}
		}},
		{name: "ExactReplayConflict", stage: m8DockerInFlight, run: func(e *m8DockerEnv, r domain.ProbeHostActionAdmissionRequest) {
			g, err := e.svc.Authorize(e.ctx, r)
			e.granted(g, err, r)
			before := m8InstallSnapshot(e.t, e.f, r.Intent.Identity.OperationID)
			g, err = e.svc.Authorize(e.ctx, r)
			e.refused(g, err, ports.ErrConflict)
			if a, err := e.store.AdmitHostAction(e.ctx, r); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, ports.ErrConflict) {
				e.t.Fatal("direct store replay returned an admission")
			}
			// A cold process on independent connections sees the same history.
			cold := m8HostService(e.t, repository.NewProbeHostActionAdmissionStore(m8InstallSecondDB(e.t, e.f, "")), m8HostAuthority(e.t, 53))
			g, err = cold.Authorize(e.ctx, r)
			e.refused(g, err, ports.ErrConflict)
			if before != m8InstallSnapshot(e.t, e.f, r.Intent.Identity.OperationID) {
				e.t.Fatal("replay modified durable history")
			}
			e.rows(1)
			if e.signer.calls.Load() != 1 {
				e.t.Fatal("replay signed again")
			}
		}},
		{name: "DifferentChallengeNoRearm", stage: m8DockerInFlight, run: func(e *m8DockerEnv, r domain.ProbeHostActionAdmissionRequest) {
			g, err := e.svc.Authorize(e.ctx, r)
			e.granted(g, err, r)
			fresh := r
			fresh.Challenge.Nonce[1]++
			g, err = e.svc.Authorize(e.ctx, fresh)
			e.refused(g, err, ports.ErrConflict)
			rotated := m8HostAuthority(e.t, 54)
			fresh.Challenge.AuthorityKeyID = domain.ProbeBinaryStageAuthorityKeyID(rotated.PublicKey())
			g, err = m8HostService(e.t, e.store, rotated).Authorize(e.ctx, fresh)
			e.refused(g, err, ports.ErrConflict)
			if rotated.calls.Load() != 0 {
				e.t.Fatal("authority rotation rearmed a consumed attempt")
			}
			e.rows(1)
		}},
		{name: "PredecessorNotApplied", stage: m8DockerPredecessorOpen,
			skip: func(kind string) string {
				if kind == domain.ProbeHostActionImageCache {
					return "image_cache is the first Docker layout resource and has no predecessor"
				}
				return ""
			},
			run: func(e *m8DockerEnv, r domain.ProbeHostActionAdmissionRequest) {
				op := r.Intent.Identity.OperationID
				before := m8InstallSnapshot(e.t, e.f, op)
				g, err := e.svc.Authorize(e.ctx, r)
				e.refused(g, err, ports.ErrConflict)
				e.rows(0)
				e.noSigning()
				if before != m8InstallSnapshot(e.t, e.f, op) {
					e.t.Fatal("refusal modified durable history")
				}
				// The journal enforces the same ordering, so no admission can
				// become eligible until the predecessor really commits applied.
				store := m8InstallStore(e.f)
				if _, _, err := store.ReserveInstallResource(e.ctx, op, r.Intent.OwnerID, r.Intent.Kind, r.Intent.AttemptID); err == nil {
					e.t.Fatal("journal reserved a resource behind an unapplied predecessor")
				}
				previous := m8DockerLayout[slices.Index(m8DockerLayout, r.Intent.Kind)-1]
				m8InstallRecordOK(e.t, store, op, previous, m8DockerAttempt(op, previous), m8InstallApplied())
				m8InstallReserveOK(e.t, store, op, r.Intent.OwnerID, r.Intent.Kind, r.Intent.AttemptID)
				g, err = e.svc.Authorize(e.ctx, r)
				e.granted(g, err, r)
				e.rows(1)
			}},
		{name: "ResourceNotInFlight", stage: m8DockerPlanned, run: func(e *m8DockerEnv, r domain.ProbeHostActionAdmissionRequest) {
			op := r.Intent.Identity.OperationID
			before := m8InstallSnapshot(e.t, e.f, op)
			g, err := e.svc.Authorize(e.ctx, r)
			e.refused(g, err, ports.ErrConflict)
			e.rows(0)
			e.noSigning()
			if before != m8InstallSnapshot(e.t, e.f, op) {
				e.t.Fatal("planned-resource refusal modified durable history")
			}
			m8InstallReserveOK(e.t, m8InstallStore(e.f), op, r.Intent.OwnerID, r.Intent.Kind, r.Intent.AttemptID)
			g, err = e.svc.Authorize(e.ctx, r)
			e.granted(g, err, r)
			e.rows(1)
		}},
		{name: "WrongAttempt", stage: m8DockerInFlight, run: func(e *m8DockerEnv, r domain.ProbeHostActionAdmissionRequest) {
			op := r.Intent.Identity.OperationID
			before := m8InstallSnapshot(e.t, e.f, op)
			// An unknown attempt and every attempt of the other Docker layout
			// resources must each be refused for this resource.
			attempts := []string{m8DockerForeignAttempt}
			for _, kind := range m8DockerLayout {
				if kind != r.Intent.Kind {
					attempts = append(attempts, m8DockerAttempt(op, kind))
				}
			}
			for _, attempt := range attempts {
				bad := r
				bad.Intent.AttemptID = attempt
				bad.Challenge.MarkerSHA256 = bad.Intent.MarkerSHA256()
				g, err := e.svc.Authorize(e.ctx, bad)
				e.refused(g, err, ports.ErrConflict)
			}
			e.rows(0)
			e.noSigning()
			if before != m8InstallSnapshot(e.t, e.f, op) {
				e.t.Fatal("wrong-attempt refusal modified durable history")
			}
			g, err := e.svc.Authorize(e.ctx, r)
			e.granted(g, err, r)
		}},
	}
}

type m8DockerEngineLeaf struct {
	name string
	run  func(t *testing.T, engine string)
}

// TestM8DockerHostActions proves the persistence side of the Docker install
// path on both engines: one-shot host-action admission of every Docker resource
// kind (including the image_cache payload binding), the kinds the other mode
// must refuse, Docker activation and cleanup over the existing stores, and the
// 089 schema widening with its guarded downgrade. The MariaDB leaves skip
// without TEST_MARIADB_DSN and must be exercised by the real-engine gate; a skip
// establishes nothing about InnoDB.
func TestM8DockerHostActions(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, kind := range m8DockerLayout {
				t.Run(kind, func(t *testing.T) {
					for _, leaf := range m8DockerKindLeaves() {
						// A leaf with a documented reason is not generated for that
						// kind, so it never shows up as a misleading skip or pass.
						if leaf.skip != nil && leaf.skip(kind) != "" {
							continue
						}
						t.Run(leaf.name, func(t *testing.T) {
							e := newM8DockerEnv(t, engine)
							r := m8DockerRequest(e.signer, m8InstallOp1, kind)
							e.prepare(r, leaf.stage)
							leaf.run(e, r)
						})
					}
				})
			}
			for _, leaf := range m8DockerEngineLeaves() {
				t.Run(leaf.name, func(t *testing.T) { leaf.run(t, engine) })
			}
		})
	}
}

func m8DockerEngineLeaves() []m8DockerEngineLeaf {
	return []m8DockerEngineLeaf{
		{"ImageCachePayloadBindsMarker", m8DockerImageCachePayload},
		{"ModeBoundMarkersStoredSeparately", m8DockerModeBoundMarkers},
		{"SystemdKindsRefusedForDockerInstall", m8DockerRefusesSystemdKinds},
		{"DockerKindsRefusedForSystemdInstall", m8DockerRefusesDockerKindsOnSystemd},
		{"ActivationCreateAndSteps", m8DockerActivationCreate},
		{"ActivationStepAdmissions", m8DockerActivationAdmissions},
		{"CleanupCreateAndAdmit", m8DockerCleanupAdmit},
		{"CleanupOverPartialInstall", m8DockerCleanupPartial},
		{"SchemaAcceptsDockerKinds", m8DockerSchemaKinds},
		{"SchemaKeepsConstraints", m8DockerSchemaConstraints},
		{"UpgradePreservesAdmissions", m8DockerUpgradePreserves},
		{"DowngradeGuard", m8DockerDowngradeGuard},
	}
}

// The marker of an image_cache admission binds the payload: a different archive
// is a different marker, the stored marker is the one that was consumed, and a
// different payload can never rearm a consumed attempt.
func m8DockerImageCachePayload(t *testing.T, engine string) {
	e := newM8DockerEnv(t, engine)
	r := m8DockerRequest(e.signer, m8InstallOp1, domain.ProbeHostActionImageCache)
	e.prepare(r, m8DockerInFlight)
	base := r.Intent.Payload
	mutate := map[string]func(*domain.ProbeHostActionPayload){
		"sha256": func(p *domain.ProbeHostActionPayload) { p.SHA256 = m8DockerPayload(2).SHA256 },
		"size":   func(p *domain.ProbeHostActionPayload) { p.Size++ },
		"layout": func(p *domain.ProbeHostActionPayload) { p.LayoutIndexSHA256 = m8DockerPayload(2).LayoutIndexSHA256 },
	}
	for name, change := range mutate {
		alt := r
		change(&alt.Intent.Payload)
		if alt.Intent.Payload == base || !alt.Intent.Valid() {
			t.Fatalf("%s: fixture payload is not a distinct valid payload", name)
		}
		if alt.Intent.MarkerSHA256() == r.Intent.MarkerSHA256() {
			t.Fatalf("%s: marker does not bind the payload", name)
		}
		// The challenge still names the other payload's marker: not this intent.
		alt.Challenge.MarkerSHA256 = r.Challenge.MarkerSHA256
		if alt.Valid() {
			t.Fatalf("%s: a request whose marker names another payload is valid", name)
		}
		if a, err := e.store.AdmitHostAction(e.ctx, alt); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: store admitted a payload its marker does not bind: %v", name, err)
		}
		g, err := e.svc.Authorize(e.ctx, alt)
		e.refused(g, err, domain.ErrValidation)
	}
	// The payload belongs to image_cache alone, and image_cache needs one.
	missing := r
	missing.Intent.Payload = domain.ProbeHostActionPayload{}
	missing.Challenge.MarkerSHA256 = missing.Intent.MarkerSHA256()
	stray := m8DockerRequest(e.signer, m8InstallOp1, domain.ProbeHostActionDataDirectory)
	stray.Intent.Payload = base
	stray.Challenge.MarkerSHA256 = stray.Intent.MarkerSHA256()
	for name, bad := range map[string]domain.ProbeHostActionAdmissionRequest{"missing_payload": missing, "payload_on_data_directory": stray} {
		if bad.Valid() {
			t.Fatalf("%s: domain accepted a payload outside the image_cache contract", name)
		}
		if a, err := e.store.AdmitHostAction(e.ctx, bad); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: store admitted it: %v", name, err)
		}
	}
	e.rows(0)
	e.noSigning()
	// Admit the exact request: the stored marker is the payload-bound one.
	g, err := e.svc.Authorize(e.ctx, r)
	e.granted(g, err, r)
	if rows := e.rawAdmissions(); len(rows) != 1 || rows[0].MarkerSHA256 != r.Challenge.MarkerSHA256 || rows[0].Kind != domain.ProbeHostActionImageCache {
		t.Fatal("stored admission does not hold the payload-bound marker")
	}
	g, err = e.svc.Authorize(e.ctx, r)
	e.refused(g, err, ports.ErrConflict)
	// A different archive is a different, fully valid marker, and still cannot
	// consume the same attempt again.
	other := r
	other.Intent.Payload = m8DockerPayload(3)
	other.Challenge.MarkerSHA256 = other.Intent.MarkerSHA256()
	other.Challenge.Nonce[1]++
	if !other.Valid() || other.Challenge.MarkerSHA256 == r.Challenge.MarkerSHA256 {
		t.Fatal("fixture: the second payload is not a distinct valid request")
	}
	g, err = e.svc.Authorize(e.ctx, other)
	e.refused(g, err, ports.ErrConflict)
	if rows := e.rawAdmissions(); len(rows) != 1 || rows[0].MarkerSHA256 != r.Challenge.MarkerSHA256 {
		t.Fatal("a different payload replaced or added to the consumed admission")
	}
	if e.signer.calls.Load() != 1 {
		t.Fatal("replays signed again")
	}
}

// The same data_directory attempt on a Docker and a systemd install hashes to
// different markers (the mode and the image or binary tuple are bound), and the
// store keeps each exactly as consumed.
func m8DockerModeBoundMarkers(t *testing.T, engine string) {
	e := newM8DockerEnv(t, engine)
	docker := m8DockerRequest(e.signer, m8InstallOp1, domain.ProbeHostActionDataDirectory)
	e.prepare(docker, m8DockerInFlight)
	systemd := m8HostRequest(e.signer, m8InstallOp2, m8InstallOwnerB, domain.ProbeHostActionDataDirectory)
	systemd.Challenge.Nonce[1]++
	m8HostPrepare(t, e.f, systemd, m8HostInFlight)
	for _, r := range []domain.ProbeHostActionAdmissionRequest{docker, systemd} {
		g, err := e.svc.Authorize(e.ctx, r)
		e.granted(g, err, r)
	}
	rows := e.rawAdmissions()
	if len(rows) != 2 || rows[0].MarkerSHA256 != docker.Challenge.MarkerSHA256 || rows[1].MarkerSHA256 != systemd.Challenge.MarkerSHA256 {
		t.Fatal("stored admissions differ from the consumed markers")
	}
	if docker.Challenge.MarkerSHA256 == systemd.Challenge.MarkerSHA256 {
		t.Fatal("a Docker and a systemd data_directory share a marker")
	}
	// Distinct Docker kinds never share one either.
	seen := map[string]string{}
	for _, kind := range m8DockerLayout {
		marker := m8DockerRequest(e.signer, m8InstallOp1, kind).Challenge.MarkerSHA256
		if other, dup := seen[marker]; dup {
			t.Fatalf("%s shares a marker with %s", kind, other)
		}
		seen[marker] = kind
	}
}

// A Docker install has no unit_file or service: the domain refuses those kinds,
// the store refuses them before reaching the database, and the journal knows no
// such resource on that operation.
func m8DockerRefusesSystemdKinds(t *testing.T, engine string) {
	e := newM8DockerEnv(t, engine)
	r := m8DockerRequest(e.signer, m8InstallOp1, domain.ProbeHostActionDataDirectory)
	e.prepare(r, m8DockerInFlight)
	op := r.Intent.Identity.OperationID
	before := m8InstallSnapshot(t, e.f, op)
	for _, kind := range []string{domain.ProbeHostActionUnitFile, domain.ProbeHostActionService, "artifact_directory"} {
		bad := domain.ProbeHostActionAdmissionRequest{
			Intent: domain.ProbeHostActionIntent{Identity: r.Intent.Identity, OwnerID: r.Intent.OwnerID, AttemptID: m8DockerForeignAttempt, Kind: kind},
			Challenge: domain.ProbeBinaryStageChallenge{
				AuthorityKeyID: r.Challenge.AuthorityKeyID, Nonce: [32]byte{1}, MarkerSHA256: strings.Repeat("e", 64),
			},
		}
		if bad.Intent.Valid() || bad.Valid() {
			t.Fatalf("%s: domain accepted a systemd kind for a Docker install", kind)
		}
		if a, err := e.store.AdmitHostAction(e.ctx, bad); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: store admitted a systemd kind for a Docker install: %v", kind, err)
		}
		g, err := e.svc.Authorize(e.ctx, bad)
		e.refused(g, err, domain.ErrValidation)
		if _, _, err := m8InstallStore(e.f).ReserveInstallResource(e.ctx, op, r.Intent.OwnerID, kind, m8DockerForeignAttempt); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("%s: journal reserved a resource a Docker install does not have: %v", kind, err)
		}
	}
	e.rows(0)
	e.noSigning()
	if before != m8InstallSnapshot(t, e.f, op) {
		t.Fatal("refusals modified durable history")
	}
	// The Docker install itself is unharmed and still admits its own kind.
	g, err := e.svc.Authorize(e.ctx, r)
	e.granted(g, err, r)
}

// The mirror image: a systemd install has no image_cache or container.
func m8DockerRefusesDockerKindsOnSystemd(t *testing.T, engine string) {
	e := newM8DockerEnv(t, engine)
	r := m8HostRequest(e.signer, m8InstallOp1, m8InstallOwnerA, domain.ProbeHostActionDataDirectory)
	m8HostPrepare(t, e.f, r, m8HostInFlight)
	op := r.Intent.Identity.OperationID
	before := m8InstallSnapshot(t, e.f, op)
	for _, kind := range []string{domain.ProbeHostActionImageCache, domain.ProbeHostActionContainer} {
		intent := domain.ProbeHostActionIntent{Identity: r.Intent.Identity, OwnerID: r.Intent.OwnerID, AttemptID: m8DockerForeignAttempt, Kind: kind}
		if kind == domain.ProbeHostActionImageCache {
			intent.Payload = m8DockerPayload(1)
		}
		bad := domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: domain.ProbeBinaryStageChallenge{
			AuthorityKeyID: r.Challenge.AuthorityKeyID, Nonce: [32]byte{1}, MarkerSHA256: strings.Repeat("e", 64),
		}}
		if bad.Intent.Valid() || bad.Valid() {
			t.Fatalf("%s: domain accepted a Docker kind for a systemd install", kind)
		}
		if a, err := e.store.AdmitHostAction(e.ctx, bad); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("%s: store admitted a Docker kind for a systemd install: %v", kind, err)
		}
		g, err := e.svc.Authorize(e.ctx, bad)
		e.refused(g, err, domain.ErrValidation)
		if _, _, err := m8InstallStore(e.f).ReserveInstallResource(e.ctx, op, r.Intent.OwnerID, kind, m8DockerForeignAttempt); !errors.Is(err, ports.ErrNotFound) {
			t.Fatalf("%s: journal reserved a resource a systemd install does not have: %v", kind, err)
		}
	}
	e.rows(0)
	e.noSigning()
	if before != m8InstallSnapshot(t, e.f, op) {
		t.Fatal("refusals modified durable history")
	}
	g, err := e.svc.Authorize(e.ctx, r)
	e.granted(g, err, r)
}

// An activation over a resources_ready Docker install stores the image-bearing
// identity exactly, derives its step references through the layout (data
// directory and container, never fixed indexes), and runs the four steps to
// active.
func m8DockerActivationCreate(t *testing.T, engine string) {
	f := newProbeRegistryFixture(t, engine)
	ctx := t.Context()
	op := m8InstallOp1
	m8DockerInstallReady(t, f, op)
	store := repository.NewProbeInstallActivationStore(f.db)
	identity := m8DockerActivationIdentity(op)
	a, created, err := store.CreateActivation(ctx, identity, m8InstallOwnerA)
	if err != nil || !created || a == nil {
		t.Fatalf("create docker activation: created=%v err=%v", created, err)
	}
	m8ActCheck(t, a)
	if a.State != domain.ProbeActivationQueued || a.Identity != identity || a.Identity.Install.Image != m8DockerIdentity(op).Image {
		t.Fatalf("docker activation identity did not round-trip: %+v", a)
	}
	data, service, ok := domain.ProbeInstallLayout(identity.Install)
	steps := identity.Steps()
	if !ok || len(steps) != 4 || len(a.Steps) != 4 || steps[0].Reference != data.Reference || steps[2].Reference != service.Reference {
		t.Fatal("docker activation steps are not addressed through the layout")
	}
	if service.Kind != domain.ProbeHostActionContainer || data.Kind != domain.ProbeHostActionDataDirectory {
		t.Fatalf("layout resolved the wrong resources: %s / %s", data.Kind, service.Kind)
	}
	for i := range steps {
		if a.Steps[i].Spec != steps[i] || a.Steps[i].State != domain.ProbeInstallResourcePlanned {
			t.Fatalf("stored step %d differs from the derived layout: %+v", i, a.Steps[i])
		}
	}
	if raw := m8ActRawSteps(t, f, op); len(raw) != 4 || m8ActCount(t, f, "probe_activation_admissions") != 0 {
		t.Fatal("create stored a wrong step graph or consumed an admission")
	}
	// An exact retry returns the stored receipt whoever presents it.
	again, created, err := store.CreateActivation(ctx, identity, m8InstallOwnerB)
	if err != nil || created {
		t.Fatalf("exact docker retry: created=%v err=%v", created, err)
	}
	m8ActRequireSame(t, again, a, "exact retry")
	if again.OwnerID != m8InstallOwnerA {
		t.Fatal("retry rebound the owner")
	}
	// Another identity for the same operation conflicts, including the systemd one.
	for name, changed := range map[string]domain.ProbeActivationIdentity{
		"systemd": m8ActIdentity(op),
		"name":    func() domain.ProbeActivationIdentity { c := identity; c.Name = "Edge EU 2"; return c }(),
		"image": func() domain.ProbeActivationIdentity {
			c := identity
			c.Install.Image.LayerCount++
			return c
		}(),
	} {
		if _, created, err := store.CreateActivation(ctx, changed, m8InstallOwnerA); created || !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("%s: changed identity must conflict: created=%v err=%v", name, created, err)
		}
	}
	if _, err := store.BeginActivation(ctx, op, m8InstallOwnerA); err != nil {
		t.Fatalf("begin: %v", err)
	}
	var last *domain.ProbeActivation
	for _, kind := range m8ActKinds {
		attempt := m8ActAttempt(op, kind)
		if _, fresh, err := store.ReserveActivationStep(ctx, op, m8InstallOwnerA, kind, attempt); err != nil || !fresh {
			t.Fatalf("reserve %s: fresh=%v err=%v", kind, fresh, err)
		}
		facts := domain.ProbeActivationFacts{}
		if kind == domain.ProbeActivationProbeInit {
			facts = m8ActFacts(op)
		}
		if last, err = store.RecordActivationStep(ctx, op, m8InstallOwnerA, kind, attempt, m8InstallApplied(), facts); err != nil {
			t.Fatalf("record %s: %v", kind, err)
		}
	}
	if last.State != domain.ProbeActivationActive || last.Facts != m8ActFacts(op) || last.Identity != identity {
		t.Fatalf("docker activation did not reach active with its facts: %+v", last)
	}
	m8ActRequireSame(t, m8ActCheck(t, last), func() *domain.ProbeActivation {
		observed, err := store.ObserveActivation(ctx, op)
		if err != nil {
			t.Fatalf("observe: %v", err)
		}
		return observed
	}(), "observe after active")
}

// The probe_init and service_start steps of a Docker activation are consumed
// once each by the activation admission store, bound to the Docker marker, and
// stay separate from the install resource admissions.
func m8DockerActivationAdmissions(t *testing.T, engine string) {
	f := newProbeRegistryFixture(t, engine)
	ctx := t.Context()
	op := m8InstallOp1
	m8DockerInstallReady(t, f, op)
	store := repository.NewProbeInstallActivationStore(f.db)
	if _, created, err := store.CreateActivation(ctx, m8DockerActivationIdentity(op), m8InstallOwnerA); err != nil || !created {
		t.Fatalf("create docker activation: created=%v err=%v", created, err)
	}
	if _, err := store.BeginActivation(ctx, op, m8InstallOwnerA); err != nil {
		t.Fatalf("begin: %v", err)
	}
	signer := m8HostAuthority(t, 53)
	svc := m8ActServiceOver(t, store, signer)
	reserve := func(kind string) {
		t.Helper()
		if _, fresh, err := store.ReserveActivationStep(ctx, op, m8InstallOwnerA, kind, m8ActAttempt(op, kind)); err != nil || !fresh {
			t.Fatalf("reserve %s: fresh=%v err=%v", kind, fresh, err)
		}
	}
	record := func(kind string) {
		t.Helper()
		facts := domain.ProbeActivationFacts{}
		if kind == domain.ProbeActivationProbeInit {
			facts = m8ActFacts(op)
		}
		if _, err := store.RecordActivationStep(ctx, op, m8InstallOwnerA, kind, m8ActAttempt(op, kind), m8InstallApplied(), facts); err != nil {
			t.Fatalf("record %s: %v", kind, err)
		}
	}
	refusedHere := func(r domain.ProbeHostActionAdmissionRequest, want error) {
		t.Helper()
		g, err := svc.Authorize(ctx, r)
		if g != (domain.ProbeHostActionGrant{}) || !errors.Is(err, want) {
			t.Fatalf("%s: expected a zero grant and %v, got %v", r.Intent.Kind, want, err)
		}
	}
	// Install resource kinds are never consumable through the activation store.
	// The store refuses them as a validation error. The service folds any store
	// validation error into an internal one, so the store is asked directly.
	foreign := m8DockerRequest(signer, op, domain.ProbeHostActionContainer)
	if a, err := store.AdmitActivationAction(ctx, foreign); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("the activation store consumed an install resource kind: %v", err)
	}
	refusedHere(foreign, domain.ErrInternal)
	if n := m8ActCount(t, f, "probe_activation_admissions"); n != 0 {
		t.Fatalf("a foreign kind consumed an activation admission: %d", n)
	}
	for i, kind := range []string{domain.ProbeActivationProbeInit, domain.ProbeActivationServiceStart} {
		if kind == domain.ProbeActivationServiceStart {
			record(domain.ProbeActivationProbeInit)
			reserve(domain.ProbeActivationRegistration)
			record(domain.ProbeActivationRegistration)
		}
		r := m8DockerActivationRequest(signer, op, m8InstallOwnerA, kind)
		// (authority, nonce) is unique across admissions: each step gets its own.
		r.Challenge.Nonce[1] = byte(i)
		if !r.Valid() {
			t.Fatalf("%s: docker activation request invalid", kind)
		}
		spec, _ := r.Intent.Resource()
		data, service, _ := domain.ProbeInstallLayout(r.Intent.Identity)
		if want := map[string]string{domain.ProbeActivationProbeInit: data.Reference, domain.ProbeActivationServiceStart: service.Reference}[kind]; spec.Reference != want {
			t.Fatalf("%s: intent references %q, want %q", kind, spec.Reference, want)
		}
		if !strings.Contains(r.Intent.Marker(), "mode=docker\n") {
			t.Fatalf("%s: marker does not bind the Docker mode", kind)
		}
		// Not in flight yet: refused without consumption.
		refusedHere(r, ports.ErrConflict)
		reserve(kind)
		g, err := svc.Authorize(ctx, r)
		if err != nil || g.Request != r || !g.ValidFor(signer.PublicKey()) {
			t.Fatalf("%s: expected a verified grant: %v", kind, err)
		}
		refusedHere(r, ports.ErrConflict)
		// The install admission store never consumes a probe step: the install
		// is resources_ready, not a running install, and has no such resource.
		if a, err := repository.NewProbeHostActionAdmissionStore(f.db).AdmitHostAction(ctx, r); a != (domain.ProbeHostActionAdmission{}) || !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("%s: the install admission store consumed an activation step: %v", kind, err)
		}
	}
	rows := m8ActRawAdmissions(t, f)
	if len(rows) != 2 || rows[0].Kind != domain.ProbeActivationProbeInit || rows[1].Kind != domain.ProbeActivationServiceStart {
		t.Fatalf("expected one admission per host step, got %+v", rows)
	}
	for _, row := range rows {
		if row.MarkerSHA256 != m8DockerActivationRequest(signer, op, m8InstallOwnerA, row.Kind).Challenge.MarkerSHA256 {
			t.Fatalf("%s: stored marker is not the Docker marker", row.Kind)
		}
	}
	if n, err := m8HostCount(ctx, f.db); err != nil || n != 0 {
		t.Fatalf("activation admissions leaked into the install admission table: %d %v", n, err)
	}
}

// A Docker install is cleanup-eligible like a systemd one: the cleanup identity
// round-trips with the image, the cleanup admission binds the Docker marker and
// is consumed once, and once cleaned no activation may be created over it.
func m8DockerCleanupAdmit(t *testing.T, engine string) {
	f := newProbeRegistryFixture(t, engine)
	ctx := t.Context()
	op := m8InstallOp1
	m8DockerInstallReady(t, f, op)
	store := repository.NewProbeInstallCleanupStore(f.db)
	install := m8DockerIdentity(op)
	identity := domain.ProbeCleanupIdentity{
		CleanupID: m8CleanID(1), Install: install, ClientKeyFingerprint: m8InstallFingerprint('c'), RequestedBy: install.RequestedBy,
	}
	created, fresh, err := store.CreateCleanup(ctx, identity, m8InstallOwnerA)
	if err != nil || !fresh || created.State != domain.ProbeCleanupQueued || created.Identity != identity || created.Identity.Install.Image != install.Image {
		t.Fatalf("create docker cleanup: fresh=%v err=%v %+v", fresh, err, created)
	}
	retry, fresh, err := store.CreateCleanup(ctx, identity, m8InstallOwnerB)
	if err != nil || fresh || retry.OwnerID != m8InstallOwnerA || retry.Identity != identity {
		t.Fatalf("exact docker cleanup retry: fresh=%v err=%v %+v", fresh, err, retry)
	}
	changed := identity
	changed.Install.Image.Pin.Version = "1.2.4"
	if _, fresh, err := store.CreateCleanup(ctx, changed, m8InstallOwnerA); fresh || err == nil {
		t.Fatalf("changed image must not create or replay a cleanup: fresh=%v err=%v", fresh, err)
	}
	running, err := store.BeginCleanup(ctx, identity.CleanupID, m8InstallOwnerA, m8CleanAttempt(1))
	if err != nil || running.State != domain.ProbeCleanupRunning || running.Identity != identity {
		t.Fatalf("begin docker cleanup: %+v err=%v", running, err)
	}
	signer := m8HostAuthority(t, 53)
	svc := m8HostService(t, m8HostRepositoryFunc(store.AdmitCleanupAction), signer)
	intent := identity.Intent(m8InstallOwnerA, m8CleanAttempt(1))
	r := domain.ProbeHostActionAdmissionRequest{Intent: intent, Challenge: m8DockerChallenge(signer, intent)}
	if !r.Valid() || !strings.Contains(intent.Marker(), "mode=docker\n") || !strings.Contains(intent.Marker(), "kind=cleanup\n") {
		t.Fatal("docker cleanup request is not a valid Docker-bound cleanup marker")
	}
	g, err := svc.Authorize(ctx, r)
	if err != nil || g.Request != r || !g.ValidFor(signer.PublicKey()) {
		t.Fatalf("docker cleanup admission refused: %v", err)
	}
	if g, err := svc.Authorize(ctx, r); g != (domain.ProbeHostActionGrant{}) || !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("exact replay must be refused: %v", err)
	}
	var rows []m8CleanRawAdmission
	if err := f.db.NewSelect().Model(&rows).Scan(ctx); err != nil || len(rows) != 1 || rows[0].MarkerSHA256 != r.Challenge.MarkerSHA256 || rows[0].CleanupID != identity.CleanupID {
		t.Fatalf("stored cleanup admission differs from the consumed marker: %+v %v", rows, err)
	}
	cleaned, err := store.RecordCleanup(ctx, identity.CleanupID, m8InstallOwnerA, m8CleanAttempt(1), m8InstallApplied())
	if err != nil || cleaned.State != domain.ProbeCleanupCleaned || cleaned.Identity != identity {
		t.Fatalf("record docker cleanup: %+v err=%v", cleaned, err)
	}
	observed, err := store.ObserveCleanup(ctx, identity.CleanupID)
	if err != nil || observed.Identity != identity || observed.State != domain.ProbeCleanupCleaned {
		t.Fatalf("observe docker cleanup: %+v err=%v", observed, err)
	}
	if latest, err := store.LatestCleanup(ctx, op); err != nil || latest.Identity != identity {
		t.Fatalf("latest docker cleanup: %+v err=%v", latest, err)
	}
	// A cleaned Docker install is never activated again.
	if _, created, err := repository.NewProbeInstallActivationStore(f.db).CreateActivation(ctx, m8DockerActivationIdentity(op), m8InstallOwnerA); created || !errors.Is(err, ports.ErrConflict) {
		t.Fatalf("activation over a cleaned docker install must conflict: created=%v err=%v", created, err)
	}
}

// Cleanup eligibility follows the install state, not the mode: a Docker install
// that failed after loading the shared image is cleanable, one still running is not.
func m8DockerCleanupPartial(t *testing.T, engine string) {
	f := newProbeRegistryFixture(t, engine)
	ctx := t.Context()
	istore := m8InstallStore(f)
	store := repository.NewProbeInstallCleanupStore(f.db)
	cleanup := func(op string, n int) domain.ProbeCleanupIdentity {
		install := m8DockerIdentity(op)
		return domain.ProbeCleanupIdentity{CleanupID: m8CleanID(n), Install: install, ClientKeyFingerprint: m8InstallFingerprint('c'), RequestedBy: install.RequestedBy}
	}
	// Running with the image still in flight: nothing may be cleaned yet.
	m8InstallCreate(t, istore, m8DockerIdentity(m8InstallOp1), m8InstallOwnerA)
	m8InstallBeginOK(t, istore, m8InstallOp1, m8InstallOwnerA)
	m8InstallReserveOK(t, istore, m8InstallOp1, m8InstallOwnerA, domain.ProbeHostActionImageCache, m8DockerAttempt(m8InstallOp1, domain.ProbeHostActionImageCache))
	if c, fresh, err := store.CreateCleanup(ctx, cleanup(m8InstallOp1, 1), m8InstallOwnerA); fresh || c != nil || !errors.Is(err, domain.ErrProbeCleanupIneligible) {
		t.Fatalf("cleanup over a running docker install must be ineligible: fresh=%v err=%v", fresh, err)
	}
	// Failed after the shared image was applied: eligible and queued.
	m8InstallRecordOK(t, istore, m8InstallOp1, domain.ProbeHostActionImageCache, m8DockerAttempt(m8InstallOp1, domain.ProbeHostActionImageCache), m8InstallApplied())
	if _, err := istore.FinishInstall(ctx, m8InstallOp1, m8InstallOwnerA, m8ActFinish(domain.ProbeInstallFailed, "execution_failed")); err != nil {
		t.Fatalf("finish docker install: %v", err)
	}
	c, fresh, err := store.CreateCleanup(ctx, cleanup(m8InstallOp1, 1), m8InstallOwnerA)
	if err != nil || !fresh || c.State != domain.ProbeCleanupQueued || c.Identity != cleanup(m8InstallOp1, 1) {
		t.Fatalf("cleanup over a failed docker install: fresh=%v err=%v %+v", fresh, err, c)
	}
}

// The 089 schema accepts every Docker kind (and the systemd kinds) for an
// existing resource row, and still refuses a kind that belongs to another table.
func m8DockerSchemaKinds(t *testing.T, engine string) {
	e := newM8DockerEnv(t, engine)
	store := m8InstallStore(e.f)
	m8InstallCreate(t, store, m8DockerIdentity(m8InstallOp1), m8InstallOwnerA)
	m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp2), m8InstallOwnerA)
	// artifact_directory has a resource row on a systemd install, so only the
	// kind CHECK can refuse it (it belongs to probe_binary_stage_admissions).
	err := e.insertRaw(m8InstallOp2, "artifact_directory", m8InstallAttempt1, 9)
	if !m8DockerCheckRefused(err) {
		t.Fatalf("the kind CHECK must refuse artifact_directory by itself: %v", err)
	}
	e.rows(0)
	nonce := byte(1)
	for _, kind := range m8DockerLayout {
		if err := e.insertRaw(m8InstallOp1, kind, m8DockerAttempt(m8InstallOp1, kind), nonce); err != nil {
			t.Fatalf("schema refused docker kind %s: %v", kind, err)
		}
		nonce++
	}
	for _, kind := range []string{domain.ProbeHostActionDataDirectory, domain.ProbeHostActionUnitFile, domain.ProbeHostActionService} {
		if err := e.insertRaw(m8InstallOp2, kind, m8HostAttempt(m8InstallOp2, kind), nonce); err != nil {
			t.Fatalf("schema refused systemd kind %s: %v", kind, err)
		}
		nonce++
	}
	e.rows(6)
	// Kinds with no resource row (or that are not kinds at all) are still refused.
	for _, kind := range []string{"bogus", "IMAGE_CACHE", "Container", ""} {
		if err := e.insertRaw(m8InstallOp1, kind, m8DockerForeignAttempt, 99); err == nil {
			t.Fatalf("schema accepted unknown kind %q", kind)
		}
	}
	e.rows(6)
	// The stored kind CHECK itself names the widened list.
	var clause string
	switch engine {
	case "sqlite":
		if err := e.f.db.NewRaw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'probe_host_action_admissions'").Scan(e.ctx, &clause); err != nil {
			t.Fatalf("read table definition: %v", err)
		}
		if !strings.Contains(clause, "'image_cache'") || !strings.Contains(clause, "'container'") {
			t.Fatalf("table definition lacks the Docker kinds: %s", clause)
		}
	default:
		var constraints []string
		if err := e.f.db.NewRaw("SELECT check_clause FROM information_schema.check_constraints WHERE constraint_schema = DATABASE() AND table_name = 'probe_host_action_admissions' AND check_clause LIKE '%unit_file%'").
			Scan(e.ctx, &constraints); err != nil {
			t.Fatalf("read kind constraint: %v", err)
		}
		if len(constraints) != 1 || !strings.Contains(constraints[0], "image_cache") || !strings.Contains(constraints[0], "container") {
			t.Fatalf("expected exactly one widened kind constraint, got %v", constraints)
		}
	}
}

// Widening kind must not loosen anything else the 086 table enforces.
func m8DockerSchemaConstraints(t *testing.T, engine string) {
	e := newM8DockerEnv(t, engine)
	store := m8InstallStore(e.f)
	m8InstallCreate(t, store, m8DockerIdentity(m8InstallOp1), m8InstallOwnerA)
	op := m8InstallOp1
	attempt := func(kind string) string { return m8DockerAttempt(op, kind) }
	if err := e.insertRaw(op, domain.ProbeHostActionImageCache, attempt(domain.ProbeHostActionImageCache), 1); err != nil {
		t.Fatalf("baseline admission: %v", err)
	}
	stamp := time.Now().UTC().Truncate(time.Microsecond)
	insert := func(operationID, kind, owner, attemptID, authority, nonce, marker string) error {
		_, err := e.f.db.ExecContext(e.ctx,
			"INSERT INTO probe_host_action_admissions (operation_id, kind, owner_id, attempt_id, authority_key_id, nonce_hex, marker_sha256, admitted_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			operationID, kind, owner, attemptID, authority, nonce, marker, stamp)
		return err
	}
	authority, fresh := strings.Repeat("a", 64), hex.EncodeToString(bytes.Repeat([]byte{7}, 32))
	marker := strings.Repeat("b", 64)
	good := func() []string {
		return []string{op, domain.ProbeHostActionContainer, m8InstallOwnerA, attempt(domain.ProbeHostActionContainer), authority, fresh, marker}
	}
	// The positive control proves the row below is refused for its one fault only.
	probe := func(change func(v []string)) error {
		v := good()
		change(v)
		return insert(v[0], v[1], v[2], v[3], v[4], v[5], v[6])
	}
	for name, change := range map[string]func(v []string){
		"duplicate_key":     func(v []string) { v[1] = domain.ProbeHostActionImageCache },
		"duplicate_attempt": func(v []string) { v[3] = attempt(domain.ProbeHostActionImageCache) },
		"duplicate_nonce":   func(v []string) { v[5] = hex.EncodeToString(bytes.Repeat([]byte{1}, 32)) },
		"missing_resource":  func(v []string) { v[0] = m8InstallOp3 },
		"bad_owner":         func(v []string) { v[2] = "invalid" },
		"zero_nonce":        func(v []string) { v[5] = strings.Repeat("0", 64) },
		"upper_authority":   func(v []string) { v[4] = strings.Repeat("A", 64) },
		"short_marker":      func(v []string) { v[6] = "bad" },
	} {
		if err := probe(change); err == nil {
			t.Fatalf("%s: schema accepted a row it must refuse", name)
		}
		e.rows(1)
	}
	if err := probe(func([]string) {}); err != nil {
		t.Fatalf("positive control refused: %v", err)
	}
	e.rows(2)
	// ON DELETE RESTRICT: a resource with a consumed admission cannot be erased.
	if _, err := e.f.db.ExecContext(e.ctx, "DELETE FROM probe_install_resources WHERE operation_id = ? AND kind = ?", op, domain.ProbeHostActionContainer); err == nil {
		t.Fatal("a resource with a consumed admission was deleted")
	}
	e.rows(2)
}

// A migration-086 admission survives the 089 upgrade verbatim, and rows keep
// refusing replay afterwards. While 089 is rolled back the Docker kinds are
// refused by the 086 CHECK.
func m8DockerUpgradePreserves(t *testing.T, engine string) {
	e := newM8DockerEnv(t, engine)
	store := m8InstallStore(e.f)
	m8InstallCreate(t, store, m8DockerIdentity(m8InstallOp1), m8InstallOwnerA)
	m8InstallCreate(t, store, m8InstallIdentity(domain.ProbeInstallSystemd, m8InstallOp2), m8InstallOwnerA)
	narrow := false
	t.Cleanup(func() {
		if narrow {
			if err := runEngineMigration(t, e.f.db, engine, m8DockerWidening, "up"); err != nil {
				t.Errorf("restore %s: %v", m8DockerWidening, err)
			}
		}
	})
	if err := runEngineMigration(t, e.f.db, engine, m8DockerWidening, "down"); err != nil {
		t.Fatalf("empty %s downgrade refused: %v", m8DockerWidening, err)
	}
	narrow = true
	if err := e.insertRaw(m8InstallOp1, domain.ProbeHostActionImageCache, m8DockerAttempt(m8InstallOp1, domain.ProbeHostActionImageCache), 1); !m8DockerCheckRefused(err) {
		t.Fatalf("the 086 schema must refuse image_cache by its kind CHECK: %v", err)
	}
	if err := e.insertRaw(m8InstallOp1, domain.ProbeHostActionContainer, m8DockerAttempt(m8InstallOp1, domain.ProbeHostActionContainer), 1); !m8DockerCheckRefused(err) {
		t.Fatalf("the 086 schema must refuse container by its kind CHECK: %v", err)
	}
	nonce := byte(10)
	for _, kind := range []string{domain.ProbeHostActionDataDirectory, domain.ProbeHostActionUnitFile, domain.ProbeHostActionService} {
		if err := e.insertRaw(m8InstallOp2, kind, m8HostAttempt(m8InstallOp2, kind), nonce); err != nil {
			t.Fatalf("the 086 schema refused its own kind %s: %v", kind, err)
		}
		nonce++
	}
	before := e.rawAdmissions()
	if len(before) != 3 {
		t.Fatalf("expected the three 086 admissions, got %d", len(before))
	}
	if err := runEngineMigration(t, e.f.db, engine, m8DockerWidening, "up"); err != nil {
		t.Fatalf("%s upgrade over stored admissions failed: %v", m8DockerWidening, err)
	}
	narrow = false
	after := e.rawAdmissions()
	if len(after) != len(before) {
		t.Fatalf("upgrade changed the admission count: %d -> %d", len(before), len(after))
	}
	for i := range before {
		b, a := before[i], after[i]
		if b.OperationID != a.OperationID || b.Kind != a.Kind || b.OwnerID != a.OwnerID || b.AttemptID != a.AttemptID ||
			b.AuthorityKeyID != a.AuthorityKeyID || b.NonceHex != a.NonceHex || b.MarkerSHA256 != a.MarkerSHA256 || !b.AdmittedAt.Equal(a.AdmittedAt) {
			t.Fatalf("upgrade rewrote admission %d: %+v -> %+v", i, b, a)
		}
	}
	// Consumption survives the upgrade, and the Docker kinds are now accepted.
	if err := e.insertRaw(m8InstallOp2, domain.ProbeHostActionDataDirectory, m8DockerForeignAttempt, 50); err == nil {
		t.Fatal("upgrade rearmed a consumed admission")
	}
	for i, kind := range []string{domain.ProbeHostActionImageCache, domain.ProbeHostActionContainer} {
		if err := e.insertRaw(m8InstallOp1, kind, m8DockerAttempt(m8InstallOp1, kind), byte(60+i)); err != nil {
			t.Fatalf("the upgraded schema refused docker kind %s: %v", kind, err)
		}
	}
	e.rows(5)
}

// An admission of a Docker resource may have authorized a real Docker action,
// so no downgrade may erase it or leave it under a CHECK that cannot hold it.
func m8DockerDowngradeGuard(t *testing.T, engine string) {
	e := newM8DockerEnv(t, engine)
	r := m8DockerRequest(e.signer, m8InstallOp1, domain.ProbeHostActionImageCache)
	e.prepare(r, m8DockerInFlight)
	legacy := m8HostRequest(e.signer, m8InstallOp2, m8InstallOwnerB, domain.ProbeHostActionDataDirectory)
	legacy.Challenge.Nonce[1]++
	m8HostPrepare(t, e.f, legacy, m8HostInFlight)
	narrow := false
	t.Cleanup(func() {
		_, _ = e.f.db.ExecContext(context.Background(), "DROP TABLE IF EXISTS probe_docker_host_actions_downgrade_guard")
		if narrow {
			if err := runEngineMigration(t, e.f.db, engine, m8DockerWidening, "up"); err != nil {
				t.Errorf("restore %s: %v", m8DockerWidening, err)
			}
		}
	})
	for _, request := range []domain.ProbeHostActionAdmissionRequest{r, legacy} {
		g, err := e.svc.Authorize(e.ctx, request)
		e.granted(g, err, request)
	}
	before := e.rawAdmissions()
	if len(before) != 2 {
		t.Fatalf("expected two admissions, got %d", len(before))
	}
	if err := runEngineMigration(t, e.f.db, engine, m8DockerWidening, "down"); err == nil {
		t.Fatal("downgrade erased or narrowed under an irreversible Docker admission")
	}
	// Refused means untouched: rows intact, the Docker kinds still accepted, and
	// the consumed attempt still refuses replay.
	after := e.rawAdmissions()
	if len(after) != 2 || after[0].MarkerSHA256 != before[0].MarkerSHA256 || after[1].MarkerSHA256 != before[1].MarkerSHA256 {
		t.Fatal("a refused downgrade changed stored admissions")
	}
	if err := e.insertRaw(m8InstallOp1, domain.ProbeHostActionContainer, m8DockerAttempt(m8InstallOp1, domain.ProbeHostActionContainer), 70); err != nil {
		t.Fatalf("a refused downgrade narrowed the schema: %v", err)
	}
	if _, err := e.f.db.ExecContext(e.ctx, "DELETE FROM probe_host_action_admissions WHERE kind = ?", domain.ProbeHostActionContainer); err != nil {
		t.Fatalf("remove probe row: %v", err)
	}
	g, err := e.svc.Authorize(e.ctx, r)
	e.refused(g, err, ports.ErrConflict)
	// Only once no Docker admission remains (a test-only removal: no product path
	// ever deletes one) does the downgrade restore the 086 kind list, and it keeps
	// the legacy admission.
	if _, err := e.f.db.ExecContext(e.ctx, "DELETE FROM probe_host_action_admissions WHERE kind = ?", domain.ProbeHostActionImageCache); err != nil {
		t.Fatalf("remove docker admission: %v", err)
	}
	if err := runEngineMigration(t, e.f.db, engine, m8DockerWidening, "down"); err != nil {
		t.Fatalf("downgrade with no Docker admission refused: %v", err)
	}
	narrow = true
	e.rows(1)
	if err := e.insertRaw(m8InstallOp1, domain.ProbeHostActionImageCache, m8DockerAttempt(m8InstallOp1, domain.ProbeHostActionImageCache), 71); !m8DockerCheckRefused(err) {
		t.Fatalf("the restored 086 schema must refuse image_cache: %v", err)
	}
	if rows := e.rawAdmissions(); len(rows) != 1 || rows[0].Kind != domain.ProbeHostActionDataDirectory || rows[0].MarkerSHA256 != before[1].MarkerSHA256 {
		t.Fatalf("downgrade lost the legacy admission: %+v", rows)
	}
	if err := runEngineMigration(t, e.f.db, engine, m8DockerWidening, "up"); err != nil {
		t.Fatalf("re-upgrade failed: %v", err)
	}
	narrow = false
	if err := e.insertRaw(m8InstallOp1, domain.ProbeHostActionImageCache, m8DockerAttempt(m8InstallOp1, domain.ProbeHostActionImageCache), 72); err != nil {
		t.Fatalf("re-upgraded schema refused image_cache: %v", err)
	}
}
