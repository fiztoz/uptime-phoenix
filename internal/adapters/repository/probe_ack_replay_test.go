package repository_test

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/adapters/probe"
	"github.com/fiztoz/uptime-phoenix/internal/adapters/repository"
	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
	"github.com/fiztoz/uptime-phoenix/internal/core/services"
)

func TestProbeCommandReplay(t *testing.T) {
	for _, engine := range []string{"sqlite", "mariadb"} {
		t.Run(engine, func(t *testing.T) {
			for _, before := range []bool{false, true} {
				name := "TelemetryBeforeResult"
				if before {
					name = "ResultBeforeTelemetry"
				}
				t.Run(name, func(t *testing.T) {
					f := newCommandFixture(t, engine)
					f.store.SetCommands(f.protector, probe.AcknowledgementCodec{})
					f.issue(t)
					if _, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second); err != nil {
						t.Fatal(err)
					}
					at := time.Now().UTC().Truncate(time.Microsecond)
					outcome := domain.ProbeCommandOutcome{CommandID: f.ack.CommandID, Status: "applied", AppliedAt: &at, Message: "Incident acknowledged"}
					if before {
						if err := f.commands.CompleteCommand(t.Context(), f.session, outcome); err != nil {
							t.Fatal(err)
						}
					}
					e := f.incident(2, 1)
					e.ObservedAt = at
					e.Incident.Status = domain.AlertStatusAcked
					e.Incident.TransitionVersion = 2
					e.Incident.AckedAt = &at
					e.Incident.AckCommandID = f.ack.CommandID
					e.Incident.AckActorDisplayName = f.ack.ActorDisplayName
					e.Incident.AckNote = f.ack.Note
					// The accepted opening still authorizes this precise ACK after
					// the assignment closes. No new incident is authorized this way.
					if _, err := f.f.db.ExecContext(t.Context(), "UPDATE monitor_probe_assignment_history SET ended_at = ?", f.at.Add(time.Second)); err != nil {
						t.Fatal(err)
					}
					b := f.batch(e)
					got := f.ingest(t, b)
					if got.AcceptedCount != 1 || len(got.Rejected) != 0 {
						t.Fatalf("issued ACK rejected: %+v", got)
					}
					if !before {
						c, err := f.commands.GetCommand(t.Context(), f.session.HubID, f.session.ProbeID, f.ack.CommandID)
						if err != nil || c.RemoteConfirmed || c.Status != "pending" {
							t.Fatal("telemetry fabricated command confirmation", err)
						}
						if err := f.commands.CompleteCommand(t.Context(), f.session, outcome); err != nil {
							t.Fatal(err)
						}
					}
					got = f.ingest(t, b)
					if got.AcceptedCount != 0 || got.DuplicateCount != 1 {
						t.Fatal("ACK replay not idempotent", got)
					}
					recovery := *e.Incident
					recovery.Status = domain.AlertStatusResolved
					recovery.TransitionVersion = 3
					rollback := f.at.Add(-time.Hour)
					recovery.ResolvedAt = &rollback
					r := domain.ProbeReplayEvent{Seq: 3, Kind: domain.ReplayKindAlertTransition, ObservedAt: rollback, Incident: &recovery}
					got = f.ingest(t, f.batch(r))
					if got.AcceptedCount != 1 || len(got.Rejected) != 0 {
						t.Fatalf("ACK recovery rejected: %+v", got)
					}
					var row struct {
						Status              string
						TransitionVersion   int64
						AckCommandID        string
						AckActorDisplayName string
						AckNote             *string
					}
					if err := f.f.db.NewRaw("SELECT status, transition_version, ack_command_id, ack_actor_display_name, ack_note FROM probe_incidents WHERE source_alert_id = ?", f.ack.SourceAlertID).Scan(t.Context(), &row); err != nil {
						t.Fatal(err)
					}
					if row.Status != "resolved" || row.TransitionVersion != 3 || row.AckCommandID != f.ack.CommandID || row.AckActorDisplayName != f.ack.ActorDisplayName || !reflect.DeepEqual(row.AckNote, f.ack.Note) {
						t.Fatalf("ACK metadata lost: %+v", row)
					}
					for _, table := range []string{"alerts", "probe_delivery_intents"} {
						if replayCount(t, f.f, table) != 0 {
							t.Fatal("hub replay dispatched source work", table)
						}
					}
				})
			}
			for _, kind := range []string{"unissued", "unsent", "actor", "note", "other stream", "rejected result", "corrupt ciphertext", "late rollback"} {
				t.Run(kind, func(t *testing.T) {
					f := newCommandFixture(t, engine)
					f.store.SetCommands(f.protector, probe.AcknowledgementCodec{})
					if kind != "unissued" {
						f.issue(t)
						if kind != "unsent" {
							if _, err := f.commands.ClaimCommand(t.Context(), f.session, time.Second); err != nil {
								t.Fatal(err)
							}
						}
					}
					at := time.Now().UTC().Truncate(time.Microsecond)
					e := f.incident(2, 1)
					e.ObservedAt = at
					e.Incident.Status = domain.AlertStatusAcked
					e.Incident.TransitionVersion = 2
					e.Incident.AckedAt = &at
					e.Incident.AckCommandID = f.ack.CommandID
					e.Incident.AckActorDisplayName = f.ack.ActorDisplayName
					e.Incident.AckNote = f.ack.Note
					switch kind {
					case "actor":
						e.Incident.AckActorDisplayName = "Forged"
					case "note":
						e.Incident.AckNote = nil
					case "other stream":
						if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_commands SET stream_id = ?", probeRegistryID3); err != nil {
							t.Fatal(err)
						}
					case "rejected result":
						if err := f.commands.CompleteCommand(t.Context(), f.session, domain.ProbeCommandOutcome{CommandID: f.ack.CommandID, Status: "rejected", Code: "target_not_found"}); err != nil {
							t.Fatal(err)
						}
					case "corrupt ciphertext":
						if _, err := f.f.db.ExecContext(t.Context(), "UPDATE probe_commands SET protected_payload = ?", []byte("corrupt")); err != nil {
							t.Fatal(err)
						}
					case "late rollback":
						defer injectOutboxFailure(t, f.f, "probe_streams", "UPDATE")()
					}
					got, err := f.store.IngestReplayBatch(t.Context(), f.session, f.batch(e), &services.AccessService{})
					if kind == "corrupt ciphertext" || kind == "late rollback" {
						if err == nil {
							t.Fatal("failed authority/write acknowledged")
						}
						if replayCount(t, f.f, "probe_telemetry_receipts") != 1 {
							t.Fatal("failed transaction retained receipt")
						}
					} else if err != nil || got.AcceptedCount != 0 || len(got.Rejected) != 1 || got.Rejected[0].Code != "acknowledgement_unauthorized" {
						t.Fatalf("unauthorized ACK result=%+v err=%v", got, err)
					}
					var status string
					if err := f.f.db.NewRaw("SELECT status FROM probe_incidents WHERE source_alert_id = ?", f.ack.SourceAlertID).Scan(t.Context(), &status); err != nil || status != "firing" {
						t.Fatal("unauthorized/failed ACK mutated incident", status, err)
					}
				})
			}
			t.Run("ServiceIssuanceRetries", func(t *testing.T) { testCommandService(t, newCommandFixture(t, engine)) })
		})
	}
}

func testCommandService(t *testing.T, f commandFixture) {
	s, err := services.NewProbeCommandService(f.commands, repository.NewProbeConnectorStore(f.f.db), f.protector, probe.AcknowledgementCodec{})
	if err != nil {
		t.Fatal(err)
	}
	issue := domain.ProbeAcknowledgementIssue{CommandID: f.ack.CommandID, HubID: f.session.HubID, ProbeID: f.ack.ProbeID, SourceAlertID: f.ack.SourceAlertID, AssignmentGeneration: f.ack.AssignmentGeneration, ActorDisplayName: f.ack.ActorDisplayName, Note: f.ack.Note, Lifetime: time.Hour}
	// Independent callers race before an immutable creation timestamp exists.
	var wg sync.WaitGroup
	results := make(chan *domain.ProbeCommand, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); r, e := s.IssueAcknowledgement(t.Context(), issue); results <- r; errs <- e }()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var original *domain.ProbeCommand
	for r := range results {
		if original == nil {
			original = r
		} else if !reflect.DeepEqual(r, original) {
			t.Fatal("racing callers replaced original identity")
		}
	}
	changed := issue
	changed.ActorDisplayName = "Different"
	if _, err := s.IssueAcknowledgement(t.Context(), changed); !errors.Is(err, ports.ErrConflict) {
		t.Fatal("changed actor reused UUID", err)
	}
	dispatch, err := s.NextCommand(t.Context(), f.session, time.Second)
	if err != nil || dispatch == nil {
		t.Fatal("durable dispatch absent", err)
	}
	ack, err := (probe.AcknowledgementCodec{}).DecodeAcknowledgement(t.Context(), dispatch.Payload)
	if err != nil || ack.CommandID != issue.CommandID || ack.ActorDisplayName != issue.ActorDisplayName {
		t.Fatal("wrong request dispatched", err)
	}
	if err := s.RecordCommandResult(t.Context(), f.session, domain.ProbeCommandOutcome{CommandID: issue.CommandID, Status: "applied", AppliedAt: &original.CreatedAt}); err != nil {
		t.Fatal(err)
	}
	r, err := s.IssueAcknowledgement(t.Context(), issue)
	if err != nil || !r.RemoteConfirmed || r.Status != "applied" || !r.CreatedAt.Equal(original.CreatedAt) {
		t.Fatal("retry lost immutable outcome", err)
	}
}
