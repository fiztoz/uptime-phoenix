package provisioning

import (
	"context"
	"os"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// LocalBinaryStageWriter materializes one fresh local binary stage beneath a
// borrowed directory descriptor. It does not close that parent or the prepared
// binary, retain a descriptor, upload bytes, or grant host or database authority.
//
// Same-package tests may set the unexported hooks below. They are not a path
// API. Stable names and sync/close target strings:
//
//	afterChildFD    after the new directory fd matches the parent name
//	beforeReadback  after the binary writer is closed, before reopen
//	beforeMarker    after source Close and callback join, before marker create
//	beforeDirCheck  after marker readback, before the binding and entry check
//	syncFault       target is binary, marker, child, or parent
//	closeFault      target is binary, binary-read, marker, marker-read, child, or parent
//	writeChunk      optional replacement for payload Write; nil uses File.Write
//
// A non-nil hook error is an outcome-unknown fault once directory creation was
// attempted. writeChunk must perform any write it reports.
type LocalBinaryStageWriter struct {
	parent         *os.File
	afterChildFD   func() error
	beforeReadback func() error
	beforeMarker   func() error
	beforeDirCheck func() error
	syncFault      func(target string) error
	closeFault     func(target string) error
	writeChunk     func(f *os.File, p []byte) (int, error)
}

var _ ports.ProbeBinaryStageWriter = (*LocalBinaryStageWriter)(nil)

// StageBinary creates a fresh stage directory and returns a receipt repeating
// intent. Once creation is attempted, every result other than an exclusive-name
// conflict is outcome-unknown and partial bytes are left untouched.
func (w *LocalBinaryStageWriter) StageBinary(ctx context.Context, intent domain.ProbeBinaryStageIntent, binary ports.PreparedProbeBinary) (domain.ProbeBinaryStageSeal, error) {
	if w == nil || imageSnapshotNil(ctx) {
		return domain.ProbeBinaryStageSeal{}, domain.ErrProbeBinaryStageWriteInput
	}
	return w.stageBinary(ctx, intent, binary)
}
