package probe

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestCommandKindsHaveValidAndInvalidFixtures(t *testing.T) {
	kinds := []string{
		CommandAlertAck, CommandProbeStop, CommandHistoryClear,
		CommandCredentialPrepare, CommandCredentialActivate,
		CommandCertificatePrepare, CommandCertificateActivate,
	}
	for _, kind := range kinds {
		slug := strings.ReplaceAll(kind, ".", "-")
		valid := readFixture(t, "valid", "command-"+slug+".json")
		_, command, err := DecodeCommandRequest(valid)
		if err != nil {
			t.Fatalf("%s valid: %v", kind, err)
		}
		if command.Kind != kind || command.CommandID == "" || command.Data == nil {
			t.Fatalf("%s decoder dropped identity", kind)
		}
	}
	invalid := map[string]string{
		CommandAlertAck:            "command-alert-ack-missing-incident.json",
		CommandProbeStop:           "command-probe-stop-with-incident.json",
		CommandHistoryClear:        "command-history-clear-zero-seq.json",
		CommandCredentialPrepare:   "command-credential-prepare-enroll-token.json",
		CommandCredentialActivate:  "command-credential-activate-with-token.json",
		CommandCertificatePrepare:  "command-certificate-prepare-zero-days.json",
		CommandCertificateActivate: "command-certificate-activate-missing-fingerprint.json",
	}
	for kind, name := range invalid {
		_, command, err := DecodeCommandRequest(readFixture(t, "invalid", name))
		if err == nil || !reflect.DeepEqual(command, CommandRequest{}) {
			t.Fatalf("%s invalid fixture returned a usable command: %v", kind, err)
		}
	}
}

func TestCommandRequestRoundTripAndUnknownFields(t *testing.T) {
	for _, name := range []string{
		"command-alert-ack.json", "command-probe-stop.json", "command-history-clear.json",
		"command-history-clear-max.json", "command-credential-prepare.json", "command-credential-activate.json",
		"command-certificate-prepare.json", "command-certificate-activate.json",
	} {
		t.Run(name, func(t *testing.T) {
			fixture := readFixture(t, "valid", name)
			envelope, command, err := DecodeCommandRequest(changePayload(t, fixture, map[string]any{"future_optional": true}))
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(command)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(encoded), "future_optional") || strings.Contains(string(encoded), `"CommandID"`) {
				t.Fatalf("typed command leaked non-contract data: %s", encoded)
			}
			envelope.Payload = encoded
			frame, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			_, again, err := DecodeCommandRequest(frame)
			if err != nil || !reflect.DeepEqual(command, again) {
				t.Fatalf("round trip changed command: %v", err)
			}
		})
	}
}

func TestCommandResultStatusesAndSecretFreeDetails(t *testing.T) {
	_, result, err := DecodeCommandResult(readFixture(t, "valid", "command-result-credential-prepare.json"))
	if err != nil {
		t.Fatal(err)
	}
	details, ok := result.Details.(CredentialPrepareDetails)
	if !ok || details.CredentialVersion != math.MaxInt64 {
		t.Fatal("credential prepare details lost signed-64-bit version")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "phx_probe_") || strings.Contains(string(encoded), "private_key") || strings.Contains(string(encoded), "token") {
		t.Fatalf("command result leaked a secret: %s", encoded)
	}
	_, cert, err := DecodeCommandResult(readFixture(t, "valid", "command-result-certificate-prepare.json"))
	if err != nil {
		t.Fatal(err)
	}
	certDetails, ok := cert.Details.(CertificatePrepareDetails)
	if !ok || certDetails.TLSFingerprint == "" || certDetails.CertificateVersion != 4 {
		t.Fatal("certificate prepare details were dropped")
	}
	for _, name := range []string{"command-result-token.json", "command-result-private-key.json", "command-result-unknown-status.json"} {
		_, got, err := DecodeCommandResult(readFixture(t, "invalid", name))
		if err == nil || !reflect.DeepEqual(got, CommandResult{}) {
			t.Fatalf("%s yielded a usable result", name)
		}
	}
}

func TestCommandRequestRejectsUnknownKindAndLocalIdentity(t *testing.T) {
	for _, name := range []string{"command-unknown-kind.json", "command-local-probe.json", "command-kind-data-mismatch.json", "command-overflow-generation.json", "command-null-id.json", "command-duplicate-data-key.json"} {
		_, command, err := DecodeCommandRequest(readFixture(t, "invalid", name))
		if err == nil || !reflect.DeepEqual(command, CommandRequest{}) {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
	_, _, err := DecodeCommandRequest(readFixture(t, "valid", "command-alert-ack.json"))
	if err != nil {
		t.Fatal(err)
	}
	wrong := mutateJSON(t, readFixture(t, "valid", "command-alert-ack.json"), func(frame map[string]any) {
		frame["type"] = "health"
	})
	if _, command, err := DecodeCommandRequest(wrong); !errors.Is(err, ErrUnsupportedPayload) || !reflect.DeepEqual(command, CommandRequest{}) {
		t.Fatalf("wrong decoder: %v", err)
	}
}

func TestCommandRequiredFieldsAreNotNullable(t *testing.T) {
	fixture := readFixture(t, "valid", "command-probe-stop.json")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(fixture, &raw); err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw["payload"], &payload); err != nil {
		t.Fatal(err)
	}
	for key := range payload {
		for _, absent := range []bool{true, false} {
			frame := mutateJSON(t, fixture, func(frame map[string]any) {
				fields := frame["payload"].(map[string]any)
				if absent {
					delete(fields, key)
				} else {
					fields[key] = nil
				}
			})
			if _, command, err := DecodeCommandRequest(frame); err == nil || !reflect.DeepEqual(command, CommandRequest{}) {
				t.Fatalf("accepted missing/null %s", key)
			}
		}
	}
}

func TestNoProbeMutationRoutesOrListeners(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	needles := []string{"/api/probes", "/ws/probe/enroll", "/ws/probe/v1", "PROBES_ENABLED"}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := info.Name()
			if base == "testdata" || base == "docs" || base == "web" || base == "charts" || base == ".git" || base == "research" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(body)
		for _, needle := range needles {
			if strings.Contains(text, needle) {
				t.Errorf("%s contains %s", path, needle)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
