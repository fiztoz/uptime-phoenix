package probe

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestStreamResetRuntimeKeepsPinAndUsesVerifiedEpochAfterRestart(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		name := "bootstrap"
		if rotate {
			name = "rotated"
		}
		t.Run(name, func(t *testing.T) {
			f := newCertificateRuntimeFixture(t)
			manifest, err := os.ReadFile(filepath.Join(f.dir, "identity.json"))
			if err != nil {
				t.Fatal(err)
			}
			bootstrapPEM, err := os.ReadFile(filepath.Join(f.dir, "tls.pem"))
			if err != nil {
				t.Fatal(err)
			}
			first := f.dial(f.oldToken, 1)
			pin, certificateVersion := f.id.Fingerprint, int64(1)
			if rotate {
				command, payload := f.prepareCertificate(2 * time.Second)
				f.send(first, 1, payload)
				prepared := f.result(first)
				details, ok := prepared.Details.(CertificatePrepareDetails)
				if !ok {
					t.Fatal("missing pin")
				}
				pin, certificateVersion = details.TLSFingerprint, 2
				second := f.dial(f.oldToken, 2)
				f.send(second, 2, f.activationPayload(command, pin))
				if out := f.result(second); out.Status != "applied" {
					t.Fatal("activation failed", out)
				}
				timer := time.NewTimer(time.Until(command.CreatedAt.Add(domain.ProbeCredentialOverlap).Add(50 * time.Millisecond)))
				defer timer.Stop()
				select {
				case <-timer.C:
				case <-t.Context().Done():
					t.Fatal(t.Context().Err())
				}
			}
			binding, err := f.store.ReadEnrollment(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			f.stop()
			anchor := domain.EdgeIdentity{ProbeID: f.id.ProbeID, StreamID: f.id.StreamID, Fingerprint: f.id.Fingerprint}
			material, err := NewEdgeCertificateMaterial(f.protector)
			if err != nil {
				t.Fatal(err)
			}
			recovery, err := edge.OpenForStreamReset(t.Context(), f.dir, anchor, edge.WithStreamResetProtection(f.protector), edge.WithCertificateMaterial(material))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewEdgeTLSManager(t.Context(), f.id, recovery, f.protector); err != nil {
				_ = recovery.Close()
				t.Fatal("recovery handle cannot authenticate retained TLS before reset", err)
			}
			plan := domain.ProbeStreamResetPlan{ResetID: uuid.NewString(), HubID: f.hubID, ProbeID: f.id.ProbeID, EnrollmentID: binding.EnrollmentID, PreviousStreamID: f.id.StreamID, StreamID: uuid.NewString(), Fingerprint: pin, CredentialVersion: 1, CertificateVersion: certificateVersion, ConnectionGeneration: 10, PreparedAt: time.Now().UTC().Truncate(time.Microsecond)}
			if _, err := recovery.ResetStream(t.Context(), plan); err != nil {
				_ = recovery.Close()
				t.Fatal(err)
			}
			if err := recovery.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.id.Close(); err != nil {
				t.Fatal(err)
			}
			f.id, err = OpenRuntimeIdentityAnchor(t.Context(), f.dir)
			if err != nil {
				t.Fatal(err)
			}
			f.openStore()
			f.start()
			currentPin, _, err := f.manager.CurrentIdentity(t.Context())
			if err != nil || currentPin != pin {
				t.Fatal("pin changed after reset", err)
			}
			for _, stream := range []string{plan.PreviousStreamID, plan.StreamID} {
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				conn, response, err := websocket.Dial(ctx, f.endpoint, &websocket.DialOptions{HTTPClient: f.client, HTTPHeader: http.Header{"Authorization": {"Bearer " + f.oldToken}}, Subprotocols: []string{"phoenix.probe.v1"}})
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				if err != nil {
					t.Fatal("same credential/pin rejected", err)
				}
				defer func() { _ = conn.CloseNow() }()
				_, data, err := conn.Read(ctx)
				if err != nil {
					t.Fatal(err)
				}
				_, hello, err := DecodeHello(data)
				if err != nil || hello.StreamID != plan.StreamID {
					t.Fatal("bootstrap epoch leaked into hello", err)
				}
				welcome := Welcome{SessionIdentity: SessionIdentity{HubID: f.hubID, ProbeID: f.id.ProbeID, StreamID: stream}, SelectedProtocol: 1, ConnectionGeneration: 11, HeartbeatSeconds: HeartbeatSeconds, MaxFrameBytes: MaxFrameBytes, MaxBatchEvents: MaxBatchEvents, MaxBatchBytes: MaxBatchBytes, HubTime: Timestamp(time.Now().UTC())}
				frame, err := encodeFrame("welcome", 11, welcome)
				if err != nil {
					t.Fatal(err)
				}
				if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
					t.Fatal(err)
				}
				_, data, err = conn.Read(ctx)
				if stream == plan.PreviousStreamID {
					if err == nil {
						t.Fatal("retired epoch admitted")
					}
					continue
				}
				if err != nil {
					t.Fatal("new epoch admission", err)
				}
				if _, _, err := DecodeHealth(data); err != nil {
					t.Fatal("new epoch did not send health", err)
				}
			}
			afterManifest, err := os.ReadFile(filepath.Join(f.dir, "identity.json"))
			if err != nil {
				t.Fatal(err)
			}
			afterPEM, err := os.ReadFile(filepath.Join(f.dir, "tls.pem"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(manifest, afterManifest) || !bytes.Equal(bootstrapPEM, afterPEM) {
				t.Fatal("reset changed immutable bootstrap files")
			}
		})
	}
}
