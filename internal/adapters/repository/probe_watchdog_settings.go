package repository

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"slices"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type probeWatchdogSettingsModel struct {
	bun.BaseModel       `bun:"table:probe_watchdog_settings,alias:pws"`
	ProbeID             string `bun:"probe_id,pk"`
	Revision            int64
	Enabled             bool
	LostAfterSeconds    int32
	RecoverAfterSeconds int32
	ResendInterval      int32
}

type probeWatchdogNotificationModel struct {
	bun.BaseModel  `bun:"table:probe_watchdog_notifications,alias:pwn"`
	ProbeID        string `bun:"probe_id,pk"`
	NotificationID int64  `bun:"notification_id,pk"`
}

// ProbeWatchdogSettingsStore saves complete operator intent on either hub engine.
// Saving does not imply preparation, application, arming or notification delivery.
type ProbeWatchdogSettingsStore struct{ db *bun.DB }

// NewProbeWatchdogSettingsStore creates the local-operator settings adapter.
func NewProbeWatchdogSettingsStore(db *bun.DB) *ProbeWatchdogSettingsStore {
	return &ProbeWatchdogSettingsStore{db: db}
}

// Get returns revision-zero disabled defaults for a registered remote probe with
// no saved settings. The registration and channel membership are one DB snapshot.
func (r *ProbeWatchdogSettingsStore) Get(ctx context.Context, probeID string) (domain.ProbeWatchdogSettings, error) {
	var out domain.ProbeWatchdogSettings
	if r == nil || r.db == nil || !domain.ValidHubID(probeID) {
		return out, domain.ErrValidation
	}
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		if err := requireRemoteWatchdogRegistration(ctx, tx, probeID, false); err != nil {
			return err
		}
		var err error
		out, err = readProbeWatchdogSettings(ctx, tx, probeID)
		return err
	})
	return out, probeRegistryError(err)
}

// Replace atomically compares the settings revision and replaces all channel
// links. A same-value retry at the current revision is a no-op. Channel deletion
// removes its link by FK and will change the complete configuration hash.
func (r *ProbeWatchdogSettingsStore) Replace(ctx context.Context, probeID string, expectedRevision int64, desired domain.ProbeWatchdogSettings) (domain.ProbeWatchdogSettings, error) {
	var out domain.ProbeWatchdogSettings
	if r == nil || r.db == nil || !domain.ValidHubID(probeID) || expectedRevision < 0 || domain.ValidateProbeWatchdogSettings(desired) != nil {
		return out, domain.ErrValidation
	}
	desired.NotificationIDs = append([]int64{}, desired.NotificationIDs...)
	slices.Sort(desired.NotificationIDs)
	err := runConfigAuthorityTx(ctx, r.db, func(ctx context.Context, tx bun.Tx) error {
		// Use the same registration-first ordering as source preparation/runtime
		// transactions. The lock also serializes first-write contenders.
		if err := requireRemoteWatchdogRegistration(ctx, tx, probeID, true); err != nil {
			return err
		}
		current, err := readProbeWatchdogSettings(ctx, tx, probeID)
		if err != nil {
			return err
		}
		if current.Revision != expectedRevision {
			return ports.ErrConflict
		}
		if current.Enabled == desired.Enabled && current.LostAfterSeconds == desired.LostAfterSeconds &&
			current.RecoverAfterSeconds == desired.RecoverAfterSeconds && current.ResendInterval == desired.ResendInterval && slices.Equal(current.NotificationIDs, desired.NotificationIDs) {
			out = current
			return nil
		}
		if current.Revision == math.MaxInt64 {
			return ports.ErrConflict
		}
		// Read actual channels before writing; disabled channels are valid saved
		// dependencies. The link FK arbitrates a concurrent channel deletion.
		count, err := tx.NewSelect().Model((*NotificationModel)(nil)).Where("id IN (?)", bun.List(desired.NotificationIDs)).Count(ctx)
		if err != nil {
			return err
		}
		if count != len(desired.NotificationIDs) {
			return domain.ErrValidation
		}
		m := &probeWatchdogSettingsModel{ProbeID: probeID, Revision: current.Revision + 1, Enabled: desired.Enabled,
			LostAfterSeconds: desired.LostAfterSeconds, RecoverAfterSeconds: desired.RecoverAfterSeconds, ResendInterval: desired.ResendInterval}
		if current.Revision == 0 {
			_, err = tx.NewInsert().Model(m).Exec(ctx)
		} else {
			_, err = tx.NewUpdate().Model(m).WherePK().Exec(ctx)
		}
		if err != nil {
			return err
		}
		if _, err := tx.NewDelete().Model((*probeWatchdogNotificationModel)(nil)).Where("probe_id = ?", probeID).Exec(ctx); err != nil {
			return err
		}
		for _, id := range desired.NotificationIDs {
			if _, err := tx.NewInsert().Model(&probeWatchdogNotificationModel{ProbeID: probeID, NotificationID: id}).Exec(ctx); err != nil {
				return err
			}
		}
		desired.Revision = m.Revision
		out = desired
		return nil
	})
	if err != nil {
		return domain.ProbeWatchdogSettings{}, probeRegistryError(err)
	}
	return out, nil
}

func requireRemoteWatchdogRegistration(ctx context.Context, tx bun.Tx, probeID string, enabledOnly bool) error {
	if _, err := tx.NewUpdate().Table("probes").Set("id = id").Where("id = ?", probeID).Exec(ctx); err != nil {
		return err
	}
	m := new(probeRegistrationModel)
	if err := tx.NewSelect().Model(m).Where("id = ?", probeID).Scan(ctx); err != nil {
		return err
	}
	if m.Kind != domain.ProbeKindRemote {
		return domain.ErrValidation
	}
	if enabledOnly && !m.Enabled {
		return ports.ErrConflict
	}
	return nil
}

func readProbeWatchdogSettings(ctx context.Context, tx bun.Tx, probeID string) (domain.ProbeWatchdogSettings, error) {
	out := domain.DefaultProbeWatchdogSettings()
	m := new(probeWatchdogSettingsModel)
	if err := tx.NewSelect().Model(m).Where("probe_id = ?", probeID).Scan(ctx); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return out, nil
		}
		return out, err
	}
	links, err := configSourceRows[probeWatchdogNotificationModel](ctx, tx.NewSelect().Where("probe_id = ?", probeID).Order("notification_id ASC"), configSourceDependencies)
	if err != nil {
		return out, err
	}
	out.Revision, out.Enabled = m.Revision, m.Enabled
	out.LostAfterSeconds, out.RecoverAfterSeconds, out.ResendInterval = m.LostAfterSeconds, m.RecoverAfterSeconds, m.ResendInterval
	for _, link := range links {
		out.NotificationIDs = append(out.NotificationIDs, link.NotificationID)
	}
	return out, domain.ValidateProbeWatchdogSettings(out)
}
