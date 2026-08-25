package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/uptrace/bun"

	// Module-root package is `assets` (//go:embed web/dist); import path is the module path.
	phxassets "github.com/fiztoz/uptime-phoenix"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/auth"
	checkeradapter "github.com/fiztoz/uptime-phoenix/internal/adapters/checker"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/eventbus"
	httppkg "github.com/fiztoz/uptime-phoenix/internal/adapters/http"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/middleware"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/logger"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/metrics"
	notifieradapter "github.com/fiztoz/uptime-phoenix/internal/adapters/notifier"
	repo "github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	mariadbrepo "github.com/fiztoz/uptime-phoenix/internal/adapters/repository/mariadb"
	sqliterepo "github.com/fiztoz/uptime-phoenix/internal/adapters/repository/sqlite"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/scheduler"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/telemetry"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/ws"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// Run starts Phoenix with the given configuration and blocks until shutdown.
func Run(cfg Config) error {
	if err := validateJWTExpireHours(cfg.JWTExpireH); err != nil {
		return err
	}

	log := logger.New(cfg.LogLevel)
	log.Info("phoenix starting", "port", cfg.Port, "db_engine", cfg.DBEngine, "mode", cfg.Mode)

	ctx := context.Background()
	otelShutdown, err := telemetry.Init(ctx, telemetry.Config{
		Endpoint:    cfg.OTELEndpoint,
		ServiceName: cfg.OTELService,
	})
	if err != nil {
		log.Error("opentelemetry init failed", "error", err)
		return err
	}
	defer func() { _ = otelShutdown(context.Background()) }()

	db, err := openDB(cfg, log)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	if err := repo.RunMigrations(db.DB, cfg.DBEngine); err != nil {
		log.Error("failed to run migrations", "error", err)
		return err
	}
	log.Info("database migrations complete")

	repos := wireRepositories(cfg.DBEngine, db)

	jwtAuth := auth.NewJWTAuthenticator(cfg.JWTSecret, cfg.JWTExpireH, repos.user)
	totpProvider := auth.NewTOTPProvider(cfg.TOTPIssuer)

	authOpts := []services.AuthServiceOption{}
	webAuthnProvider, waErr := auth.NewWebAuthnProvider(auth.WebAuthnConfig{
		RPID:          cfg.WebAuthnRPID,
		RPDisplayName: cfg.WebAuthnRPName,
		RPOrigins:     splitAndTrim(cfg.WebAuthnRPOrigins),
	})
	if waErr != nil {
		// Passkeys are an optional feature: log and continue without them so a
		// bad RP config never blocks the rest of auth from starting.
		log.Error("webauthn provider init failed, passkeys disabled", "error", waErr)
	} else {
		authOpts = append(authOpts, services.WithWebAuthn(webAuthnProvider, repos.webAuthnCred))
		log.Info("webauthn (passkey) provider initialized", "rp_id", cfg.WebAuthnRPID)
	}

	if oidcOpt, oidcErr := buildOIDCOption(cfg, repos, log); oidcErr != nil {
		// Misconfiguration is fatal when the operator set an issuer — do not
		// silently boot with SSO half-on.
		log.Error("oidc provider init failed", "error", oidcErr)
		return fmt.Errorf("oidc: %w", oidcErr)
	} else if oidcOpt != nil {
		authOpts = append(authOpts, oidcOpt)
	}

	authSvc := services.NewAuthService(repos.user, repos.apiKey, jwtAuth, totpProvider, authOpts...)
	log.Info("auth service initialized")

	if cfg.BootstrapUsername != "" && cfg.BootstrapPassword != "" {
		if err := bootstrapUser(context.Background(), authSvc, repos.user, cfg.BootstrapUsername, cfg.BootstrapPassword); err != nil {
			log.Error("bootstrap user failed", "error", err)
			return err
		}
	}

	_ = checkeradapter.Get
	_ = notifieradapter.Get

	bus := wireEventBus(cfg, log)
	defer bus.Close()

	monitorSvc := services.NewMonitorService(repos.monitor, bus)
	monitorSvc.SetProxyRepo(repos.proxy)
	// Without this the service rejects every monitor that carries a GroupID, so
	// filing a monitor into a group would fail at runtime while the tests (which
	// wire the repo themselves) stay green.
	monitorSvc.SetGroupRepo(repos.monitorGroup)
	monitorSvc.SetDefaultNotificationLinker(repos.notification, repos.monitorNotif)
	monitorSvc.SetConditionRepository(repos.monitorCondition)
	proxySvc := services.NewProxyService(repos.proxy)
	monitorGroupSvc := services.NewMonitorGroupService(repos.monitorGroup, repos.monitor, repos.heartbeat, log)
	// Deleting a group re-homes its monitors; without the bus, open browsers keep
	// showing them inside the folder that no longer exists.
	monitorGroupSvc.SetEventBus(bus)
	heartbeatSvc := services.NewHeartbeatService(repos.heartbeat, bus)
	heartbeatSvc.SetTLSInfoRepo(repos.tlsInfo)

	notificationSvc := services.NewNotificationService(repos.notification, repos.monitorNotif)
	notificationSvc.SetTemplateRepository(repos.notificationTemplate)
	notificationTemplateSvc := services.NewNotificationTemplateService(repos.notificationTemplate)
	// Folder alerting: without this the group attach/detach routes fail closed
	// rather than 2xx-ing into the void, and no folder ever alerts.
	notificationSvc.SetGroupNotificationRepo(repos.groupNotif)
	for _, t := range []string{
		"telegram", "discord", "slack", "smtp", "webhook",
		"teams", "mattermost", "gotify",
		"bark", "feishu", "line",
	} {
		if sender, ok := notifieradapter.Get(t); ok {
			notificationSvc.RegisterSender(sender)
			log.Info("notification sender registered", "type", t)
		}
	}

	passwordHasher := auth.NewPasswordHasher()
	statusPageSvc := services.NewStatusPageService(
		repos.statusPage,
		repos.incident,
		repos.cname,
		repos.spMonitor,
		repos.monitor,
		repos.heartbeat,
		passwordHasher,
	)
	// Public status cert expiry surface (Sprint C F2.1 public slice).
	statusPageSvc.SetTLSInfoRepo(repos.tlsInfo)
	statusPageSvc.SetIncidentUpdateRepo(repos.incidentUpdate)

	// Status-page email subscriptions (Sprint C F3.1). Requires PUBLIC_URL and a
	// per-page active SMTP notification channel; empty PublicURL disables mail.
	tokenCodec := auth.NewSubscriberTokenCodec(cfg.JWTSecret)
	txMailer := notifieradapter.NewTransactionalSMTP()
	subscriptionSvc := services.NewSubscriptionService(
		repos.statusPage,
		repos.spSubscriber,
		repos.notification,
		tokenCodec,
		txMailer,
		passwordHasher,
		cfg.PublicURL,
	)
	statusPageSvc.SetIncidentNotifier(subscriptionSvc)
	statusPageSvc.SetSubscriptionAvailability(subscriptionSvc)

	tagSvc := services.NewTagService(repos.tag, repos.monitorTag)
	notificationSvc.SetTagReader(tagSvc)
	cronEval := scheduler.NewCronEvaluator()
	maintenanceSvc := services.NewMaintenanceService(repos.maintenance, repos.maintMonitor, cronEval)
	// Maintenance create/reschedule fan-out to status-page email subscribers.
	maintenanceSvc.SetAnnouncementNotifier(subscriptionSvc)
	conditionSvc := services.NewMonitorConditionService(repos.monitorCondition, notificationSvc, maintenanceSvc, bus)
	heartbeatSvc.SetConditionEvaluator(conditionSvc)

	// The single authorization choke point. Every handler, every middleware and the
	// WebSocket hub resolve "may this user see / do this?" through this one service
	// — see services.AccessService. All four repos are required: without the group
	// and monitor repos a group grant cannot be expanded, and the service would
	// (correctly, but uselessly) fail closed on every non-admin.
	accessSvc := services.NewAccessService(repos.user, repos.userPerm, repos.monitorGroup, repos.monitor)
	authSvc.SetUserChangeHook(accessSvc.InvalidateUser)
	log.Info("access service initialized")

	backupSvc := services.NewBackupService(
		repos.monitor,
		repos.monitorGroup,
		repos.notification,
		repos.monitorNotif,
		repos.tag,
		repos.monitorTag,
		repos.statusPage,
		repos.spMonitor,
		repos.cname,
		repos.incident,
		repos.maintenance,
		repos.maintMonitor,
		repos.proxy,
	)
	backupSvc.SetGroupNotificationRepo(repos.groupNotif)
	backupSvc.SetNotificationTemplateRepo(repos.notificationTemplate)
	backupSvc.SetSubscriberRepo(repos.spSubscriber)
	backupSvc.SetMonitorService(monitorSvc)
	backupSvc.SetProxyService(proxySvc)
	backupSvc.SetMonitorGroupService(monitorGroupSvc)

	configSvc := services.NewConfigService(
		repos.configKey,
		repos.tag,
		repos.proxy,
		repos.notification,
		repos.monitorGroup,
		repos.monitor,
		repos.monitorTag,
		repos.monitorNotif,
		repos.groupNotif,
		repos.statusPage,
		repos.spMonitor,
		repos.maintenance,
		repos.maintMonitor,
		passwordHasher,
	)

	// Wire automatic alerting: the dispatcher turns confirmed status transitions
	// into notifications (with maintenance suppression and resend throttling).
	// It runs inside the heartbeat path, in the monitor's owning worker, so each
	// transition alerts exactly once even under sharded/HA deployments.
	// On recovery it also auto-resolves status-page incidents when the flag is set.
	// F2.2: alert lifecycle entity + ack suppression of resends.
	alertSvc := services.NewAlertService(repos.alert)
	notifDispatcher := services.NewNotificationDispatcher(notificationSvc, maintenanceSvc)
	notifDispatcher.SetAutoResolver(statusPageSvc)
	notifDispatcher.SetAlertLifecycle(alertSvc)
	notifDispatcher.SetPublicURL(cfg.PublicURL)
	// Folder alerting rides the same heartbeat path, for the same reason: a
	// bus-subscribed alerter would fire once per worker under Redis fan-out. The
	// remaining race — two workers moving the same folder at once — is closed by
	// the compare-and-set inside GroupAlertService, not by this wiring.
	groupAlertSvc := services.NewGroupAlertService(
		repos.monitorGroup,
		repos.groupNotif,
		repos.monitor,
		repos.heartbeat,
		notificationSvc,
	)
	notifDispatcher.SetGroupEvaluator(groupAlertSvc)
	// F2.3: escalation ladders. StartForAlert runs AFTER the dispatcher's own
	// step-zero notification, and cancellation is wired into AlertService rather
	// than into each caller — ack arrives from the admin API and from the public
	// deep link, resolution from the heartbeat path, and a cancellation forgotten
	// at any one of those sites would keep paging someone who already responded.
	escalationSvc := services.NewEscalationService(
		repos.escalationPolicy,
		repos.escalationAssign,
		repos.alertEscalation,
		repos.alert,
		repos.monitor,
		repos.monitorGroup,
		notificationSvc,
	)
	escalationSvc.SetWorkerID(cfg.WorkerID)
	notifDispatcher.SetEscalationStarter(escalationSvc)
	alertSvc.SetEscalationCanceller(escalationSvc)
	heartbeatSvc.SetDispatcher(notifDispatcher)
	// Certificate-expiry alerts (opt-in per monitor). Same owning-worker path as
	// status alerts so Redis fan-out cannot duplicate them (Sprint C F2.1).
	certAlertSvc := services.NewCertificateAlertService(repos.tlsInfo, notificationSvc, maintenanceSvc)
	heartbeatSvc.SetCertAlert(certAlertSvc)
	log.Info("notification dispatcher wired to heartbeat service", "group_alerting", true, "cert_alerts", true, "alert_lifecycle", true, "escalation", true)

	aggregateSvc := services.NewAggregateService(repos.heartbeat, repos.monitor, log)
	monitorStatsSvc := services.NewMonitorStatsService(repos.heartbeat, repos.monitor, repos.tlsInfo, aggregateSvc)
	reliabilityReader, ok := repos.heartbeat.(ports.ReliabilityReader)
	if !ok {
		return fmt.Errorf("heartbeat repository does not support reliability transition reads")
	}
	aggregateReader, ok := repos.heartbeat.(ports.AggregateBatchReader)
	if !ok {
		return fmt.Errorf("heartbeat repository does not support batched reliability rollups")
	}
	insightsSvc := services.NewInsightsService(reliabilityReader, aggregateReader, repos.monitor, repos.monitorGroup, accessSvc)
	log.Info("aggregate and insights services initialized")

	metricsExporter := metrics.NewPrometheusExporter()

	isAPI := cfg.Mode == "all" || cfg.Mode == "api"
	isWorker := cfg.Mode == "all" || cfg.Mode == "worker"

	// The hub filters every outbound frame against the receiving client's visible
	// monitor set. Without accessSvc it would fail closed and emit nothing.
	hub := ws.NewHub(bus, repos.monitor, repos.heartbeat, accessSvc, tagSvc, slog.Default())

	// Make dropped events observable. Both the bus subscriber buffer and the
	// per-client send buffer discard on overflow by design; without these counters
	// that loss is invisible and a backlogged install looks identical to an idle
	// one. Wired for whichever bus was selected, so split mode is covered too.
	// The same attach drives phoenix_ws_connections_active from Add/RemoveClient.
	hub.SetDropMetrics(metricsExporter)
	switch b := bus.(type) {
	case *eventbus.MemoryBus:
		b.SetDropMetrics(metricsExporter)
	case *eventbus.RedisBus:
		b.SetDropMetrics(metricsExporter)
	}

	var sched ports.Scheduler
	var schedCancel context.CancelFunc
	if isWorker {
		if cfg.WorkerID != "" {
			// Sharded mode: multiple workers share the load via DB leases.
			sharded := scheduler.NewShardedScheduler(
				repos.monitor,
				checkeradapter.Get,
				heartbeatSvc,
				maintenanceSvc,
				slog.Default(),
				scheduler.ShardedSchedulerConfig{
					WorkerID:  cfg.WorkerID,
					BatchSize: cfg.ShardBatchSize,
					LeaseTTL:  time.Duration(cfg.ShardLeaseTTL) * time.Second,
					PollEvery: time.Duration(cfg.ShardPollEvery) * time.Second,
				},
			)
			sharded.SetProxyRepo(repos.proxy)
			sched = sharded
			log.Info("sharded scheduler configured",
				"worker_id", cfg.WorkerID,
				"batch_size", cfg.ShardBatchSize,
				"lease_ttl", cfg.ShardLeaseTTL,
				"poll_every", cfg.ShardPollEvery,
			)
		} else {
			// Local mode: single worker owns all monitors.
			local := scheduler.NewLocalScheduler(
				repos.monitor,
				repos.heartbeat,
				checkeradapter.Get,
				heartbeatSvc,
				maintenanceSvc,
				slog.Default(),
			)
			local.SetProxyRepo(repos.proxy)
			sched = local
		}
		var schedCtx context.Context
		schedCtx, schedCancel = context.WithCancel(context.Background())
		defer schedCancel()
		go func() {
			if err := sched.Run(schedCtx); err != nil {
				log.Error("scheduler stopped with error", "error", err)
			}
		}()
		log.Info("scheduler started", "mode", cfg.Mode, "sharded", cfg.WorkerID != "")

		// Start aggregate rollup goroutines.
		go func() {
			aggregateRollupLoop(schedCtx, aggregateSvc, log)
		}()
		// F2.3 escalation runner.
		go func() {
			escalationRunnerLoop(schedCtx, escalationSvc, escalationPollInterval(cfg), log)
		}()
		// Heartbeat retention — prune rows older than HEARTBEAT_RETENTION_DAYS.
		if cfg.HeartbeatRetentionDays > 0 {
			go func() {
				heartbeatRetentionLoop(schedCtx, heartbeatSvc, cfg.HeartbeatRetentionDays, log)
			}()
		} else {
			log.Info("heartbeat retention disabled (HEARTBEAT_RETENTION_DAYS=0)")
		}
	}

	healthHandlers := handlers.NewHealthHandlers(func() bool {
		pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return db.PingContext(pingCtx) == nil
	})

	authHandlers := handlers.NewAuthHandlers(authSvc)
	monitorHandlers := handlers.NewMonitorHandlers(monitorSvc, accessSvc, tagSvc, repos.monitorGroup)
	monitorGroupHandlers := handlers.NewMonitorGroupHandlers(monitorGroupSvc, accessSvc)
	notificationHandlers := handlers.NewNotificationHandlers(notificationSvc, accessSvc)
	notificationTemplateHandlers := handlers.NewNotificationTemplateHandlers(notificationTemplateSvc, accessSvc)
	statusPageHandlers := handlers.NewStatusPageHandlers(statusPageSvc)
	statusPageHandlers.SetSubscriptionService(subscriptionSvc)
	feedSvc := services.NewFeedService(
		repos.statusPage,
		repos.incident,
		repos.spMonitor,
		maintenanceSvc,
		passwordHasher,
		cfg.PublicURL,
	)
	feedHandlers := handlers.NewFeedHandlers(feedSvc)
	wsHandlers := handlers.NewWSHandlers(hub, authSvc, handlers.WSConfig{
		AllowedOriginPatterns:   splitAndTrim(cfg.WSAllowedOrigins),
		InsecureSkipOriginCheck: cfg.WSAllowAnyOrigin,
	})
	tagHandlers := handlers.NewTagHandlers(tagSvc, accessSvc)
	maintenanceHandlers := handlers.NewMaintenanceHandlers(maintenanceSvc, accessSvc)
	apiKeyHandlers := handlers.NewAPIKeyHandlers(authSvc)
	proxyHandlers := handlers.NewProxyHandlers(proxySvc)
	userHandlers := handlers.NewUserHandlers(authSvc, accessSvc)
	heartbeatHandlers := handlers.NewHeartbeatHandlers(heartbeatSvc, accessSvc)
	conditionHandlers := handlers.NewMonitorConditionHandlers(conditionSvc, accessSvc)
	statsHandlers := handlers.NewStatsHandlers(monitorStatsSvc, accessSvc)
	pushHandler := handlers.NewPushHandler(monitorSvc, heartbeatSvc)
	badgeHandlers := handlers.NewBadgeHandlers(repos.monitor, repos.heartbeat, aggregateSvc)
	backupHandlers := handlers.NewBackupHandlers(backupSvc)
	configHandlers := handlers.NewConfigHandlers(configSvc)
	alertHandlers := handlers.NewAlertHandlers(alertSvc, accessSvc)
	// The alert list shows each alert's ladder progress. Batched, never per row.
	alertHandlers.SetEscalationReader(escalationSvc)
	escalationHandlers := handlers.NewEscalationHandlers(escalationSvc)
	insightsHandlers := handlers.NewInsightsHandlers(insightsSvc)
	extensionHandlers := handlers.NewExtensionHandlers(cfg.ExtensionsJSON)

	httpOpts := httppkg.RouterOptions{
		Production: cfg.Production,
		RateLimit: middleware.RateLimitConfig{
			RequestsPerSecond: cfg.RateLimitRPS,
			Burst:             cfg.RateLimitBurst,
			RedisURL:          cfg.RedisURL,
		},
		CORS: corsConfigFromEnv(cfg),
	}

	e := httppkg.NewRouter(
		healthHandlers,
		authHandlers,
		monitorHandlers,
		monitorGroupHandlers,
		notificationHandlers,
		notificationTemplateHandlers,
		statusPageHandlers,
		feedHandlers,
		wsHandlers,
		tagHandlers,
		maintenanceHandlers,
		apiKeyHandlers,
		proxyHandlers,
		userHandlers,
		heartbeatHandlers,
		conditionHandlers,
		statsHandlers,
		pushHandler,
		badgeHandlers,
		backupHandlers,
		configHandlers,
		alertHandlers,
		escalationHandlers,
		insightsHandlers,
		extensionHandlers,
		authSvc,
		accessSvc,
		repos.apiKey,
		metricsExporter,
		phxassets.WebAssets,
		httpOpts,
		statusPageSvc,
		cfg.PublicURL,
	)

	if isAPI {
		go func() {
			addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
			log.Info("http server listening", "addr", addr, "mode", cfg.Mode)
			if err := e.Start(addr); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("server start failed", "error", err)
				os.Exit(1)
			}
		}()
	} else {
		log.Info("http server disabled (worker mode)")
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	<-sigCtx.Done()
	log.Info("shutdown signal received")

	if schedCancel != nil {
		schedCancel()
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if isAPI {
		if err := e.Shutdown(shutdownCtx); err != nil {
			log.Error("server shutdown error", "error", err)
		}
	}

	bus.Close()
	_ = db.Close()

	log.Info("phoenix stopped gracefully")
	return nil
}

func corsConfigFromEnv(cfg Config) middleware.CORSConfig {
	if cfg.CORSAllowOrigins == "" {
		return defaultCORSForMode(cfg)
	}
	parts := strings.Split(cfg.CORSAllowOrigins, ",")
	origins := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			origins = append(origins, s)
		}
	}
	if len(origins) == 0 {
		return defaultCORSForMode(cfg)
	}
	c := middleware.DefaultCORSConfig()
	c.AllowOrigins = origins
	return c
}

// defaultCORSForMode picks the unconfigured-CORS default: the permissive
// wildcard in dev, deny-by-default in production. A production deployment
// must opt in to cross-origin access via CORS_ALLOW_ORIGINS — it never
// inherits the dev `*`.
func defaultCORSForMode(cfg Config) middleware.CORSConfig {
	if cfg.Production {
		return middleware.SecureCORSConfig()
	}
	return middleware.DefaultCORSConfig()
}

// splitAndTrim splits a comma-separated env value into non-empty, trimmed
// items. Used for WebAuthn RP origins and WebSocket origin patterns.
func splitAndTrim(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func openDB(cfg Config, log *logger.SlogLogger) (*bun.DB, error) {
	switch cfg.DBEngine {
	case "mariadb":
		db, err := mariadbrepo.NewDBWithPool(cfg.DBDSN, mariadbrepo.PoolSettings{
			MaxOpenConns:    cfg.DBMaxOpenConns,
			MaxIdleConns:    cfg.DBMaxIdleConns,
			ConnMaxIdleTime: time.Duration(cfg.DBConnMaxIdleSeconds) * time.Second,
			ConnMaxLifetime: time.Duration(cfg.DBConnMaxLifetimeSeconds) * time.Second,
		})
		if err != nil {
			log.Error("failed to connect to mariadb", "error", err)
			return nil, err
		}
		return db, nil
	case "sqlite":
		db, err := sqliterepo.NewDB(cfg.DBDSN)
		if err != nil {
			log.Error("failed to connect to sqlite", "error", err)
			return nil, err
		}
		return db, nil
	default:
		log.Error("unknown db engine", "engine", cfg.DBEngine)
		return nil, fmt.Errorf("unknown db engine: %s", cfg.DBEngine)
	}
}

type repoBundle struct {
	user                 ports.UserRepository
	apiKey               ports.APIKeyRepository
	monitor              ports.MonitorRepository
	monitorGroup         ports.MonitorGroupRepository
	heartbeat            ports.HeartbeatRepository
	monitorCondition     ports.MonitorConditionRepository
	tlsInfo              ports.TLSInfoRepository
	notification         ports.NotificationRepository
	notificationTemplate ports.NotificationTemplateRepository
	monitorNotif         ports.MonitorNotificationRepository
	groupNotif           ports.GroupNotificationRepository
	statusPage           ports.StatusPageRepository
	incident             ports.IncidentRepository
	incidentUpdate       ports.IncidentUpdateRepository
	cname                ports.StatusPageCNAMERepository
	spMonitor            ports.StatusPageMonitorRepository
	spSubscriber         ports.StatusPageSubscriberRepository
	tag                  ports.TagRepository
	maintenance          ports.MaintenanceRepository
	proxy                ports.ProxyRepository
	monitorTag           ports.MonitorTagRepository
	maintMonitor         ports.MaintenanceWindowMonitorRepository
	webAuthnCred         ports.WebAuthnCredentialRepository
	userPerm             ports.UserPermissionRepository
	oidcIdentity         ports.OIDCIdentityRepository
	configKey            ports.ConfigKeyRepository
	alert                ports.AlertRepository
	escalationPolicy     ports.EscalationPolicyRepository
	escalationAssign     ports.EscalationAssignmentRepository
	alertEscalation      ports.AlertEscalationRepository
}

func wireRepositories(engine string, db *bun.DB) repoBundle {
	var b repoBundle
	switch engine {
	case "mariadb":
		r := mariadbrepo.NewRepository(db)
		b.user = r.UserRepo
		b.apiKey = r.APIKeyRepo
		b.monitor = r.MonitorRepo
		b.monitorGroup = r.MonitorGroupRepo
		b.heartbeat = r.HeartbeatRepo
		b.monitorCondition = r.MonitorConditionRepo
		b.tlsInfo = r.TLSInfoRepo
		b.notification = r.NotificationRepo
		b.notificationTemplate = r.NotificationTemplateRepo
		b.monitorNotif = r.MonitorNotificationRepo
		b.groupNotif = r.GroupNotificationRepo
		b.statusPage = r.StatusPageRepo
		b.incident = r.IncidentRepo
		b.incidentUpdate = r.IncidentUpdateRepo
		b.cname = r.StatusPageCnameRepo
		b.spMonitor = r.StatusPageMonitorRepo
		b.spSubscriber = r.StatusPageSubscriberRepo
		b.tag = r.TagRepo
		b.maintenance = r.MaintenanceRepo
		b.proxy = r.ProxyRepo
		b.monitorTag = r.MonitorTagRepo
		b.maintMonitor = r.MaintenanceWindowMonitorRepo
		b.webAuthnCred = r.WebAuthnCredentialRepo
		b.userPerm = r.UserPermissionRepo
		b.oidcIdentity = r.OIDCIdentityRepo
		b.configKey = r.ConfigKeyRepo
		b.alert = r.AlertRepo
		b.escalationPolicy = r.EscalationPolicyRepo
		b.escalationAssign = r.EscalationAssignmentRepo
		b.alertEscalation = r.AlertEscalationRepo
	case "sqlite":
		r := sqliterepo.NewRepository(db)
		b.user = r.UserRepo
		b.apiKey = r.APIKeyRepo
		b.monitor = r.MonitorRepo
		b.monitorGroup = r.MonitorGroupRepo
		b.heartbeat = r.HeartbeatRepo
		b.monitorCondition = r.MonitorConditionRepo
		b.tlsInfo = r.TLSInfoRepo
		b.notification = r.NotificationRepo
		b.notificationTemplate = r.NotificationTemplateRepo
		b.monitorNotif = r.MonitorNotificationRepo
		b.groupNotif = r.GroupNotificationRepo
		b.statusPage = r.StatusPageRepo
		b.incident = r.IncidentRepo
		b.incidentUpdate = r.IncidentUpdateRepo
		b.cname = r.StatusPageCnameRepo
		b.spMonitor = r.StatusPageMonitorRepo
		b.spSubscriber = r.StatusPageSubscriberRepo
		b.tag = r.TagRepo
		b.maintenance = r.MaintenanceRepo
		b.proxy = r.ProxyRepo
		b.monitorTag = r.MonitorTagRepo
		b.maintMonitor = r.MaintenanceWindowMonitorRepo
		b.webAuthnCred = r.WebAuthnCredentialRepo
		b.userPerm = r.UserPermissionRepo
		b.oidcIdentity = r.OIDCIdentityRepo
		b.configKey = r.ConfigKeyRepo
		b.alert = r.AlertRepo
		b.escalationPolicy = r.EscalationPolicyRepo
		b.escalationAssign = r.EscalationAssignmentRepo
		b.alertEscalation = r.AlertEscalationRepo
	}
	return b
}

// buildOIDCOption constructs the AuthService OIDC option when OIDC_ISSUER is set.
// Returns (nil, nil) when OIDC is not configured.
func buildOIDCOption(cfg Config, repos repoBundle, log *logger.SlogLogger) (services.AuthServiceOption, error) {
	issuer := strings.TrimSpace(cfg.OIDCIssuer)
	if issuer == "" {
		return nil, nil
	}
	redirectURL := strings.TrimSpace(cfg.OIDCRedirectURL)
	if redirectURL == "" && strings.TrimSpace(cfg.PublicURL) != "" {
		redirectURL = strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/") + "/api/auth/oidc/callback"
	}
	if cfg.OIDCClientID == "" || cfg.OIDCClientSecret == "" || redirectURL == "" {
		return nil, fmt.Errorf("OIDC_ISSUER is set but OIDC_CLIENT_ID, OIDC_CLIENT_SECRET, and OIDC_REDIRECT_URL (or PUBLIC_URL) are required")
	}
	grantMap, err := services.ParseOIDCGrantMap(cfg.OIDCGrantMap)
	if err != nil {
		return nil, fmt.Errorf("OIDC_GRANT_MAP: %w", err)
	}
	scopes := services.SplitCSV(cfg.OIDCScopes)
	provider, err := auth.NewOIDCProvider(context.Background(), auth.OIDCConfig{
		Issuer:       issuer,
		ClientID:     cfg.OIDCClientID,
		ClientSecret: cfg.OIDCClientSecret,
		RedirectURL:  redirectURL,
		Scopes:       scopes,
		GroupsClaim:  cfg.OIDCGroupsClaim,
	})
	if err != nil {
		return nil, err
	}
	policy := services.OIDCPolicy{
		JITEnabled:                      cfg.OIDCJITEnabled,
		LinkByEmail:                     cfg.OIDCLinkByEmail,
		AllowedGroups:                   services.SplitCSV(cfg.OIDCAllowedGroups),
		AdminGroups:                     services.SplitCSV(cfg.OIDCAdminGroups),
		CapNotificationsGroups:          services.SplitCSV(cfg.OIDCCapNotificationsGroups),
		CapMaintenanceGroups:            services.SplitCSV(cfg.OIDCCapMaintenanceGroups),
		CapCreateMonitorsGroups:         services.SplitCSV(cfg.OIDCCapCreateMonitorsGroups),
		CapCreateTopLevelMonitorsGroups: services.SplitCSV(cfg.OIDCCapCreateTopLevelMonitorsGroups),
		CapCreateGroupsGroups:           services.SplitCSV(cfg.OIDCCapCreateGroupsGroups),
		CapEditGroupMetadataGroups:      services.SplitCSV(cfg.OIDCCapEditGroupMetadataGroups),
		CapViewExtensionsGroups:         services.SplitCSV(cfg.OIDCCapViewExtensionsGroups),
		GrantMap:                        grantMap,
		StateSecret:                     cfg.JWTSecret,
		FrontendRedirect:                strings.TrimRight(strings.TrimSpace(cfg.PublicURL), "/"),
	}
	log.Info("oidc sso enabled", "issuer", issuer, "redirect_url", redirectURL, "jit", cfg.OIDCJITEnabled)
	return services.WithOIDC(provider, repos.oidcIdentity, repos.userPerm, policy), nil
}

func wireEventBus(cfg Config, log *logger.SlogLogger) ports.EventBus {
	if cfg.RedisURL == "" {
		return eventbus.NewMemoryBus()
	}

	// REDIS_URL is chosen once at process start. A first-ping miss used to
	// fall back to MemoryBus permanently, which in split mode looks healthy
	// while API WebSockets never see worker events. Retry so in-release
	// Valkey (or a slow external Redis) can finish coming up.
	const attempts = 15
	log.Info("initializing redis eventbus (multi-pod mode)")
	var last error
	for i := 1; i <= attempts; i++ {
		rbus, err := eventbus.NewRedisBus(context.Background(), cfg.RedisURL, log)
		if err == nil {
			return rbus
		}
		last = err
		if i < attempts {
			log.Warn("redis eventbus not ready, retrying", "attempt", i, "error", err)
			time.Sleep(2 * time.Second)
		}
	}
	log.Error("redis eventbus failed, falling back to memory", "error", last)
	return eventbus.NewMemoryBus()
}

func bootstrapUser(ctx context.Context, authSvc *services.AuthService, userRepo ports.UserRepository, username, password string) error {
	count, err := userRepo.Count(ctx)
	if err != nil {
		return fmt.Errorf("checking user count: %w", err)
	}
	if count > 0 {
		slog.Info("users already exist, skipping bootstrap", "count", count)
		return nil
	}

	slog.Info("bootstrapping initial user", "username", username)
	if _, err := authSvc.Register(ctx, username, password); err != nil {
		return fmt.Errorf("creating bootstrap user: %w", err)
	}
	slog.Info("bootstrap user created successfully", "username", username)
	return nil
}

// heartbeatRetentionLoop periodically deletes heartbeats older than retentionDays.
// The cutoff is always computed in UTC (rule 6). Runs once shortly after start,
// then daily. Blocks until ctx is canceled.
func heartbeatRetentionLoop(ctx context.Context, hbSvc *services.HeartbeatService, retentionDays int, log *logger.SlogLogger) {
	log.Info("heartbeat retention loop starting", "retention_days", retentionDays)
	defer log.Info("heartbeat retention loop stopped")

	runOnce := func() {
		// Always UTC — a local-zoned cutoff would delete up to ~7h of "future"
		// (relative to UTC wall-clock rows) on a UTC+7 host.
		cutoff := time.Now().UTC().AddDate(0, 0, -retentionDays)
		retCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		defer cancel()
		if err := hbSvc.DeleteOlderThan(retCtx, cutoff); err != nil {
			log.Error("heartbeat retention failed", "cutoff", cutoff.Format(time.RFC3339), "error", err)
			return
		}
		log.Info("heartbeat retention completed", "cutoff", cutoff.Format(time.RFC3339))
	}

	// Initial pass after a short delay so startup isn't blocked by a large delete.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
		runOnce()
	}

	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}

// escalationPollInterval clamps ESCALATION_POLL_SECONDS into a sane range. A
// zero or negative value would spin the runner; anything past a minute would
// make a "wait 1 minute" rung arrive arbitrarily late.
func escalationPollInterval(cfg Config) time.Duration {
	secs := cfg.EscalationPollSeconds
	if secs < 1 {
		secs = 1
	}
	if secs > 60 {
		secs = 60
	}
	return time.Duration(secs) * time.Second
}

// escalationRunnerLoop advances firing alerts through their escalation ladders
// (F2.3). Blocks until ctx is canceled.
//
// It runs in the WORKER, alongside the scheduler, for the same reason the
// notification dispatcher does: escalation must fire once per step, not once
// per pod. Correctness does not depend on that placement, though — every step
// is claimed with a database compare-and-set lease, so running this loop in
// several workers at once is safe by construction rather than by deployment
// convention.
func escalationRunnerLoop(ctx context.Context, svc *services.EscalationService, interval time.Duration, log *logger.SlogLogger) {
	log.Info("escalation runner loop starting", "interval", interval.String())
	defer log.Info("escalation runner loop stopped")

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runCtx, cancel := context.WithTimeout(ctx, interval*4)
			sent, err := svc.RunDue(runCtx)
			cancel()
			if err != nil {
				log.Error("escalation runner failed", "error", err)
				continue
			}
			if sent > 0 {
				log.Info("escalation steps dispatched", "count", sent)
			}
		}
	}
}

// aggregateRollupLoop runs aggregate rollup jobs on a schedule.
// It runs 1m rollups every minute, 1h rollups every 10 minutes,
// and 1d rollups every hour. Blocks until ctx is canceled.
func aggregateRollupLoop(ctx context.Context, aggSvc *services.AggregateService, log *logger.SlogLogger) {
	log.Info("aggregate rollup loop starting")
	defer log.Info("aggregate rollup loop stopped")

	// Stagger the tickers so they don't all fire at once.
	ticker1m := time.NewTicker(1 * time.Minute)
	ticker1h := time.NewTicker(10 * time.Minute)
	ticker1d := time.NewTicker(1 * time.Hour)
	defer ticker1m.Stop()
	defer ticker1h.Stop()
	defer ticker1d.Stop()

	// Catch up on startup. Periodic ticks only look back 2m / 2h / 2d, so a
	// restart would otherwise leave heartbeat_1h/1d empty for the 24h Insights
	// window.
	now := time.Now().UTC()
	rollupCtx, rollupCancel := context.WithTimeout(ctx, 5*time.Minute)
	if err := aggSvc.Rollup1m(rollupCtx, now.Add(-26*time.Hour), now); err != nil {
		log.Error("initial 1m rollup failed", "error", err)
	}
	rollupCancel()

	rollupCtx, rollupCancel = context.WithTimeout(ctx, 2*time.Minute)
	if err := aggSvc.Rollup1h(rollupCtx, now.Add(-26*time.Hour), now); err != nil {
		log.Error("initial 1h rollup failed", "error", err)
	}
	rollupCancel()

	rollupCtx, rollupCancel = context.WithTimeout(ctx, 5*time.Minute)
	if err := aggSvc.Rollup1d(rollupCtx, now.Add(-90*24*time.Hour), now); err != nil {
		log.Error("initial 1d rollup failed", "error", err)
	}
	rollupCancel()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker1m.C:
			now := time.Now().UTC()
			from := now.Add(-2 * time.Minute)
			rollupCtx, rollupCancel = context.WithTimeout(ctx, 30*time.Second)
			if err := aggSvc.Rollup1m(rollupCtx, from, now); err != nil {
				log.Error("1m rollup failed", "error", err)
			}
			rollupCancel()

		case <-ticker1h.C:
			now := time.Now().UTC()
			from := now.Add(-2 * time.Hour)
			rollupCtx, rollupCancel = context.WithTimeout(ctx, 1*time.Minute)
			if err := aggSvc.Rollup1h(rollupCtx, from, now); err != nil {
				log.Error("1h rollup failed", "error", err)
			}
			rollupCancel()

		case <-ticker1d.C:
			now := time.Now().UTC()
			from := now.Add(-2 * 24 * time.Hour)
			rollupCtx, rollupCancel = context.WithTimeout(ctx, 5*time.Minute)
			if err := aggSvc.Rollup1d(rollupCtx, from, now); err != nil {
				log.Error("1d rollup failed", "error", err)
			}
			rollupCancel()
		}
	}
}
