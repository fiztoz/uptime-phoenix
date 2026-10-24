//go:build linux || darwin

package provisioning

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

const (
	cleanupTreeBatch  = 256
	cleanupUnitSuffix = ".service"
)

// cleanupTreeMaxDepth and cleanupTreeMaxEntries bound the data directory walk:
// a tree beyond either is not something one install created, and is reported
// Unknown rather than chased without limit. They are variables only so a test
// can lower them; nothing else assigns them.
var (
	cleanupTreeMaxDepth   = 64
	cleanupTreeMaxEntries = 100000
)

// cleanupInspection is the phase 1 result: which of the three identity-derived
// names exist, and for the directories a retained descriptor plus the stat it
// was inspected under, so the effects act on the inspected object.
type cleanupInspection struct {
	unitPresent bool
	artifact    *os.File
	artifactSt  unix.Stat_t
	data        *os.File
	dataSt      unix.Stat_t
}

func (i *cleanupInspection) close() {
	if i.artifact != nil {
		_ = i.artifact.Close()
	}
	if i.data != nil {
		_ = i.data.Close()
	}
}

// ApplyProbeCleanup removes everything one systemd install operation created
// beneath three borrowed protected parents: the data directory beneath
// dataParent (the host's /var/lib), the artifact directory beneath optParent
// (/opt) and the unit file beneath unitParent (/etc/systemd/system), stopping
// and disabling the service first. An absent resource counts as removed, so a
// partial install is cleaned and a clean host is a no-op that never touches
// systemctl.
//
// Everything is inspected before anything changes: a unit file that is not
// exactly ProbeSystemdUnit(identity), an artifact directory holding anything
// but the pinned release file and its stage marker, a link or foreign owner,
// or a present unit with no systemctl is a definite refusal (Conflict,
// Unavailable or Input). Once an effect may have happened every failure is
// Unknown, including a final check that all three names are absent.
func ApplyProbeCleanup(ctx context.Context, dataParent, optParent, unitParent *os.File, intent domain.ProbeHostActionIntent, systemctl ProbeSystemctl) error {
	name, err := hostActionName(ctx, dataParent, intent, domain.ProbeHostActionCleanup)
	if err != nil {
		return err
	}
	if optParent == nil || unitParent == nil {
		return domain.ErrProbeBinaryStageWriteInput
	}
	pin := intent.Identity.Binary
	release := pin.ReleaseName()
	unitName := name + cleanupUnitSuffix
	unit := []byte(domain.ProbeSystemdUnit(intent.Identity))
	if len(unit) == 0 || !singleComponent(release) || !singleComponent(unitName) || release == domain.ProbeBinaryStageMarkerName {
		return domain.ErrProbeBinaryStageWriteInput
	}

	// Phase 1: inspect. No effects.
	insp, err := cleanupInspect(dataParent, optParent, unitParent, name, unitName, release, unit)
	if err != nil {
		return err
	}
	defer insp.close()
	if insp.unitPresent && imageSnapshotNil(systemctl) {
		return domain.ErrProbeBinaryStageWriteInput
	}
	if !insp.unitPresent && insp.artifact == nil && insp.data == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Phase 2: effects. Every failure from here is Unknown.
	if insp.unitPresent {
		if err := cleanupRemoveUnit(ctx, unitParent, unitName, unit, systemctl); err != nil {
			return err
		}
	}
	if ctx.Err() != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	if insp.artifact != nil {
		if err := cleanupRemoveArtifact(optParent, name, release, insp); err != nil {
			return domain.ErrProbeBinaryStageWriteUnknown
		}
	}
	if ctx.Err() != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	if insp.data != nil {
		if err := cleanupRemoveData(ctx, dataParent, name, insp); err != nil {
			return domain.ErrProbeBinaryStageWriteUnknown
		}
	}
	if ctx.Err() != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}

	// Phase 3: verify every name is gone, by name and without following links.
	if !cleanupAbsent(unitParent, unitName) || !cleanupAbsent(optParent, name) || !cleanupAbsent(dataParent, name) {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	return nil
}

// cleanupInspect classifies the three names. Absent is fine; present must be
// exactly ours. It returns Conflict for something present that is not ours and
// Unavailable when a name cannot be inspected.
func cleanupInspect(dataParent, optParent, unitParent *os.File, name, unitName, release string, unit []byte) (*cleanupInspection, error) {
	insp := &cleanupInspection{}
	fail := func(err error) (*cleanupInspection, error) {
		insp.close()
		return nil, err
	}

	var st unix.Stat_t
	switch err := fstatAt(unitParent, unitName, &st); {
	case err == nil:
		if !hostActionUnitMatches(unitParent, unitName, unit) {
			return fail(domain.ErrProbeBinaryStageWriteConflict)
		}
		insp.unitPresent = true
	case !errors.Is(err, unix.ENOENT):
		return fail(domain.ErrProbeBinaryStageWriteUnavailable)
	}

	artifact, artifactSt, err := cleanupOpenOwnedDir(optParent, name)
	if err != nil {
		return fail(err)
	}
	if artifact != nil {
		insp.artifact, insp.artifactSt = artifact, artifactSt
		if err := cleanupInspectArtifact(artifact, release); err != nil {
			return fail(err)
		}
	}

	data, dataSt, err := cleanupOpenOwnedDir(dataParent, name)
	if err != nil {
		return fail(err)
	}
	if data != nil {
		insp.data, insp.dataSt = data, dataSt
	}
	return insp, nil
}

// cleanupOpenOwnedDir returns a nil file for an absent name. A present name
// must be a real directory (never a link) owned by the effective uid; the
// returned descriptor is the object that was classified.
func cleanupOpenOwnedDir(parent *os.File, name string) (*os.File, unix.Stat_t, error) {
	var named unix.Stat_t
	if err := fstatAt(parent, name, &named); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil, named, nil
		}
		return nil, named, domain.ErrProbeBinaryStageWriteUnavailable
	}
	if named.Mode&unix.S_IFMT != unix.S_IFDIR || int(named.Uid) != os.Geteuid() {
		return nil, named, domain.ErrProbeBinaryStageWriteConflict
	}
	dir, err := openAt(parent, name, localStageDirFlags, 0)
	if err != nil {
		return nil, named, domain.ErrProbeBinaryStageWriteUnavailable
	}
	opened, err := fstatFile(dir)
	if err != nil {
		_ = dir.Close()
		return nil, named, domain.ErrProbeBinaryStageWriteUnavailable
	}
	if !sameStat(opened, named) || opened.Mode&unix.S_IFMT != unix.S_IFDIR || int(opened.Uid) != os.Geteuid() {
		_ = dir.Close()
		return nil, named, domain.ErrProbeBinaryStageWriteConflict
	}
	return dir, opened, nil
}

// cleanupInspectArtifact requires the directory to hold at most the pinned
// release file and the stage marker, each a regular single-link file owned by
// the effective uid. Modes are not compared: a half-finished stage may not have
// reached its final mode yet.
func cleanupInspectArtifact(dir *os.File, release string) error {
	if _, err := dir.Seek(0, io.SeekStart); err != nil {
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	names, err := dir.Readdirnames(3)
	if err != nil && !errors.Is(err, io.EOF) {
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	if len(names) > 2 {
		return domain.ErrProbeBinaryStageWriteConflict
	}
	for _, entry := range names {
		if entry != release && entry != domain.ProbeBinaryStageMarkerName {
			return domain.ErrProbeBinaryStageWriteConflict
		}
		var st unix.Stat_t
		if err := fstatAt(dir, entry, &st); err != nil {
			return domain.ErrProbeBinaryStageWriteUnavailable
		}
		if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || int(st.Uid) != os.Geteuid() {
			return domain.ErrProbeBinaryStageWriteConflict
		}
	}
	return nil
}

// cleanupRemoveUnit stops and disables the service, removes the unit file and
// reloads systemd. The result of reset-failed is ignored: it only clears a
// failed-state record and may legitimately find none.
func cleanupRemoveUnit(ctx context.Context, unitParent *os.File, unitName string, unit []byte, systemctl ProbeSystemctl) error {
	if _, code, err := systemctl.Run(ctx, "disable", "--now", unitName); err != nil || code != 0 {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	if !hostActionUnitMatches(unitParent, unitName, unit) {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	if err := cleanupUnlinkAt(unitParent, unitName, 0); err != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	if err := hostActionSyncDir(unitParent); err != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	if _, code, err := systemctl.Run(ctx, "daemon-reload"); err != nil || code != 0 {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	_, _, _ = systemctl.Run(ctx, "reset-failed", unitName)
	return nil
}

// cleanupRemoveArtifact removes the two staged files and then the directory,
// acting on the directory that was inspected.
func cleanupRemoveArtifact(optParent *os.File, name, release string, insp *cleanupInspection) error {
	if err := cleanupStillNamed(optParent, name, insp.artifactSt); err != nil {
		return err
	}
	for _, entry := range []string{release, domain.ProbeBinaryStageMarkerName} {
		if err := cleanupUnlinkAt(insp.artifact, entry, 0); err != nil {
			return err
		}
	}
	if err := cleanupUnlinkAt(optParent, name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return hostActionSyncDir(optParent)
}

// cleanupRemoveData removes the whole data tree without following links.
func cleanupRemoveData(ctx context.Context, dataParent *os.File, name string, insp *cleanupInspection) error {
	if err := cleanupStillNamed(dataParent, name, insp.dataSt); err != nil {
		return err
	}
	entries := 0
	if err := cleanupRemoveContents(ctx, insp.data, insp.dataSt, 0, &entries); err != nil {
		return err
	}
	if err := cleanupUnlinkAt(dataParent, name, unix.AT_REMOVEDIR); err != nil {
		return err
	}
	return hostActionSyncDir(dataParent)
}

// cleanupStillNamed requires name to still be the object that was inspected.
func cleanupStillNamed(parent *os.File, name string, want unix.Stat_t) error {
	var now unix.Stat_t
	if err := fstatAt(parent, name, &now); err != nil {
		return err
	}
	if !sameStat(now, want) || now.Mode&unix.S_IFMT != unix.S_IFDIR {
		return errLocalStageInternal
	}
	return nil
}

// cleanupRemoveContents empties dir. Symlinks, files and special files are
// unlinked themselves; only real directories on the root's device are entered,
// through a no-follow open that is checked against the entry that was listed.
// The directory is rewound between batches so removing entries never skips
// any.
func cleanupRemoveContents(ctx context.Context, dir *os.File, root unix.Stat_t, depth int, entries *int) error {
	if depth > cleanupTreeMaxDepth {
		return errLocalStageInternal
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := dir.Seek(0, io.SeekStart); err != nil {
			return err
		}
		names, err := dir.Readdirnames(cleanupTreeBatch)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		if len(names) == 0 {
			return nil
		}
		for _, entry := range names {
			if !singleComponent(entry) {
				return errLocalStageInternal
			}
			*entries++
			if *entries > cleanupTreeMaxEntries {
				return errLocalStageInternal
			}
			if err := cleanupRemoveEntry(ctx, dir, entry, root, depth, entries); err != nil {
				return err
			}
		}
	}
}

func cleanupRemoveEntry(ctx context.Context, dir *os.File, entry string, root unix.Stat_t, depth int, entries *int) error {
	var st unix.Stat_t
	if err := fstatAt(dir, entry, &st); err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return cleanupUnlinkAt(dir, entry, 0)
	}
	if st.Dev != root.Dev {
		return errLocalStageInternal
	}
	child, err := openAt(dir, entry, localStageDirFlags, 0)
	if err != nil {
		return err
	}
	opened, err := fstatFile(child)
	if err != nil || !sameStat(opened, st) {
		_ = child.Close()
		return errLocalStageInternal
	}
	removeErr := cleanupRemoveContents(ctx, child, root, depth+1, entries)
	closeErr := child.Close()
	if removeErr != nil {
		return removeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return cleanupUnlinkAt(dir, entry, unix.AT_REMOVEDIR)
}

// cleanupUnlinkAt unlinks one single-component name beneath dir. A name that is
// already gone is success: the postcondition is absence.
func cleanupUnlinkAt(dir *os.File, name string, flags int) error {
	if !singleComponent(name) {
		return errLocalStageInternal
	}
	err := withFD(dir, func(fd int) error { return unix.Unlinkat(fd, name, flags) })
	if err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

// cleanupAbsent reports whether name does not exist beneath parent, without
// following a link.
func cleanupAbsent(parent *os.File, name string) bool {
	var st unix.Stat_t
	return errors.Is(fstatAt(parent, name, &st), unix.ENOENT)
}
