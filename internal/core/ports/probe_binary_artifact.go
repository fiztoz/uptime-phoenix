package ports

import (
	"context"
	"io"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeBinaryPreparer owns input on every return; closing input must unblock Read.
// It verifies a private snapshot, never executes bytes or authorizes installation.
type ProbeBinaryPreparer interface {
	PrepareBinary(ctx context.Context, pin domain.ProbeBinaryPin, input io.ReadCloser) (PreparedProbeBinary, error)
}

// PreparedProbeBinary owns one immutable pin and private verified snapshot.
// Open readers are revoked by Close. Callers must close readers and the snapshot.
type PreparedProbeBinary interface {
	Pin() domain.ProbeBinaryPin
	Open(ctx context.Context) (io.ReadCloser, error)
	Close(ctx context.Context) error
}
