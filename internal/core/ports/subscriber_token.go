package ports

import (
	"fmt"
	"time"
)

// Subscriber token purposes — purpose-bound JWTs cannot be swapped.
const (
	// SubscriberTokenConfirm activates a pending email subscription.
	SubscriberTokenConfirm = "subscriber_confirm"
	// SubscriberTokenUnsubscribe permanently removes a subscriber.
	SubscriberTokenUnsubscribe = "subscriber_unsubscribe"
)

// ErrSubscriberToken is returned for expired, wrong-purpose, or malformed tokens.
var ErrSubscriberToken = fmt.Errorf("invalid subscriber token")

// SubscriberTokenCodec issues and verifies purpose-bound bearer tokens for
// status-page email subscription flows. Implementations must not store
// plaintext tokens; only the signed JWT leaves the process.
type SubscriberTokenCodec interface {
	// IssueConfirmation signs a short-lived confirmation token for subscriberID.
	// expiresAt is the absolute UTC expiry (typically now+24h).
	IssueConfirmation(subscriberID int64, expiresAt time.Time) (token string, err error)
	// IssueUnsubscribe signs a long-lived unsubscribe token for subscriberID.
	// The token becomes useless once the subscriber row is deleted.
	IssueUnsubscribe(subscriberID int64) (token string, err error)
	// Verify validates signature, expiry, and purpose, returning the subscriber ID.
	Verify(token string, expectedPurpose string) (subscriberID int64, err error)
}
