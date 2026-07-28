package middleware

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"
	"golang.org/x/time/rate"
)

// RateLimitConfig configures API ingress rate limiting (token bucket per client key).
type RateLimitConfig struct {
	// RequestsPerSecond is the sustained rate per key (IP or authenticated subject).
	RequestsPerSecond float64
	// Burst is the maximum tokens per key.
	Burst int
	// RedisURL when set uses a Redis-backed limiter (shared across API pods).
	RedisURL string
}

// DefaultRateLimitConfig is suitable for single-pod or low-traffic deployments.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		RequestsPerSecond: 50,
		Burst:             100,
	}
}

// RateLimit returns middleware that limits /api traffic. Health probes are exempt.
func RateLimit(cfg RateLimitConfig) echo.MiddlewareFunc {
	if cfg.RequestsPerSecond <= 0 {
		cfg.RequestsPerSecond = DefaultRateLimitConfig().RequestsPerSecond
	}
	if cfg.Burst <= 0 {
		cfg.Burst = DefaultRateLimitConfig().Burst
	}

	var redisLimiter *redisRateLimiter
	if cfg.RedisURL != "" {
		if rl, err := newRedisRateLimiter(cfg.RedisURL, cfg.RequestsPerSecond, cfg.Burst); err == nil {
			redisLimiter = rl
		}
	}

	mem := &memoryRateLimiter{
		limiters: make(map[string]*rate.Limiter),
		r:        cfg.RequestsPerSecond,
		b:        cfg.Burst,
	}

	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			p := c.Request().URL.Path
			// Limit API ingress only — static SPA assets must not be throttled.
			if !strings.HasPrefix(p, "/api/") {
				return next(c)
			}
			if p == "/api/health/live" || p == "/api/health/ready" {
				return next(c)
			}

			key := clientRateLimitKey(c)
			var allowed bool
			if redisLimiter != nil {
				allowed = redisLimiter.allow(c.Request().Context(), key)
			} else {
				allowed = mem.allow(key)
			}
			if !allowed {
				return c.JSON(http.StatusTooManyRequests, map[string]string{
					"error": "rate limit exceeded",
				})
			}
			return next(c)
		}
	}
}

func clientRateLimitKey(c echo.Context) string {
	if sub, ok := c.Get(ContextUserIDKey).(int64); ok && sub > 0 {
		return "user:" + strconv.FormatInt(sub, 10)
	}
	return "ip:" + c.RealIP()
}

type memoryRateLimiter struct {
	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	r        float64
	b        int
}

func (m *memoryRateLimiter) allow(key string) bool {
	m.mu.Lock()
	lim, ok := m.limiters[key]
	if !ok {
		lim = rate.NewLimiter(rate.Limit(m.r), m.b)
		m.limiters[key] = lim
	}
	m.mu.Unlock()
	return lim.Allow()
}

type redisRateLimiter struct {
	client *redis.Client
	rps    float64
	burst  int
}

func newRedisRateLimiter(url string, rps float64, burst int) (*redisRateLimiter, error) {
	opts, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	return &redisRateLimiter{
		client: redis.NewClient(opts),
		rps:    rps,
		burst:  burst,
	}, nil
}

// allow implements a per-second fixed window in Redis for cross-pod limiting.
func (r *redisRateLimiter) allow(ctx context.Context, key string) bool {
	window := time.Now().UTC().Truncate(time.Second).Unix()
	redisKey := "phoenix:ratelimit:" + key + ":" + strconv.FormatInt(window, 10)
	pipe := r.client.Pipeline()
	incr := pipe.Incr(ctx, redisKey)
	pipe.Expire(ctx, redisKey, 2*time.Second)
	if _, err := pipe.Exec(ctx); err != nil {
		return true // fail open on Redis errors
	}
	count, err := incr.Result()
	if err != nil {
		return true
	}
	max := int64(r.rps) + int64(r.burst)
	if max < 1 {
		max = 1
	}
	return count <= max
}
