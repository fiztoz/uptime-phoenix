package domain

import "time"

// AggregateStandbyLease fences hub-owned planning, never source or provider I/O.
type AggregateStandbyLease struct {
	MonitorID int64
	OwnerID   string
	Epoch     int64
	Until     time.Time
}

// AggregateStandbyResult describes a committed standby proposal, not send authority.
type AggregateStandbyResult struct {
	MonitorID         int64
	IncidentID        string
	TransitionVersion int64
	Action            AggregatePagingAction
	Health            MonitorHealth
}

// AggregateStandbyChannel is nonsecret exact channel planning identity.
type AggregateStandbyChannel struct {
	NotificationID int64
	RevisionSHA256 string
}
