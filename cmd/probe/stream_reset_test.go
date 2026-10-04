package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

type resetLostOutput struct{}

func (resetLostOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestProbeResetCLIRecoversPendingArchiveAndLostOutput(t *testing.T) {
	t.Setenv("PROBE_SECRET_KEY_FILE", "")
	dir := filepath.Join(t.TempDir(), "edge")
	var output, stderr bytes.Buffer
	if code := run(t.Context(), []string{"init", "--data-dir", dir}, &output, &stderr); code != 0 {
		t.Fatal(stderr.String())
	}
	var initialized struct {
		Token string `json:"enrollment_token"`
	}
	if err := json.Unmarshal(output.Bytes(), &initialized); err != nil {
		t.Fatal(err)
	}
	id, err := probe.OpenRuntimeIdentityAnchor(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	key, err := auth.NewProbeConfigProtectorFromFile(t.Context(), filepath.Join(dir, "config.key"))
	if err != nil {
		t.Fatal(err)
	}
	material, err := probe.NewEdgeCertificateMaterial(key)
	if err != nil {
		t.Fatal(err)
	}
	store, err := edge.Open(t.Context(), dir, domain.EdgeIdentity{ProbeID: id.ProbeID, StreamID: id.StreamID, Fingerprint: id.Fingerprint}, edge.WithStreamResetProtection(key), edge.WithCertificateMaterial(material))
	if err != nil {
		t.Fatal(err)
	}
	bind := domain.EdgeEnrollment{HubID: uuid.NewString(), ProbeID: id.ProbeID, EnrollmentID: uuid.NewString(), CredentialVersion: 1, TokenHash: sha256.Sum256([]byte("retained test token")), AppliedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if err := store.CommitEnrollment(t.Context(), sha256.Sum256([]byte(initialized.Token)), bind); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	plan := domain.ProbeStreamResetPlan{ResetID: uuid.NewString(), HubID: bind.HubID, ProbeID: id.ProbeID, EnrollmentID: bind.EnrollmentID, PreviousStreamID: id.StreamID, StreamID: uuid.NewString(), Fingerprint: id.Fingerprint, CredentialVersion: 1, CertificateVersion: 1, ConnectionGeneration: 2, PreparedAt: time.Now().UTC().Truncate(time.Microsecond)}
	codec := probe.StreamResetCodec{}
	data, err := codec.EncodeStreamResetPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	planFile := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(planFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"reset-stream", "--data-dir", dir, "--plan-file", planFile}
	output.Reset()
	stderr.Reset()
	if code := run(t.Context(), args, &output, &stderr); code == 0 || output.Len() != 0 {
		t.Fatal("reset bypassed running owner OS lock")
	}
	if err := id.Close(); err != nil {
		t.Fatal(err)
	}
	anchors := map[string][32]byte{}
	for _, name := range []string{"identity.json", "tls.pem", "config.key"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		anchors[name] = sha256.Sum256(b)
	}
	// Reserve first, then fail archive publication. Retry must authenticate TLS
	// through the recovery read-only path while ordinary startup remains fenced.
	archiveRoot := filepath.Join(dir, "stream-archives")
	if err := os.Mkdir(archiveRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	blocker := filepath.Join(archiveRoot, plan.ResetID)
	if err := os.WriteFile(blocker, []byte("foreign evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	stderr.Reset()
	if code := run(t.Context(), args, &output, &stderr); code == 0 || output.Len() != 0 {
		t.Fatal("unsafe archive reported success")
	}
	if code := run(t.Context(), []string{"inspect", "--data-dir", dir}, io.Discard, io.Discard); code == 0 {
		t.Fatal("pending reset allowed ordinary startup")
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	if code := run(t.Context(), args, resetLostOutput{}, io.Discard); code == 0 {
		t.Fatal("failed output reported success")
	}
	output.Reset()
	stderr.Reset()
	if code := run(t.Context(), args, &output, &stderr); code != 0 {
		t.Fatal("lost-output retry failed", stderr.String())
	}
	receipt, err := codec.DecodeStreamResetReceipt(t.Context(), output.Bytes())
	if err != nil || receipt.Plan != plan || receipt.State != "applied" {
		t.Fatal("invalid source receipt", err)
	}
	first := bytes.Clone(output.Bytes())
	output.Reset()
	if code := run(t.Context(), args, &output, &stderr); code != 0 || !bytes.Equal(output.Bytes(), first) {
		t.Fatal("exact retry changed committed receipt", stderr.String())
	}
	archive, err := os.ReadFile(filepath.Join(archiveRoot, plan.ResetID, "edge.db"))
	if err != nil || int64(len(archive)) != receipt.ArchiveBytes {
		t.Fatal("missing preserved archive", err)
	}
	for name, want := range anchors {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || sha256.Sum256(b) != want {
			t.Fatal("bootstrap identity/key changed", name, err)
		}
	}
	output.Reset()
	if code := run(t.Context(), []string{"inspect", "--data-dir", dir}, &output, &stderr); code != 0 {
		t.Fatal("ordinary restart failed", stderr.String())
	}
	var inspection struct {
		StreamID string `json:"stream_id"`
	}
	if err := json.Unmarshal(output.Bytes(), &inspection); err != nil || inspection.StreamID != plan.StreamID {
		t.Fatal("inspection used bootstrap stream", err)
	}
}
