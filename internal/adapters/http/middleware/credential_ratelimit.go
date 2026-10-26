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
	// Scope namespaces the counters so two limiters (status-page access codes
	// and sign-in, say) never share a budget. Empty keeps the original keys.
	Scope string
	// Message is the 429 error text. Empty keeps the access-code wording.
	Message string
	// RefundSuccess gives an attempt back when the handler answers below 400,
	// so only failed attempts spend the budget. Sign-in uses it: many people
	// behind one NAT, or one test suite, can log in often, while guessing
	// still runs out of attempts.
	RefundSuccess bool
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
	if cfg.Message == "" {
		cfg.Message = "too many access-code attempts"
	}
	scope := ""
	if cfg.Scope != "" {
		scope = cfg.Scope + ":"
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
			key := scope + c.RealIP() + ":" + c.Param("slug")
			allowed := false
			refund := func() { local.refund(key, time.Now().UTC()) }
			if shared != nil {
				redisKey := shared.windowKey(key, time.Now().UTC())
				var err error
				allowed, err = shared.allow(c.Request().Context(), redisKey)
				if err != nil {
					allowed = local.allow(key, time.Now().UTC())
				} else {
					refund = func() { shared.refund(context.WithoutCancel(c.Request().Context()), redisKey) }
				}
			} else {
				allowed = local.allow(key, time.Now().UTC())
			}
			if !allowed {
				return c.JSON(http.StatusTooManyRequests, map[string]string{
					"error": cfg.Message,
				})
			}
			err := next(c)
			if cfg.RefundSuccess && err == nil && c.Response().Committed && c.Response().Status < http.StatusBadRequest {
				refund()
			}
			return err
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

// refund returns one attempt to key's current window.
func (m *credentialMemoryLimiter) refund(key string, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[key]
	if !ok || !now.Before(entry.resetAt) || entry.count == 0 {
		return
	}
	entry.count--
	m.entries[key] = entry
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

// windowKey is the Redis counter for key in the fixed window containing now.
func (r *credentialRedisLimiter) windowKey(key string, now time.Time) string {
	windowSeconds := int64(r.window / time.Second)
	if windowSeconds < 1 {
		windowSeconds = 1
	}
	windowID := now.Unix() / windowSeconds
	return "phoenix:credential-limit:" + key + ":" + strconv.FormatInt(windowID, 10)
}

func (r *credentialRedisLimiter) allow(ctx context.Context, redisKey string) (bool, error) {
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

// credentialRefundScript decrements a window counter only while it still
// exists and is positive. A plain DECR on a key whose window already expired
// would create it at -1 with no TTL: a permanent key that also hands the
// next window one extra attempt.
var credentialRefundScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v and tonumber(v) > 0 then
	return redis.call('DECR', KEYS[1])
end
return 0
`)

// refund gives back the attempt allow counted. A failed refund only leaves
// the window one attempt tighter, so the error is dropped.
func (r *credentialRedisLimiter) refund(ctx context.Context, redisKey string) {
	_ = credentialRefundScript.Run(ctx, r.client, []string{redisKey}).Err()
}
