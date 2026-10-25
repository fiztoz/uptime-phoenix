package domain

import "errors"

// Local staging errors never carry source paths or storage diagnostics.
var (
	ErrProbeBinaryStageWriteInput       = errors.New("invalid local probe binary staging input")
	ErrProbeBinaryStageWriteConflict    = errors.New("local probe binary staging directory already exists")
	ErrProbeBinaryStageWriteUnavailable = errors.New("local probe binary staging unavailable before creation")
	ErrProbeBinaryStageWriteUnknown     = errors.New("local probe binary staging outcome unknown")
	// ErrProbeBinaryStageWriteUnauthorized is a definite pre-creation refusal:
	// the stage grant did not verify under the host's trusted authority key.
	ErrProbeBinaryStageWriteUnauthorized = errors.New("local probe binary staging grant refused before creation")
)

// ProbeBinaryStageSeal reports matching local bytes during one fresh staging call.
// It is not a remote observation, database grant, host fence or cleanup authority.
// The supplied parent descriptor is not asserted to name /opt or the identity host.
type ProbeBinaryStageSeal struct {
	Intent ProbeBinaryStageIntent
}

// Valid checks receipt shape only, not the work or authority it describes.
func (s ProbeBinaryStageSeal) Valid() bool { return s.Intent.Valid() }

// DirectoryName derives a single relative component, never a caller-selected path.
func (s ProbeBinaryStageSeal) DirectoryName() string {
	if !s.Valid() {
		return ""
	}
	return "uptime-phoenix-probe-" + s.Intent.Identity.OperationID
}
