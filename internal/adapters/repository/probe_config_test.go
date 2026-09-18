package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestPreparedProbeConfigContract(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for name, test := range map[string]func(*testing.T, probeRegistryFixture){
				"RoundTripRotation":       testPreparedConfigRoundTrip,
				"ConcurrentWriters":       testPreparedConfigConcurrency,
				"RollbackMigrationBounds": testPreparedConfigRollback,
			} {
				t.Run(name, func(t *testing.T) { test(t, newProbeRegistryFixture(t, engine)) })
			}
		})
	}
}

func configRepo(f probeRegistryFixture, db *bun.DB) ports.ProbeConfigRepository {
	if f.engine == "sqlite" {
		return sqlite.NewProbeConfigRepo(db)
	}
	return mariadb.NewProbeConfigRepo(db)
}

func protectedConfigService(t *testing.T, r ports.ProbeConfigRepository) *services.ProbeConfigService {
	t.Helper()
	protector, err := auth.NewProbeConfigProtector(bytes.Repeat([]byte{37}, 32))
	if err != nil {
		t.Fatal(err)
	}
	return services.NewProbeConfigService(r, probe.ConfigInspector{}, protector)
}

func configDocument(t *testing.T, probeID string, revision int64, secret string) ([]byte, domain.ProbeConfigTarget) {
	t.Helper()
	fixture, err := os.ReadFile("../probe/testdata/v1/valid/config-snapshot-http.json")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := probe.DecodeConfigSnapshot(fixture)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.ProbeID, snapshot.Revision = probeID, probe.Decimal(revision)
	snapshot.CreatedAt = probe.Timestamp(time.Date(2026, 9, 18, 1, 2, 3, 123456789, time.UTC))
	for i := range snapshot.NotificationChannels {
		snapshot.NotificationChannels[i].Version = probe.Decimal(revision)
	}
	for i := range snapshot.NotificationTemplates {
		snapshot.NotificationTemplates[i].Version = probe.Decimal(revision)
	}
	for i := range snapshot.ProxyBindings {
		snapshot.ProxyBindings[i].Version = probe.Decimal(revision)
	}
	for i := range snapshot.EscalationPolicies {
		snapshot.EscalationPolicies[i].Version = probe.Decimal(revision)
	}
	snapshot.NotificationChannels[0].Config, err = json.Marshal(map[string]string{"url": "https://example.test/hook", "token": secret})
	if err != nil {
		t.Fatal(err)
	}
	if probeID == "local" {
		snapshot.NotificationChannels[0].IncludeAckURL = true
	}
	doc, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return doc, domain.ProbeConfigTarget{HubID: snapshot.HubID, ProbeID: probeID}
}

func reopenConfigDB(t *testing.T, f probeRegistryFixture) *bun.DB {
	t.Helper()
	var db *bun.DB
	var err error
	if f.engine == "sqlite" {
		db, err = sqlite.NewDB(f.dsn)
	} else {
		db, err = mariadb.NewDB(f.dsn)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func testPreparedConfigRoundTrip(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	r := configRepo(f, f.db)
	s := protectedConfigService(t, r)
	doc, target := configDocument(t, "local", 1, "private-before-rotation")
	meta, err := s.Prepare(ctx, target, doc, 0)
	if err != nil || meta.Revision != 1 || meta.CreatedAt.Location() != time.UTC || meta.CreatedAt.Nanosecond() != 123456000 {
		t.Fatalf("prepare metadata: %+v %v", meta, err)
	}
	stored, err := r.Latest(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored.ProtectedPayload, []byte("private-before-rotation")) || bytes.Contains(stored.ProtectedPayload, []byte("fixture-secret")) || stored.StoredAt.Location() != time.UTC {
		t.Fatal("stored plaintext or non-UTC time")
	}
	var payload []byte
	if err := f.db.NewSelect().Table("probe_config_snapshots").Column("protected_payload").Where("probe_id = ?", "local").Scan(ctx, &payload); err != nil || !bytes.Equal(payload, stored.ProtectedPayload) {
		t.Fatal("database payload differs")
	}
	// Fresh encryption on a retry must retain the original durable bytes and receipt.
	if _, err := s.Prepare(ctx, target, doc, 0); err != nil {
		t.Fatal(err)
	}
	retry, err := r.Latest(ctx, "local")
	if err != nil || !reflect.DeepEqual(stored, retry) {
		t.Fatal("retry replaced immutable ciphertext/time")
	}
	restarted := protectedConfigService(t, configRepo(f, reopenConfigDB(t, f)))
	plain, got, err := restarted.Read(ctx, target, 1)
	if err != nil || !bytes.Equal(plain, doc) || !domain.SameProbeConfigMetadata(got, meta) {
		t.Fatal("restart lost exact bytes or metadata")
	}
	next, _ := configDocument(t, "local", 2, "private-after-rotation")
	if _, err := s.Prepare(ctx, target, next, 1); err != nil {
		t.Fatal(err)
	}
	plain, got, err = s.Read(ctx, target, 0)
	if err != nil || got.Revision != 2 || !bytes.Equal(plain, next) {
		t.Fatal("latest returned old credentials")
	}
	plain, _, err = s.Read(ctx, target, 1)
	if err != nil || !bytes.Equal(plain, doc) {
		t.Fatal("explicit historical read lost immutable document")
	}
	if _, err := s.Prepare(ctx, target, doc, 0); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("older snapshot became latest")
	}
	conflict, _ := configDocument(t, "local", 2, "conflicting-same-revision")
	if _, err := s.Prepare(ctx, target, conflict, 2); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("same revision replaced credentials")
	}
	wrong := target
	wrong.HubID = probeRegistryID2
	if plain, _, err := s.Read(ctx, wrong, 2); !errors.Is(err, ports.ErrConflict) || plain != nil {
		t.Fatal("other authority read credentials")
	}
	changedAuthority := bytes.ReplaceAll(next, []byte(target.HubID), []byte(wrong.HubID))
	if _, err := s.Prepare(ctx, wrong, changedAuthority, 2); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("authority changed through prepare")
	}
	if _, err := r.Get(ctx, probeRegistryID1, 1); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("cross-probe snapshot leak")
	}
	// Corruption fails; latest reads never resurrect a valid older credential.
	latest, err := r.Latest(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	latest.ProtectedPayload[len(latest.ProtectedPayload)-1] ^= 1
	if _, err := f.db.NewUpdate().Table("probe_config_snapshots").Set("protected_payload = ?", latest.ProtectedPayload).Where("probe_id = ? AND revision = 2", "local").Exec(ctx); err != nil {
		t.Fatal(err)
	}
	if plain, _, err := s.Read(ctx, target, 0); err == nil || plain != nil || strings.Contains(err.Error(), "private-after-rotation") {
		t.Fatal("corrupt latest returned/leaked plaintext")
	}
	for _, table := range []string{"heartbeats", "probe_incidents", "probe_delivery_intents"} {
		assertTableCount(t, f, table, 0)
	}
}

func testPreparedConfigConcurrency(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	f.remote(t, probeRegistryID1, "prepared-east")
	r := configRepo(f, f.db)
	other := configRepo(f, reopenConfigDB(t, f))
	// Concurrent initial snapshots on independent probes also exercise empty-index gaps.
	type candidate struct {
		service *services.ProbeConfigService
		doc     []byte
		target  domain.ProbeConfigTarget
	}
	candidates := make([]candidate, 0, 12)
	for _, probeID := range []string{"local", probeRegistryID1} {
		for i := range 6 {
			doc, target := configDocument(t, probeID, 1, fmt.Sprintf("candidate-%d", i))
			repo := r
			if i%2 == 1 {
				repo = other
			}
			candidates = append(candidates, candidate{protectedConfigService(t, repo), doc, target})
		}
	}
	type outcome struct {
		probeID string
		err     error
	}
	results := make(chan outcome, len(candidates))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, c := range candidates {
		wg.Go(func() {
			<-start
			_, err := c.service.Prepare(ctx, c.target, c.doc, 0)
			results <- outcome{c.target.ProbeID, err}
		})
	}
	close(start)
	wg.Wait()
	close(results)
	winners := map[string]int{}
	for result := range results {
		if result.err == nil {
			winners[result.probeID]++
		} else if !errors.Is(result.err, ports.ErrConflict) {
			t.Fatal(result.err)
		}
	}
	if winners["local"] != 1 || winners[probeRegistryID1] != 1 {
		t.Fatalf("concurrent revision winners: %v", winners)
	}
	assertTableCount(t, f, "probe_config_snapshots", 2)
	// An independent connection with a stale expected revision cannot advance state.
	doc, target := configDocument(t, "local", 2, "newer")
	s := protectedConfigService(t, other)
	if _, err := s.Prepare(ctx, target, doc, 0); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("stale preparation accepted")
	}
	if _, err := s.Prepare(ctx, target, doc, 1); err != nil {
		t.Fatal(err)
	}
}

func testPreparedConfigRollback(t *testing.T, f probeRegistryFixture) {
	t.Helper()
	ctx := context.Background()
	for range 2 {
		if err := runEngineMigration(t, f.db, f.engine, "047_probe_config_snapshots", "down"); err != nil {
			t.Fatal(err)
		}
		if err := runEngineMigration(t, f.db, f.engine, "047_probe_config_snapshots", "up"); err != nil {
			t.Fatal(err)
		}
	}
	r := configRepo(f, f.db)
	s := protectedConfigService(t, r)
	doc, target := configDocument(t, "local", 1, "rollback-secret")
	clear := injectOutboxFailure(t, f, "probe_config_snapshots", "INSERT")
	meta, err := s.Prepare(ctx, target, doc, 0)
	clear()
	if err == nil || meta.Revision != 0 {
		t.Fatal("failed insert returned a prepared revision")
	}
	assertTableCount(t, f, "probe_config_snapshots", 0)
	if _, err := s.Prepare(ctx, target, doc, 0); err != nil {
		t.Fatal(err)
	}
	if err := runEngineMigration(t, f.db, f.engine, "047_probe_config_snapshots", "down"); err == nil {
		t.Fatal("downgrade discarded prepared configuration")
	}
	if _, err := f.db.ExecContext(ctx, "DELETE FROM probes WHERE id = ?", "local"); err == nil {
		t.Fatal("registration deletion discarded snapshot history")
	}
	if err := repository.RunMigrations(f.db.DB, f.engine); err != nil {
		t.Fatal(err)
	}
	plain, _, err := s.Read(ctx, target, 1)
	if err != nil || !bytes.Equal(plain, doc) {
		t.Fatal("startup/failed downgrade lost protected content")
	}
	missing, missingTarget := configDocument(t, probeRegistryID2, 1, "unregistered")
	if _, err := s.Prepare(ctx, missingTarget, missing, 0); !errors.Is(err, ports.ErrNotFound) {
		t.Fatal("unknown registration accepted")
	}
	for _, statement := range []string{
		"UPDATE probe_config_snapshots SET revision = 0",
		"UPDATE probe_config_snapshots SET schema_version = 2",
		"UPDATE probe_config_snapshots SET sha256 = 'bad'",
		"UPDATE probe_config_snapshots SET protected_payload = X'01'",
	} {
		if _, err := f.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("invalid snapshot schema accepted: %s", statement)
		}
	}
	stored, err := r.Latest(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*domain.ProtectedProbeConfig){
		func(s *domain.ProtectedProbeConfig) { s.ProtectedPayload = nil },
		func(s *domain.ProtectedProbeConfig) { s.SHA256 = "bad" },
		func(s *domain.ProtectedProbeConfig) { s.Revision = -1 },
	} {
		bad := *stored
		mutate(&bad)
		if _, err := r.Save(ctx, bad, 1); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid snapshot input accepted")
		}
	}
	maximum, _ := configDocument(t, "local", math.MaxInt64, "max-revision")
	if _, err := s.Prepare(ctx, target, maximum, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, target, maximum, math.MaxInt64); err != nil {
		t.Fatal("maximum revision retry failed")
	}
	if _, err := s.Prepare(ctx, target, doc, math.MaxInt64); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("exhausted revision rolled back")
	}
}
