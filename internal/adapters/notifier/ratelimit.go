package notifier

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// extractRateLimit detects HTTP 429 responses and parses the Retry-After header.
// Supports both integer seconds and RFC1123 HTTP-date format.
// Returns the duration to wait (0 if not rate limited).
func extractRateLimit(resp *http.Response) time.Duration {
	if resp.StatusCode != 429 {
		return 0
	}
	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return 2 * time.Second
	}
	if secs, err := strconv.Atoi(ra); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}
	// Try HTTP-date formats
	for _, layout := range []string{time.RFC1123, time.RFC1123Z, time.RFC850, time.ANSIC} {
		if t, err := time.Parse(layout, ra); err == nil {
			if d := time.Until(t); d > 0 {
				return d
			}
		}
	}
	return 2 * time.Second
}

// retryWithBackoff executes fn. On 429 it parses delay, sleeps, and retries exactly once.
// Respects context cancellation. Closes body on rate-limit response before retry.
func retryWithBackoff(ctx context.Context, fn func(context.Context) (*http.Response, error)) (*http.Response, error) {
	resp, err := fn(ctx)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 429 {
		return resp, nil
	}
	delay := extractRateLimit(resp)
	_ = resp.Body.Close()
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		}
	} else {
		time.Sleep(1 * time.Second)
	}
	// retry once
	return fn(ctx)
}
