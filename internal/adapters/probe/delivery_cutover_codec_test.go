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

// deliveryCutoverFixture builds one closed both-mode source-gate request with
// distinct sub-IEEE-754 counters so wire mutations can target each field.
func deliveryCutoverFixture(t *testing.T, kind string) (domain.DeliveryCutoverCommand, []byte) {
	t.Helper()
	at := time.Date(2026, 2, 3, 4, 5, 6, 789000000, time.UTC)
	c := domain.DeliveryCutoverCommand{
		ProbeDeliveryModeApplication: domain.ProbeDeliveryModeApplication{
			ProbeConfigTarget:  domain.ProbeConfigTarget{HubID: "11111111-2222-4333-8444-555555555555", ProbeID: "21111111-2222-4333-8444-555555555555"},
			MonitorID:          9007199254740993,
			AssignmentRevision: 9007199254740995,
			Generation:         9007199254740993,
			ConfigRevision:     9007199254740997,
			SHA256:             strings.Repeat("ab", 32),
			StreamID:           "31111111-2222-4333-8444-555555555555",
			AlertDelivery:      domain.AlertDeliveryBoth,
		},
		CommandID:            "41111111-2222-4333-8444-555555555555",
		BarrierID:            "51111111-2222-4333-8444-555555555555",
		PreparationCommandID: "61111111-2222-4333-8444-555555555555",
		Kind:                 kind,
		CreatedAt:            at,
		ExpiresAt:            at.Add(24 * time.Hour),
	}
	payload, err := (DeliveryCutoverCodec{}).EncodeDeliveryCutover(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	return c, payload
}

func mutateDeliveryCutover(t *testing.T, payload []byte, old, replacement string) []byte {
	t.Helper()
	mutated := bytes.Replace(payload, []byte(old), []byte(replacement), 1)
	if bytes.Equal(mutated, payload) {
		t.Fatalf("fixture did not change wire bytes for %q", old)
	}
	return mutated
}

func TestDeliveryCutoverCodecBindsExactCanonicalPayload(t *testing.T) {
	codec := DeliveryCutoverCodec{}
	for _, kind := range []string{CommandDeliveryQuiesce, CommandDeliveryApply, CommandDeliveryResume} {
		t.Run(kind, func(t *testing.T) {
			c, payload := deliveryCutoverFixture(t, kind)
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
			if len(data) != 9 || data["generation"] != nil || data["barrier_id"] == nil || data["preparation_command_id"] == nil ||
				target["assignment_generation"] == nil || !bytes.Equal(target["source_alert_id"], []byte("null")) {
				t.Fatalf("closed request shape changed: target=%v data=%v", target, data)
			}
			for _, key := range []string{"hub_id", "stream_id", "monitor_id", "assignment_revision", "config_revision", "sha256", "alert_delivery"} {
				if data[key] == nil {
					t.Fatalf("closed data lost %s: %v", key, data)
				}
			}
			got, err := codec.DecodeDeliveryCutover(t.Context(), payload)
			if err != nil {
				t.Fatal(err)
			}
			want := c
			want.PayloadHash = sha256.Sum256(payload)
			if !sameDeliveryCutoverCommand(got, want) || got.PayloadHash != want.PayloadHash {
				t.Fatalf("typed cutover command changed: %+v", got)
			}
			// Counters above 2^53 must survive exactly, never through float rounding.
			if got.Generation != 9007199254740993 || got.AssignmentRevision != 9007199254740995 ||
				got.ConfigRevision != 9007199254740997 || got.MonitorID != 9007199254740993 {
				t.Fatalf("large revision precision lost: %+v", got.ProbeDeliveryModeApplication)
			}
			canonical, err := codec.EncodeDeliveryCutover(t.Context(), got)
			if err != nil || !bytes.Equal(canonical, payload) {
				t.Fatal("canonical output was not reproducible", err)
			}
		})
	}
}

func TestDeliveryCutoverCodecRejectsInvalidRequests(t *testing.T) {
	codec := DeliveryCutoverCodec{}
	_, payload := deliveryCutoverFixture(t, CommandDeliveryQuiesce)
	sha := strings.Repeat("ab", 32)
	frame, err := encodeCommandRequestFrame(Decimal(3), payload)
	if err != nil {
		t.Fatal(err)
	}
	type mutation struct {
		name string
		bad  []byte
	}
	var mutations []mutation
	add := func(name string, bad []byte) {
		mutations = append(mutations, mutation{name, bad})
	}
	mutate := func(name, old, replacement string) {
		add(name, mutateDeliveryCutover(t, payload, old, replacement))
	}
	add("leading_space", append([]byte(" "), payload...))
	add("trailing_space", append(append([]byte{}, payload...), ' '))
	add("envelope_frame", frame)
	add("duplicate_key", append([]byte(`{"kind":"delivery.quiesce",`), payload[1:]...))
	add("oversized", bytes.Repeat([]byte("x"), domain.MaxProbeCommandBytes+1))
	// Closed data: unknown and reserved claim fields never decode.
	mutate("unknown_data_field", `"alert_delivery":"both"`, `"alert_delivery":"both","future_optional":1`)
	mutate("reserved_application_claim", `"alert_delivery":"both"`, `"alert_delivery":"both","mode_application_supported":true`)
	mutate("missing_monitor_id", `"monitor_id":9007199254740993,`, ``)
	mutate("monitor_zero", `"monitor_id":9007199254740993`, `"monitor_id":0`)
	mutate("monitor_negative", `"monitor_id":9007199254740993`, `"monitor_id":-7`)
	mutate("monitor_string", `"monitor_id":9007199254740993`, `"monitor_id":"7"`)
	mutate("monitor_fractional", `"monitor_id":9007199254740993`, `"monitor_id":7.5`)
	mutate("monitor_null", `"monitor_id":9007199254740993`, `"monitor_id":null`)
	mutate("assignment_zero", `"assignment_revision":"9007199254740995"`, `"assignment_revision":"0"`)
	mutate("assignment_number", `"assignment_revision":"9007199254740995"`, `"assignment_revision":9007199254740995`)
	mutate("assignment_leading_zero", `"assignment_revision":"9007199254740995"`, `"assignment_revision":"09007199254740995"`)
	mutate("assignment_negative", `"assignment_revision":"9007199254740995"`, `"assignment_revision":"-1"`)
	mutate("assignment_overflow", `"assignment_revision":"9007199254740995"`, `"assignment_revision":"9223372036854775808"`)
	mutate("assignment_null", `"assignment_revision":"9007199254740995"`, `"assignment_revision":null`)
	mutate("config_zero", `"config_revision":"9007199254740997"`, `"config_revision":"0"`)
	mutate("generation_zero", `"assignment_generation":"9007199254740993"`, `"assignment_generation":"0"`)
	mutate("generation_null", `"assignment_generation":"9007199254740993"`, `"assignment_generation":null`)
	mutate("generation_missing", `"assignment_generation":"9007199254740993",`, ``)
	mutate("generation_invalid", `"assignment_generation":"9007199254740993"`, `"assignment_generation":"9007199254740993x"`)
	mutate("generation_negative", `"assignment_generation":"9007199254740993"`, `"assignment_generation":"-1"`)
	mutate("generation_number", `"assignment_generation":"9007199254740993"`, `"assignment_generation":9007199254740993`)
	mutate("generation_leading_zero", `"assignment_generation":"9007199254740993"`, `"assignment_generation":"09007199254740993"`)
	mutate("source_alert_present", `"source_alert_id":null`, `"source_alert_id":"71111111-2222-4333-8444-555555555555"`)
	mutate("hub_zero_uuid", `"hub_id":"11111111-2222-4333-8444-555555555555"`, `"hub_id":"00000000-0000-0000-0000-000000000000"`)
	mutate("hub_noncanonical", `"hub_id":"11111111-2222-4333-8444-555555555555"`, `"hub_id":"11111111-2222-4333-8444-55555555555A"`)
	mutate("missing_hub_id", `"hub_id":"11111111-2222-4333-8444-555555555555",`, ``)
	mutate("stream_local_reserved", `"stream_id":"31111111-2222-4333-8444-555555555555"`, `"stream_id":"00000000-0000-4000-8000-000000000001"`)
	mutate("stream_noncanonical", `"stream_id":"31111111-2222-4333-8444-555555555555"`, `"stream_id":"31111111-2222-4333-8444-55555555555G"`)
	mutate("probe_zero_uuid", `"probe_id":"21111111-2222-4333-8444-555555555555"`, `"probe_id":"00000000-0000-0000-0000-000000000000"`)
	mutate("command_id_invalid", `"command_id":"41111111-2222-4333-8444-555555555555"`, `"command_id":"not-a-uuid"`)
	mutate("barrier_zero_uuid", `"barrier_id":"51111111-2222-4333-8444-555555555555"`, `"barrier_id":"00000000-0000-0000-0000-000000000000"`)
	mutate("barrier_noncanonical", `"barrier_id":"51111111-2222-4333-8444-555555555555"`, `"barrier_id":"51111111-2222-4333-8444-55555555555B"`)
	mutate("barrier_null", `"barrier_id":"51111111-2222-4333-8444-555555555555"`, `"barrier_id":null`)
	mutate("missing_barrier_id", `"barrier_id":"51111111-2222-4333-8444-555555555555",`, ``)
	mutate("preparation_zero_uuid", `"preparation_command_id":"61111111-2222-4333-8444-555555555555"`, `"preparation_command_id":"00000000-0000-0000-0000-000000000000"`)
	mutate("preparation_noncanonical", `"preparation_command_id":"61111111-2222-4333-8444-555555555555"`, `"preparation_command_id":"61111111-2222-4333-8444-55555555555C"`)
	mutate("missing_preparation_command_id", `,"preparation_command_id":"61111111-2222-4333-8444-555555555555"`, ``)
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
	mutate("expires_over_one_day", `"expires_at":"2026-02-04T04:05:06.789Z"`, `"expires_at":"2026-02-04T04:05:06.79Z"`)
	for _, entry := range mutations {
		t.Run(entry.name, func(t *testing.T) {
			if _, err := codec.DecodeDeliveryCutover(t.Context(), entry.bad); err == nil {
				t.Fatal("invalid cutover request decoded")
			}
		})
	}
}

func TestDeliveryCutoverCodecRejectsForeignAndMismatchedKinds(t *testing.T) {
	codec := DeliveryCutoverCodec{}
	_, payload := deliveryCutoverFixture(t, CommandDeliveryQuiesce)
	// A foreign kind with cutover data never decodes as a cutover command...
	if _, err := codec.DecodeDeliveryCutover(t.Context(), mutateDeliveryCutover(t, payload, `"kind":"delivery.quiesce"`, `"kind":"delivery.activate"`)); !errors.Is(err, ErrUnsupportedPayload) {
		t.Fatalf("delivery.activate must stay unsupported: %v", err)
	}
	if _, err := codec.DecodeDeliveryCutover(t.Context(), mutateDeliveryCutover(t, payload, `"kind":"delivery.quiesce"`, `"kind":"delivery.prepare"`)); err == nil {
		t.Fatal("delivery.prepare decoded as a cutover command")
	}
	// ...and a preparation request never decodes as a cutover command.
	_, preparation := deliveryPreparationFixture(t)
	if _, err := codec.DecodeDeliveryCutover(t.Context(), preparation); err == nil {
		t.Fatal("preparation request decoded as a cutover command")
	}
	// Kind and success code always pair exactly; the request's own kind is the
	// only claim the decoder binds.
	for _, kind := range []string{CommandDeliveryQuiesce, CommandDeliveryApply, CommandDeliveryResume} {
		_, p := deliveryCutoverFixture(t, kind)
		got, err := codec.DecodeDeliveryCutover(t.Context(), p)
		if err != nil || got.Kind != kind || domain.DeliveryCutoverSuccessCode(got.Kind) == "" {
			t.Fatalf("kind binding lost for %s: %+v %v", kind, got, err)
		}
	}
}

func TestDeliveryCutoverEncodeBindsCanonicalOutputHash(t *testing.T) {
	codec := DeliveryCutoverCodec{}
	c, payload := deliveryCutoverFixture(t, CommandDeliveryApply)
	// A zero PayloadHash is accepted and a stale one is never copied into new
	// bytes: identity is bound from the canonical output alone.
	c.PayloadHash = [32]byte{1}
	encoded, err := codec.EncodeDeliveryCutover(t.Context(), c)
	if err != nil || !bytes.Equal(encoded, payload) {
		t.Fatal("encode changed canonical bytes", err)
	}
	got, err := codec.DecodeDeliveryCutover(t.Context(), encoded)
	if err != nil || got.PayloadHash != sha256.Sum256(encoded) || got.PayloadHash == c.PayloadHash {
		t.Fatal("payload hash was not bound to canonical output", err)
	}
}

func TestDeliveryCutoverEncodeRejectsInvalidScope(t *testing.T) {
	codec := DeliveryCutoverCodec{}
	c, _ := deliveryCutoverFixture(t, CommandDeliveryResume)
	check := func(t *testing.T, name string, mutate func(*domain.DeliveryCutoverCommand)) {
		t.Run(name, func(t *testing.T) {
			bad := c
			mutate(&bad)
			if _, err := codec.EncodeDeliveryCutover(t.Context(), bad); err == nil {
				t.Fatal("invalid cutover command encoded")
			}
		})
	}
	check(t, "regional_mode", func(c *domain.DeliveryCutoverCommand) { c.AlertDelivery = domain.AlertDeliveryRegional })
	check(t, "local_stream", func(c *domain.DeliveryCutoverCommand) { c.StreamID = domain.LocalStreamID })
	check(t, "local_probe", func(c *domain.DeliveryCutoverCommand) { c.ProbeID = domain.LocalProbeID })
	check(t, "zero_generation", func(c *domain.DeliveryCutoverCommand) { c.Generation = 0 })
	check(t, "zero_monitor", func(c *domain.DeliveryCutoverCommand) { c.MonitorID = 0 })
	check(t, "zero_barrier", func(c *domain.DeliveryCutoverCommand) { c.BarrierID = "00000000-0000-0000-0000-000000000000" })
	check(t, "zero_preparation", func(c *domain.DeliveryCutoverCommand) { c.PreparationCommandID = "" })
	check(t, "unknown_kind", func(c *domain.DeliveryCutoverCommand) { c.Kind = "delivery.activate" })
	check(t, "empty_kind", func(c *domain.DeliveryCutoverCommand) { c.Kind = "" })
	check(t, "sub_microsecond", func(c *domain.DeliveryCutoverCommand) { c.CreatedAt = c.CreatedAt.Add(time.Nanosecond) })
	check(t, "day_plus_lifetime", func(c *domain.DeliveryCutoverCommand) { c.ExpiresAt = c.ExpiresAt.Add(time.Millisecond) })
}

func TestDeliveryCutoverCodecRejectsCanceledContext(t *testing.T) {
	codec := DeliveryCutoverCodec{}
	c, payload := deliveryCutoverFixture(t, CommandDeliveryQuiesce)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := codec.DecodeDeliveryCutover(ctx, payload); err == nil {
		t.Fatal("canceled decode accepted")
	}
	if _, err := codec.EncodeDeliveryCutover(ctx, c); err == nil {
		t.Fatal("canceled encode accepted")
	}
}

// TestCommandOutcomeCutoverKindCodes pins the kind-specific result shape: one
// kind's success code never claims another kind's effect and a code-empty
// cutover success never leaves this process.
func TestCommandOutcomeCutoverKindCodes(t *testing.T) {
	at := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	outcome := func(_ string, status, code string) domain.ProbeCommandOutcome {
		o := domain.ProbeCommandOutcome{CommandID: "41111111-2222-4333-8444-555555555555", Status: status, Code: code, Message: "done"}
		if status == "applied" || status == "already_applied" || status == "already_resolved" {
			o.AppliedAt = &at
		}
		return o
	}
	kinds := []string{CommandDeliveryQuiesce, CommandDeliveryApply, CommandDeliveryResume}
	for _, kind := range kinds {
		if err := validCommandOutcomeForKind(kind, "41111111-2222-4333-8444-555555555555", outcome(kind, "applied", domain.DeliveryCutoverSuccessCode(kind))); err != nil {
			t.Fatalf("%s own success code refused: %v", kind, err)
		}
		if err := validCommandOutcomeForKind(kind, "41111111-2222-4333-8444-555555555555", outcome(kind, "already_applied", domain.DeliveryCutoverSuccessCode(kind))); err != nil {
			t.Fatalf("%s duplicate success refused: %v", kind, err)
		}
		for _, other := range kinds {
			if other == kind {
				continue
			}
			if err := validCommandOutcomeForKind(kind, "41111111-2222-4333-8444-555555555555", outcome(kind, "applied", domain.DeliveryCutoverSuccessCode(other))); err == nil {
				t.Fatalf("%s accepted %s success code", kind, other)
			}
		}
		bad := []domain.ProbeCommandOutcome{
			outcome(kind, "applied", ""),
			outcome(kind, "applied", deliveryPreparedCode),
			outcome(kind, "already_resolved", domain.DeliveryCutoverSuccessCode(kind)),
			outcome(kind, "applied", domain.DeliveryCutoverSuccessCode(kind)),
			outcome(kind, "rejected", domain.DeliveryCutoverSuccessCode(kind)),
			outcome(kind, "rejected", ""),
			outcome(kind, "expired", domain.DeliveryCutoverSuccessCode(kind)),
			outcome(kind, "pending", domain.DeliveryCutoverSuccessCode(kind)),
		}
		bad[3].CredentialVersion = 2 // a cutover receipt can never carry details
		for index, o := range bad {
			if err := validCommandOutcomeForKind(kind, "41111111-2222-4333-8444-555555555555", o); err == nil {
				t.Fatalf("%s accepted invalid outcome %d: %+v", kind, index, o)
			}
		}
		if err := validCommandOutcomeForKind(kind, "51111111-2222-4333-8444-555555555555", outcome(kind, "applied", domain.DeliveryCutoverSuccessCode(kind))); err == nil {
			t.Fatal("outcome identity mismatch accepted")
		}
		if err := validCommandOutcomeForKind(kind, "41111111-2222-4333-8444-555555555555", outcome(kind, "rejected", "busy_source")); err != nil {
			t.Fatalf("%s rejected refusal refused: %v", kind, err)
		}
		if err := validCommandOutcomeForKind(kind, "41111111-2222-4333-8444-555555555555", outcome(kind, "expired", "")); err != nil {
			t.Fatalf("%s expired outcome refused: %v", kind, err)
		}
	}
	// Existing kinds keep their historical shapes: a preparation receipt claims
	// preparation only, and a cutover code never rides an existing kind's result.
	if err := validCommandOutcomeForKind(CommandDeliveryPrepare, "41111111-2222-4333-8444-555555555555", outcome(CommandDeliveryPrepare, "applied", deliveryPreparedCode)); err != nil {
		t.Fatal("preparation success code refused", err)
	}
	for _, kind := range []string{CommandDeliveryPrepare, CommandAlertAck, CommandCredentialActivate} {
		if err := validCommandOutcomeForKind(kind, "41111111-2222-4333-8444-555555555555", outcome(kind, "applied", domain.DeliveryAppliedCode)); err == nil {
			t.Fatalf("%s accepted a cutover success code", kind)
		}
	}
}

// TestCommandResultCutoverCodesTransport proves the mandated kind-specific
// success codes survive the generic receipt decode while pairing rules stay
// closed: coded receipts never carry details and unknown codes never decode.
func TestCommandResultCutoverCodesTransport(t *testing.T) {
	at := Timestamp(time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC))
	for _, code := range []string{domain.DeliveryQuiescedCode, domain.DeliveryAppliedCode, domain.DeliveryResumedCode} {
		result := CommandResult{CommandID: "41111111-2222-4333-8444-555555555555", Status: "applied", AppliedAt: &at, Code: &code, Message: "cutover"}
		frame, err := encodeFrame("command.result", Decimal(1), result)
		if err != nil {
			t.Fatal(err)
		}
		_, got, err := DecodeCommandResult(frame)
		if err != nil || got.Code == nil || *got.Code != code || got.Details != nil {
			t.Fatalf("cutover receipt changed: %+v %v", got, err)
		}
		result.Details = CredentialPrepareDetails{CredentialVersion: Decimal(2)}
		if frame, err = encodeFrame("command.result", Decimal(1), result); err != nil {
			t.Fatal(err)
		}
		if _, _, err := DecodeCommandResult(frame); err == nil {
			t.Fatalf("coded receipt %s carried details", code)
		}
		result.Details = nil
		result.AppliedAt = nil
		if frame, err = encodeFrame("command.result", Decimal(1), result); err != nil {
			t.Fatal(err)
		}
		if _, _, err := DecodeCommandResult(frame); err == nil {
			t.Fatalf("success %s without applied_at decoded", code)
		}
	}
	unknown := "delivery_patched"
	result := CommandResult{CommandID: "41111111-2222-4333-8444-555555555555", Status: "applied", AppliedAt: &at, Code: &unknown, Message: "cutover"}
	frame, err := encodeFrame("command.result", Decimal(1), result)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := DecodeCommandResult(frame); err == nil {
		t.Fatal("unknown success code decoded")
	}
}
