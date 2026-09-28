package services

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// testTLSPin returns a fabricated pin placeholder for unit tests.
func testTLSPin() string {
	return fmt.Sprintf("%064d", 1)
}

// testEnrollmentToken returns a fabricated operator authorization placeholder
// whose shape satisfies the write-only request contract.
func testEnrollmentToken() string {
	return fmt.Sprintf("%s%043d", enrollmentTokenPrefix, 7)
}

func TestRuntimeEndpoint(t *testing.T) {
	for _, tc := range []struct{ name, declared, want string }{
		{"host port", "probe.example.test:443", "wss://probe.example.test:443/ws/probe/v1"},
		{"host only", "probe.example.test", "wss://probe.example.test/ws/probe/v1"},
		{"canonical runtime url", "wss://edge.example/ws/probe/v1", "wss://edge.example/ws/probe/v1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := RuntimeEndpoint(tc.declared)
			if err != nil || got != tc.want {
				t.Fatalf("RuntimeEndpoint(%q) = %q %v, want %q", tc.declared, got, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"http://example.test/ws/probe/v1", "wss://example.test/other", "probe.example.test:443/path", "bad host:443"} {
		if _, err := RuntimeEndpoint(bad); !errors.Is(err, ErrInvalidRegistration) {
			t.Fatalf("accepted %q: %v", bad, err)
		}
	}
}

func TestValidEnrollmentToken(t *testing.T) {
	wrongPrefix := fmt.Sprintf("phx_probe_%043d", 7)
	for _, tc := range []struct {
		name  string
		value string
		want  bool
	}{
		{"valid", testEnrollmentToken(), true},
		{"wrong prefix", wrongPrefix, false},
		{"short suffix", fmt.Sprintf("%s%042d", enrollmentTokenPrefix, 1), false},
		{"empty", "", false},
		{"overlong", fmt.Sprintf("%s%0300d", enrollmentTokenPrefix, 1), false},
		{"bad characters", fmt.Sprintf("%s%042d!", enrollmentTokenPrefix, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ValidEnrollmentToken(tc.value); got != tc.want {
				t.Fatalf("ValidEnrollmentToken(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestProbeAdminOperations(t *testing.T) {
	t.Run("EnrollPersistsBeforeAcknowledgement", func(t *testing.T) {
		svc, operations, _, connections, enroll, _, _ := adminTestService()
		if _, err := svc.CreateRegistration(t.Context(), adminTestRegistrationRequest()); err != nil {
			t.Fatal(err)
		}
		authorization := testEnrollmentToken()
		operation, err := svc.EnrollSource(t.Context(), adminTestProbe, authorization, "44444444-4444-4444-8444-444444444444")
		if err != nil {
			t.Fatal(err)
		}
		if operation.Status != domain.ProbeOperationSucceeded || operation.Phase != "exchanged" || operation.Error != nil {
			t.Fatalf("receipt: %+v", operation)
		}
		if stored, ok := operations.rows[operation.OperationID]; !ok || stored.Status != domain.ProbeOperationSucceeded {
			t.Fatalf("operation not durable: %+v", operations.rows)
		}
		// First enrollment prepared the connection from the frozen trust.
		if len(enroll.prepared) != 4 || enroll.prepared[1] != "44444444-4444-4444-8444-444444444444" || enroll.prepared[2] != "wss://probe.example.test:443/ws/probe/v1" || enroll.prepared[3] != testTLSPin() || connections.connection != nil {
			t.Fatalf("prepare: %v", enroll.prepared)
		}
		if enroll.presented != authorization {
			t.Fatal("operator authorization not delivered to the exchange")
		}
	})

	t.Run("InvalidRequestPersistsNothing", func(t *testing.T) {
		svc, operations, _, _, _, _, _ := adminTestService()
		if _, err := svc.CreateRegistration(t.Context(), adminTestRegistrationRequest()); err != nil {
			t.Fatal(err)
		}
		wrongPrefix := fmt.Sprintf("phx_probe_%043d", 7)
		for _, value := range []string{"", wrongPrefix, enrollmentTokenPrefix + "short"} {
			if _, err := svc.Enroll(t.Context(), adminTestProbe, value); !errors.Is(err, ErrInvalidOperation) {
				t.Fatalf("accepted a malformed request: %v", err)
			}
		}
		if len(operations.rows) != 0 {
			t.Fatalf("invalid requests persisted operations: %v", operations.rows)
		}
	})

	t.Run("FailedExchangeRedactsInput", func(t *testing.T) {
		svc, operations, _, _, enroll, _, _ := adminTestService()
		if _, err := svc.CreateRegistration(t.Context(), adminTestRegistrationRequest()); err != nil {
			t.Fatal(err)
		}
		authorization := testEnrollmentToken()
		enroll.err = errors.New("dial failed on the pinned endpoint")
		operation, err := svc.EnrollSource(t.Context(), adminTestProbe, authorization, "44444444-4444-4444-8444-444444444444")
		if err != nil {
			t.Fatal(err)
		}
		if operation.Status != domain.ProbeOperationFailed || operation.Error == nil || operation.Error.Code != "enrollment_failed" {
			t.Fatalf("failed receipt: %+v", operation)
		}
		if strings.Contains(operation.Error.Message, "phx_") || strings.Contains(operation.Error.Message, authorization) {
			t.Fatalf("request input leaked into the receipt: %+v", operation.Error)
		}
		if stored := operations.rows[operation.OperationID]; stored.Error == nil || stored.Error.Code != "enrollment_failed" {
			t.Fatalf("failure not durable: %+v", stored)
		}
	})

	t.Run("RotateCredentialUsesTheOperationIdentity", func(t *testing.T) {
		svc, _, _, _, _, rotations, _ := adminTestService()
		if _, err := svc.CreateRegistration(t.Context(), adminTestRegistrationRequest()); err != nil {
			t.Fatal(err)
		}
		operation, err := svc.RotateCredential(t.Context(), adminTestProbe, 2)
		if err != nil || operation.Status != domain.ProbeOperationSucceeded || operation.Phase != "issued" {
			t.Fatalf("rotation: %+v %v", operation, err)
		}
		if rotations.issue.RotationID != operation.OperationID || rotations.issue.CredentialVersion != 2 || rotations.issue.ProbeID != adminTestProbe {
			t.Fatalf("rotation issue: %+v", rotations.issue)
		}
		if _, err := svc.RotateCredential(t.Context(), adminTestProbe, 0); !errors.Is(err, ErrInvalidOperation) {
			t.Fatalf("zero version accepted: %v", err)
		}
		rotations.err = ports.ErrConflict
		operation, err = svc.RotateCredential(t.Context(), adminTestProbe, 2)
		if err != nil || operation.Status != domain.ProbeOperationFailed || operation.Error.Code != "rotation_conflict" {
			t.Fatalf("conflicting rotation: %+v %v", operation, err)
		}
	})

	t.Run("ResetStreamRetainsTheCallerIdentity", func(t *testing.T) {
		svc, operations, _, connections, _, _, resets := adminTestService()
		if _, err := svc.CreateRegistration(t.Context(), adminTestRegistrationRequest()); err != nil {
			t.Fatal(err)
		}
		connections.connection = &domain.ProbeConnection{ProbeCredentialMetadata: domain.ProbeCredentialMetadata{ProbeID: adminTestProbe, StreamID: "11111111-1111-4111-8111-111111111111"}}
		newStream := "1395c134-da65-4d86-9b11-c9ce20c61748"
		operation, err := svc.ResetStream(t.Context(), adminTestProbe, newStream, adminTestOperation)
		if err != nil || operation.OperationID != adminTestOperation || operation.Status != domain.ProbeOperationSucceeded || operation.Phase != "prepared" {
			t.Fatalf("reset: %+v %v", operation, err)
		}
		if resets.issue.ResetID != adminTestOperation || resets.issue.PreviousStreamID != "11111111-1111-4111-8111-111111111111" || resets.issue.StreamID != newStream {
			t.Fatalf("reset issue: %+v", resets.issue)
		}
		if _, ok := operations.rows[adminTestOperation]; !ok {
			t.Fatal("reset operation not durable under the caller identity")
		}
		// Same stream, malformed identities and absent connections reject.
		if _, err := svc.ResetStream(t.Context(), adminTestProbe, "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222"); !errors.Is(err, ErrInvalidOperation) {
			t.Fatalf("same-stream reset accepted: %v", err)
		}
		if _, err := svc.ResetStream(t.Context(), adminTestProbe, newStream, "not-a-uuid"); !errors.Is(err, ErrInvalidOperation) {
			t.Fatalf("malformed operation identity accepted: %v", err)
		}
		connections.connection = nil
		if _, err := svc.ResetStream(t.Context(), adminTestProbe, newStream, "33333333-3333-4333-8333-333333333333"); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("reset without a connection accepted: %v", err)
		}
	})

	t.Run("TerminalReceiptsAreImmutable", func(t *testing.T) {
		svc, operations, _, _, _, _, _ := adminTestService()
		if _, err := svc.CreateRegistration(t.Context(), adminTestRegistrationRequest()); err != nil {
			t.Fatal(err)
		}
		if err := operations.FinishOperation(t.Context(), adminTestOperation, domain.ProbeOperationSucceeded, "exchanged", nil, time.Now()); err == nil {
			t.Fatal("finished an operation that was never created")
		}
		if err := operations.CreateOperation(t.Context(), domain.ProbeOperation{OperationID: adminTestOperation, ProbeID: adminTestProbe, Kind: domain.ProbeOperationEnroll, Status: domain.ProbeOperationPending, Phase: "exchanging", CreatedAt: time.Now(), UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := operations.FinishOperation(t.Context(), adminTestOperation, domain.ProbeOperationSucceeded, "exchanged", nil, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := operations.FinishOperation(t.Context(), adminTestOperation, domain.ProbeOperationFailed, "exchanging", &domain.ProbeOperationError{Code: "late_failure", Message: "late"}, time.Now()); !errors.Is(err, ports.ErrConflict) {
			t.Fatalf("terminal receipt rewritten: %v", err)
		}
	})
}
