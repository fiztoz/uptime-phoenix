// Package bootstrap is the shared composition root for cmd/app, cmd/api, and cmd/worker.
package bootstrap

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/caarlos0/env/v11"
)

// Config holds all configuration loaded from environment variables.
type Config struct {
	Port     int    `env:"PORT" envDefault:"3000"`
	Host     string `env:"HOST" envDefault:"0.0.0.0"`
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`
	DBEngine string `env:"DB_ENGINE" envDefault:"sqlite"`
	DBDSN    string `env:"DB_DSN" envDefault:"file:phoenix.db?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)"`
	// MariaDB pool. Ignored for SQLite (which is one connection). The idle
	// timeout keeps returned sessions from sitting on the server until
	// ConnMaxLifetime.
	DBMaxOpenConns           int    `env:"DB_MAX_OPEN_CONNS" envDefault:"10"`
	DBMaxIdleConns           int    `env:"DB_MAX_IDLE_CONNS" envDefault:"2"`
	DBConnMaxIdleSeconds     int    `env:"DB_CONN_MAX_IDLE_SECONDS" envDefault:"30"`
	DBConnMaxLifetimeSeconds int    `env:"DB_CONN_MAX_LIFETIME_SECONDS" envDefault:"300"`
	JWTSecret                string `env:"JWT_SECRET" envDefault:"change-me-in-production"`
	JWTExpireH               int    `env:"JWT_EXPIRE_HOURS" envDefault:"24"`
	TOTPIssuer               string `env:"TOTP_ISSUER" envDefault:"Phoenix"`
	Production               bool   `env:"PRODUCTION" envDefault:"false"`
	RedisURL                 string `env:"REDIS_URL" envDefault:""`

	// PublicURL is the absolute public origin (http/https) used in status-page
	// subscription emails (confirmation / unsubscribe links). Empty disables
	// subscriptions rather than generating broken relative links. No production
	// default — operators must set it explicitly.
	PublicURL string `env:"PUBLIC_URL" envDefault:""`

	// WebAuthn / passkey relying-party configuration.
	// In production set WEBAUTHN_RP_ID to the registrable domain
	// (e.g. "phoenix.example.com") and WEBAUTHN_RP_ORIGINS to the exact
	// browser origin(s) (comma-separated, e.g. "https://phoenix.example.com").
	WebAuthnRPID      string `env:"WEBAUTHN_RP_ID" envDefault:"localhost"`
	WebAuthnRPName    string `env:"WEBAUTHN_RP_NAME" envDefault:"Phoenix"`
	WebAuthnRPOrigins string `env:"WEBAUTHN_RP_ORIGINS" envDefault:"http://localhost:3000,http://localhost:5173"`

	BootstrapUsername string `env:"BOOTSTRAP_USERNAME" envDefault:""`
	BootstrapPassword string `env:"BOOTSTRAP_PASSWORD" envDefault:""`

	// Mode controls component startup (api | worker | all).
	Mode string `env:"MODE" envDefault:"all"`

	// CORSAllowOrigins is a comma-separated allow-list of CORS origins.
	// Empty means the dev wildcard (*) when PRODUCTION=false, and
	// deny-by-default (no CORS headers at all) when PRODUCTION=true.
	CORSAllowOrigins string `env:"CORS_ALLOW_ORIGINS" envDefault:""`

	// WSAllowedOrigins is a comma-separated list of extra origin HOST patterns
	// (no scheme — e.g. "status.example.com,localhost:5173") permitted to open
	// WebSocket connections in addition to same-origin requests.
	WSAllowedOrigins string `env:"WS_ALLOWED_ORIGINS" envDefault:""`

	// WSAllowAnyOrigin disables the WebSocket origin check entirely. Dev
	// convenience only (the Vite dev server on :5173 talks to the API on
	// :3000); never enable it in production — any website could then open
	// authenticated WebSocket connections from a visitor's browser.
	WSAllowAnyOrigin bool `env:"WS_ALLOW_ANY_ORIGIN" envDefault:"false"`

	// API rate limiting (ingress).
	RateLimitRPS   float64 `env:"RATE_LIMIT_RPS" envDefault:"50"`
	RateLimitBurst int     `env:"RATE_LIMIT_BURST" envDefault:"100"`

	// OpenTelemetry (optional).
	OTELEndpoint string `env:"OTEL_EXPORTER_OTLP_ENDPOINT" envDefault:""`
	OTELService  string `env:"OTEL_SERVICE_NAME" envDefault:"phoenix"`

	// Sharded worker (optional, for multi-worker setups).
	WorkerID       string `env:"WORKER_ID" envDefault:""`           // unique per pod (e.g. hostname)
	ShardBatchSize int    `env:"SHARD_BATCH_SIZE" envDefault:"200"` // max monitors per worker
	ShardLeaseTTL  int    `env:"SHARD_LEASE_TTL" envDefault:"300"`  // lease TTL in seconds
	ShardPollEvery int    `env:"SHARD_POLL_EVERY" envDefault:"30"`  // lease refresh interval in seconds

	// EscalationPollSeconds is how often a worker looks for due escalation steps
	// (F2.3). It bounds how late a step can be, so it must stay well under the
	// smallest useful wait. Exposed mainly so the end-to-end suite can drive the
	// ladder in seconds instead of minutes.
	EscalationPollSeconds int `env:"ESCALATION_POLL_SECONDS" envDefault:"15"`

	// HeartbeatRetentionDays is how long raw heartbeats are kept before the
	// retention ticker deletes them. Default 180. Set to 0 to disable.
	HeartbeatRetentionDays int `env:"HEARTBEAT_RETENTION_DAYS" envDefault:"180"`

	// ── OIDC SSO (Phase 5 Sprint 13) ─────────────────────────────────────
	// Disabled when OIDC_ISSUER is empty. All credentials stay in env/Secrets.
	OIDCIssuer       string `env:"OIDC_ISSUER" envDefault:""`
	OIDCClientID     string `env:"OIDC_CLIENT_ID" envDefault:""`
	OIDCClientSecret string `env:"OIDC_CLIENT_SECRET" envDefault:""`
	// OIDCRedirectURL is the absolute callback URL registered with the IdP
	// (e.g. https://phoenix.example.com/api/auth/oidc/callback). When empty
	// and PUBLIC_URL is set, it is derived as PUBLIC_URL + /api/auth/oidc/callback.
	OIDCRedirectURL string `env:"OIDC_REDIRECT_URL" envDefault:""`
	// OIDCScopes is a comma-separated scope list. Default openid,profile,email.
	OIDCScopes string `env:"OIDC_SCOPES" envDefault:"openid,profile,email"`
	// OIDCGroupsClaim names the ID-token/userinfo claim holding group membership.
	OIDCGroupsClaim string `env:"OIDC_GROUPS_CLAIM" envDefault:"groups"`
	// OIDCJITEnabled creates Phoenix users on first successful OIDC login.
	OIDCJITEnabled bool `env:"OIDC_JIT_ENABLED" envDefault:"true"`
	// OIDCLinkByEmail links an unlinked existing user when the IdP asserts a
	// verified email that matches the username.
	OIDCLinkByEmail bool `env:"OIDC_LINK_BY_EMAIL" envDefault:"false"`
	// OIDCAllowedGroups, when non-empty, requires membership in at least one.
	OIDCAllowedGroups string `env:"OIDC_ALLOWED_GROUPS" envDefault:""`
	OIDCAdminGroups   string `env:"OIDC_ADMIN_GROUPS" envDefault:""`
	// Capability group lists (comma-separated IdP group names).
	OIDCCapNotificationsGroups          string `env:"OIDC_CAP_NOTIFICATIONS_GROUPS" envDefault:""`
	OIDCCapMaintenanceGroups            string `env:"OIDC_CAP_MAINTENANCE_GROUPS" envDefault:""`
	OIDCCapCreateMonitorsGroups         string `env:"OIDC_CAP_CREATE_MONITORS_GROUPS" envDefault:""`
	OIDCCapCreateTopLevelMonitorsGroups string `env:"OIDC_CAP_CREATE_TOP_LEVEL_MONITORS_GROUPS" envDefault:""`
	OIDCCapCreateGroupsGroups           string `env:"OIDC_CAP_CREATE_GROUPS_GROUPS" envDefault:""`
	OIDCCapEditGroupMetadataGroups      string `env:"OIDC_CAP_EDIT_GROUP_METADATA_GROUPS" envDefault:""`
	OIDCCapViewExtensionsGroups         string `env:"OIDC_CAP_VIEW_EXTENSIONS_GROUPS" envDefault:""`
	OIDCCapViewAllMonitorsGroups        string `env:"OIDC_CAP_VIEW_ALL_MONITORS_GROUPS" envDefault:""`
	// OIDCGrantMap is "idp-group:group:5,idp-team:monitor:12" (optional :shallow).
	OIDCGrantMap string `env:"OIDC_GRANT_MAP" envDefault:""`

	// ExtensionsJSON is an optional JSON array of K8s extension catalog
	// entries. Empty or unset → GET /api/extensions returns []. Only id,
	// title, path, and icon are consumed; image, secretName, and credentials
	// are ignored even if present. This is a sidebar iframe hook, not a monitor type.
	ExtensionsJSON string `env:"PHOENIX_EXTENSIONS" envDefault:""`
}

// LoadConfig parses environment into Config.
func LoadConfig() (Config, error) {
	cfg := Config{}
	if err := env.Parse(&cfg); err != nil {
		return Config{}, err
	}
	if err := validatePublicURL(cfg.PublicURL); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validatePublicURL accepts empty (subscriptions disabled) or an absolute
// http/https origin without a path fragment that would break link joining.
func validateJWTExpireHours(hours int) error {
	if hours <= 0 {
		return fmt.Errorf("JWT_EXPIRE_HOURS must be a positive number of hours, got %d", hours)
	}
	return nil
}

func validatePublicURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("PUBLIC_URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("PUBLIC_URL: must be an absolute http or https origin")
	}
	if u.Host == "" {
		return fmt.Errorf("PUBLIC_URL: must include a host")
	}
	return nil
}
