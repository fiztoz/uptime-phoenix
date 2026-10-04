package probe

import (
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func buildReplayGapFrame(streamID string, gen Decimal, batch *domain.EdgeReplayBatch) ([]byte, error) {
	if batch == nil || batch.Gap == nil || len(batch.Items) != 0 || batch.StreamID != streamID || batch.Gap.StreamID != streamID || batch.FirstSeq != batch.Gap.FromSeq || batch.LastSeq != batch.Gap.ThroughSeq {
		return nil, domain.ErrValidation
	}
	g := batch.Gap
	frame, err := encodeFrame("telemetry.gap", gen, TelemetryGap{StreamID: streamID, FromSeq: Decimal(g.FromSeq), ThroughSeq: Decimal(g.ThroughSeq), Reason: g.Reason, ObservedFrom: Timestamp(g.ObservedFrom), ObservedThrough: Timestamp(g.ObservedThrough), AffectedMonitorIDs: g.AffectedMonitorIDs})
	if err != nil {
		return nil, err
	}
	if _, _, err := DecodeTelemetryGap(frame); err != nil {
		return nil, domain.ErrValidation
	}
	return frame, nil
}

func decodeReplayGap(data []byte, probeID string) (domain.ProbeReplayBatch, error) {
	_, g, err := DecodeTelemetryGap(data)
	if err != nil {
		return domain.ProbeReplayBatch{}, err
	}
	gap := &domain.ProbeTelemetryGap{StreamID: g.StreamID, FromSeq: int64(g.FromSeq), ThroughSeq: int64(g.ThroughSeq), Reason: g.Reason, ObservedFrom: time.Time(g.ObservedFrom).UTC(), ObservedThrough: time.Time(g.ObservedThrough).UTC(), AffectedMonitorIDs: g.AffectedMonitorIDs}
	return domain.ProbeReplayBatch{ProbeID: probeID, StreamID: g.StreamID, FirstSeq: gap.FromSeq, LastSeq: gap.ThroughSeq, Gap: gap}, nil
}
