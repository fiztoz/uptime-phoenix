package ports

// DatabaseConfig holds the database connection configuration.
type DatabaseConfig struct {
	Engine   string // "mariadb" or "sqlite"
	DSN      string // connection string or file path
	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLMode  string
}

// RedisConfig holds the Redis connection configuration.
type RedisConfig struct {
	Enabled  bool
	URL      string
	Password string
	DB       int
}

// HTTPConfig holds the HTTP server configuration.
type HTTPConfig struct {
	Port        int
	Host        string
	CORSOrigins []string
}

// AuthConfig holds authentication configuration.
type AuthConfig struct {
	JWTSecret          string
	JWTExpirationHours int
	BcryptCost         int
	TOTPIssuer         string
}

// ConfigProvider aggregates all configuration sections.
type ConfigProvider interface {
	DatabaseConfig() DatabaseConfig
	RedisConfig() RedisConfig
	HTTPConfig() HTTPConfig
	AuthConfig() AuthConfig
	IsProduction() bool
	LogLevel() string
}
