//go:build linux || darwin

package provisioning

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const (
	hostActionDataDirPerm  = uint32(0o700)
	hostActionUnitFilePerm = uint32(0o644)
	hostActionUnitMaxRead  = 64 * 1024
)

// ApplyProbeDataDirectory creates the intent's data directory, fresh-only,
// beneath a borrowed parent (the host's protected /var/lib). An existing entry
// is a definite Conflict. Any failure after mkdirat is Unknown.
func ApplyProbeDataDirectory(ctx context.Context, parent *os.File, intent domain.ProbeHostActionIntent) error {
	name, err := hostActionName(ctx, parent, intent, domain.ProbeHostActionDataDirectory)
	if err != nil {
		return err
	}
	if err := mkdirAt(parent, name, hostActionDataDirPerm); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return domain.ErrProbeBinaryStageWriteConflict
		}
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	dir, err := openAt(parent, name, localStageDirFlags, 0)
	if err != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	defer func() { _ = dir.Close() }()
	// Set the exact mode on the descriptor, independent of umask.
	if err := withFD(dir, func(fd int) error { return unix.Fchmod(fd, hostActionDataDirPerm) }); err != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	st, err := fstatFile(dir)
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || uint32(st.Mode)&0o7777 != hostActionDataDirPerm || int(st.Uid) != os.Geteuid() {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	if err := hostActionSyncDir(parent); err != nil || ctx.Err() != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	return nil
}

// ApplyProbeUnitFile writes exactly ProbeSystemdUnit(identity) as a new file
// beneath a borrowed parent (the host's protected /etc/systemd/system), syncs
// it and its directory, and reads it back. An existing entry is Conflict.
func ApplyProbeUnitFile(ctx context.Context, parent *os.File, intent domain.ProbeHostActionIntent) error {
	name, err := hostActionName(ctx, parent, intent, domain.ProbeHostActionUnitFile)
	if err != nil {
		return err
	}
	content := []byte(domain.ProbeSystemdUnit(intent.Identity))
	if len(content) == 0 {
		return domain.ErrProbeBinaryStageWriteInput
	}
	file, err := openAt(parent, name, localStageWriteFlags, localStageCreatePerm)
	if err != nil {
		if errors.Is(err, unix.EEXIST) {
			return domain.ErrProbeBinaryStageWriteConflict
		}
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	writeErr := func() error {
		if n, err := file.Write(content); err != nil || n != len(content) {
			return errLocalStageInternal
		}
		if err := withFD(file, func(fd int) error { return unix.Fchmod(fd, hostActionUnitFilePerm) }); err != nil {
			return err
		}
		return file.Sync()
	}()
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil || ctx.Err() != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	if err := hostActionSyncDir(parent); err != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	if !hostActionUnitMatches(parent, name, content) {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	return nil
}

// ApplyProbeService enables (does not start) the intent's unit. The unit file
// beneath the borrowed parent must hold exactly the derived bytes; a unit that
// is already enabled is Conflict. Applied requires is-enabled to report
// "enabled" after daemon-reload and enable.
func ApplyProbeService(ctx context.Context, parent *os.File, intent domain.ProbeHostActionIntent, systemctl ProbeSystemctl) error {
	name, err := hostActionName(ctx, parent, intent, domain.ProbeHostActionService)
	if err != nil {
		return err
	}
	if imageSnapshotNil(systemctl) {
		return domain.ErrProbeBinaryStageWriteInput
	}
	if !hostActionUnitMatches(parent, name, []byte(domain.ProbeSystemdUnit(intent.Identity))) {
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	// is-enabled exits 0 only for enabled-like states (enabled, static,
	// alias...). It reads unit files directly, so the freshly written unit
	// must report exactly "disabled"; any other answer, including a transient
	// systemctl failure, refuses before mutation rather than guessing.
	if state, code, err := systemctl.Run(ctx, "is-enabled", name); err != nil {
		return domain.ErrProbeBinaryStageWriteUnavailable
	} else if code == 0 {
		return domain.ErrProbeBinaryStageWriteConflict
	} else if state != "disabled\n" {
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	// From here systemd state may change: every failure is Unknown.
	if _, code, err := systemctl.Run(ctx, "daemon-reload"); err != nil || code != 0 {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	if _, code, err := systemctl.Run(ctx, "enable", name); err != nil || code != 0 {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	state, code, err := systemctl.Run(ctx, "is-enabled", name)
	if err != nil || code != 0 || state != "enabled\n" || ctx.Err() != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	return nil
}

// hostActionName validates the call and returns the single path component the
// action owns: the base of the intent's identity-derived reference. The
// service action owns the unit name, which is also the unit file's name.
func hostActionName(ctx context.Context, parent *os.File, intent domain.ProbeHostActionIntent, kind string) (string, error) {
	if imageSnapshotNil(ctx) || parent == nil || !intent.Valid() || intent.Kind != kind {
		return "", domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	spec, ok := intent.Resource()
	if !ok {
		return "", domain.ErrProbeBinaryStageWriteInput
	}
	name := path.Base(spec.Reference)
	if !singleComponent(name) || name == "." || name == ".." {
		return "", domain.ErrProbeBinaryStageWriteInput
	}
	return name, nil
}

// hostActionUnitMatches reads a regular single-link file beneath parent with
// no symlink following and compares it to want exactly.
func hostActionUnitMatches(parent *os.File, name string, want []byte) bool {
	file, err := openAt(parent, name, localStageReadFlags, 0)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	st, err := fstatFile(file)
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || int(st.Uid) != os.Geteuid() ||
		uint32(st.Mode)&0o7777 != hostActionUnitFilePerm || st.Size != int64(len(want)) {
		return false
	}
	got := make([]byte, hostActionUnitMaxRead+1)
	n, err := io.ReadFull(file, got)
	if n != len(want) || (err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF)) {
		return false
	}
	return bytes.Equal(got[:n], want)
}

func hostActionSyncDir(parent *os.File) error {
	dup, err := dupParentDir(parent)
	if err != nil {
		return err
	}
	syncErr := dup.Sync()
	if closeErr := dup.Close(); syncErr == nil {
		syncErr = closeErr
	}
	return syncErr
}
