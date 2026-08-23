// Package http provides the Echo router adapter and HTTP handlers.
package http

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/handlers"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/http/middleware"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

// RouterOptions configures global HTTP middleware (Phase 3 hardening).
type RouterOptions struct {
	Production bool
	RateLimit  middleware.RateLimitConfig
	CORS       middleware.CORSConfig
}

// NewRouter creates and configures the Echo router with all routes and
// middleware.
//
// The auth service is required so the auth endpoints have something to
// delegate to. Public health endpoints are registered unconditionally;
// CORS is enabled with the dev default; auth routes are split into
// public and protected groups so the same router is usable both with
// and without a session token.
// webAssets provides the embedded frontend static files for SPA serving.
// statusMeta resolves published status pages for server-side OG/unfurl injection
// into the SPA index shell (Sprint C R2.8). publicOrigin is the absolute PUBLIC_URL
// used for og:url when set; empty falls back to the request scheme+host.
func NewRouter(
	healthHandlers *handlers.HealthHandlers,
	authHandlers *handlers.AuthHandlers,
	monitorHandlers *handlers.MonitorHandlers,
	monitorGroupHandlers *handlers.MonitorGroupHandlers,
	notificationHandlers *handlers.NotificationHandlers,
	notificationTemplateHandlers *handlers.NotificationTemplateHandlers,
	statusPageHandlers *handlers.StatusPageHandlers,
	feedHandlers *handlers.FeedHandlers,
	wsHandlers *handlers.WSHandlers,
	tagHandlers *handlers.TagHandlers,
	maintenanceHandlers *handlers.MaintenanceHandlers,
	apiKeyHandlers *handlers.APIKeyHandlers,
	proxyHandlers *handlers.ProxyHandlers,
	userHandlers *handlers.UserHandlers,
	heartbeatHandlers *handlers.HeartbeatHandlers,
	conditionHandlers *handlers.MonitorConditionHandlers,
	statsHandlers *handlers.StatsHandlers,
	pushHandler *handlers.PushHandler,
	badgeHandlers *handlers.BadgeHandlers,
	backupHandlers *handlers.BackupHandlers,
	configHandlers *handlers.ConfigHandlers,
	alertHandlers *handlers.AlertHandlers,
	escalationHandlers *handlers.EscalationHandlers,
	insightsHandlers *handlers.InsightsHandlers,
	extensionHandlers *handlers.ExtensionHandlers,
	authSvc *services.AuthService,
	accessSvc *services.AccessService,
	apiKeyRepo ports.APIKeyRepository,
	metricsExporter ports.MetricsExporter,
	webAssets embed.FS,
	opts RouterOptions,
	statusMeta StatusPageMetaResolver,
	publicOrigin string,
) *echo.Echo {
	e := echo.New()
	e.HideBanner = true
	e.HidePort = true
	// Trust only the nearest untrusted hop in X-Forwarded-For. Echo's legacy
	// fallback accepts the client-controlled leftmost value, which would let a
	// caller rotate spoofed IPs around credential and API rate limits.
	e.IPExtractor = echo.ExtractIPFromXFFHeader()

	corsCfg := opts.CORS
	// A zero-value config falls back to the permissive dev default, but an
	// explicit deny (DisableCrossOrigin) must survive untouched — production
	// bootstrap relies on it when CORS_ALLOW_ORIGINS is unset.
	if len(corsCfg.AllowOrigins) == 0 && !corsCfg.DisableCrossOrigin {
		corsCfg = middleware.DefaultCORSConfig()
	}

	// Global middleware order: CORS → request ID → security headers → rate limit.
	e.Use(middleware.CORS(corsCfg))
	e.Use(middleware.RequestID())
	e.Use(middleware.SecurityHeaders(middleware.SecurityHeadersConfig{Production: opts.Production}))
	e.Use(middleware.RateLimit(opts.RateLimit))

	// Health check endpoints (no auth).
	e.GET("/api/health/live", healthHandlers.Live)
	e.GET("/api/health/ready", healthHandlers.Ready)

	// Public auth endpoints (no auth required).
	authGroup := e.Group("/api/auth")
	authGroup.GET("/has-users", authHandlers.HasUsers)
	authGroup.POST("/register", authHandlers.Register)
	authGroup.POST("/login", authHandlers.Login)
	authGroup.POST("/verify-2fa", authHandlers.Verify2FA)
	// Public passkey (WebAuthn) assertion endpoints — used for passwordless login.
	authGroup.POST("/webauthn/login/begin", authHandlers.WebAuthnLoginBegin)
	authGroup.POST("/webauthn/login/finish", authHandlers.WebAuthnLoginFinish)
	// OIDC SSO (public). Status always answers; login/callback 404 when disabled.
	authGroup.GET("/oidc/status", authHandlers.OIDCStatus)
	authGroup.GET("/oidc/login", authHandlers.OIDCLogin)
	authGroup.GET("/oidc/callback", authHandlers.OIDCCallback)
	authGroup.GET("/oidc/logout", authHandlers.OIDCLogout)

	// Protected auth endpoints (auth required).
	if authSvc != nil {
		protectedGroup := e.Group("/api/auth", middleware.AuthMiddleware(authSvc))
		protectedGroup.GET("/me", authHandlers.Me)
		protectedGroup.POST("/setup-2fa", authHandlers.Setup2FA)
		protectedGroup.POST("/enable-2fa", authHandlers.Enable2FA)
		protectedGroup.POST("/disable-2fa", authHandlers.Disable2FA)
		// Passkey (WebAuthn) registration + management (needs a session).
		protectedGroup.POST("/webauthn/register/begin", authHandlers.WebAuthnRegisterBegin)
		protectedGroup.POST("/webauthn/register/finish", authHandlers.WebAuthnRegisterFinish)
		protectedGroup.GET("/webauthn/credentials", authHandlers.WebAuthnListCredentials)
		protectedGroup.DELETE("/webauthn/credentials/:id", authHandlers.WebAuthnDeleteCredential)
	}

	// RBAC gates. requireAdmin guards install-wide resources; the capability gates
	// guard notification/maintenance management, extension visibility and
	// creation. Monitor/group reads are not gated here — a middleware can reject
	// a request but cannot narrow a result set, so read scoping lives in the
	// handlers (which consult the same services.AccessService).
	requireAdmin := middleware.RequireAdmin(authSvc)
	requireNotifications := middleware.RequireCapability(accessSvc, middleware.CapManageNotifications)
	requireMaintenance := middleware.RequireCapability(accessSvc, middleware.CapManageMaintenance)
	requireExtensions := middleware.RequireCapability(accessSvc, middleware.CapViewExtensions)
	requireCreateMonitors := middleware.RequireCapability(accessSvc, middleware.CapCreateMonitors)
	requireCreateGroups := middleware.RequireCapability(accessSvc, middleware.CapCreateGroups)

	// Reliability read model. The handler applies monitor visibility before it
	// computes any rows, so this route needs authentication but no install-wide
	// capability gate.
	if insightsHandlers != nil && authSvc != nil {
		e.GET("/api/insights", insightsHandlers.GetInsights, middleware.AuthMiddleware(authSvc))
	}

	// K8s extensions catalog. Admins always have access; non-admins need the
	// view_extensions capability. Empty or unset PHOENIX_EXTENSIONS is []. This
	// gates Phoenix discovery/launching; the :id/frame redirect is the launch
	// point the iframe uses (and the only surface that releases an entry's
	// launch credential). The extension's direct Ingress path must enforce its
	// own authorization.
	if extensionHandlers != nil && authSvc != nil {
		e.GET("/api/extensions", extensionHandlers.List, middleware.AuthMiddleware(authSvc), requireExtensions)
		e.GET("/api/extensions/:id/frame", extensionHandlers.Frame, middleware.AuthMiddleware(authSvc), requireExtensions)
	}

	// Monitor routes (protected with auth middleware). Three different gates,
	// because "may I?" has three different answers here:
	//
	//	read    — no middleware; the handler scopes the result to what the caller
	//	          can see (a middleware cannot narrow a list, only reject it);
	//	create  — the create_monitors capability, a user-level question a
	//	          middleware CAN answer since there is no target resource yet;
	//	mutate  — NO middleware. Deliberate. Update/Delete/Clone name an existing
	//	          monitor, and the answer depends on who owns THAT monitor, which
	//	          the router cannot know. Gating them with requireCreateMonitors
	//	          would let any monitor-creating user edit everyone else's. The
	//	          handlers call requireMonitorEditAccess instead — that is the gate,
	//	          not a second layer behind one.
	if monitorHandlers != nil && authSvc != nil {
		monitorGroup := e.Group("/api/monitors", middleware.AuthMiddleware(authSvc))
		monitorGroup.POST("", monitorHandlers.Create, requireCreateMonitors)
		monitorGroup.GET("", monitorHandlers.List)
		monitorGroup.GET("/:id", monitorHandlers.GetByID)
		monitorGroup.PUT("/:id", monitorHandlers.Update)
		monitorGroup.DELETE("/:id", monitorHandlers.Delete)
		monitorGroup.POST("/:id/clone", monitorHandlers.Clone, requireCreateMonitors)

		if statsHandlers != nil {
			monitorGroup.GET("/:id/stats", statsHandlers.GetStats)
		}
	}

	// Monitor group (folder) routes. Same three-way split as monitors above.
	if monitorGroupHandlers != nil && authSvc != nil {
		monitorGroupsGroup := e.Group("/api/monitor-groups", middleware.AuthMiddleware(authSvc))
		monitorGroupsGroup.POST("", monitorGroupHandlers.Create, requireCreateGroups)
		monitorGroupsGroup.GET("", monitorGroupHandlers.List)
		monitorGroupsGroup.GET("/:id", monitorGroupHandlers.GetByID)
		monitorGroupsGroup.PUT("/:id", monitorGroupHandlers.Update)
		monitorGroupsGroup.DELETE("/:id", monitorGroupHandlers.Delete)
	}

	// Heartbeat query routes (protected with auth middleware). Reads need view
	// access to the monitor (checked in the handler); clearing history destroys
	// data and is admin-only.
	if heartbeatHandlers != nil && authSvc != nil {
		hbGroup := e.Group("/api/monitors/:id/heartbeats", middleware.AuthMiddleware(authSvc))
		hbGroup.GET("", heartbeatHandlers.ListByMonitor)
		hbGroup.GET("/chart", heartbeatHandlers.GetChartData)
		hbGroup.DELETE("", heartbeatHandlers.ClearHistory, requireAdmin)
	}

	// Latest auxiliary monitor conditions (capacity, session pool, and future
	// typed observations). The handler scopes every row through AccessService.
	if conditionHandlers != nil && authSvc != nil {
		e.GET("/api/monitor-conditions", conditionHandlers.List, middleware.AuthMiddleware(authSvc))
	}

	// Monitor-notification list (for monitor detail page UI).
	if notificationHandlers != nil && authSvc != nil {
		monNotifGroup := e.Group("/api/monitors/:id/notifications", middleware.AuthMiddleware(authSvc))
		monNotifGroup.GET("", notificationHandlers.ListForMonitor)
	}

	// Reusable message templates share the notification-management capability.
	// Provider is immutable after creation so a saved notification can never be
	// left pointing at a template for a different channel type.
	if notificationTemplateHandlers != nil && authSvc != nil {
		templateGroup := e.Group("/api/notification-templates", middleware.AuthMiddleware(authSvc), requireNotifications)
		templateGroup.POST("", notificationTemplateHandlers.Create)
		templateGroup.GET("", notificationTemplateHandlers.List)
		templateGroup.GET("/variables", notificationTemplateHandlers.Variables)
		templateGroup.GET("/:id", notificationTemplateHandlers.GetByID)
		templateGroup.PUT("/:id", notificationTemplateHandlers.Update)
		templateGroup.DELETE("/:id", notificationTemplateHandlers.Delete)
	}

	// Group-notification list (for the monitor-group form's provider checkboxes).
	// A notification attached here alerts on the FOLDER's own derived status.
	if notificationHandlers != nil && authSvc != nil {
		groupNotifGroup := e.Group("/api/monitor-groups/:id/notifications", middleware.AuthMiddleware(authSvc))
		groupNotifGroup.GET("", notificationHandlers.ListForGroup)
	}

	// Notification routes. Mutations (including "test", which sends a real message
	// to the outside world) require the can_manage_notifications capability; admins
	// hold it implicitly. Reads are scoped in the handler: capability holders see
	// every notification in the install, everyone else only those attached to
	// monitors they can view.
	if notificationHandlers != nil && authSvc != nil {
		notifGroup := e.Group("/api/notifications", middleware.AuthMiddleware(authSvc))
		notifGroup.POST("", notificationHandlers.Create, requireNotifications)
		notifGroup.GET("", notificationHandlers.List)
		notifGroup.GET("/:id", notificationHandlers.GetByID)
		notifGroup.PUT("/:id", notificationHandlers.Update, requireNotifications)
		notifGroup.DELETE("/:id", notificationHandlers.Delete, requireNotifications)
		notifGroup.POST("/:id/test", notificationHandlers.Test, requireNotifications)

		// Monitor-notification association.
		notifGroup.POST("/:id/monitor/:monitorId", notificationHandlers.AttachToMonitor, requireNotifications)
		notifGroup.DELETE("/:id/monitor/:monitorId", notificationHandlers.DetachFromMonitor, requireNotifications)

		// Group-notification association — the folder alerts on its own rollup.
		notifGroup.POST("/:id/group/:groupId", notificationHandlers.AttachToGroup, requireNotifications)
		notifGroup.DELETE("/:id/group/:groupId", notificationHandlers.DetachFromGroup, requireNotifications)
	}

	// Status page routes — ADMIN-ONLY, reads included. Status pages expose monitors
	// publicly, so who may curate them is an admin decision, and the authenticated
	// list would otherwise leak the names of monitors a non-admin cannot see.
	// (The PUBLIC status endpoints below are unauthenticated by design.)
	if statusPageHandlers != nil && authSvc != nil {
		spGroup := e.Group("/api/status-pages", middleware.AuthMiddleware(authSvc), requireAdmin)
		spGroup.POST("", statusPageHandlers.Create)
		spGroup.GET("", statusPageHandlers.List)
		spGroup.GET("/:id", statusPageHandlers.GetByID)
		spGroup.PUT("/:id", statusPageHandlers.Update)
		spGroup.DELETE("/:id", statusPageHandlers.Delete)

		// Incident management under status pages.
		spGroup.POST("/:spId/incidents", statusPageHandlers.CreateIncident)
		spGroup.GET("/:spId/incidents", statusPageHandlers.ListIncidents)
		spGroup.PUT("/:spId/incidents/:incId", statusPageHandlers.UpdateIncident)
		spGroup.GET("/:spId/incidents/:incId/updates", statusPageHandlers.ListIncidentUpdates)
		spGroup.POST("/:spId/incidents/:incId/updates", statusPageHandlers.CreateIncidentUpdate)
		spGroup.DELETE("/:spId/incidents/:incId", statusPageHandlers.DeleteIncident)
		spGroup.POST("/:spId/incidents/:incId/resolve", statusPageHandlers.ResolveIncident)

		// Status page monitor assignments.
		spGroup.POST("/:spId/monitors", statusPageHandlers.AddMonitor)
		spGroup.GET("/:spId/monitors", statusPageHandlers.ListMonitors)
		spGroup.PUT("/:spId/monitors", statusPageHandlers.ReorderMonitors)
		spGroup.DELETE("/:spId/monitors/:monitorId", statusPageHandlers.RemoveMonitor)

		// Status page CNAME management.
		spGroup.POST("/:spId/cnames", statusPageHandlers.AddCNAME)
		spGroup.GET("/:spId/cnames", statusPageHandlers.ListCNAMEs)
		spGroup.DELETE("/:spId/cnames/:cnameId", statusPageHandlers.RemoveCNAME)

		// Email subscription admin (Sprint C F3.1).
		spGroup.GET("/:spId/subscribers", statusPageHandlers.ListSubscribers)
		spGroup.DELETE("/:spId/subscribers/:subscriberId", statusPageHandlers.DeleteSubscriber)
		spGroup.GET("/:spId/subscription-channel", statusPageHandlers.GetSubscriptionChannel)
		spGroup.PUT("/:spId/subscription-channel", statusPageHandlers.SetSubscriptionChannel)
		spGroup.DELETE("/:spId/subscription-channel", statusPageHandlers.DeleteSubscriptionChannel)
	}

	// Public status page endpoints (no auth required).
	if statusPageHandlers != nil {
		e.GET("/api/status/pages/:slug", statusPageHandlers.GetBySlug)
		e.GET("/api/status/:slug", statusPageHandlers.GetPublicStatus)
		e.GET("/api/status/resolve", statusPageHandlers.ResolveDomain)
		credentialLimit := middleware.DefaultCredentialRateLimitConfig()
		credentialLimit.RedisURL = opts.RateLimit.RedisURL
		e.POST(
			"/api/status/:slug/verify-access",
			statusPageHandlers.VerifyAccess,
			middleware.CredentialRateLimit(credentialLimit),
		)
		// Double-opt-in email subscribe / confirm / unsubscribe (rate-limited).
		e.POST(
			"/api/status/:slug/subscribers",
			statusPageHandlers.Subscribe,
			middleware.CredentialRateLimit(credentialLimit),
		)
		e.POST(
			"/api/status/subscriptions/confirm",
			statusPageHandlers.ConfirmSubscription,
			middleware.CredentialRateLimit(credentialLimit),
		)
		e.POST(
			"/api/status/subscriptions/unsubscribe",
			statusPageHandlers.Unsubscribe,
			middleware.CredentialRateLimit(credentialLimit),
		)
	}
	// Public status-page feeds (F3.6). Fail closed on access-protected pages.
	if feedHandlers != nil {
		e.GET("/api/status/:slug/feed.xml", feedHandlers.Atom)
		e.GET("/api/status/:slug/calendar.ics", feedHandlers.Calendar)
	}

	// Tag routes (protected). Reading tags is open to any authenticated user — the
	// dashboard's tag filter needs the list, and a tag is just a name and a color.
	// Writing is admin-only: tags are install-wide (deleting one strips it from
	// every monitor carrying it), and assigning one to a monitor mutates that
	// monitor, which non-admins may not do.
	if tagHandlers != nil && authSvc != nil {
		tagGroup := e.Group("/api/tags", middleware.AuthMiddleware(authSvc))
		tagGroup.POST("", tagHandlers.Create, requireAdmin)
		tagGroup.GET("", tagHandlers.List)
		tagGroup.PUT("/:id", tagHandlers.Update, requireAdmin)
		tagGroup.DELETE("/:id", tagHandlers.Delete, requireAdmin)

		// Monitor tag assignments. GET is view-scoped in the handler.
		mtGroup := e.Group("/api/monitors/:id/tags", middleware.AuthMiddleware(authSvc))
		mtGroup.POST("", tagHandlers.AssignToMonitor, requireAdmin)
		mtGroup.DELETE("/:tag_id", tagHandlers.RemoveFromMonitor, requireAdmin)
		mtGroup.GET("", tagHandlers.ListForMonitor)
	}

	// Top-level incidents (protected).
	if statusPageHandlers != nil && authSvc != nil {
		e.GET("/api/incidents", statusPageHandlers.ListAllIncidents, middleware.AuthMiddleware(authSvc))
	}

	// F2.2 alert lifecycle. List/get/ack are session-scoped (view RBAC in handler).
	// Token ack is public — the high-entropy ack_token is the credential.
	if alertHandlers != nil {
		e.POST("/api/alerts/ack-by-token", alertHandlers.AcknowledgeByToken)
		if authSvc != nil {
			alertGroup := e.Group("/api/alerts", middleware.AuthMiddleware(authSvc))
			alertGroup.GET("", alertHandlers.List)
			alertGroup.GET("/:id", alertHandlers.Get)
			alertGroup.POST("/:id/ack", alertHandlers.Acknowledge)
		}
	}

	// F2.3 escalation policies.
	//
	// Two different gates on purpose. Policy CRUD needs can_manage_notifications
	// (admins hold it implicitly) because a policy is a notification-routing
	// object whose steps name channels already behind that capability. But
	// ASSIGNING a policy to a monitor or a folder changes what that monitor does
	// when it fails, and AGENTS.md keeps monitor and group writes admin-only —
	// so assignment carries requireAdmin as well, and a non-admin notification
	// manager cannot rewire the paging of a monitor they may not even see.
	if escalationHandlers != nil && authSvc != nil {
		escGroup := e.Group("/api/escalation-policies", middleware.AuthMiddleware(authSvc), requireNotifications)
		escGroup.GET("", escalationHandlers.List)
		escGroup.POST("", escalationHandlers.Create)
		escGroup.GET("/:id", escalationHandlers.Get)
		escGroup.PUT("/:id", escalationHandlers.Update)
		escGroup.DELETE("/:id", escalationHandlers.Delete)
		// Reverse listing (policy → monitors/groups). Same gate as List — a
		// notification manager may see who is assigned. Writes stay admin-only
		// on the monitor/group assignment routes below.
		escGroup.GET("/:id/assignments", escalationHandlers.ListAssignments)

		monEscGroup := e.Group("/api/monitors/:id/escalation-policy",
			middleware.AuthMiddleware(authSvc), requireAdmin, requireNotifications)
		monEscGroup.GET("", escalationHandlers.GetMonitorAssignment)
		monEscGroup.PUT("", escalationHandlers.SetMonitorAssignment)

		grpEscGroup := e.Group("/api/monitor-groups/:id/escalation-policy",
			middleware.AuthMiddleware(authSvc), requireAdmin, requireNotifications)
		grpEscGroup.GET("", escalationHandlers.GetGroupAssignment)
		grpEscGroup.PUT("", escalationHandlers.SetGroupAssignment)
	}

	// Maintenance window routes. Mutations require the can_manage_maintenance
	// capability (admins hold it implicitly); reads are scoped in the handler —
	// capability holders see every window, everyone else only the windows covering
	// monitors they can view.
	if maintenanceHandlers != nil && authSvc != nil {
		maintGroup := e.Group("/api/maintenance", middleware.AuthMiddleware(authSvc))
		maintGroup.POST("", maintenanceHandlers.Create, requireMaintenance)
		maintGroup.GET("", maintenanceHandlers.List)
		maintGroup.GET("/:id", maintenanceHandlers.Get)
		maintGroup.PUT("/:id", maintenanceHandlers.Update, requireMaintenance)
		maintGroup.DELETE("/:id", maintenanceHandlers.Delete, requireMaintenance)

		maintMonGroup := e.Group("/api/maintenance/:id/monitors", middleware.AuthMiddleware(authSvc))
		maintMonGroup.POST("", maintenanceHandlers.AssignMonitor, requireMaintenance)
		maintMonGroup.DELETE("/:monitor_id", maintenanceHandlers.UnassignMonitor, requireMaintenance)
		maintMonGroup.GET("", maintenanceHandlers.ListMonitors)
	}

	// API key routes — ADMIN-ONLY, reads included. An API key is a credential that
	// carries its owner's authority; minting one is an admin act.
	if apiKeyHandlers != nil && authSvc != nil {
		keyGroup := e.Group("/api/api-keys", middleware.AuthMiddleware(authSvc), requireAdmin)
		keyGroup.POST("", apiKeyHandlers.Create)
		keyGroup.GET("", apiKeyHandlers.List)
		keyGroup.DELETE("/:id", apiKeyHandlers.Delete)
	}

	// Proxy routes — ADMIN-ONLY, reads included. Proxies carry credentials and
	// decide where a check's traffic is routed; that is infrastructure config.
	if proxyHandlers != nil && authSvc != nil {
		proxyGroup := e.Group("/api/proxies", middleware.AuthMiddleware(authSvc), requireAdmin)
		proxyGroup.POST("", proxyHandlers.Create)
		proxyGroup.GET("", proxyHandlers.List)
		proxyGroup.PUT("/:id", proxyHandlers.Update)
		proxyGroup.DELETE("/:id", proxyHandlers.Delete)
	}

	// Backup export/import — ADMIN-ONLY. Export includes secrets by design (see
	// handlers.BackupHandlers.Export and services.BackupDocument), so it is an
	// install-wide credential dump: nothing short of admin may call it.
	if backupHandlers != nil && authSvc != nil {
		backupGroup := e.Group("/api/backup", middleware.AuthMiddleware(authSvc), requireAdmin)
		backupGroup.GET("/export", backupHandlers.Export)
		backupGroup.POST("/import", backupHandlers.Import)
	}

	// Declarative config-as-code — ADMIN-ONLY. Export is secret-redacted;
	// apply is idempotent and prune-optional. Auth is session JWT or a
	// write-scoped API key (GitOps CI). See docs/F5-S14-CONFIG-AS-CODE-CONTRACTS.md.
	if configHandlers != nil && authSvc != nil {
		configAuth := middleware.AuthMiddleware(authSvc)
		if apiKeyRepo != nil {
			configAuth = middleware.SessionOrAPIKey(authSvc, apiKeyRepo, "write")
		}
		configGroup := e.Group("/api/config", configAuth, requireAdmin)
		configGroup.GET("/export", configHandlers.Export)
		configGroup.POST("/validate", configHandlers.Validate)
		configGroup.POST("/plan", configHandlers.Plan)
		configGroup.POST("/apply", configHandlers.Apply)
	}

	// Admin user-management routes. Self-registration is disabled (see
	// AuthHandlers.Register), so these are reachable via a session JWT or
	// an API key with the "write" scope, AND the resolved principal must
	// be an admin (domain.User.IsAdmin) — enforced by RequireAdmin, which
	// runs after SessionOrAPIKey has resolved ContextUserIDKey.
	if userHandlers != nil && authSvc != nil && apiKeyRepo != nil {
		userGroup := e.Group("/api/users",
			middleware.SessionOrAPIKey(authSvc, apiKeyRepo, "write"),
			middleware.RequireAdmin(authSvc),
		)
		userGroup.POST("", userHandlers.Create)
		userGroup.GET("", userHandlers.List)
		userGroup.GET("/:id", userHandlers.GetByID)
		userGroup.PUT("/:id", userHandlers.Update)
		userGroup.DELETE("/:id", userHandlers.Delete)

		// RBAC grant administration: which monitors and groups a user may see.
		// PUT replaces the user's whole grant set (see UserHandlers.UpdatePermissions).
		userGroup.GET("/:id/permissions", userHandlers.GetPermissions)
		userGroup.PUT("/:id/permissions", userHandlers.UpdatePermissions)
	}

	// Prometheus metrics (protected by API key middleware).
	if metricsExporter != nil && apiKeyRepo != nil {
		m := middleware.APIKeyMiddleware(apiKeyRepo)
		h, _ := metricsExporter.Handler()
		if hh, ok := h.(http.Handler); ok {
			e.GET("/metrics", echo.WrapHandler(hh), m)
		}
	}

	// WebSocket upgrade endpoint.
	// Auth is handled inside the WS handler via query param token.
	if wsHandlers != nil {
		e.GET("/ws", wsHandlers.HandleWS)
	}

	// Public push ingest endpoint (no auth; token in path is the credential).
	if pushHandler != nil {
		e.POST("/api/push/:token", pushHandler.Receive)
		e.GET("/api/push/:token", pushHandler.Receive)
	}

	// Public status badge endpoints (no auth). These render shields.io-style
	// SVG images (status/uptime/ping) for embedding in external READMEs, the
	// same way Uptime Kuma's badges work — anyone who knows a monitor id can
	// read that monitor's status/uptime/ping via its badge image.
	if badgeHandlers != nil {
		e.GET("/api/badge/:id/status.svg", badgeHandlers.Status)
		e.GET("/api/badge/:id/uptime.svg", badgeHandlers.Uptime)
		e.GET("/api/badge/:id/ping.svg", badgeHandlers.Ping)
	}

	// Embedded frontend SPA static file serving.
	// Overrides Echo's HTTPErrorHandler so that 404s for non-API/WS paths
	// serve index.html (SPA fallback). API and WS 404s pass through to
	// the default handler. Manual file serving avoids http.FileServer's
	// directory redirect (301) behavior.
	//
	// statusMeta + publicOrigin are closed over so only the HTML fallback
	// gets per-page OG tags; JS/CSS assets stay byte-identical (Sprint C R2.8).
	if _, statErr := fs.Stat(webAssets, "web/dist/index.html"); statErr == nil {
		distFS, subErr := fs.Sub(webAssets, "web/dist")
		if subErr == nil {
			prevHandler := e.HTTPErrorHandler
			e.HTTPErrorHandler = func(err error, c echo.Context) {
				if c.Response().Committed {
					return
				}
				he, ok := err.(*echo.HTTPError)
				if ok && he.Code == http.StatusNotFound {
					path := c.Request().URL.Path
					if !strings.HasPrefix(path, "/api") && !strings.HasPrefix(path, "/ws") {
						servePath := strings.TrimPrefix(path, "/")
						if servePath == "" {
							servePath = "index.html"
						}
						if serveErr := serveEmbeddedFile(c, distFS, servePath, statusMeta, publicOrigin); serveErr == nil {
							return
						}
					}
				}
				prevHandler(err, c)
			}
		}
	}

	return e
}

// serveEmbeddedFile serves a file from the embedded FS directly to the response.
// Falls back to index.html for SPA client-side routes. Avoids http.FileServer
// which issues 301 redirects for directory paths.
func serveEmbeddedFile(c echo.Context, fsys fs.FS, reqPath string, statusMeta StatusPageMetaResolver, publicOrigin string) error {
	// Try to open the requested file.
	f, err := fsys.Open(reqPath)
	if err != nil {
		// File not found → serve index.html (SPA fallback).
		return serveFileContent(c, fsys, "index.html", statusMeta, publicOrigin)
	}
	defer func() { _ = f.Close() }()

	stat, err := f.Stat()
	if err != nil {
		return serveFileContent(c, fsys, "index.html", statusMeta, publicOrigin)
	}

	// If it's a directory, try index.html inside it, then fall back to root index.html.
	if stat.IsDir() {
		indexPath := strings.TrimSuffix(reqPath, "/") + "/index.html"
		if _, err := fsys.Open(indexPath); err == nil {
			return serveFileContent(c, fsys, indexPath, statusMeta, publicOrigin)
		}
		return serveFileContent(c, fsys, "index.html", statusMeta, publicOrigin)
	}

	return serveFileContent(c, fsys, reqPath, statusMeta, publicOrigin)
}

// serveFileContent serves a single file from the embedded FS using Echo's response.
// When name is index.html, page-specific OG/Twitter metadata is injected for
// published public status pages (slug or custom-domain root). Asset bytes are
// never transformed.
func serveFileContent(c echo.Context, fsys fs.FS, name string, statusMeta StatusPageMetaResolver, publicOrigin string) error {
	data, err := fs.ReadFile(fsys, name)
	if err != nil {
		return echo.NewHTTPError(http.StatusNotFound, "Not Found")
	}

	if name == "index.html" || strings.HasSuffix(name, "/index.html") {
		origin := strings.TrimRight(strings.TrimSpace(publicOrigin), "/")
		if origin == "" {
			scheme := "http"
			if c.Request().TLS != nil {
				scheme = "https"
			}
			if xf := c.Request().Header.Get("X-Forwarded-Proto"); xf != "" {
				scheme = xf
			}
			origin = scheme + "://" + c.Request().Host
		}
		data = InjectStatusPageMeta(
			c.Request().Context(),
			statusMeta,
			c.Request().URL.Path,
			c.Request().Host,
			origin,
			data,
		)
	}

	ct := mime.TypeByExtension(filepath.Ext(name))
	if ct == "" {
		ct = "text/html; charset=utf-8"
	}
	return c.Blob(http.StatusOK, ct, data)
}
