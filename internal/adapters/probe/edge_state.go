package probe

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

type edgeStatePump struct {
	repo            ports.EdgeStateRepository
	session         *Session
	fence           domain.EdgeReplayFence
	minimumRevision int64
	replay          *edgeReplayPump
	mu              sync.Mutex
	ready           bool
	pending         *StateApplied
	pendingReceived bool
	last            *StateApplied
	receipts        chan StateApplied
	wake            chan struct{}
	interval        time.Duration
	timeout         time.Duration
}

func newEdgeStatePump(repo ports.EdgeStateRepository, session *Session, fence domain.EdgeReplayFence, revision int64, replay *edgeReplayPump) *edgeStatePump {
	if replay != nil {
		replay.setStatePending(true)
	}
	return &edgeStatePump{repo: repo, session: session, fence: fence, minimumRevision: revision, replay: replay, receipts: make(chan StateApplied, 1), wake: make(chan struct{}, 1), interval: 15 * time.Second, timeout: StateTransferTimeout}
}

func (p *edgeStatePump) updateHubHealth(h Health) {
	p.mu.Lock()
	p.ready = h.Role == "hub" && h.Ready && h.DBWritable && int64(h.ConfigRevision) >= p.minimumRevision
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func sameStateReceipt(a, b StateApplied) bool {
	return a.StateTransferIdentity == b.StateTransferIdentity && a.SHA256 == b.SHA256 && a.StateCount == b.StateCount
}

func (p *edgeStatePump) handleApplied(generation Decimal, receipt StateApplied) error {
	if generation != Decimal(p.fence.ConnectionGeneration) {
		return ErrHandshakeGeneration
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.pending != nil && sameStateReceipt(*p.pending, receipt) {
		if p.pendingReceived {
			return nil
		}
		select {
		case p.receipts <- receipt:
			p.pendingReceived = true
		default:
		} // Identical duplicate of this in-flight receipt.
		return nil
	}
	if p.last != nil && sameStateReceipt(*p.last, receipt) {
		return nil
	}
	return errors.New("unsolicited current state receipt")
}

func (p *edgeStatePump) run(ctx context.Context) error {
	for {
		p.mu.Lock()
		ready := p.ready
		p.mu.Unlock()
		if !ready {
			if err := p.waitReady(ctx); err != nil {
				return err
			}
			continue
		}
		opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		source, err := p.repo.ReadCurrentSnapshot(opCtx, p.fence, time.Now().UTC())
		cancel()
		if errors.Is(err, ports.ErrNotFound) || err == nil && source.Identity.ConfigRevision < p.minimumRevision {
			if err := p.waitReady(ctx); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		snapshot, document, err := encodeCurrentSnapshot(source)
		if err != nil {
			return err
		}
		receipt, err := prepareStateReceipt(snapshot, document)
		if err != nil {
			return err
		}
		p.mu.Lock()
		p.pending = &receipt
		p.pendingReceived = false
		p.mu.Unlock()
		// Only this actor sends state transfers. The receipt deadline covers all
		// chunks and commit, so a slow receiver cannot accumulate overlapping state.
		transferCtx, done := context.WithTimeout(ctx, p.timeout)
		err = sendCurrentSnapshot(transferCtx, p.session, Decimal(p.fence.ConnectionGeneration), receipt, snapshot, document)
		if err == nil {
			select {
			case accepted := <-p.receipts:
				p.mu.Lock()
				p.pending = nil
				p.last = &accepted
				p.mu.Unlock()
				if p.replay != nil {
					p.replay.setStatePending(false)
				}
			case <-transferCtx.Done():
				err = transferCtx.Err()
			}
		}
		done()
		clear(document)
		if err != nil {
			return err
		}
		timer := time.NewTimer(p.interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *edgeStatePump) waitReady(ctx context.Context) error {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.wake:
	case <-timer.C:
	}
	return nil
}
