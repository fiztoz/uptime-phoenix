package middleware

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
)

const credentialLimiterMaxKeys = 4096

// CredentialRateLimitConfig configures a fixed-window limiter for expensive
// public credential-verification endpoints.
type CredentialRateLimitConfig struct {
	MaxAttempts int
	Window      time.Duration
	RedisURL    string
}

// DefaultCredentialRateLimitConfig allows five attempts per IP and protected
// resource per minute. This is intentionally much tighter than API ingress.
func DefaultCredentialRateLimitConfig() CredentialRateLimitConfig {
	return CredentialRateLimitConfig{
		MaxAttempts: 5,
		Window:      time.Minute,
	}
}

// CredentialRateLimit limits attempts by client IP and route slug. When Redis
// is configured it shares counters across API pods; Redis failures fall back to
// the local limiter so credential checks never become completely unthrottled.
func CredentialRateLimit(cfg CredentialRateLimitConfig) echo.MiddlewareFunc {
	defaults := DefaultCredentialRateLimitConfig()
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaults.MaxAttempts
	}
	if cfg.Window <= 0 {
		cfg.Window = defaults.Window
	}

	local := &credentialMemoryLimiter{
		entries:     make(map[string]credentialAttemptWindow),
		maxAttempts: cfg.MaxAttempts,
		window:      cfg.Window,
	}
	var shared *credentialRedisLimiter
	if cfg.RedisURL != "" {
		if limiter, err := newCredentialRedisLimiter(cfg); err == nil {
			shared = limiter
		}
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			key := c.RealIP() + ":" + c.Param("slug")
			allowed := false
			if shared != nil {
				var err error
				allowed, err = shared.allow(c.Request().Context(), key)
				if err != nil {
					allowed = local.allow(key, time.Now().UTC())
				}
			} else {
				allowed = local.allow(key, time.Now().UTC())
			}
			if !allowed {
				return c.JSON(http.StatusTooManyRequests, map[string]string{
					"error": "too many access-code attempts",
				})
			}
			return next(c)
		}
	}
}

type credentialAttemptWindow struct {
	count   int
	resetAt time.Time
}

type credentialMemoryLimiter struct {
	mu          sync.Mutex
	entries     map[string]credentialAttemptWindow
	maxAttempts int
	window      time.Duration
}

func (m *credentialMemoryLimiter) allow(key string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, ok := m.entries[key]
	if !ok && len(m.entries) >= credentialLimiterMaxKeys {
		for existingKey, existing := range m.entries {
			if !now.Before(existing.resetAt) {
				delete(m.entries, existingKey)
			}
		}
		if len(m.entries) >= credentialLimiterMaxKeys {
			return false
		}
	}
	if !ok || !now.Before(entry.resetAt) {
		m.entries[key] = credentialAttemptWindow{count: 1, resetAt: now.Add(m.window)}
		return true
	}
	if entry.count >= m.maxAttempts {
		return false
	}
	entry.count++
	m.entries[key] = entry
	return true
}

type credentialRedisLimiter struct {
	client      *redis.Client
	maxAttempts int64
	window      time.Duration
}

func newCredentialRedisLimiter(cfg CredentialRateLimitConfig) (*credentialRedisLimiter, error) {
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return nil, err
	}
	return &credentialRedisLimiter{
		client:      redis.NewClient(opts),
		maxAttempts: int64(cfg.MaxAttempts),
		window:      cfg.Window,
	}, nil
}

func (r *credentialRedisLimiter) allow(ctx context.Context, key string) (bool, error) {
	windowSeconds := int64(r.window / time.Second)
	if windowSeconds < 1 {
		windowSeconds = 1
	}
	windowID := time.Now().UTC().Unix() / windowSeconds
	redisKey := "phoenix:credential-limit:" + key + ":" + strconv.FormatInt(windowID, 10)

	pipe := r.client.Pipeline()
	count := pipe.Incr(ctx, redisKey)
	pipe.Expire(ctx, redisKey, r.window+time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		return false, err
	}
	value, err := count.Result()
	if err != nil {
		return false, err
	}
	return value <= r.maxAttempts, nil
}
