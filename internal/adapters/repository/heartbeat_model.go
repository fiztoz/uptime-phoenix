package repository

import (
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// HeartbeatModel maps the heartbeats table.
type HeartbeatModel struct {
	bun.BaseModel `bun:"table:heartbeats"`

	ID                   int64      `bun:"id,pk,autoincrement"`
	MonitorID            int64      `bun:"monitor_id,notnull"`
	ProbeID              string     `bun:"probe_id,notnull,default:'local'"`
	Status               int        `bun:"status,notnull"`
	Time                 time.Time  `bun:"time,notnull"`
	Msg                  string     `bun:"msg"`
	Ping                 *int       `bun:"ping"`
	Duration             int        `bun:"duration,notnull,default:0"`
	Important            bool       `bun:"important,notnull,default:false"`
	DownCount            int        `bun:"down_count,notnull,default:0"`
	StreamID             *string    `bun:"stream_id"`
	SourceSeq            *int64     `bun:"source_seq"`
	AssignmentGeneration int64      `bun:"assignment_generation,notnull,default:1"`
	ReceivedAt           *time.Time `bun:"received_at"`
	ConfigRevision       int64      `bun:"config_revision,notnull,default:0"`
}

// ToDomain converts a HeartbeatModel to a domain.Heartbeat.
func (m *HeartbeatModel) ToDomain() *domain.Heartbeat {
	ping := 0
	if m.Ping != nil {
		ping = *m.Ping
	}
	streamID := ""
	if m.StreamID != nil {
		streamID = *m.StreamID
	}
	var sourceSeq int64
	if m.SourceSeq != nil {
		sourceSeq = *m.SourceSeq
	}
	var receivedAt time.Time
	if m.ReceivedAt != nil {
		receivedAt = *m.ReceivedAt
	}
	generation := m.AssignmentGeneration
	if generation < 1 {
		generation = 1
	}
	return &domain.Heartbeat{
		ID:                   m.ID,
		MonitorID:            m.MonitorID,
		Status:               domain.Status(m.Status),
		Time:                 m.Time,
		Msg:                  m.Msg,
		Ping:                 ping,
		Duration:             m.Duration,
		Important:            m.Important,
		DownCount:            m.DownCount,
		ProbeID:              domain.NormalizeProbeID(m.ProbeID),
		StreamID:             streamID,
		SourceSeq:            sourceSeq,
		AssignmentGeneration: generation,
		ReceivedAt:           receivedAt,
		ConfigRevision:       m.ConfigRevision,
	}
}

// HeartbeatModelFromDomain converts a domain.Heartbeat to a HeartbeatModel.
func HeartbeatModelFromDomain(h *domain.Heartbeat) *HeartbeatModel {
	ping := h.Ping
	generation := h.AssignmentGeneration
	if generation < 1 {
		generation = 1
	}
	m := &HeartbeatModel{
		ID:                   h.ID,
		MonitorID:            h.MonitorID,
		ProbeID:              domain.NormalizeProbeID(h.ProbeID),
		Status:               int(h.Status),
		Time:                 h.Time,
		Msg:                  h.Msg,
		Ping:                 &ping,
		Duration:             h.Duration,
		Important:            h.Important,
		DownCount:            h.DownCount,
		AssignmentGeneration: generation,
		ConfigRevision:       h.ConfigRevision,
	}
	if h.StreamID != "" {
		streamID := h.StreamID
		m.StreamID = &streamID
	}
	if h.SourceSeq != 0 {
		sourceSeq := h.SourceSeq
		m.SourceSeq = &sourceSeq
	}
	if !h.ReceivedAt.IsZero() {
		receivedAt := h.ReceivedAt.UTC()
		m.ReceivedAt = &receivedAt
	}
	return m
}

// AggregateModel maps heartbeat_1m, heartbeat_1h, and heartbeat_1d tables.
// The same Go struct is used for all three; table name is overridden per query.
type AggregateModel struct {
	bun.BaseModel `bun:"table:heartbeat_1m"`

	ID           int64     `bun:"id,pk,autoincrement"`
	MonitorID    int64     `bun:"monitor_id,notnull"`
	ProbeID      string    `bun:"probe_id,notnull,default:'local'"`
	Bucket       time.Time `bun:"bucket,notnull"`
	UpCount      int       `bun:"up_count,notnull,default:0"`
	DownCount    int       `bun:"down_count,notnull,default:0"`
	PendingCount int       `bun:"pending_count,notnull,default:0"`
	MaintCount   int       `bun:"maint_count,notnull,default:0"`
	UnknownCount int       `bun:"unknown_count,notnull,default:0"`
	AvgPing      float64   `bun:"avg_ping"`
	MinPing      *int      `bun:"min_ping"`
	MaxPing      *int      `bun:"max_ping"`
	PingCount    int       `bun:"ping_count,notnull,default:0"`
	TotalChecks  int       `bun:"total_checks,notnull,default:0"`
}

// Aggregate1mFromDomain converts a ports.Aggregate1m to an AggregateModel.
func Aggregate1mFromDomain(a *ports.Aggregate1m) *AggregateModel {
	return &AggregateModel{
		MonitorID:    a.MonitorID,
		ProbeID:      domain.NormalizeProbeID(a.ProbeID),
		Bucket:       a.Bucket,
		UpCount:      a.UpCount,
		DownCount:    a.DownCount,
		PendingCount: a.PendingCount,
		MaintCount:   a.MaintCount,
		UnknownCount: a.UnknownCount,
		AvgPing:      a.AvgPing,
		MinPing:      intPtrOrNil(a.MinPing),
		MaxPing:      intPtrOrNil(a.MaxPing),
		PingCount:    a.PingCount,
		TotalChecks:  a.TotalChecks,
	}
}

// Aggregate1hFromDomain converts a ports.Aggregate1h to an AggregateModel.
func Aggregate1hFromDomain(a *ports.Aggregate1h) *AggregateModel {
	return &AggregateModel{
		MonitorID:    a.MonitorID,
		ProbeID:      domain.NormalizeProbeID(a.ProbeID),
		Bucket:       a.Bucket,
		UpCount:      a.UpCount,
		DownCount:    a.DownCount,
		PendingCount: a.PendingCount,
		MaintCount:   a.MaintCount,
		UnknownCount: a.UnknownCount,
		AvgPing:      a.AvgPing,
		MinPing:      intPtrOrNil(a.MinPing),
		MaxPing:      intPtrOrNil(a.MaxPing),
		PingCount:    a.PingCount,
		TotalChecks:  a.TotalChecks,
	}
}

// Aggregate1dFromDomain converts a ports.Aggregate1d to an AggregateModel.
func Aggregate1dFromDomain(a *ports.Aggregate1d) *AggregateModel {
	return &AggregateModel{
		MonitorID:    a.MonitorID,
		ProbeID:      domain.NormalizeProbeID(a.ProbeID),
		Bucket:       a.Bucket,
		UpCount:      a.UpCount,
		DownCount:    a.DownCount,
		PendingCount: a.PendingCount,
		MaintCount:   a.MaintCount,
		UnknownCount: a.UnknownCount,
		AvgPing:      a.AvgPing,
		MinPing:      intPtrOrNil(a.MinPing),
		MaxPing:      intPtrOrNil(a.MaxPing),
		PingCount:    a.PingCount,
		TotalChecks:  a.TotalChecks,
	}
}

// ToAggregate1m converts an AggregateModel to a ports.Aggregate1m.
func (m *AggregateModel) ToAggregate1m() *ports.Aggregate1m {
	return &ports.Aggregate1m{
		MonitorID:    m.MonitorID,
		ProbeID:      domain.NormalizeProbeID(m.ProbeID),
		Bucket:       m.Bucket,
		UpCount:      m.UpCount,
		DownCount:    m.DownCount,
		PendingCount: m.PendingCount,
		MaintCount:   m.MaintCount,
		UnknownCount: m.UnknownCount,
		AvgPing:      m.AvgPing,
		MinPing:      derefInt(m.MinPing),
		MaxPing:      derefInt(m.MaxPing),
		PingCount:    m.PingCount,
		TotalChecks:  m.TotalChecks,
	}
}

// ToAggregate1h converts an AggregateModel to a ports.Aggregate1h.
func (m *AggregateModel) ToAggregate1h() *ports.Aggregate1h {
	return &ports.Aggregate1h{
		MonitorID:    m.MonitorID,
		ProbeID:      domain.NormalizeProbeID(m.ProbeID),
		Bucket:       m.Bucket,
		UpCount:      m.UpCount,
		DownCount:    m.DownCount,
		PendingCount: m.PendingCount,
		MaintCount:   m.MaintCount,
		UnknownCount: m.UnknownCount,
		AvgPing:      m.AvgPing,
		MinPing:      derefInt(m.MinPing),
		MaxPing:      derefInt(m.MaxPing),
		PingCount:    m.PingCount,
		TotalChecks:  m.TotalChecks,
	}
}

// ToAggregate1d converts an AggregateModel to a ports.Aggregate1d.
func (m *AggregateModel) ToAggregate1d() *ports.Aggregate1d {
	return &ports.Aggregate1d{
		MonitorID:    m.MonitorID,
		ProbeID:      domain.NormalizeProbeID(m.ProbeID),
		Bucket:       m.Bucket,
		UpCount:      m.UpCount,
		DownCount:    m.DownCount,
		PendingCount: m.PendingCount,
		MaintCount:   m.MaintCount,
		UnknownCount: m.UnknownCount,
		AvgPing:      m.AvgPing,
		MinPing:      derefInt(m.MinPing),
		MaxPing:      derefInt(m.MaxPing),
		PingCount:    m.PingCount,
		TotalChecks:  m.TotalChecks,
	}
}

// AggregateConflictTarget is the SQLite ON CONFLICT clause after migration 037.
const AggregateConflictTarget = "CONFLICT(monitor_id, probe_id, bucket) DO UPDATE"

func intPtrOrNil(v int) *int { return &v }

func derefInt(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
