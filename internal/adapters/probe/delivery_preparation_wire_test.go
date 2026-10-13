package probe

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// deliveryPreparationFixture builds one closed both-mode request with distinct
// sub-IEEE-754 counters so wire mutations can target each field separately.
func deliveryPreparationFixture(t *testing.T) (domain.ProbeDeliveryPreparation, []byte) {
	t.Helper()
	at := time.Date(2026, 2, 3, 4, 5, 6, 789000000, time.UTC)
	c := domain.ProbeDeliveryPreparation{
		ProbeDeliveryModeApplication: domain.ProbeDeliveryModeApplication{
			ProbeConfigTarget:  domain.ProbeConfigTarget{HubID: "11111111-2222-4333-8444-555555555555", ProbeID: "21111111-2222-4333-8444-555555555555"},
			MonitorID:          7,
			AssignmentRevision: 9007199254740995,
			Generation:         9007199254740993,
			ConfigRevision:     9007199254740997,
			SHA256:             strings.Repeat("ab", 32),
			StreamID:           "31111111-2222-4333-8444-555555555555",
			AlertDelivery:      domain.AlertDeliveryBoth,
		},
		CommandID: "41111111-2222-4333-8444-555555555555",
		CreatedAt: at,
		ExpiresAt: at.Add(24 * time.Hour),
	}
	payload, err := (DeliveryPreparationCodec{}).EncodeDeliveryPreparation(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	return c, payload
}

func mutateDeliveryPreparation(t *testing.T, payload []byte, old, replacement string) []byte {
	t.Helper()
	mutated := bytes.Replace(payload, []byte(old), []byte(replacement), 1)
	if bytes.Equal(mutated, payload) {
		t.Fatalf("fixture did not change wire bytes for %q", old)
	}
	return mutated
}

func TestDeliveryPreparationCodecBindsExactCanonicalPayload(t *testing.T) {
	codec := DeliveryPreparationCodec{}
	c, payload := deliveryPreparationFixture(t)
	var request map[string]json.RawMessage
	if err := json.Unmarshal(payload, &request); err != nil {
		t.Fatal(err)
	}
	var target, data map[string]json.RawMessage
	if err := json.Unmarshal(request["target"], &target); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(request["data"], &data); err != nil {
		t.Fatal(err)
	}
	if len(data) != 7 || data["generation"] != nil || target["assignment_generation"] == nil ||
		!bytes.Equal(target["source_alert_id"], []byte("null")) {
		t.Fatalf("closed request shape changed: target=%v data=%v", target, data)
	}
	for _, key := range []string{"hub_id", "stream_id", "monitor_id", "assignment_revision", "config_revision", "sha256", "alert_delivery"} {
		if data[key] == nil {
			t.Fatalf("closed data lost %s: %v", key, data)
		}
	}
	got, err := codec.DecodeDeliveryPreparation(t.Context(), payload)
	if err != nil {
		t.Fatal(err)
	}
	want := c
	want.PayloadHash = sha256.Sum256(payload)
	if !sameDeliveryPreparationRequest(got, want) || got.PayloadHash != want.PayloadHash {
		t.Fatalf("typed preparation changed: %+v", got)
	}
	// Counters above 2^53 must survive exactly, never through float rounding.
	if got.Generation != 9007199254740993 || got.AssignmentRevision != 9007199254740995 || got.ConfigRevision != 9007199254740997 {
		t.Fatalf("large revision precision lost: %+v", got.ProbeDeliveryModeApplication)
	}
	canonical, err := codec.EncodeDeliveryPreparation(t.Context(), got)
	if err != nil || !bytes.Equal(canonical, payload) {
		t.Fatal("canonical output was not reproducible", err)
	}
}

func TestDeliveryPreparationCodecRejectsInvalidRequests(t *testing.T) {
	codec := DeliveryPreparationCodec{}
	_, payload := deliveryPreparationFixture(t)
	sha := strings.Repeat("ab", 32)
	frame, err := encodeCommandRequestFrame(Decimal(3), payload)
	if err != nil {
		t.Fatal(err)
	}
	type prepMutation struct {
		name string
		bad  []byte
	}
	var mutations []prepMutation
	add := func(name string, bad []byte) {
		mutations = append(mutations, prepMutation{name, bad})
	}
	mutate := func(name, old, replacement string) {
		add(name, mutateDeliveryPreparation(t, payload, old, replacement))
	}
	add("leading_space", append([]byte(" "), payload...))
	add("trailing_space", append(append([]byte{}, payload...), ' '))
	add("envelope_frame", frame)
	add("duplicate_key", append([]byte(`{"kind":"delivery.prepare",`), payload[1:]...))
	add("oversized", bytes.Repeat([]byte("x"), domain.MaxProbeCommandBytes+1))
	mutate("missing_monitor_id", `"monitor_id":7,`, ``)
	mutate("monitor_zero", `"monitor_id":7`, `"monitor_id":0`)
	mutate("monitor_negative", `"monitor_id":7`, `"monitor_id":-7`)
	mutate("monitor_string", `"monitor_id":7`, `"monitor_id":"7"`)
	mutate("monitor_fractional", `"monitor_id":7`, `"monitor_id":7.5`)
	mutate("monitor_null", `"monitor_id":7`, `"monitor_id":null`)
	mutate("assignment_zero", `"assignment_revision":"9007199254740995"`, `"assignment_revision":"0"`)
	mutate("assignment_number", `"assignment_revision":"9007199254740995"`, `"assignment_revision":9007199254740995`)
	mutate("assignment_leading_zero", `"assignment_revision":"9007199254740995"`, `"assignment_revision":"09007199254740995"`)
	mutate("assignment_negative", `"assignment_revision":"9007199254740995"`, `"assignment_revision":"-1"`)
	mutate("assignment_overflow", `"assignment_revision":"9007199254740995"`, `"assignment_revision":"9223372036854775808"`)
	mutate("assignment_null", `"assignment_revision":"9007199254740995"`, `"assignment_revision":null`)
	mutate("config_zero", `"config_revision":"9007199254740997"`, `"config_revision":"0"`)
	mutate("generation_zero", `"assignment_generation":"9007199254740993"`, `"assignment_generation":"0"`)
	mutate("generation_null", `"assignment_generation":"9007199254740993"`, `"assignment_generation":null`)
	mutate("generation_negative", `"assignment_generation":"9007199254740993"`, `"assignment_generation":"-1"`)
	mutate("generation_number", `"assignment_generation":"9007199254740993"`, `"assignment_generation":9007199254740993`)
	mutate("generation_leading_zero", `"assignment_generation":"9007199254740993"`, `"assignment_generation":"09007199254740993"`)
	mutate("source_alert_present", `"source_alert_id":null`, `"source_alert_id":"51111111-2222-4333-8444-555555555555"`)
	mutate("hub_zero_uuid", `"hub_id":"11111111-2222-4333-8444-555555555555"`, `"hub_id":"00000000-0000-0000-0000-000000000000"`)
	mutate("hub_noncanonical", `"hub_id":"11111111-2222-4333-8444-555555555555"`, `"hub_id":"11111111-2222-4333-8444-55555555555A"`)
	mutate("missing_hub_id", `"hub_id":"11111111-2222-4333-8444-555555555555",`, ``)
	mutate("stream_local", `"stream_id":"31111111-2222-4333-8444-555555555555"`, `"stream_id":"00000000-0000-4000-8000-000000000001"`)
	mutate("probe_zero_uuid", `"probe_id":"21111111-2222-4333-8444-555555555555"`, `"probe_id":"00000000-0000-0000-0000-000000000000"`)
	mutate("command_id_invalid", `"command_id":"41111111-2222-4333-8444-555555555555"`, `"command_id":"not-a-uuid"`)
	mutate("sha_uppercase", `"sha256":"`+sha+`"`, `"sha256":"`+strings.ToUpper(sha)+`"`)
	mutate("sha_short", `"sha256":"`+sha+`"`, `"sha256":"ab"`)
	mutate("sha_null", `"sha256":"`+sha+`"`, `"sha256":null`)
	mutate("mode_regional", `"alert_delivery":"both"`, `"alert_delivery":"regional"`)
	mutate("mode_empty", `"alert_delivery":"both"`, `"alert_delivery":""`)
	mutate("mode_unknown", `"alert_delivery":"both"`, `"alert_delivery":"aggregates"`)
	mutate("mode_bare", `"alert_delivery":"both"`, `"alert_delivery":both`)
	mutate("created_sub_microsecond", `"created_at":"2026-02-03T04:05:06.789Z"`, `"created_at":"2026-02-03T04:05:06.7890001Z"`)
	mutate("created_non_utc", `"created_at":"2026-02-03T04:05:06.789Z"`, `"created_at":"2026-02-03T04:05:06.789+00:00"`)
	mutate("created_null", `"created_at":"2026-02-03T04:05:06.789Z"`, `"created_at":null`)
	mutate("expires_not_after", `"expires_at":"2026-02-04T04:05:06.789Z"`, `"expires_at":"2026-02-03T04:05:06.789Z"`)
	mutate("expires_over_one_week", `"expires_at":"2026-02-04T04:05:06.789Z"`, `"expires_at":"2026-02-11T04:05:06.789Z"`)
	for _, entry := range mutations {
		t.Run(entry.name, func(t *testing.T) {
			if _, err := codec.DecodeDeliveryPreparation(t.Context(), entry.bad); err == nil {
				t.Fatal("invalid preparation request decoded")
			}
		})
	}
}

func TestDeliveryPreparationCodecHasNoActivateKind(t *testing.T) {
	codec := DeliveryPreparationCodec{}
	_, payload := deliveryPreparationFixture(t)
	activate := mutateDeliveryPreparation(t, payload, `"kind":"delivery.prepare"`, `"kind":"delivery.activate"`)
	if _, err := codec.DecodeDeliveryPreparation(t.Context(), activate); !errors.Is(err, ErrUnsupportedPayload) {
		t.Fatalf("delivery.activate must stay unsupported: %v", err)
	}
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	cert := domain.ProbeCertificateCommand{CommandID: "41111111-2222-4333-8444-555555555555", ProbeID: "21111111-2222-4333-8444-555555555555", Kind: CommandCertificatePrepare, CreatedAt: at, ExpiresAt: at.Add(time.Hour), RotationID: "51111111-2222-4333-8444-555555555555", CertificateVersion: 2, ValidForDays: 365}
	certPayload, err := (CertificateCommandCodec{}).EncodeCertificateCommand(t.Context(), cert)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := codec.DecodeDeliveryPreparation(t.Context(), certPayload); err == nil {
		t.Fatal("foreign command kind decoded as a preparation request")
	}
}

func TestDeliveryPreparationCodecKeepsUnknownOptionalFieldsCompatible(t *testing.T) {
	codec := DeliveryPreparationCodec{}
	c, payload := deliveryPreparationFixture(t)
	extended := mutateDeliveryPreparation(t, payload, `"alert_delivery":"both"`, `"alert_delivery":"both","future_optional":{"nested":[1,2,3]}`)
	got, err := codec.DecodeDeliveryPreparation(t.Context(), extended)
	if err != nil {
		t.Fatal(err)
	}
	want := c
	want.PayloadHash = sha256.Sum256(extended)
	if !sameDeliveryPreparationRequest(got, want) {
		t.Fatalf("unknown optional fields changed the request: %+v", got)
	}
	canonical, err := codec.EncodeDeliveryPreparation(t.Context(), got)
	if err != nil || !bytes.Equal(canonical, payload) {
		t.Fatal("canonical output was not preserved", err)
	}
}

func TestDeliveryPreparationCodecClaimsNoModeApplication(t *testing.T) {
	codec := DeliveryPreparationCodec{}
	c, payload := deliveryPreparationFixture(t)
	claimed := mutateDeliveryPreparation(t, payload, `"alert_delivery":"both"`,
		`"alert_delivery":"both","mode_application_supported":true,"applied":true,"mode_application":{"alert_delivery":"both"}`)
	got, err := codec.DecodeDeliveryPreparation(t.Context(), claimed)
	if err != nil {
		t.Fatal(err)
	}
	// Unknown optional claims stay compatible but capture nothing: the decoded
	// value is exactly the request binding and can hold no application evidence.
	want := c
	want.PayloadHash = sha256.Sum256(claimed)
	if !sameDeliveryPreparationRequest(got, want) {
		t.Fatalf("preparation carried a mode application claim: %+v", got)
	}
	canonical, err := codec.EncodeDeliveryPreparation(t.Context(), got)
	if err != nil || !bytes.Equal(canonical, payload) {
		t.Fatal("canonical request gained an application claim", err)
	}
	for _, claim := range []string{"mode_application", `"applied"`} {
		if bytes.Contains(canonical, []byte(claim)) {
			t.Fatalf("canonical output carries %q: %s", claim, canonical)
		}
	}
	// The tolerated claims never relax validation of the requested mode.
	regional := mutateDeliveryPreparation(t, payload, `"alert_delivery":"both"`, `"alert_delivery":"regional","mode_application_supported":true`)
	if _, err := codec.DecodeDeliveryPreparation(t.Context(), regional); err == nil {
		t.Fatal("regional mode decoded as a preparation request")
	}
}

func TestDeliveryPreparationEncodeBindsCanonicalOutputHash(t *testing.T) {
	codec := DeliveryPreparationCodec{}
	c, payload := deliveryPreparationFixture(t)
	// A zero PayloadHash is accepted and a stale one is never copied into new
	// bytes: identity is bound from the canonical output alone.
	c.PayloadHash = [32]byte{1}
	encoded, err := codec.EncodeDeliveryPreparation(t.Context(), c)
	if err != nil || !bytes.Equal(encoded, payload) {
		t.Fatal("encode changed canonical bytes", err)
	}
	got, err := codec.DecodeDeliveryPreparation(t.Context(), encoded)
	if err != nil || got.PayloadHash != sha256.Sum256(encoded) || got.PayloadHash == c.PayloadHash {
		t.Fatal("payload hash was not bound to canonical output", err)
	}
}

func TestDeliveryPreparationEncodeRejectsInvalidScope(t *testing.T) {
	codec := DeliveryPreparationCodec{}
	c, _ := deliveryPreparationFixture(t)
	check := func(t *testing.T, name string, mutate func(*domain.ProbeDeliveryPreparation)) {
		t.Run(name, func(t *testing.T) {
			bad := c
			mutate(&bad)
			if _, err := codec.EncodeDeliveryPreparation(t.Context(), bad); err == nil {
				t.Fatal("invalid preparation encoded")
			}
		})
	}
	check(t, "regional_mode", func(p *domain.ProbeDeliveryPreparation) { p.AlertDelivery = domain.AlertDeliveryRegional })
	check(t, "local_stream", func(p *domain.ProbeDeliveryPreparation) { p.StreamID = domain.LocalStreamID })
	check(t, "zero_generation", func(p *domain.ProbeDeliveryPreparation) { p.Generation = 0 })
	check(t, "zero_monitor", func(p *domain.ProbeDeliveryPreparation) { p.MonitorID = 0 })
	check(t, "zero_assignment", func(p *domain.ProbeDeliveryPreparation) { p.AssignmentRevision = 0 })
	check(t, "sub_microsecond", func(p *domain.ProbeDeliveryPreparation) { p.CreatedAt = p.CreatedAt.Add(time.Nanosecond) })
	check(t, "week_lifetime", func(p *domain.ProbeDeliveryPreparation) { p.ExpiresAt = p.ExpiresAt.Add(7 * 24 * time.Hour) })
}

func TestDeliveryPreparationCodecRejectsCanceledContext(t *testing.T) {
	codec := DeliveryPreparationCodec{}
	c, payload := deliveryPreparationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := codec.DecodeDeliveryPreparation(ctx, payload); err == nil {
		t.Fatal("canceled decode accepted")
	}
	if _, err := codec.EncodeDeliveryPreparation(ctx, c); err == nil {
		t.Fatal("canceled encode accepted")
	}
}

func TestCommandResultDeliveryPreparedCodeClaimsOnlyPreparation(t *testing.T) {
	at := Timestamp(time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC))
	code := deliveryPreparedCode
	result := CommandResult{CommandID: "41111111-2222-4333-8444-555555555555", Status: "applied", AppliedAt: &at, Code: &code, Message: "delivery prepared"}
	frame, err := encodeFrame("command.result", Decimal(1), result)
	if err != nil {
		t.Fatal(err)
	}
	_, got, err := DecodeCommandResult(frame)
	if err != nil || got.Code == nil || *got.Code != deliveryPreparedCode || got.Details != nil {
		t.Fatalf("delivery_prepared receipt changed: %+v %v", got, err)
	}
	// The success code records the PREPARATION command only: it can never carry
	// credential, certificate or application details, and no other code decodes.
	result.Details = CredentialPrepareDetails{CredentialVersion: Decimal(2)}
	if frame, err = encodeFrame("command.result", Decimal(1), result); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeCommandResult(frame); err == nil {
		t.Fatal("delivery_prepared receipt carried credential details")
	}
	result.Details = nil
	other := "delivery_applied"
	result.Code = &other
	if frame, err = encodeFrame("command.result", Decimal(1), result); err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeCommandResult(frame); err == nil {
		t.Fatal("unknown success code decoded")
	}
	legacy := CommandResult{CommandID: "41111111-2222-4333-8444-555555555555", Status: "applied", AppliedAt: &at, Message: "applied"}
	if frame, err = encodeFrame("command.result", Decimal(1), legacy); err != nil {
		t.Fatal(err)
	}
	if _, got, err = DecodeCommandResult(frame); err != nil || got.Code != nil {
		t.Fatalf("null success code changed: %+v %v", got, err)
	}
}
