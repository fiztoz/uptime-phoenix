package probe

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

func TestConfigInspectorLocalAndRemoteBoundary(t *testing.T) {
	doc := readFixture(t, "valid", "config-snapshot-http.json")
	snapshot, err := DecodeConfigSnapshot(doc)
	if err != nil {
		t.Fatal(err)
	}
	target := domain.ProbeConfigTarget{HubID: snapshot.HubID, ProbeID: snapshot.ProbeID}
	remote, err := (ConfigInspector{}).Inspect(doc, target)
	if err != nil || remote.Revision != 12 {
		t.Fatal("remote preparation failed")
	}
	// Hash original bytes, including whitespace, rather than a reserialized DTO.
	spaced, err := (ConfigInspector{}).Inspect(append(bytes.Clone(doc), ' '), target)
	if err != nil || spaced.SHA256 == remote.SHA256 {
		t.Fatal("original byte identity lost")
	}
	if _, err := (ConfigInspector{}).Inspect(doc, domain.ProbeConfigTarget{HubID: target.HubID, ProbeID: "local"}); err == nil {
		t.Fatal("untrusted payload selected dialect")
	}
	snapshot.ProbeID = "local"
	snapshot.NotificationChannels[0].IncludeAckURL = true
	for _, kind := range []string{"http", "push", "docker"} {
		snapshot.Assignments[0].Monitor.Type = kind
		snapshot.Assignments[0].RequiredCapabilities = []string{"checker." + kind + ".v1"}
		localDoc, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (ConfigInspector{}).Inspect(localDoc, domain.ProbeConfigTarget{HubID: target.HubID, ProbeID: "local"}); err != nil {
			t.Fatalf("local %s: %v", kind, err)
		}
		if _, err := DecodeConfigSnapshot(localDoc); err == nil {
			t.Fatal("remote decoder accepted local dialect")
		}
		// A UUID destination cannot smuggle local behavior into the remote decoder.
		snapshot.ProbeID = target.ProbeID
		remoteDoc, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DecodeConfigSnapshot(remoteDoc); err == nil {
			t.Fatal("remote accepted local ack preference")
		}
		snapshot.ProbeID = "local"
	}
	snapshot.NotificationChannels[0].Version--
	bad, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeLocalConfigSnapshot(bad); err == nil {
		t.Fatal("local snapshot bypassed dependency revision checks")
	}
}
