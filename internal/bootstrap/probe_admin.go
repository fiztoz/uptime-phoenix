package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/logger"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

const probeAdminUsage = `Usage: phoenix-probe-admin <register|enroll|assign|prepare|watchdog|status> [options]
Uses hub DB_ENGINE, DB_DSN, PROBE_SECRET_KEY_FILE and optional PROBE_ENDPOINT_POLICY_FILE.
register --probe-id UUID --stream-id UUID --key SLUG --name NAME [--location LOCATION] --endpoint wss://HOST/ws/probe/v1 --fingerprint SHA256
enroll --probe-id UUID --token-file PATH
assign --monitor-id ID --expected-revision N --probes UUID[,local]
prepare --probe-id UUID --expected-revision N --file PATH
watchdog --probe-id UUID --expected-revision N --enabled=true|false [--notifications ID,ID] [--lost-after-seconds 90] [--recover-after-seconds 30] [--resend-interval 0]
status --probe-id UUID
Token and complete snapshot files must be private regular files. Commands print metadata only.
Registration persists the recoverable protected runtime credential before enrollment.
Watchdog replaces saved settings; expected-revision is the settings revision reported by status, initially zero. Saving is not an applied-config receipt.
Run compatible hub workers with PROBES_ENABLED=true after enrollment. Workers synchronize supported configurations and replay retained telemetry.
Fleet UI and explicit gap/reset recovery remain later milestones.
`

// RunProbeAdmin is the explicit local-operator composition root for the M2
// engineering flow. It exposes no HTTP endpoint and never prints configuration
// plaintext or credentials. Database access is the operator's authority.
func RunProbeAdmin(ctx context.Context, cfg Config, args []string, out, stderr io.Writer) int {
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help") {
		_, _ = io.WriteString(out, probeAdminUsage)
		return 0
	}
	if len(args) == 0 || !slices.Contains([]string{"register", "enroll", "assign", "prepare", "watchdog", "status"}, args[0]) {
		_, _ = io.WriteString(stderr, probeAdminUsage)
		return 2
	}
	var probeID, streamID, key, name, location, endpoint, pin, tokenFile, documentFile, members, channelIDs string
	var monitorID, revision int64
	var enabled bool
	var lostSeconds, recoverSeconds, resendMinutes int64
	f := flag.NewFlagSet("phoenix-probe-admin", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.StringVar(&probeID, "probe-id", "", "trusted local probe ID")
	f.StringVar(&streamID, "stream-id", "", "trusted local stream ID")
	f.StringVar(&key, "key", "", "stable registration slug")
	f.StringVar(&name, "name", "", "display name")
	f.StringVar(&location, "location", "", "display location")
	f.StringVar(&channelIDs, "notifications", "", "complete watchdog channel set")
	f.BoolVar(&enabled, "enabled", false, "enable connection watchdog")
	f.Int64Var(&lostSeconds, "lost-after-seconds", 90, "application health loss interval")
	f.Int64Var(&recoverSeconds, "recover-after-seconds", 30, "stable recovery interval")
	f.Int64Var(&resendMinutes, "resend-interval", 0, "reminder interval in minutes")
	f.StringVar(&endpoint, "endpoint", "", "pinned runtime endpoint")
	f.StringVar(&pin, "fingerprint", "", "verified local certificate fingerprint")
	f.StringVar(&tokenFile, "token-file", "", "private enrollment token file")
	f.StringVar(&documentFile, "file", "", "private complete snapshot file")
	f.StringVar(&members, "probes", "", "complete desired assignment set")
	f.Int64Var(&monitorID, "monitor-id", 0, "hub monitor ID")
	f.Int64Var(&revision, "expected-revision", 0, "current revision")
	if f.Parse(args[1:]) != nil || f.NArg() != 0 || cfg.ProbeSecretKeyFile == "" || revision < 0 {
		_, _ = io.WriteString(stderr, probeAdminUsage)
		return 2
	}
	if args[0] != "assign" && (!domain.ValidHubID(probeID) || probeID == domain.LocalProbeID) {
		_, _ = io.WriteString(stderr, "A canonical remote probe ID is required\n")
		return 2
	}
	fail := func(message string) int { _, _ = fmt.Fprintln(stderr, message); return 1 }
	protector, err := auth.NewProbeConfigProtectorFromFile(ctx, cfg.ProbeSecretKeyFile)
	if err != nil {
		return fail("Hub protection key is unavailable")
	}
	policy, err := probe.LoadEndpointPolicy(cfg.ProbeEndpointPolicyFile)
	if err != nil {
		return fail("Probe endpoint policy is invalid")
	}
	db, err := openDB(cfg, logger.New("error"))
	if err != nil {
		return fail("Hub database is unavailable")
	}
	defer func() { _ = db.Close() }()
	if repository.RunMigrations(db.DB, cfg.DBEngine) != nil {
		return fail("Hub migrations failed")
	}
	repos := wireRepositories(cfg.DBEngine, db)
	installation, err := services.NewProbeInstallationService(repos.probeInstallation).InitializeOrVerify(ctx, protector, cfg.ProbeHubID)
	if err != nil {
		return fail("Hub installation verification failed")
	}
	owner, err := uuid.NewRandom()
	if err != nil {
		return fail("Connector owner unavailable")
	}
	connections := repository.NewProbeConnectorStore(db)
	configs := services.NewProbeConfigService(repos.probeConfig, probe.ConfigInspector{}, protector)
	connector, err := services.NewProbeConnectorService(connections, connections, connections, protector, configs, probe.NewHubTransport(policy), installation.HubID, owner.String(), func(f int, h time.Duration) time.Duration { return probe.ReconnectDelay(f, h, rand.Float64()) })
	if err != nil {
		return fail("Connector initialization failed")
	}
	registry := repository.NewProbeRegistryStore(db)
	type assignmentView struct {
		ProbeID    string        `json:"probe_id"`
		Generation probe.Decimal `json:"generation"`
	}
	result := struct {
		HubID           string                  `json:"hub_id"`
		ProbeID         string                  `json:"probe_id,omitempty"`
		State           string                  `json:"state"`
		Revision        probe.Decimal           `json:"revision,omitempty"`
		Assignments     []assignmentView        `json:"assignments,omitempty"`
		AppliedRevision *probe.Decimal          `json:"applied_revision,omitempty"`
		SyncPending     *bool                   `json:"sync_pending,omitempty"`
		Watchdog        *probeAdminWatchdogView `json:"watchdog,omitempty"`
	}{HubID: installation.HubID, ProbeID: probeID}
	switch args[0] {
	case "register":
		// Validate the network trust tuple before creating even a registration.
		if _, err := probe.NewPinnedHTTPClient(endpoint, pin, policy); err != nil || !domain.ValidHubID(streamID) {
			return fail("Invalid endpoint, pin or stream ID")
		}
		p, err := registry.GetByID(ctx, probeID)
		if errors.Is(err, ports.ErrNotFound) {
			p = &domain.Probe{ID: probeID, Key: key, Name: name, Location: location, Kind: domain.ProbeKindRemote, Enabled: true}
			err = registry.Create(ctx, p)
		}
		if err != nil || p.Key != key || p.Name != name || p.Location != location || !p.Enabled {
			return fail("Registration conflict or invalid registration")
		}
		c, err := connector.Prepare(ctx, probeID, streamID, endpoint, pin)
		if err != nil {
			return fail("Credential preparation failed; existing identity was retained")
		}
		result.State = c.State
	case "enroll":
		token, err := readPrivateProbeInput(tokenFile, 256)
		if err != nil {
			return fail("Enrollment token file must be private and bounded")
		}
		defer clear(token)
		if connector.Enroll(ctx, probeID, strings.TrimSpace(string(token))) != nil {
			return fail("Enrollment failed; retain prepared credentials and retry runtime connection before issuing another token")
		}
		result.State = "enrollment_applied"
	case "assign":
		if monitorID <= 0 || members == "" {
			return fail("Monitor ID and complete probe assignment set are required")
		}
		current, err := repos.probeAssignments.GetByMonitorID(ctx, monitorID)
		if err != nil {
			return fail("Monitor assignment state is unavailable")
		}
		set, err := repos.probeAssignments.Replace(ctx, monitorID, revision, strings.Split(members, ","), current.HealthPolicy)
		if err != nil {
			return fail("Assignment replacement failed; check the current revision and enabled registrations")
		}
		result.State, result.Revision = "assigned", probe.Decimal(set.Revision)
		for _, member := range set.Assignments {
			result.Assignments = append(result.Assignments, assignmentView{ProbeID: member.ProbeID, Generation: probe.Decimal(member.Generation)})
		}
	case "prepare":
		if _, err := connections.GetConnection(ctx, probeID); err != nil {
			return fail("Prepare registration credentials before configuration")
		}
		document, err := readPrivateProbeInput(documentFile, domain.MaxProbeConfigBytes)
		if err != nil {
			return fail("Snapshot file must be private and bounded")
		}
		defer clear(document)
		target := domain.ProbeConfigTarget{HubID: installation.HubID, ProbeID: probeID}
		resolved, err := probe.NewEdgeConfigDecoder(checker.Get, notifier.Get).DecodeEdge(ctx, document, target)
		if err != nil {
			return fail("Snapshot is invalid or requires unsupported edge features")
		}
		for _, a := range resolved.Assignments {
			set, err := repos.probeAssignments.GetByMonitorID(ctx, a.Monitor.ID)
			if err != nil {
				return fail("Snapshot assignment is not registered in the hub")
			}
			matches := false
			for _, member := range set.Assignments {
				if member.ProbeID == probeID && member.Generation == a.Generation {
					matches = true
				}
			}
			if !matches {
				return fail("Snapshot generation differs from the hub assignment")
			}
		}
		metadata, err := configs.Prepare(ctx, target, document, revision)
		if err != nil {
			return fail("Snapshot preparation failed; check expected revision and immutable revision bytes")
		}
		result.State, result.Revision = "prepared", probe.Decimal(metadata.Revision)
	case "watchdog":
		explicitEnabled := false
		f.Visit(func(option *flag.Flag) { explicitEnabled = explicitEnabled || option.Name == "enabled" })
		settings, err := probeAdminWatchdogSettings(enabled, lostSeconds, recoverSeconds, resendMinutes, channelIDs)
		if err != nil || !explicitEnabled {
			return fail("Complete watchdog settings require explicit --enabled=true|false, bounded positive seconds and unique notification IDs")
		}
		saved, err := repository.NewProbeWatchdogSettingsStore(db).Replace(ctx, probeID, revision, settings)
		if err != nil {
			return fail("Watchdog settings failed; check current settings revision, enabled registration and notification IDs")
		}
		result.State, result.Watchdog = "watchdog_saved", probeAdminWatchdog(saved)
	case "status":
		c, err := connections.GetConnection(ctx, probeID)
		if err != nil {
			return fail("Probe connection is not registered")
		}
		result.State = c.State
		latest, readErr := configs.LatestRevision(ctx, probeID)
		result.Revision, err = probe.Decimal(latest), readErr
		if err != nil {
			return fail("Prepared configuration is unavailable")
		}
		appliedRevision := probe.Decimal(0)
		active, err := repository.NewProbeActivationStore(db, probe.LocalConfigEncoder{}).GetActive(ctx, probeID)
		if err == nil {
			appliedRevision = probe.Decimal(active.Revision)
		} else if !errors.Is(err, ports.ErrNotFound) {
			return fail("Applied configuration receipt is unavailable")
		}
		pending := result.Revision > appliedRevision
		result.AppliedRevision, result.SyncPending = &appliedRevision, &pending
		settings, err := repository.NewProbeWatchdogSettingsStore(db).Get(ctx, probeID)
		if err != nil {
			return fail("Watchdog settings are unavailable")
		}
		result.Watchdog = probeAdminWatchdog(settings)
	}
	if json.NewEncoder(out).Encode(result) != nil {
		return fail("Result output failed")
	}
	return 0
}

type probeAdminWatchdogView struct {
	Revision            probe.Decimal `json:"revision"`
	Enabled             bool          `json:"enabled"`
	LostAfterSeconds    int32         `json:"lost_after_seconds"`
	RecoverAfterSeconds int32         `json:"recover_after_seconds"`
	ResendInterval      int32         `json:"resend_interval"`
	NotificationIDs     []int64       `json:"notification_ids"`
}

func probeAdminWatchdog(s domain.ProbeWatchdogSettings) *probeAdminWatchdogView {
	return &probeAdminWatchdogView{Revision: probe.Decimal(s.Revision), Enabled: s.Enabled, LostAfterSeconds: s.LostAfterSeconds, RecoverAfterSeconds: s.RecoverAfterSeconds, ResendInterval: s.ResendInterval, NotificationIDs: append([]int64{}, s.NotificationIDs...)}
}

func probeAdminWatchdogSettings(enabled bool, lost, recoverAfter, resend int64, channels string) (domain.ProbeWatchdogSettings, error) {
	var s domain.ProbeWatchdogSettings
	if lost < 1 || lost > 1<<31-1 || recoverAfter < 1 || recoverAfter > 1<<31-1 || resend < 0 || resend > 1<<31-1 {
		return s, domain.ErrValidation
	}
	s = domain.ProbeWatchdogSettings{Enabled: enabled, LostAfterSeconds: int32(lost), RecoverAfterSeconds: int32(recoverAfter), ResendInterval: int32(resend), NotificationIDs: []int64{}}
	if channels != "" {
		for _, text := range strings.Split(channels, ",") {
			id, err := strconv.ParseInt(text, 10, 64)
			if err != nil || strconv.FormatInt(id, 10) != text {
				return s, domain.ErrValidation
			}
			s.NotificationIDs = append(s.NotificationIDs, id)
		}
	}
	return s, domain.ValidateProbeWatchdogSettings(s)
}

func readPrivateProbeInput(path string, limit int) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > int64(limit) {
		return nil, errors.New("invalid private input file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("input unavailable")
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if err != nil || len(data) > limit {
		clear(data)
		return nil, errors.New("input exceeds bound")
	}
	return data, nil
}
