package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestCertificateCommandCodecBindsOriginalWireBytes(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Microsecond)
	c := domain.ProbeCertificateCommand{CommandID: uuid.NewString(), ProbeID: uuid.NewString(), Kind: CommandCertificatePrepare, CreatedAt: now, ExpiresAt: now.Add(time.Hour), RotationID: uuid.NewString(), CertificateVersion: 2, ValidForDays: 365}
	codec := CertificateCommandCodec{}
	compact, err := codec.EncodeCertificateCommand(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	spaced := bytes.Replace(compact, []byte(`,"kind"`), []byte(",\n \"kind\""), 1)
	if bytes.Equal(compact, spaced) {
		t.Fatal("fixture did not change wire bytes")
	}
	decoded, err := codec.DecodeCertificateCommand(t.Context(), spaced)
	if err != nil || decoded.PayloadHash != sha256.Sum256(spaced) || decoded.PayloadHash == sha256.Sum256(compact) {
		t.Fatal("exact command digest lost", err)
	}
	decoded.PayloadHash = [32]byte{}
	if decoded != c {
		t.Fatal("decoded command changed its scope")
	}
	for name, payload := range map[string][]byte{
		"duplicate":     append([]byte(`{"kind":"certificate.prepare",`), compact[1:]...),
		"leading_space": append([]byte(" "), compact...),
		"oversized":     bytes.Repeat([]byte("x"), domain.MaxProbeCommandBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := codec.DecodeCertificateCommand(t.Context(), payload); err == nil {
				t.Fatal("invalid command accepted")
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := codec.DecodeCertificateCommand(ctx, compact); err == nil {
		t.Fatal("canceled decode accepted")
	}
	c.Kind, c.ValidForDays, c.ExpectedFingerprint = CommandCertificateActivate, 0, string(bytes.Repeat([]byte("a"), 64))
	activation, err := codec.EncodeCertificateCommand(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := codec.DecodeCertificateCommand(t.Context(), activation)
	if err != nil {
		t.Fatal(err)
	}
	got.PayloadHash = [32]byte{}
	if got != c {
		t.Fatal("activation lost expected pin")
	}
}

func TestCommandCodecsEnforceJSONStructureAndOptionalExtensions(t *testing.T) {
	for _, fixture := range []string{"command-certificate-prepare.json", "command-credential-prepare.json", "command-alert-ack.json"} {
		t.Run(fixture, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "v1", "valid", fixture))
			if err != nil {
				t.Fatal(err)
			}
			var envelope struct {
				Payload json.RawMessage `json:"payload"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				t.Fatal(err)
			}
			decode := func(payload []byte) error {
				switch fixture {
				case "command-certificate-prepare.json":
					_, err := (CertificateCommandCodec{}).DecodeCertificateCommand(t.Context(), payload)
					return err
				case "command-credential-prepare.json":
					_, err := (CredentialCommandCodec{}).DecodeCredentialCommand(t.Context(), payload)
					return err
				default:
					_, err := (AcknowledgementCodec{}).DecodeAcknowledgement(t.Context(), payload)
					return err
				}
			}
			if err := decode(envelope.Payload); err != nil {
				t.Fatal("fixture invalid", err)
			}
			extended := append([]byte(`{"future_optional":true,`), envelope.Payload[1:]...)
			if err := decode(extended); err != nil {
				t.Fatal("optional extension rejected", err)
			}
			duplicate := append([]byte(`{"command_id":"ignored",`), envelope.Payload[1:]...)
			if err := decode(duplicate); err == nil {
				t.Fatal("duplicate field accepted outside envelope decoder")
			}
			invalidUTF8 := append([]byte{'{', '"', 0xff, '"', ':', '0', ','}, envelope.Payload[1:]...)
			if err := decode(invalidUTF8); err == nil {
				t.Fatal("invalid UTF-8 accepted")
			}
		})
	}
}
