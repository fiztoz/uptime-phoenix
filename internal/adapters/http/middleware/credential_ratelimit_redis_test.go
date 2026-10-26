package middleware

import (
	"context"
	"os"
	"testing"
	"time"
)

// newTestRedisLimiter connects to PHOENIX_TEST_REDIS_URL (a scratch Redis
// database; the test only touches its own keys). Without it the test is
// skipped: the in-memory limiter is covered elsewhere.
func newTestRedisLimiter(t *testing.T) *credentialRedisLimiter {
	t.Helper()
	url := os.Getenv("PHOENIX_TEST_REDIS_URL")
	if url == "" {
		t.Skip("PHOENIX_TEST_REDIS_URL not set")
	}
	l, err := newCredentialRedisLimiter(CredentialRateLimitConfig{MaxAttempts: 3, Window: time.Minute, RedisURL: url})
	if err != nil {
		t.Fatalf("redis limiter: %v", err)
	}
	t.Cleanup(func() { _ = l.client.Close() })
	return l
}

// A refund that lands after its window expired used to DECR a missing key,
// leaving "-1" with no TTL forever (and an extra attempt in a reused key).
func TestCredentialRedisRefundNeverCreatesAKey(t *testing.T) {
	l := newTestRedisLimiter(t)
	ctx := context.Background()
	key := l.windowKey("refund-test:"+t.Name(), time.Now().UTC())
	t.Cleanup(func() { _ = l.client.Del(context.Background(), key).Err() })
	_ = l.client.Del(ctx, key).Err()

	l.refund(ctx, key) // window gone
	if n, err := l.client.Exists(ctx, key).Result(); err != nil || n != 0 {
		t.Fatalf("refund on a missing key created it (exists=%d err=%v)", n, err)
	}

	if ok, err := l.allow(ctx, key); err != nil || !ok {
		t.Fatalf("allow = %v, %v", ok, err)
	}
	l.refund(ctx, key)
	l.refund(ctx, key) // a second refund must not go below zero
	v, err := l.client.Get(ctx, key).Int()
	if err != nil || v != 0 {
		t.Fatalf("counter after allow+2 refunds = %d (%v); want 0", v, err)
	}
	if ttl, err := l.client.TTL(ctx, key).Result(); err != nil || ttl <= 0 {
		t.Fatalf("counter TTL = %v (%v); the refund must keep the window's expiry", ttl, err)
	}
}
