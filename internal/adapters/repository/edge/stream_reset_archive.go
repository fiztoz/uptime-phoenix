package edge

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/uptrace/bun"
	"github.com/uptrace/bun/dialect/sqlitedialect"
	"modernc.org/sqlite"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

const (
	resetArchiveDir                 = "stream-archives"
	maxResetArchiveBytes      int64 = 1 << 30
	maxResetArchiveTotalBytes int64 = 4 << 30
	maxResetManifestBytes     int64 = 16 << 10
)

// Private per-store I/O dependencies allow deterministic storage fault tests.
// Nil functions select the real filesystem and driver implementations.
type resetArchiveIO struct {
	backup func(context.Context, string) error
	sync   func(*os.File) error
}

type resetArchiveManifest struct {
	Record json.RawMessage `json:"record"`
	Proof  []byte          `json:"proof"`
}

func (s *Store) syncResetFile(f *os.File) error {
	if s.archiveIO.sync != nil {
		return s.archiveIO.sync(f)
	}
	return f.Sync()
}

func (s *Store) openArchiveRoot() (*os.Root, error) {
	root, err := os.OpenRoot(s.dataDir)
	if err != nil {
		return nil, ErrStorage
	}
	defer func() { _ = root.Close() }()
	if err := root.Mkdir(resetArchiveDir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, ErrStorage
	}
	info, err := root.Lstat(resetArchiveDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, ErrStorage
	}
	dir, err := root.Open(".")
	if err != nil {
		return nil, ErrStorage
	}
	err = s.syncResetFile(dir)
	closeErr := dir.Close()
	if err != nil || closeErr != nil {
		return nil, ErrStorage
	}
	archive, err := root.OpenRoot(resetArchiveDir)
	if err != nil {
		return nil, ErrStorage
	}
	return archive, nil
}

func archiveFile(root *os.Root, name string, maximum int64) (*os.File, error) {
	info, err := root.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() < 1 || info.Size() > maximum {
		return nil, ErrStorage
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, ErrStorage
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = f.Close()
		return nil, ErrStorage
	}
	return f, nil
}

func archiveLayout(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return ErrStorage
	}
	defer func() { _ = dir.Close() }()
	info, err := dir.Stat()
	if err != nil || info.Mode().Perm() != 0700 {
		return ErrStorage
	}
	entries, err := dir.ReadDir(3)
	if err != nil && !errors.Is(err, io.EOF) || len(entries) != 2 {
		return ErrStorage
	}
	for _, entry := range entries {
		limit := maxResetArchiveBytes
		switch entry.Name() {
		case "edge.db":
		case "manifest.json":
			limit = maxResetManifestBytes
		default:
			return ErrStorage
		}
		f, err := archiveFile(root, entry.Name(), limit)
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return ErrStorage
		}
	}
	return nil
}

func archiveDigest(ctx context.Context, root *os.Root, name string) (int64, string, error) {
	f, err := archiveFile(root, name, maxResetArchiveBytes)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return 0, "", err
		}
		n, err := f.Read(buffer)
		total += int64(n)
		if total > maxResetArchiveBytes {
			return 0, "", ErrQueueFull
		}
		_, _ = h.Write(buffer[:n])
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, "", ErrStorage
		}
	}
	return total, hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Store) archiveForStreamReset(ctx context.Context, pending domain.EdgeStreamResetRecord) (domain.EdgeStreamResetRecord, error) {
	root, err := s.openArchiveRoot()
	if err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	defer func() { _ = root.Close() }()
	name := pending.Plan.ResetID
	if _, err := root.Lstat(name); err == nil {
		return s.verifyResetArchive(ctx, pending)
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	staging := "." + name + ".pending"
	// Only this operation's unpublished staging directory can be removed. Its
	// authoritative evidence still exists in the fenced current database.
	if info, err := root.Lstat(staging); err == nil {
		if !info.IsDir() || info.Mode().Perm() != 0700 {
			return domain.EdgeStreamResetRecord{}, ErrStorage
		}
		if err := root.RemoveAll(staging); err != nil {
			return domain.EdgeStreamResetRecord{}, ErrStorage
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	if err := s.checkArchiveCapacity(ctx, root); err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	if err := root.Mkdir(staging, 0700); err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	defer func() { _ = root.RemoveAll(staging) }()
	stage, err := root.OpenRoot(staging)
	if err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	defer func() { _ = stage.Close() }()
	f, err := stage.OpenFile("edge.db", os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	if err := f.Close(); err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	path := filepath.Join(s.dataDir, resetArchiveDir, staging, "edge.db")
	backup := s.backupResetDatabase
	if s.archiveIO.backup != nil {
		backup = s.archiveIO.backup
	}
	if err := backup(ctx, path); err != nil {
		return domain.EdgeStreamResetRecord{}, storageError(ctx, err)
	}
	if err := s.validateArchiveDatabase(ctx, path, pending); err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	r := pending
	r.State = "archived"
	r.ArchiveBytes, r.ArchiveSHA256, err = archiveDigest(ctx, stage, "edge.db")
	if err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	data, err := encodeResetRecord(r)
	if err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	proof, err := s.resetProof.SealStreamReset(ctx, r)
	if err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	manifest, err := json.Marshal(resetArchiveManifest{Record: data, Proof: proof})
	if err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	m, err := stage.OpenFile("manifest.json", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	n, writeErr := m.Write(manifest)
	syncErr := s.syncResetFile(m)
	closeErr := m.Close()
	if writeErr != nil || n != len(manifest) || syncErr != nil || closeErr != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	for _, file := range []string{"edge.db", "."} {
		f, err := stage.Open(file)
		if err != nil {
			return domain.EdgeStreamResetRecord{}, ErrStorage
		}
		err = s.syncResetFile(f)
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return domain.EdgeStreamResetRecord{}, ErrStorage
		}
	}
	if err := ctx.Err(); err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	// The private parent and OS directory lock exclude cooperating publishers.
	// A nonempty final directory is never overwritten. Rename publishes the DB
	// and authenticated manifest together; a crash exposes both or neither.
	if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return domain.EdgeStreamResetRecord{}, ports.ErrConflict
	}
	if err := root.Rename(staging, name); err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	dir, err := root.Open(".")
	if err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	err = s.syncResetFile(dir)
	closeErr = dir.Close()
	if err != nil || closeErr != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	return s.verifyResetArchive(ctx, pending)
}

func (s *Store) checkArchiveCapacity(ctx context.Context, root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return ErrStorage
	}
	defer func() { _ = dir.Close() }()
	entries, err := dir.ReadDir(maxEdgeStreamResets + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return ErrStorage
	}
	if len(entries) >= maxEdgeStreamResets {
		return ErrQueueFull
	}
	var used int64
	for _, entry := range entries {
		if !entry.IsDir() || !domain.ValidHubID(entry.Name()) {
			return ErrStorage
		}
		child, err := root.OpenRoot(entry.Name())
		if err != nil {
			return ErrStorage
		}
		if err := archiveLayout(child); err != nil {
			_ = child.Close()
			return err
		}
		f, err := archiveFile(child, "edge.db", maxResetArchiveBytes)
		if err == nil {
			info, statErr := f.Stat()
			if statErr == nil {
				used += info.Size()
			} else {
				err = statErr
			}
			_ = f.Close()
		}
		_ = child.Close()
		if err != nil {
			return ErrStorage
		}
	}
	var pages, pageSize int64
	if err := s.db.NewRaw("PRAGMA page_count").Scan(ctx, &pages); err != nil {
		return ErrStorage
	}
	if err := s.db.NewRaw("PRAGMA page_size").Scan(ctx, &pageSize); err != nil {
		return ErrStorage
	}
	if pages <= 0 || pageSize < 512 || pageSize > 65536 || pages > maxResetArchiveBytes/pageSize {
		return ErrQueueFull
	}
	if used+pages*pageSize+(int64(len(entries))+1)*maxResetManifestBytes > maxResetArchiveTotalBytes {
		return ErrQueueFull
	}
	return nil
}

func (s *Store) backupResetDatabase(ctx context.Context, path string) error {
	conn, err := s.db.DB.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	return conn.Raw(func(driverConnection any) error {
		backupSource, ok := driverConnection.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return ErrStorage
		}
		u := url.URL{Scheme: "file", Path: path}
		backup, err := backupSource.NewBackup(u.String())
		if err != nil {
			return err
		}
		finished := false
		defer func() {
			if !finished {
				_ = backup.Finish()
			}
		}()
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			more, err := backup.Step(128)
			if err != nil {
				return err
			}
			if !more {
				break
			}
		}
		destination, err := backup.Commit()
		finished = true
		if err != nil {
			return err
		}
		return destination.Close()
	})
}

func (s *Store) verifyResetArchive(ctx context.Context, expected domain.EdgeStreamResetRecord) (domain.EdgeStreamResetRecord, error) {
	root, err := s.openArchiveRoot()
	if err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	defer func() { _ = root.Close() }()
	info, err := root.Lstat(expected.Plan.ResetID)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	archive, err := root.OpenRoot(expected.Plan.ResetID)
	if err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	defer func() { _ = archive.Close() }()
	if err := archiveLayout(archive); err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	f, err := archiveFile(archive, "manifest.json", maxResetManifestBytes)
	if err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxResetManifestBytes+1))
	closeErr := f.Close()
	if err != nil || closeErr != nil || int64(len(data)) > maxResetManifestBytes {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	var manifest resetArchiveManifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&manifest); err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	r, err := decodeResetRecord(manifest.Record)
	if err != nil || r.State != "archived" {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	if err := s.resetProof.VerifyStreamReset(ctx, r, manifest.Proof); err != nil {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	prepared := r
	prepared.State, prepared.ArchiveBytes, prepared.ArchiveSHA256 = "prepared", 0, ""
	comparison := expected
	comparison.State, comparison.AppliedAt = "prepared", time.Time{}
	comparison.ArchiveBytes, comparison.ArchiveSHA256 = 0, ""
	if comparison != prepared {
		return domain.EdgeStreamResetRecord{}, ports.ErrConflict
	}
	if expected.State == "applied" && (expected.ArchiveBytes != r.ArchiveBytes || expected.ArchiveSHA256 != r.ArchiveSHA256) {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	size, hash, err := archiveDigest(ctx, archive, "edge.db")
	if err != nil || size != r.ArchiveBytes || hash != r.ArchiveSHA256 {
		return domain.EdgeStreamResetRecord{}, ErrStorage
	}
	path := filepath.Join(s.dataDir, resetArchiveDir, r.Plan.ResetID, "edge.db")
	if err := s.validateArchiveDatabase(ctx, path, prepared); err != nil {
		return domain.EdgeStreamResetRecord{}, err
	}
	// A prior attempt may have renamed successfully but failed the parent sync.
	// Visibility and a valid digest are not durable publication. Re-establish
	// all sync boundaries on reuse before the caller can clear the live epoch.
	for _, entry := range []struct {
		root *os.Root
		name string
	}{{archive, "edge.db"}, {archive, "manifest.json"}, {archive, "."}, {root, "."}} {
		f, err := entry.root.Open(entry.name)
		if err != nil {
			return domain.EdgeStreamResetRecord{}, ErrStorage
		}
		err = s.syncResetFile(f)
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			return domain.EdgeStreamResetRecord{}, ErrStorage
		}
	}
	return r, nil
}

func (s *Store) validateArchiveDatabase(ctx context.Context, path string, pending domain.EdgeStreamResetRecord) error {
	// immutable=1 forbids SQLite from creating WAL/SHM files beside the archive.
	// Reject any sidecar left by backup; the published main file must stand alone.
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			return ErrStorage
		}
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Set("mode", "ro")
	q.Set("immutable", "1")
	u.RawQuery = q.Encode()
	sqldb, err := sql.Open("sqlite", u.String())
	if err != nil {
		return ErrStorage
	}
	db := bun.NewDB(sqldb, sqlitedialect.New())
	defer func() { _ = db.Close() }()
	var integrity string
	if err := db.NewRaw("PRAGMA integrity_check(1)").Scan(ctx, &integrity); err != nil || strings.TrimSpace(integrity) != "ok" {
		return ErrStorage
	}
	var invalidLinks int
	if err := db.NewRaw("SELECT COUNT(*) FROM (SELECT 1 FROM pragma_foreign_key_check LIMIT 1)").Scan(ctx, &invalidLinks); err != nil || invalidLinks != 0 {
		return ErrStorage
	}
	var appID int
	if err := db.NewRaw("PRAGMA application_id").Scan(ctx, &appID); err != nil || appID != applicationID {
		return ErrStorage
	}
	i, err := readIdentity(ctx, db)
	if err != nil || i != pending.Source {
		return ErrStorage
	}
	var row edgeResetRow
	if err := db.NewSelect().Model(&row).Where("reset_id = ?", pending.Plan.ResetID).Scan(ctx); err != nil {
		return ErrStorage
	}
	r, err := s.authenticateResetRow(ctx, row)
	if err != nil || r != pending {
		return ErrStorage
	}
	return nil
}
