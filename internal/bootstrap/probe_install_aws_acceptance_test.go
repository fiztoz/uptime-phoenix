//go:build m8aws

package bootstrap

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/provisioning"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// awsEnv reads one required acceptance input. The m8aws tag keeps this
// operator-run test out of every default suite; it contacts a real host.
func awsEnv(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is required for the m8aws acceptance run", name)
	}
	return v
}

func awsUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

// awsForgedHostActions signs a structurally valid host-action grant with a key
// the host does not trust, without any DB admission.
type awsForgedHostActions struct{ key ed25519.PrivateKey }

func (a awsForgedHostActions) Authorize(_ context.Context, request domain.ProbeHostActionAdmissionRequest) (domain.ProbeHostActionGrant, error) {
	grant := domain.ProbeHostActionGrant{Request: request}
	copy(grant.Signature[:], ed25519.Sign(a.key, request.SigningMessage()))
	return grant, nil
}

// TestM8AWSInstallLifecycle drives the production install runner, journal,
// admission stores, authorization services, signer, executor and SSH sender
// against one real host's sshd, authorized-dispatch and helpers. Host-side
// effects are verified separately over the operator session.
func TestM8AWSInstallLifecycle(t *testing.T) {
	addr := awsEnv(t, "M8AWS_ADDR")
	hostFP := awsEnv(t, "M8AWS_HOST_FP")
	clientPEM, err := os.ReadFile(awsEnv(t, "M8AWS_CLIENT_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	clientFP := awsEnv(t, "M8AWS_CLIENT_FP")
	arch := awsEnv(t, "M8AWS_ARCH")
	version := awsEnv(t, "M8AWS_PROBE_VERSION")
	data, err := os.ReadFile(awsEnv(t, "M8AWS_PROBE_BINARY"))
	if err != nil {
		t.Fatal(err)
	}
	resultPath := awsEnv(t, "M8AWS_RESULT")
	signer, err := LoadProbeStageGrantSigner(t.Context(), awsEnv(t, "M8AWS_AUTHORITY_KEY"))
	if err != nil {
		t.Fatal(err)
	}
	ip := addr[:strings.LastIndex(addr, ":")]
	policy := probe.EndpointPolicy{AllowedCIDRs: []netip.Prefix{netip.PrefixFrom(netip.MustParseAddr(ip), 32)}}

	sum := sha256.Sum256(data)
	identity := domain.ProbeInstallIdentity{
		OperationID:          awsUUID(t),
		Host:                 domain.ProbeSSHHost{Address: addr, User: "root"},
		HostKeyFingerprint:   hostFP,
		ClientKeyFingerprint: clientFP,
		RequestedBy:          1,
		LayoutVersion:        1,
		Mode:                 domain.ProbeInstallSystemd,
		Binary:               domain.ProbeBinaryPin{Version: version, Architecture: arch, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(data))},
	}
	if !identity.Valid() {
		t.Fatal("acceptance identity invalid")
	}
	owner := awsUUID(t)

	dsn := "file:" + filepath.Join(t.TempDir(), "aws.db") + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)"
	db := lifecycleOpenDB(t, dsn, true)
	journal := lifecycleJournal(t, db)
	stage, err := services.NewProbeBinaryStageAuthorizationService(repository.NewProbeBinaryStageAdmissionStore(db), signer)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := services.NewProbeHostActionAuthorizationService(repository.NewProbeHostActionAdmissionStore(db), signer)
	if err != nil {
		t.Fatal(err)
	}
	sender, err := provisioning.NewSSHBinaryStageSender(t.Context(), clientPEM, hostFP, clientFP, policy)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := provisioning.NewProbeBinaryPreparer(t.TempDir()).PrepareBinary(t.Context(), identity.Binary, io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prepared.Close(context.Background()) })
	executor, err := provisioning.NewSSHProbeInstallExecutor(sender, prepared, stage, actions)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := services.NewProbeInstallRunner(journal, executor)
	if err != nil {
		t.Fatal(err)
	}
	if _, fresh, err := journal.Create(t.Context(), identity, owner); err != nil || !fresh {
		t.Fatalf("create install intent: fresh=%v err=%v", fresh, err)
	}
	attempts := []string{awsUUID(t), awsUUID(t), awsUUID(t), awsUUID(t)}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	op, err := runner.Run(ctx, identity, owner, attempts)
	if err != nil || op == nil {
		t.Fatalf("runner: %v", err)
	}
	if op.State != domain.ProbeInstallResourcesReady {
		t.Fatalf("operation state %q code %q, want resources_ready", op.State, op.ErrorCode)
	}
	for i, r := range op.Resources {
		if r.State != domain.ProbeInstallResourceApplied || r.AttemptID != attempts[i] {
			t.Fatalf("resource %s state %q attempt %q", r.Spec.Kind, r.State, r.AttemptID)
		}
	}

	// Replays of consumed attempts are refused by the DB before any stdin.
	stageIntent := domain.ProbeBinaryStageIntent{Identity: identity, OwnerID: owner, AttemptID: attempts[1]}
	if _, err := sender.SendAuthorized(ctx, stageIntent, prepared, stage); !errors.Is(err, provisioning.ErrSSHBinaryStageNotStaged) {
		t.Fatalf("artifact replay: %v", err)
	}
	for i, kind := range []string{domain.ProbeHostActionDataDirectory, "", domain.ProbeHostActionUnitFile, domain.ProbeHostActionService} {
		if kind == "" {
			continue
		}
		intent := domain.ProbeHostActionIntent{Identity: identity, OwnerID: owner, AttemptID: attempts[i], Kind: kind}
		if _, err := sender.SendHostAction(ctx, intent, actions); !errors.Is(err, provisioning.ErrSSHBinaryStageNotStaged) {
			t.Fatalf("%s replay: %v", kind, err)
		}
	}

	// A grant from an untrusted authority for a fresh operation is refused by
	// the real helper after the frames; the host must hold no such directory.
	_, forgedKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	forged := identity
	forged.OperationID = awsUUID(t)
	forgedIntent := domain.ProbeHostActionIntent{Identity: forged, OwnerID: owner, AttemptID: awsUUID(t), Kind: domain.ProbeHostActionDataDirectory}
	if _, err := sender.SendHostAction(ctx, forgedIntent, awsForgedHostActions{key: forgedKey}); !errors.Is(err, provisioning.ErrSSHBinaryStageUnknown) {
		t.Fatalf("forged grant: %v", err)
	}

	specs := identity.Resources()
	unit := domain.ProbeSystemdUnit(identity)
	unitSum := sha256.Sum256([]byte(unit))
	result := map[string]any{
		"operation_id":        identity.OperationID,
		"forged_operation_id": forged.OperationID,
		"data_directory":      specs[0].Reference,
		"artifact_directory":  specs[1].Reference,
		"unit_file":           specs[2].Reference,
		"service":             specs[3].Reference,
		"release_name":        identity.Binary.ReleaseName(),
		"binary_sha256":       identity.Binary.SHA256,
		"marker_sha256":       stageIntent.MarkerSHA256(),
		"unit_sha256":         hex.EncodeToString(unitSum[:]),
		"forged_data_dir":     forgedIntent.Identity.Resources()[0].Reference,
	}
	body, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(resultPath, append(body, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	fmt.Println("m8aws lifecycle applied", identity.OperationID)
}
