// Package edge implements the standalone probe's private, durable SQLite store.
package edge

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"github.com/uptrace/bun/migrate"
	_ "modernc.org/sqlite" // Register the CGO-free edge driver.

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const applicationID = 0x50485845 // PHXE; rejects an accidentally supplied hub DB.

//go:embed migrations/*.sql
var migrations embed.FS

// Store holds one SQLite connection. The composition root must hold the runtime
// identity's exclusive directory lock from before Open until after Close.
type Store struct {
	db        *bun.DB
	telemetry ports.EdgeTelemetryEncoder
}

// Option supplies an optional execution dependency before the store is published.
type Option func(*Store) error

// WithTelemetryEncoder enables atomic source recording with the real wire codec.
// A store without an encoder can manage identity/config but cannot record checks.
func WithTelemetryEncoder(encoder ports.EdgeTelemetryEncoder) Option {
	return func(s *Store) error {
		if encoder == nil {
			return domain.ErrValidation
		}
		s.telemetry = encoder
		return nil
	}
}

var (
	_ ports.EdgeIdentityRepository   = (*Store)(nil)
	_ ports.EdgeEnrollmentRepository = (*Store)(nil)
	_ ports.EdgeConfigRepository     = (*Store)(nil)
	_ ports.EdgeReplayRepository     = (*Store)(nil)
	// ErrStorage is deliberately redacted; SQL diagnostics can contain input data.
	ErrStorage = errors.New("edge storage operation failed")
)

// Open initializes or reopens the private edge database and checks file identity.
// It never initializes TLS identity or substitutes a new stream on mismatch.
func Open(ctx context.Context, dataDir string, identity domain.EdgeIdentity, options ...Option) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !domain.ValidEdgeIdentity(identity) || identity.HubID != "" || identity.LastCreatedSeq != 0 || identity.CommittedSeq != 0 || identity.ConnectionGeneration != 0 || identity.ConfigRevision != 0 {
		return nil, domain.ErrValidation
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, domain.ErrValidation
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("edge data directory must be a private mode-0700 directory")
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, ErrStorage
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"edge.db", "edge.db-wal", "edge.db-shm", "edge.db-journal"} {
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return nil, errors.New("edge database files must be regular mode-0600 files")
		}
	}
	f, err := root.OpenFile("edge.db", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		if err = f.Close(); err != nil {
			return nil, ErrStorage
		}
	} else if !errors.Is(err, fs.ErrExist) {
		return nil, ErrStorage
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(abs, "edge.db")}
	q := u.Query()
	for _, pragma := range []string{"foreign_keys(1)", "journal_mode(WAL)", "busy_timeout(5000)", "synchronous(FULL)"} {
		q.Add("_pragma", pragma)
	}
	u.RawQuery = q.Encode()
	sqldb, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, ErrStorage
	}
	sqldb.SetMaxOpenConns(1)
	sqldb.SetMaxIdleConns(1)
	s := &Store{db: bun.NewDB(sqldb, sqlitedialect.New())}
	for _, option := range options {
		if option == nil {
			_ = s.Close()
			return nil, domain.ErrValidation
		}
		if err := option(s); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	if err := s.initialize(ctx, identity); err != nil {
		_ = s.Close()
		return nil, storageError(ctx, err)
	}
	return s, nil
}

// Close releases database handles after scheduling and session writers stop.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) initialize(ctx context.Context, identity domain.EdgeIdentity) error {
	var appID int
	if err := s.db.NewRaw("PRAGMA application_id").Scan(ctx, &appID); err != nil {
		return err
	}
	if appID == 0 {
		var tables int
		if err := s.db.NewRaw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'").Scan(ctx, &tables); err != nil {
			return err
		}
		if tables != 0 {
			return ports.ErrConflict
		}
		if _, err := s.db.ExecContext(ctx, fmt.Sprintf("PRAGMA application_id = %d", applicationID)); err != nil {
			return err
		}
	} else if appID != applicationID {
		return ports.ErrConflict
	}
	ms := migrate.NewMigrations()
	if err := ms.Discover(migrations); err != nil {
		return err
	}
	m := migrate.NewMigrator(s.db, ms, migrate.WithTableName("edge_migrations"), migrate.WithLocksTableName("edge_migration_locks"), migrate.WithMarkAppliedOnSuccess(true))
	if err := m.Init(ctx); err != nil {
		return err
	}
	// The OS directory lock also covers migrations. Unlike a persisted migration
	// lock row, that lock is automatically released after a process crash.
	if _, err := m.Migrate(ctx); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO edge_identity (id, probe_id, stream_id, fingerprint) VALUES (1, ?, ?, ?) ON CONFLICT (id) DO NOTHING", identity.ProbeID, identity.StreamID, identity.Fingerprint)
	if err != nil {
		return err
	}
	stored, err := s.ReadIdentity(ctx)
	if err != nil {
		return err
	}
	if stored.ProbeID != identity.ProbeID || stored.StreamID != identity.StreamID || stored.Fingerprint != identity.Fingerprint {
		return ports.ErrConflict
	}
	return nil
}

// ReadIdentity returns durable counters without reconstructing them from history.
func (s *Store) ReadIdentity(ctx context.Context) (domain.EdgeIdentity, error) {
	return readIdentity(ctx, s.db)
}

func readIdentity(ctx context.Context, db bun.IDB) (domain.EdgeIdentity, error) {
	var row domain.EdgeIdentity
	err := db.NewRaw("SELECT probe_id, stream_id, fingerprint, hub_id, last_created_seq, committed_seq, connection_generation, config_revision FROM edge_identity WHERE id = 1").Scan(ctx, &row)
	if err != nil {
		return domain.EdgeIdentity{}, storageError(ctx, err)
	}
	if !domain.ValidEdgeIdentity(row) {
		return domain.EdgeIdentity{}, ErrStorage
	}
	return row, nil
}

func (s *Store) write(ctx context.Context, fn func(context.Context, bun.Tx, domain.EdgeIdentity) error) error {
	err := s.db.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		// Acquire the SQLite writer before any eligibility read.
		if _, err := tx.ExecContext(ctx, "UPDATE edge_identity SET id = id WHERE id = 1"); err != nil {
			return err
		}
		i, err := readIdentity(ctx, tx)
		if err != nil {
			return err
		}
		return fn(ctx, tx, i)
	})
	return storageError(ctx, err)
}

// AcceptConnectionGeneration persists the highest authenticated hub fence.
func (s *Store) AcceptConnectionGeneration(ctx context.Context, hubID string, generation int64) error {
	if !domain.ValidHubID(hubID) || generation <= 0 {
		return domain.ErrValidation
	}
	return s.write(ctx, func(ctx context.Context, tx bun.Tx, i domain.EdgeIdentity) error {
		if i.HubID != hubID || generation <= i.ConnectionGeneration {
			return ports.ErrConflict
		}
		_, err := tx.ExecContext(ctx, "UPDATE edge_identity SET connection_generation = ? WHERE id = 1", generation)
		return err
	})
}

func storageError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ports.ErrNotFound
	}
	for _, sentinel := range []error{ports.ErrNotFound, ports.ErrConflict, ports.ErrStaleLocalState, domain.ErrValidation, ErrQueueFull} {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	return ErrStorage
}
