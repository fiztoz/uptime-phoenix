package bootstrap

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func runProbeAdminReset(ctx context.Context, resets ports.ProbeStreamResetRepository, command, hubID, probeID, resetID, previousStreamID, streamID, receiptFile string, out, stderr io.Writer) int {
	fail := func(message string) int { _, _ = io.WriteString(stderr, message+"\n"); return 1 }
	if !domain.ValidHubID(resetID) {
		return fail("A canonical reset ID is required; retain it and the exact stream IDs for retries")
	}
	codec := probe.StreamResetCodec{}
	var operation *domain.ProbeStreamResetOperation
	var err error
	switch command {
	case "prepare-reset":
		operation, err = resets.PrepareStreamReset(ctx, domain.ProbeStreamResetIssue{ResetID: resetID, HubID: hubID, ProbeID: probeID, PreviousStreamID: previousStreamID, StreamID: streamID})
		if err != nil {
			return fail("Reset preparation failed; check identities, active enrollment, unresolved rotations and existing resets")
		}
		data, err := codec.EncodeStreamResetPlan(ctx, operation.Plan)
		if err != nil {
			return fail("Reset plan encoding failed; retry the same reset ID and streams")
		}
		if _, err := out.Write(append(data, '\n')); err != nil {
			return 1
		}
		return 0
	case "activate-reset":
		data, err := readPrivateProbeInput(receiptFile, 8192)
		if err != nil {
			return fail("Source receipt must be a private regular file of at most 8192 bytes")
		}
		receipt, err := codec.DecodeStreamResetReceipt(ctx, data)
		if err != nil || receipt.Plan.ResetID != resetID || receipt.Plan.HubID != hubID || receipt.Plan.ProbeID != probeID {
			return fail("Source receipt does not match this reset, hub and probe")
		}
		operation, err = resets.ActivateStreamReset(ctx, receipt)
		if err != nil {
			return fail("Reset activation failed; preserve source evidence and retry the original matching receipt")
		}
	case "reset-status":
		operation, err = resets.GetStreamReset(ctx, hubID, probeID, resetID)
		if err != nil {
			return fail("Reset operation is unavailable for this hub and probe")
		}
	default:
		return fail("Unknown stream reset operation")
	}
	result := struct {
		Plan        probe.StreamResetPlan     `json:"plan"`
		State       string                    `json:"state"`
		Source      *probe.StreamResetReceipt `json:"source,omitempty"`
		ActivatedAt *time.Time                `json:"activated_at,omitempty"`
		ConfirmedAt *time.Time                `json:"confirmed_at,omitempty"`
	}{Plan: probe.StreamResetPlanView(operation.Plan), State: operation.State, ActivatedAt: operation.ActivatedAt, ConfirmedAt: operation.ConfirmedAt}
	if operation.Source != nil {
		v := probe.StreamResetReceiptView(*operation.Source)
		result.Source = &v
	}
	if err := json.NewEncoder(out).Encode(result); err != nil {
		return 1
	}
	return 0
}
