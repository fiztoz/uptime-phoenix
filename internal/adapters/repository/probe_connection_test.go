package repository_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestProbeConnectionPreparationContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			f := newProbeRegistryFixture(t, engine)
			f.remote(t, probeRegistryID1, "enrollment")
			protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{9}, 32))
			if err != nil {
				t.Fatal(err)
			}
			inst, err := services.NewProbeInstallationService(installationRepo(f, f.db)).InitializeOrVerify(t.Context(), protector, "")
			if err != nil {
				t.Fatal(err)
			}
			s := repository.NewProbeConnectorStore(f.db)
			m := domain.ProbeCredentialMetadata{HubID: inst.HubID, ProbeID: probeRegistryID1, StreamID: probeRegistryID2, EnrollmentID: probeRegistryID3, CredentialVersion: 1, Endpoint: "wss://edge.example/ws/probe/v1", Fingerprint: strings.Repeat("a", 64)}
			token := "phx_probe_" + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))
			cipher, err := protector.SealCredential(t.Context(), m, token)
			if err != nil {
				t.Fatal(err)
			}
			c := domain.ProbeConnection{ProbeCredentialMetadata: m, ProtectedCredential: cipher, State: "prepared", PreparedAt: time.Now().In(time.FixedZone("local", 7*3600))}
			trigger := "CREATE TRIGGER fail_enrollment_stream BEFORE INSERT ON probe_streams BEGIN SELECT RAISE(ABORT, 'stream insertion fault'); END"
			if engine == "mariadb" {
				trigger = "CREATE TRIGGER fail_enrollment_stream BEFORE INSERT ON probe_streams FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'stream insertion fault'"
			}
			if _, err := f.db.ExecContext(t.Context(), trigger); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := f.db.ExecContext(cleanup, "DROP TRIGGER IF EXISTS fail_enrollment_stream"); err != nil {
					t.Errorf("remove enrollment fault injection: %v", err)
				}
			})
			if _, err := s.PrepareConnection(t.Context(), c); err == nil {
				t.Fatal("failed stream binding reported success")
			}
			if _, err := s.GetConnection(t.Context(), m.ProbeID); !errors.Is(err, ports.ErrNotFound) {
				t.Fatalf("expected absent credential after failed preparation, got %v", err)
			}
			if _, err := f.db.ExecContext(t.Context(), "DROP TRIGGER fail_enrollment_stream"); err != nil {
				t.Fatal(err)
			}
			prepared, err := s.PrepareConnection(t.Context(), c)
			if err != nil {
				t.Fatal(err)
			}
			if prepared.PreparedAt.Location() != time.UTC {
				t.Fatal("non UTC preparation")
			}
			if cursor, err := s.GetConnectionCursor(t.Context(), m.ProbeID, m.StreamID); err != nil || cursor != 0 {
				t.Fatal("prepared stream has no durable cursor")
			}
			if _, err := s.GetConnectionCursor(t.Context(), m.ProbeID, m.EnrollmentID); !errors.Is(err, ports.ErrNotFound) {
				t.Fatal("unknown stream invented a cursor")
			}
			if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_streams SET retired_at = ? WHERE probe_id = ?", time.Now().UTC(), m.ProbeID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.GetConnectionCursor(t.Context(), m.ProbeID, m.StreamID); !errors.Is(err, ports.ErrNotFound) {
				t.Fatal("retired stream accepted")
			}
			if _, err := f.db.ExecContext(t.Context(), "UPDATE probe_streams SET retired_at = NULL WHERE probe_id = ?", m.ProbeID); err != nil {
				t.Fatal(err)
			}
			otherCipher, err := protector.SealCredential(t.Context(), m, token)
			if err != nil {
				t.Fatal(err)
			}
			c.ProtectedCredential = otherCipher
			retry, err := s.PrepareConnection(t.Context(), c)
			if err != nil || !bytes.Equal(retry.ProtectedCredential, cipher) {
				t.Fatal("retry replaced recoverable credential")
			}
			changed := c
			changed.Fingerprint = strings.Repeat("b", 64)
			if _, err := s.PrepareConnection(t.Context(), changed); !errors.Is(err, ports.ErrConflict) {
				t.Fatal("preparation silently rotated identity")
			}
			read, err := repository.NewProbeConnectorStore(f.db).GetConnection(t.Context(), m.ProbeID)
			if err != nil {
				t.Fatal(err)
			}
			if plaintext, err := protector.OpenCredential(t.Context(), read.ProbeCredentialMetadata, read.ProtectedCredential); err != nil || plaintext != token {
				t.Fatal("lost response cannot recover original token")
			}
			if err := s.ActivateConnection(t.Context(), m.ProbeID, m.EnrollmentID, 2, time.Now().UTC()); !errors.Is(err, ports.ErrConflict) {
				t.Fatal("wrong credential activated")
			}
			if err := s.ActivateConnection(t.Context(), m.ProbeID, m.EnrollmentID, 1, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			connections, err := s.ListConnections(t.Context())
			if err != nil || len(connections) != 1 || connections[0].State != "active" {
				t.Fatal("active connection absent")
			}
			if err := runEngineMigration(t, f.db, engine, "053_probe_connections", "down"); err == nil {
				t.Fatal("downgrade discarded recoverable credential")
			}
		})
	}
}
