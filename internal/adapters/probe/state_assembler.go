package probe

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

// StateTransfer stages one snapshot for a single session reader. It is not safe
// for concurrent use. The owner must discard it on session cancellation/timeout,
// and supply times from the same local monotonic clock to all operations.
// There is no repository, active projection, cursor, or notification authority here.
type StateTransfer struct {
	begin      StateBegin
	generation Decimal
	deadline   time.Time
	chunks     [][]byte
	totalBytes int
	received   int
	closed     bool
}

// NewStateTransfer decodes a begin frame and starts its fixed staging deadline.
// The caller must first authenticate the stream and fence the envelope generation.
func NewStateTransfer(data []byte, now time.Time) (*StateTransfer, error) {
	envelope, begin, err := DecodeStateBegin(data)
	if err != nil {
		return nil, err
	}
	return &StateTransfer{
		begin: begin, generation: envelope.ConnectionGeneration,
		deadline: now.Add(StateTransferTimeout), chunks: make([][]byte, begin.ChunkCount),
	}, nil
}

// Discard releases staged chunks and makes the transfer permanently unusable.
// An interrupted transfer cannot change previously applied state.
func (s *StateTransfer) Discard() {
	s.chunks = nil
	s.totalBytes = 0
	s.received = 0
	s.closed = true
}

// AddChunk accepts out-of-order chunks and identical retries. Any failure aborts
// staging; a retry never extends the deadline or counts the same bytes twice.
func (s *StateTransfer) AddChunk(data []byte, now time.Time) (err error) {
	defer func() {
		if err != nil {
			s.Discard()
		}
	}()
	if err := s.checkOpen(now); err != nil {
		return err
	}
	envelope, chunk, err := DecodeStateChunk(data)
	if err != nil {
		return err
	}
	if chunk.StateTransferIdentity != s.begin.StateTransferIdentity || envelope.ConnectionGeneration != s.generation {
		return errors.New("state chunk transfer identity or connection generation mismatch")
	}
	if chunk.Index >= len(s.chunks) {
		return errors.New("state chunk index exceeds declared chunk_count")
	}
	if previous := s.chunks[chunk.Index]; previous != nil {
		if !bytes.Equal(previous, chunk.Data) {
			return errors.New("conflicting duplicate state chunk")
		}
		return nil
	}
	if len(chunk.Data) > s.begin.TotalBytes-s.totalBytes {
		return errors.New("state chunks exceed declared total_bytes")
	}
	s.chunks[chunk.Index] = chunk.Data
	s.totalBytes += len(chunk.Data)
	s.received++
	return nil
}

// Commit verifies exact bytes/hash, decodes the complete state, and consumes the
// staging transfer on both success and failure. Its result is structurally valid
// evidence only; authorize and persist atomically before sending state.applied.
func (s *StateTransfer) Commit(data []byte, now time.Time) (StateSnapshot, error) {
	defer s.Discard()
	if err := s.checkOpen(now); err != nil {
		return StateSnapshot{}, err
	}
	envelope, commit, err := DecodeStateCommit(data)
	if err != nil {
		return StateSnapshot{}, err
	}
	if commit.StateTransferIdentity != s.begin.StateTransferIdentity || envelope.ConnectionGeneration != s.generation {
		return StateSnapshot{}, errors.New("state commit transfer identity or connection generation mismatch")
	}
	if commit.SHA256 != s.begin.SHA256 {
		return StateSnapshot{}, errors.New("state commit sha256 differs from begin")
	}
	if s.received != s.begin.ChunkCount || s.totalBytes != s.begin.TotalBytes {
		return StateSnapshot{}, errors.New("state commit requires every chunk and exact total_bytes")
	}
	content := make([]byte, 0, s.totalBytes)
	for _, chunk := range s.chunks {
		content = append(content, chunk...)
	}
	hash := sha256.Sum256(content)
	if hex.EncodeToString(hash[:]) != s.begin.SHA256 {
		return StateSnapshot{}, errors.New("state snapshot sha256 mismatch")
	}
	snapshot, err := DecodeStateSnapshot(content)
	if err != nil {
		return StateSnapshot{}, err
	}
	if snapshot.StreamID != s.begin.StreamID || snapshot.ConfigRevision != s.begin.ConfigRevision ||
		snapshot.LastCreatedSeq != s.begin.LastCreatedSeq || !time.Time(snapshot.CreatedAt).Equal(time.Time(s.begin.CreatedAt)) {
		return StateSnapshot{}, errors.New("state snapshot metadata differs from begin")
	}
	return snapshot, nil
}

func (s *StateTransfer) checkOpen(now time.Time) error {
	if s.closed || s.chunks == nil {
		return errors.New("state transfer is closed")
	}
	if !now.Before(s.deadline) {
		return errors.New("state transfer expired")
	}
	return nil
}
