package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

func TestSubscriberTokenCodec_PurposeBound(t *testing.T) {
	c := NewSubscriberTokenCodec("test-secret-key-32-bytes-long!!")
	tok, err := c.IssueConfirmation(42, time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	id, err := c.Verify(tok, ports.SubscriberTokenConfirm)
	if err != nil || id != 42 {
		t.Fatalf("confirm verify: id=%d err=%v", id, err)
	}
	if _, err := c.Verify(tok, ports.SubscriberTokenUnsubscribe); !errors.Is(err, ports.ErrSubscriberToken) {
		t.Fatalf("wrong purpose should fail, got %v", err)
	}

	unsub, err := c.IssueUnsubscribe(42)
	if err != nil {
		t.Fatal(err)
	}
	id, err = c.Verify(unsub, ports.SubscriberTokenUnsubscribe)
	if err != nil || id != 42 {
		t.Fatalf("unsub verify: id=%d err=%v", id, err)
	}
	if _, err := c.Verify(unsub, ports.SubscriberTokenConfirm); !errors.Is(err, ports.ErrSubscriberToken) {
		t.Fatalf("unsub as confirm should fail, got %v", err)
	}
}

func TestSubscriberTokenCodec_Expired(t *testing.T) {
	c := NewSubscriberTokenCodec("test-secret-key-32-bytes-long!!")
	tok, err := c.IssueConfirmation(7, time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Verify(tok, ports.SubscriberTokenConfirm); !errors.Is(err, ports.ErrSubscriberToken) {
		t.Fatalf("expired token should fail, got %v", err)
	}
}

func TestSubscriberTokenCodec_DifferentSecrets(t *testing.T) {
	a := NewSubscriberTokenCodec("secret-a")
	b := NewSubscriberTokenCodec("secret-b")
	tok, _ := a.IssueUnsubscribe(1)
	if _, err := b.Verify(tok, ports.SubscriberTokenUnsubscribe); !errors.Is(err, ports.ErrSubscriberToken) {
		t.Fatalf("cross-secret verify should fail, got %v", err)
	}
}
