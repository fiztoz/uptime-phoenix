package probe

import (
	"bytes"
	"crypto/sha256"
	"reflect"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestAcknowledgementCodecPreservesExactPayload(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	note := "Investigating"
	c := domain.ProbeAlertAcknowledgement{CommandID: "41111111-2222-4333-8444-555555555555", ProbeID: "21111111-2222-4333-8444-555555555555", SourceAlertID: "51111111-2222-4333-8444-555555555555", AssignmentGeneration: 9007199254740993, CreatedAt: at, ExpiresAt: at.Add(time.Hour), ActorDisplayName: "Operator", Note: &note}
	codec := AcknowledgementCodec{}
	payload, err := codec.EncodeAcknowledgement(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := codec.DecodeAcknowledgement(t.Context(), payload)
	c.PayloadHash = sha256.Sum256(payload)
	if err != nil || !reflect.DeepEqual(got, c) {
		t.Fatalf("typed ACK changed: %+v %v", got, err)
	}
	spaced := bytes.Replace(payload, []byte(`,"kind"`), []byte(",\n  \"kind\""), 1)
	got, err = codec.DecodeAcknowledgement(t.Context(), spaced)
	if err != nil || got.PayloadHash == c.PayloadHash {
		t.Fatal("raw byte identity was normalized", err)
	}
	for _, bad := range [][]byte{
		append([]byte(" \n"), payload...),
		bytes.Replace(payload, []byte(`"kind":"alert.ack"`), []byte(`"kind":"shell.exec"`), 1),
		bytes.Replace(payload, []byte(`"note":"Investigating"`), []byte(`"note":"Investigating","shell":"sh"`), 1),
		bytes.Replace(payload, []byte(`"9007199254740993"`), []byte(`null`), 1),
	} {
		if _, err := codec.DecodeAcknowledgement(t.Context(), bad); err == nil {
			t.Fatal("invalid control payload decoded")
		}
	}
}
