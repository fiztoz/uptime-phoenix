package ws

import (
	"testing"
)

// TestSendAfterCloseDoesNotPanic guards the disconnect race: broadcast may still
// hold a client pointer after readPump has closed client.send.
func TestSendAfterCloseDoesNotPanic(t *testing.T) {
	h := &Hub{}
	c := NewClient("c1", 1)
	c.closed.Store(true)
	close(c.send)
	// Must not panic.
	h.send(c, []byte(`{"type":"ping"}`))
}
