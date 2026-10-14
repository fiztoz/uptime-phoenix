package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// cutoverStoreFake serves one captured active tuple and poisons repeat reads so
// a second authenticated storage read is detectable instead of invisible. It
// implements both repository ports because the executor resolves the cutover
// dependency from the service's existing repository field.
type cutoverStoreFake struct {
	configReads  int
	primary      domain.EdgeActiveConfig
	secondary    *domain.EdgeActiveConfig
	prior        *domain.ProbeCommandOutcome
	priorErr     error
	captured     []*domain.EdgeActiveConfig
	receiptReads int
	writes       int
}

var (
	_ ports.EdgeConfigRepository          = (*cutoverStoreFake)(nil)
	_ ports.EdgeDeliveryCutoverRepository = (*cutoverStoreFake)(nil)
)

func (*cutoverStoreFake) ActivateConfig(context.Context, domain.EdgeActiveConfig) error {
	return errors.New("unexpected activation")
}

func (f *cutoverStoreFake) ReadActiveConfig(context.Context) (domain.EdgeActiveConfig, error) {
	f.configReads++
	if f.secondary != nil && f.configReads > 1 {
		return *f.secondary, nil
	}
	return f.primary, nil
}

func (f *cutoverStoreFake) ReadDeliveryCutoverReceipt(context.Context, domain.EdgeCommandAuthority, domain.DeliveryCutoverCommand) (*domain.ProbeCommandOutcome, error) {
	f.receiptReads++
	return f.prior, f.priorErr
}

func (f *cutoverStoreFake) ApplyDeliveryCutover(_ context.Context, _ domain.EdgeCommandAuthority, c domain.DeliveryCutoverCommand, capture *domain.EdgeActiveConfig) (domain.ProbeCommandOutcome, error) {
	f.writes++
	f.captured = append(f.captured, capture)
	return domain.ProbeCommandOutcome{CommandID: c.CommandID, Status: "applied", Code: domain.DeliveryCutoverSuccessCode(c.Kind)}, nil
}

// cutoverConfigOnlyFake implements the config repository without the cutover
// gate/journal ports, so an unconfigured composition is refused up front.
type cutoverConfigOnlyFake struct{}

func (cutoverConfigOnlyFake) ActivateConfig(context.Context, domain.EdgeActiveConfig) error {
	return errors.New("unexpected activation")
}
func (cutoverConfigOnlyFake) ReadActiveConfig(context.Context) (domain.EdgeActiveConfig, error) {
	return domain.EdgeActiveConfig{}, errors.New("unexpected config read")
}

type cutoverDecoderFake struct{ resolved *domain.EdgeResolvedConfig }

func (d cutoverDecoderFake) DecodeEdge(context.Context, []byte, domain.ProbeConfigTarget) (*domain.EdgeResolvedConfig, error) {
	return d.resolved, nil
}

type cutoverProtectorFake struct{}

func (cutoverProtectorFake) Seal(context.Context, domain.ProbeConfigMetadata, []byte) ([]byte, error) {
	return nil, errors.New("unexpected seal")
}
func (cutoverProtectorFake) Open(context.Context, domain.ProbeConfigMetadata, []byte) ([]byte, error) {
	return []byte("{}"), nil
}
func (cutoverProtectorFake) KeyHash(string) string { return "k" }

const cutoverTestSetRevision = int64(9007199254740993)

func cutoverTestCommand(t *testing.T, kind string, at time.Time) domain.DeliveryCutoverCommand {
	t.Helper()
	target := domain.ProbeConfigTarget{HubID: "11111111-1111-4111-8111-111111111111", ProbeID: "22222222-2222-4222-8222-222222222222"}
	return domain.DeliveryCutoverCommand{
		ProbeDeliveryModeApplication: domain.ProbeDeliveryModeApplication{ProbeConfigTarget: target,
			MonitorID: 17, AssignmentRevision: cutoverTestSetRevision, Generation: 1, ConfigRevision: 2,
			SHA256:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			StreamID: "33333333-3333-4333-8333-333333333333", AlertDelivery: domain.AlertDeliveryAggregate},
		CommandID: "44444444-4444-4444-8444-444444444444", BarrierID: "55555555-5555-4555-8555-555555555555",
		PreparationCommandID: "66666666-6666-4666-8666-666666666666", Kind: kind,
		CreatedAt: at.Add(-time.Hour), ExpiresAt: at.Add(time.Hour), PayloadHash: [32]byte{1},
	}
}

func cutoverTestAuthority(c domain.DeliveryCutoverCommand) domain.EdgeCommandAuthority {
	return domain.EdgeCommandAuthority{HubID: c.HubID, ProbeID: c.ProbeID, StreamID: c.StreamID, ConnectionGeneration: 2}
}

func cutoverTestResolved(metadata domain.ProbeConfigMetadata, mode domain.AlertDelivery) *domain.EdgeResolvedConfig {
	return &domain.EdgeResolvedConfig{Metadata: metadata, Assignments: []domain.EdgeResolvedAssignment{{
		Monitor: &domain.Monitor{ID: 17, Active: true}, Generation: 1,
		AssignmentRevision: cutoverTestSetRevision, DesiredAlertDelivery: mode,
	}}}
}

// TestDeliveryCutoverReceiptRecoveryPrecedesConfigLoad verifies recovery of an
// exact historical outcome happens before, and instead of, loading any current
// configuration. Recovery is never new eligibility.
func TestDeliveryCutoverReceiptRecoveryPrecedesConfigLoad(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	command := cutoverTestCommand(t, domain.DeliveryQuiesceKind, at)
	prior := domain.ProbeCommandOutcome{CommandID: command.CommandID, Status: "applied", Code: domain.DeliveryQuiescedCode, AppliedAt: &at}
	fake := &cutoverStoreFake{prior: &prior}
	service := &EdgeConfigService{store: fake} // No decoder/protector: any load fails.
	got, err := service.ExecuteDeliveryCutover(t.Context(), cutoverTestAuthority(command), command)
	if err != nil || got.CommandID != command.CommandID || got.Code != domain.DeliveryQuiescedCode || fake.configReads != 0 || fake.writes != 0 {
		t.Fatalf("recovery reinterpreted current state: %+v %v reads=%d writes=%d", got, err, fake.configReads, fake.writes)
	}
}

// TestDeliveryCutoverResumeNeverLoadsConfig proves resume authenticates only its
// stored barrier tuple; it must not read or substitute a current configuration.
func TestDeliveryCutoverResumeNeverLoadsConfig(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	command := cutoverTestCommand(t, domain.DeliveryResumeKind, at)
	fake := &cutoverStoreFake{}
	service := &EdgeConfigService{store: fake}
	if _, err := service.ExecuteDeliveryCutover(t.Context(), cutoverTestAuthority(command), command); err != nil {
		t.Fatal(err)
	}
	if fake.configReads != 0 || fake.writes != 1 || len(fake.captured) != 1 || fake.captured[0] != nil {
		t.Fatalf("resume consulted a configuration: reads=%d captured=%v", fake.configReads, fake.captured)
	}
}

// TestDeliveryCutoverSingleCaptureAuthenticated proves the executor hands the
// one authenticated tuple to storage and never authenticates a second read.
func TestDeliveryCutoverSingleCaptureAuthenticated(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	command := cutoverTestCommand(t, domain.DeliveryQuiesceKind, at)
	metadata := domain.ProbeConfigMetadata{ProbeConfigTarget: command.ProbeConfigTarget, Revision: command.ConfigRevision, SchemaVersion: 1, SHA256: command.SHA256, CreatedAt: at, EffectiveAt: at}
	primary := domain.EdgeActiveConfig{Snapshot: domain.ProtectedProbeConfig{ProbeConfigMetadata: metadata, KeyConfirmation: "k", ProtectedPayload: []byte("{}")}, AppliedAt: at, ConnectionGeneration: 2}
	poison := primary
	poison.Snapshot.KeyConfirmation = "poison"
	fake := &cutoverStoreFake{primary: primary, secondary: &poison}
	service := &EdgeConfigService{store: fake, decoder: cutoverDecoderFake{cutoverTestResolved(metadata, domain.AlertDeliveryAggregate)}, protector: cutoverProtectorFake{}}
	if _, err := service.ExecuteDeliveryCutover(t.Context(), cutoverTestAuthority(command), command); err != nil {
		t.Fatal(err)
	}
	if fake.configReads != 1 || fake.writes != 1 || len(fake.captured) != 1 || fake.captured[0] == nil || fake.captured[0].Snapshot.KeyConfirmation != "k" {
		t.Fatalf("authenticated a second tuple: reads=%d captured=%v", fake.configReads, fake.captured)
	}
}

// TestDeliveryCutoverDesiredBindingRefusedWithoutCapture proves a document that
// does not carry the exact desired binding cannot apply a new configuration; the
// repository receives no capture and must reject without writing success.
func TestDeliveryCutoverDesiredBindingRefusedWithoutCapture(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	command := cutoverTestCommand(t, domain.DeliveryApplyKind, at)
	metadata := domain.ProbeConfigMetadata{ProbeConfigTarget: command.ProbeConfigTarget, Revision: command.ConfigRevision, SchemaVersion: 1, SHA256: command.SHA256, CreatedAt: at, EffectiveAt: at}
	fake := &cutoverStoreFake{primary: domain.EdgeActiveConfig{Snapshot: domain.ProtectedProbeConfig{ProbeConfigMetadata: metadata, KeyConfirmation: "k", ProtectedPayload: []byte("{}")}, AppliedAt: at, ConnectionGeneration: 2}}
	// The document desires both while the command is built for aggregate.
	service := &EdgeConfigService{store: fake, decoder: cutoverDecoderFake{cutoverTestResolved(metadata, domain.AlertDeliveryBoth)}, protector: cutoverProtectorFake{}}
	if _, err := service.ExecuteDeliveryCutover(t.Context(), cutoverTestAuthority(command), command); err != nil {
		t.Fatal(err)
	}
	if fake.writes != 1 || len(fake.captured) != 1 || fake.captured[0] != nil {
		t.Fatalf("unbound document supplied a capture: %v", fake.captured)
	}
}

// TestDeliveryCutoverExecutorRejectsInvalidCommand refuses malformed shapes
// before consulting any repository.
func TestDeliveryCutoverExecutorRejectsInvalidCommand(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	command := cutoverTestCommand(t, domain.DeliveryQuiesceKind, at)
	command.Kind = "delivery.unknown"
	fake := &cutoverStoreFake{}
	service := &EdgeConfigService{store: fake}
	if _, err := service.ExecuteDeliveryCutover(t.Context(), cutoverTestAuthority(command), command); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid command accepted: %v", err)
	}
	if fake.receiptReads != 0 || fake.writes != 0 {
		t.Fatal("invalid command reached storage")
	}
}

// TestNewEdgeDeliveryCutoverExecutorRequiresJournal refuses a store that cannot
// fence source gates and provider attempts.
func TestNewEdgeDeliveryCutoverExecutorRequiresJournal(t *testing.T) {
	if _, err := NewEdgeDeliveryCutoverExecutor(nil); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("nil service accepted: %v", err)
	}
	if _, err := NewEdgeDeliveryCutoverExecutor(&EdgeConfigService{store: cutoverConfigOnlyFake{}}); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("gate-less store accepted: %v", err)
	}
	executor, err := NewEdgeDeliveryCutoverExecutor(&EdgeConfigService{store: &cutoverStoreFake{}})
	if err != nil || executor == nil {
		t.Fatalf("configured store refused: %v", err)
	}
}
