package probe

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestEnrollRequestPreservesIdentityAndMaxVersion(t *testing.T) {
	_, request, err := DecodeEnrollRequest(readFixture(t, "valid", "enroll-request-max-version.json"))
	if err != nil {
		t.Fatal(err)
	}
	if request.EnrollmentID != "a1123604-32c5-40aa-89f9-b62f93dceac2" || request.CredentialVersion != math.MaxInt64 {
		t.Fatal("enrollment identity or max version changed")
	}
	if !strings.HasPrefix(request.Token, RuntimeTokenPrefix) || strings.HasPrefix(request.Token, EnrollmentTokenPrefix) {
		t.Fatal("runtime token prefix changed")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "future_optional") || strings.Contains(string(encoded), EnrollmentTokenPrefix) {
		t.Fatalf("enrollment request leaked optional or enrollment token: %s", encoded)
	}
}

func TestEnrollResultRejectsSecretsAndUnknownStatus(t *testing.T) {
	_, result, err := DecodeEnrollResult(readFixture(t, "valid", "enroll-result-applied.json"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "applied" || result.TLSFingerprint == nil || result.AppliedAt == nil {
		t.Fatal("applied enrollment lost pin or timestamp")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "phx_probe_") || strings.Contains(string(encoded), "private_key") {
		t.Fatalf("enroll result leaked a secret: %s", encoded)
	}
	for _, name := range []string{"enroll-result-token.json", "enroll-result-unknown-status.json"} {
		_, got, err := DecodeEnrollResult(readFixture(t, "invalid", name))
		if err == nil || !reflect.DeepEqual(got, EnrollResult{}) {
			t.Fatalf("%s yielded a usable enrollment: %v", name, err)
		}
	}
}

func TestEnrollRequestRejectsLocalAndEnrollmentTokens(t *testing.T) {
	for _, name := range []string{"enroll-request-local.json", "enroll-request-enroll-token.json", "enroll-request-nil-id.json"} {
		_, request, err := DecodeEnrollRequest(readFixture(t, "invalid", name))
		if err == nil || !reflect.DeepEqual(request, EnrollRequest{}) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}

func TestHTTPEnrollmentRotationAndReceipts(t *testing.T) {
	token, err := DecodeEnrollmentTokenRequest(readFixture(t, "valid", "http-enroll-request.json"))
	if err != nil || !strings.HasPrefix(token.EnrollmentToken, EnrollmentTokenPrefix) {
		t.Fatalf("enroll HTTP: %v", err)
	}
	rotate, err := DecodeRotateCredentialRequest(readFixture(t, "valid", "http-rotate-credential-max.json"))
	if err != nil || rotate.CredentialVersion != math.MaxInt64 {
		t.Fatalf("rotate HTTP: %v", err)
	}
	receipt, err := DecodeOperationReceipt(readFixture(t, "valid", "http-operation-receipt.json"))
	if err != nil || receipt.Error != nil || receipt.Status != "pending" {
		t.Fatalf("operation receipt: %v", err)
	}
	failed, err := DecodeOperationReceipt(readFixture(t, "valid", "http-operation-receipt-failed.json"))
	if err != nil || failed.Error == nil || failed.Error.Code != "enrollment_expired" {
		t.Fatalf("failed receipt: %v", err)
	}
	command, err := DecodeCommandReceipt(readFixture(t, "valid", "http-command-receipt-pending.json"))
	if err != nil || command.RemoteConfirmed || command.Status != "pending" {
		t.Fatalf("command receipt: %v", err)
	}
	revoke, err := DecodeRevokeReceipt(readFixture(t, "valid", "http-revoke-receipt.json"))
	if err != nil || revoke.RemoteConfirmed || revoke.Status != "succeeded" {
		t.Fatalf("revoke receipt: %v", err)
	}
	for _, encoded := range [][]byte{
		mustJSON(t, receipt), mustJSON(t, failed), mustJSON(t, command), mustJSON(t, revoke),
	} {
		if strings.Contains(string(encoded), "phx_probe_") || strings.Contains(string(encoded), "private_key") {
			t.Fatalf("receipt leaked a secret: %s", encoded)
		}
	}
	if _, err := DecodeEnrollmentTokenRequest(readFixture(t, "invalid", "http-enroll-runtime-token.json")); err == nil {
		t.Fatal("runtime token accepted as enrollment token")
	}
	if got, err := DecodeOperationReceipt(readFixture(t, "invalid", "http-operation-local-probe.json")); err == nil || !reflect.DeepEqual(got, OperationReceipt{}) {
		t.Fatal("local probe receipt accepted")
	}
	if got, err := DecodeCommandReceipt(readFixture(t, "invalid", "http-command-unknown-status.json")); err == nil || !reflect.DeepEqual(got, CommandReceipt{}) {
		t.Fatal("command receipt accepted result-only status")
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
