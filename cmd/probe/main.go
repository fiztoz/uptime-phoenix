// Command probe runs an autonomous TLS-only monitoring edge with private SQLite.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository/edge"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

type edgeOptions struct {
	DataDir                 string `env:"PROBE_DATA_DIR" envDefault:"/var/lib/uptime-phoenix/probe"`
	Listen                  string `env:"PROBE_LISTEN_ADDR" envDefault:":8443"`
	KeyFile                 string `env:"PROBE_SECRET_KEY_FILE"`
	TelemetryMaxBytes       int64  `env:"PROBE_TELEMETRY_MAX_BYTES" envDefault:"536870912"`
	TelemetryRetentionHours int    `env:"PROBE_TELEMETRY_RETENTION_HOURS" envDefault:"168"`
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	const usage = "Usage: probe <init|token|inspect|run> [--data-dir PATH] [--key-file PATH] [--listen HOST:PORT]\ninit creates a private identity, SQLite store and protection key; prints a 10-minute enrollment token once.\ntoken replaces an unused enrollment token while stopped. inspect reads local progress while stopped.\nrun opens an existing identity; it never regenerates missing secrets.\n"
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		_, _ = io.WriteString(stdout, usage)
		return 0
	}
	if len(args) == 0 || (args[0] != "init" && args[0] != "token" && args[0] != "inspect" && args[0] != "run") {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	var cfg edgeOptions
	if err := env.Parse(&cfg); err != nil {
		_, _ = io.WriteString(stderr, "Invalid probe environment configuration\n")
		return 2
	}
	flags := flag.NewFlagSet("probe", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "private local data directory")
	flags.StringVar(&cfg.KeyFile, "key-file", cfg.KeyFile, "protected configuration key")
	flags.StringVar(&cfg.Listen, "listen", cfg.Listen, "TLS listen address")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || cfg.DataDir == "" || cfg.TelemetryMaxBytes < 1<<20 || cfg.TelemetryMaxBytes > 1<<40 || cfg.TelemetryRetentionHours < 1 || cfg.TelemetryRetentionHours > 365*24 {
		_, _ = io.WriteString(stderr, usage)
		return 2
	}
	if cfg.KeyFile == "" {
		cfg.KeyFile = filepath.Join(cfg.DataDir, "config.key")
	}
	var identity *probe.RuntimeIdentity
	var err error
	if args[0] == "init" {
		identity, err = probe.InitializeRuntimeIdentity(ctx, cfg.DataDir)
	} else {
		identity, err = probe.OpenRuntimeIdentity(ctx, cfg.DataDir)
	}
	if err != nil {
		_, _ = io.WriteString(stderr, "Probe identity could not be opened; inspect file permissions, ownership and the exclusive lock\n")
		return 1
	}
	defer func() { _ = identity.Close() }()
	if args[0] == "init" {
		if _, err := os.Lstat(cfg.KeyFile); errors.Is(err, os.ErrNotExist) {
			// A missing key beside a retained DB is recovery, never provisioning.
			if _, dbErr := os.Lstat(filepath.Join(identity.DataDir, "edge.db")); !errors.Is(dbErr, os.ErrNotExist) {
				_, _ = io.WriteString(stderr, "Restore the original protection key before opening retained edge storage\n")
				return 1
			}
			if err := auth.CreateProbeSecretKeyFile(ctx, cfg.KeyFile); err != nil {
				_, _ = io.WriteString(stderr, "Probe protection key provisioning failed\n")
				return 1
			}
		}
	}
	protector, err := auth.NewProbeConfigProtectorFromFile(ctx, cfg.KeyFile)
	if err != nil {
		_, _ = io.WriteString(stderr, "Probe protection key is missing or invalid\n")
		return 1
	}
	store, err := edge.Open(ctx, identity.DataDir, domain.EdgeIdentity{ProbeID: identity.ProbeID, StreamID: identity.StreamID, Fingerprint: identity.Fingerprint}, edge.WithTelemetryEncoder(probe.EdgeTelemetryEncoder{}), edge.WithRetentionPolicy(edge.RetentionPolicy{MaxBytes: cfg.TelemetryMaxBytes, MaxAge: time.Duration(cfg.TelemetryRetentionHours) * time.Hour}))
	if err != nil {
		_, _ = io.WriteString(stderr, "Probe storage could not be opened\n")
		return 1
	}
	defer func() { _ = store.Close() }()
	configs := services.NewEdgeConfigService(store, store, probe.NewEdgeConfigDecoder(checker.Get, notifier.Get), protector)
	progress, err := store.ReadIdentity(ctx)
	if err != nil {
		_, _ = io.WriteString(stderr, "Probe identity storage is invalid\n")
		return 1
	}
	if progress.ConfigRevision > 0 {
		if _, err := configs.Load(ctx); err != nil {
			_, _ = io.WriteString(stderr, "Accepted probe configuration could not be authenticated or validated\n")
			return 1
		}
	}
	enrollment := services.NewEdgeEnrollmentService(store)
	if args[0] == "run" {
		if err := serveEdge(ctx, cfg, identity, store, configs, enrollment); err != nil && !errors.Is(err, context.Canceled) {
			_, _ = io.WriteString(stderr, "Probe runtime stopped with an error\n")
			return 1
		}
		return 0
	}
	// Explicit CLI DTO: digests, protected payloads and TLS private keys never
	// leave storage. The new local enrollment token is shown only on issuance.
	result := struct {
		ProbeID         string        `json:"probe_id"`
		StreamID        string        `json:"stream_id"`
		Fingerprint     string        `json:"certificate_fingerprint"`
		HubID           string        `json:"hub_id,omitempty"`
		Sequence        probe.Decimal `json:"last_created_seq"`
		Revision        probe.Decimal `json:"config_revision"`
		EnrollmentToken string        `json:"enrollment_token,omitempty"`
		ExpiresAt       *time.Time    `json:"expires_at,omitempty"`
	}{ProbeID: identity.ProbeID, StreamID: identity.StreamID, Fingerprint: identity.Fingerprint, HubID: progress.HubID, Sequence: probe.Decimal(progress.LastCreatedSeq), Revision: probe.Decimal(progress.ConfigRevision)}
	if args[0] == "init" || args[0] == "token" {
		at := time.Now().UTC()
		token, err := enrollment.Issue(ctx, at)
		if err != nil {
			_, _ = io.WriteString(stderr, "Enrollment token could not be issued; an enrolled probe cannot be rebound\n")
			return 1
		}
		expires := at.Add(10 * time.Minute)
		result.EnrollmentToken, result.ExpiresAt = token, &expires
	}
	if err := json.NewEncoder(stdout).Encode(result); err != nil {
		return 1
	}
	return 0
}
