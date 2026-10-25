//go:build linux || darwin

package provisioning

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// The permission constants are deliberately untyped: unix.Stat_t.Mode is a
// uint32 on Linux but a uint16 on Darwin, and the mode comparisons below must
// compile on both without a platform-dependent conversion.
const (
	hostActionDataDirPerm  = 0o700
	hostActionUnitFilePerm = 0o644
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
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o7777 != hostActionDataDirPerm || int(st.Uid) != os.Geteuid() {
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
		st.Mode&0o7777 != hostActionUnitFilePerm || st.Size != int64(len(want)) {
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

const (
	probeServiceStartWait = 20 * time.Second
	probeServicePoll      = 500 * time.Millisecond
)

// ApplyProbeInit runs `probe init` for the intent's install, once. Before
// executing anything it requires the data directory (beneath dataParent, the
// host's protected /var/lib) to be an empty 0700 directory, and the staged
// binary (beneath optParent, the protected /opt) to be the exact pinned bytes
// in a 0555 single-link file. Those refusals are definite. Once the binary
// runs, every failure is Unknown: identity material may already exist.
func ApplyProbeInit(ctx context.Context, dataParent, optParent *os.File, intent domain.ProbeHostActionIntent, runner ProbeCommandRunner) (ProbeInitResult, error) {
	var zero ProbeInitResult
	name, err := hostActionName(ctx, dataParent, intent, domain.ProbeActivationProbeInit)
	if err != nil {
		return zero, err
	}
	if optParent == nil || imageSnapshotNil(runner) {
		return zero, domain.ErrProbeBinaryStageWriteInput
	}
	install := intent.Identity.Resources()
	dataPath, artifactPath := install[0].Reference, install[1].Reference
	pin := intent.Identity.Binary
	binaryPath := artifactPath + "/" + pin.ReleaseName()

	data, err := openAt(dataParent, name, localStageDirFlags, 0)
	if err != nil {
		return zero, domain.ErrProbeBinaryStageWriteUnavailable
	}
	st, statErr := fstatFile(data)
	entries, readErr := data.ReadDir(1)
	_ = data.Close()
	if statErr != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o7777 != hostActionDataDirPerm || int(st.Uid) != os.Geteuid() {
		return zero, domain.ErrProbeBinaryStageWriteUnavailable
	}
	if len(entries) != 0 {
		return zero, domain.ErrProbeBinaryStageWriteConflict
	}
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return zero, domain.ErrProbeBinaryStageWriteUnavailable
	}
	if !probeStagedBinaryMatches(optParent, name, pin) {
		return zero, domain.ErrProbeBinaryStageWriteUnavailable
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	// From here the probe may have created identity material.
	stdout, code, err := runner.Run(ctx, binaryPath, "init", "--data-dir", dataPath)
	if err != nil || code != 0 || ctx.Err() != nil {
		return zero, domain.ErrProbeBinaryStageWriteUnknown
	}
	result, ok := parseProbeInitOutput(stdout)
	if !ok {
		return zero, domain.ErrProbeBinaryStageWriteUnknown
	}
	return result, nil
}

// ApplyProbeServiceStart starts the intent's enabled unit and waits for it to
// be active. The unit file must be exactly the derived bytes and enabled; an
// already active unit is Conflict. After `systemctl start` every failure,
// including not becoming active in time, is Unknown.
func ApplyProbeServiceStart(ctx context.Context, unitParent *os.File, intent domain.ProbeHostActionIntent, systemctl ProbeSystemctl) error {
	name, err := hostActionName(ctx, unitParent, intent, domain.ProbeActivationServiceStart)
	if err != nil {
		return err
	}
	if imageSnapshotNil(systemctl) {
		return domain.ErrProbeBinaryStageWriteInput
	}
	if !hostActionUnitMatches(unitParent, name, []byte(domain.ProbeSystemdUnit(intent.Identity))) {
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	if state, code, err := systemctl.Run(ctx, "is-enabled", name); err != nil || code != 0 || state != "enabled\n" {
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	if _, code, err := systemctl.Run(ctx, "is-active", name); err != nil {
		return domain.ErrProbeBinaryStageWriteUnavailable
	} else if code == 0 {
		return domain.ErrProbeBinaryStageWriteConflict
	}
	if _, code, err := systemctl.Run(ctx, "start", name); err != nil || code != 0 {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	deadline := time.Now().Add(probeServiceStartWait)
	for {
		state, code, err := systemctl.Run(ctx, "is-active", name)
		if err == nil && code == 0 && state == "active\n" {
			return nil
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return domain.ErrProbeBinaryStageWriteUnknown
		}
		select {
		case <-ctx.Done():
			return domain.ErrProbeBinaryStageWriteUnknown
		case <-time.After(probeServicePoll):
		}
	}
}

// probeStagedBinaryMatches reads optParent/<dir>/<release> without following
// links and requires a root-owned, single-link 0555 file with the pin's exact
// size and SHA-256.
func probeStagedBinaryMatches(optParent *os.File, dirName string, pin domain.ProbeBinaryPin) bool {
	dir, err := openAt(optParent, dirName, localStageDirFlags, 0)
	if err != nil {
		return false
	}
	defer func() { _ = dir.Close() }()
	// The binary runs by absolute path after this check, so its directory
	// must also be ours alone: no one else can swap the file in between.
	dst, err := fstatFile(dir)
	if err != nil || dst.Mode&unix.S_IFMT != unix.S_IFDIR || int(dst.Uid) != os.Geteuid() || dst.Mode&0o7777 != localStageDirPerm {
		return false
	}
	file, err := openAt(dir, pin.ReleaseName(), localStageReadFlags, 0)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	st, err := fstatFile(file)
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || int(st.Uid) != os.Geteuid() ||
		st.Mode&0o7777 != localStageBinPerm || st.Size != pin.Size {
		return false
	}
	hash := sha256.New()
	if n, err := io.Copy(hash, io.LimitReader(file, pin.Size+1)); err != nil || n != pin.Size {
		return false
	}
	return hex.EncodeToString(hash.Sum(nil)) == pin.SHA256
}
