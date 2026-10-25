//go:build linux || darwin

package provisioning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

// localStageDirPerm and localStageBinPerm are untyped so they compare against
// unix.Stat_t.Mode, which is a uint32 on Linux and a uint16 on Darwin.
const localStageDirPerm = 0o700
const localStageCreatePerm = uint32(0o600)
const localStageBinPerm = 0o555
const localStageMarkPerm = uint32(0o444)
const localStageCopyBuf = 32 * 1024
const localStageTargetBinary = "binary"
const localStageTargetBinaryRead = "binary-read"
const localStageTargetMarker = "marker"
const localStageTargetMarkerRead = "marker-read"
const localStageTargetChild = "child"
const localStageTargetParent = "parent"
const localStageDirFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
const localStageReadFlags = unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
const localStageWriteFlags = unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC
const localStageRWFlags = unix.O_RDWR | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_NONBLOCK | unix.O_CLOEXEC

// errLocalStageInternal is never returned to callers.
var errLocalStageInternal = errors.New("local probe binary staging internal")

// NewLocalBinaryStageWriter borrows parent. It duplicates once to prove the
// descriptor is a live directory, then closes that duplicate. The call does not
// retain an fd and has no Close method.
func NewLocalBinaryStageWriter(ctx context.Context, parent *os.File) (*LocalBinaryStageWriter, error) {
	if imageSnapshotNil(ctx) || parent == nil {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dup, err := dupParentDir(parent)
	if err != nil {
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	if err := dup.Close(); err != nil {
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &LocalBinaryStageWriter{parent: parent}, nil
}

func (w *LocalBinaryStageWriter) stageBinary(ctx context.Context, intent domain.ProbeBinaryStageIntent, binary ports.PreparedProbeBinary) (domain.ProbeBinaryStageSeal, error) {
	seal, attempted, err := w.performStage(ctx, intent, binary)
	err = sanitizeLocalStageError(attempted, err)
	if err != nil || !seal.Valid() || seal.Intent != intent {
		if err == nil {
			err = domain.ErrProbeBinaryStageWriteUnknown
		}
		return domain.ProbeBinaryStageSeal{}, err
	}
	return seal, nil
}

func (w *LocalBinaryStageWriter) performStage(ctx context.Context, intent domain.ProbeBinaryStageIntent, binary ports.PreparedProbeBinary) (seal domain.ProbeBinaryStageSeal, attempted bool, err error) {
	pin, dirName, binName, marker, err := validateLocalStage(ctx, intent, binary)
	if err != nil {
		return domain.ProbeBinaryStageSeal{}, false, err
	}
	parentDup, err := dupParentDir(w.parent)
	if err != nil {
		return domain.ProbeBinaryStageSeal{}, false, err
	}
	var child, binaryW, binaryR, markerW, markerR *os.File
	var reader io.ReadCloser
	var stop func() bool
	var readerOnce sync.Once
	var readerErr error
	closeReader := func() error {
		readerOnce.Do(func() {
			if !imageSnapshotNil(reader) {
				readerErr = reader.Close()
			}
		})
		return readerErr
	}
	var joined chan struct{}
	joinSource := func() {
		if stop == nil {
			return
		}
		s := stop
		stop = nil
		if !s() {
			<-joined
		}
	}
	defer func() {
		joinSource()
		if cerr := closeReader(); cerr != nil && err == nil {
			err = errLocalStageInternal
			seal = domain.ProbeBinaryStageSeal{}
		}
		closeLeft := func(f **os.File, target string) {
			if f == nil || *f == nil {
				return
			}
			if cerr := w.closeOwned(*f, target); cerr != nil && err == nil {
				err = errLocalStageInternal
				seal = domain.ProbeBinaryStageSeal{}
			}
			*f = nil
		}
		closeLeft(&markerR, localStageTargetMarkerRead)
		closeLeft(&markerW, localStageTargetMarker)
		closeLeft(&binaryR, localStageTargetBinaryRead)
		closeLeft(&binaryW, localStageTargetBinary)
		closeLeft(&child, localStageTargetChild)
		closeLeft(&parentDup, localStageTargetParent)
	}()

	if err := ctx.Err(); err != nil {
		return domain.ProbeBinaryStageSeal{}, false, err
	}
	opened, openErr := binary.Open(ctx)
	if !imageSnapshotNil(opened) {
		reader = opened
	}
	if openErr != nil || imageSnapshotNil(reader) {
		if ctx.Err() != nil {
			return domain.ProbeBinaryStageSeal{}, false, ctx.Err()
		}
		if errors.Is(openErr, context.Canceled) || errors.Is(openErr, context.DeadlineExceeded) {
			return domain.ProbeBinaryStageSeal{}, false, openErr
		}
		return domain.ProbeBinaryStageSeal{}, false, errLocalStageInternal
	}
	joined = make(chan struct{})
	stop = context.AfterFunc(ctx, func() {
		defer close(joined)
		_ = closeReader()
	})
	if err := ctx.Err(); err != nil {
		return domain.ProbeBinaryStageSeal{}, false, err
	}

	attempted = true
	if err := mkdirAt(parentDup, dirName, localStageDirPerm); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return domain.ProbeBinaryStageSeal{}, true, domain.ErrProbeBinaryStageWriteConflict
		}
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	var named unix.Stat_t
	if err := fstatAt(parentDup, dirName, &named); err != nil || !dirMode(named, false) {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	child, err = openAt(parentDup, dirName, localStageDirFlags, 0)
	if err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	openedSt, err := fstatFile(child)
	if err != nil || !sameStat(openedSt, named) || openedSt.Mode&unix.S_IFMT != unix.S_IFDIR {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	if err := fchmod(child, localStageDirPerm); err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	childSt, err := fstatFile(child)
	if err != nil || !dirMode(childSt, true) || !sameStat(childSt, named) {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	if err := w.runHook(w.afterChildFD); err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}

	binaryW, err = openAt(child, binName, localStageRWFlags, localStageCreatePerm)
	if err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	fresh, err := fstatFile(binaryW)
	if err != nil || !freshRegular(fresh) {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	if err := w.writeSource(ctx, binaryW, reader, pin); err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	binSt, err := w.syncChmodStat(binaryW, localStageTargetBinary, localStageBinPerm, pin.Size)
	if err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	if err := w.closeOwned(binaryW, localStageTargetBinary); err != nil {
		binaryW = nil
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	binaryW = nil
	if err := w.runHook(w.beforeReadback); err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	binaryR, err = w.readback(ctx, child, binName, binSt, localStageBinPerm, pin.Size, pin.SHA256, &pin, nil, localStageTargetBinaryRead)
	if err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	if err := w.closeOwned(binaryR, localStageTargetBinaryRead); err != nil {
		binaryR = nil
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	binaryR = nil

	if cerr := closeReader(); cerr != nil || ctx.Err() != nil {
		joinSource()
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	joinSource()
	if err := w.runHook(w.beforeMarker); err != nil || ctx.Err() != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}

	markerW, err = openAt(child, domain.ProbeBinaryStageMarkerName, localStageWriteFlags, localStageCreatePerm)
	if err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	fresh, err = fstatFile(markerW)
	if err != nil || !freshRegular(fresh) {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	if err := w.writeFull(markerW, []byte(marker)); err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	markSt, err := w.syncChmodStat(markerW, localStageTargetMarker, localStageMarkPerm, int64(len(marker)))
	if err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	if err := w.closeOwned(markerW, localStageTargetMarker); err != nil {
		markerW = nil
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	markerW = nil
	markerR, err = w.readback(ctx, child, domain.ProbeBinaryStageMarkerName, markSt, localStageMarkPerm, int64(len(marker)), intent.MarkerSHA256(), nil, []byte(marker), localStageTargetMarkerRead)
	if err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	if err := w.closeOwned(markerR, localStageTargetMarkerRead); err != nil {
		markerR = nil
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	markerR = nil
	if err := w.runHook(w.beforeDirCheck); err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	if err := directoryFinal(parentDup, child, dirName, binName, childSt, binSt, markSt, pin.Size, int64(len(marker))); err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	if err := w.syncOwned(child, localStageTargetChild); err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	if err := w.syncOwned(parentDup, localStageTargetParent); err != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	if err := w.closeOwned(child, localStageTargetChild); err != nil {
		child = nil
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	child = nil
	if err := w.closeOwned(parentDup, localStageTargetParent); err != nil {
		parentDup = nil
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	parentDup = nil
	if ctx.Err() != nil {
		return domain.ProbeBinaryStageSeal{}, true, errLocalStageInternal
	}
	return domain.ProbeBinaryStageSeal{Intent: intent}, true, nil
}

func validateLocalStage(ctx context.Context, intent domain.ProbeBinaryStageIntent, binary ports.PreparedProbeBinary) (domain.ProbeBinaryPin, string, string, string, error) {
	if imageSnapshotNil(ctx) || imageSnapshotNil(binary) || !intent.Valid() {
		return domain.ProbeBinaryPin{}, "", "", "", domain.ErrProbeBinaryStageWriteInput
	}
	pin := binary.Pin()
	if pin != intent.Identity.Binary {
		return domain.ProbeBinaryPin{}, "", "", "", domain.ErrProbeBinaryStageWriteInput
	}
	dirName := (domain.ProbeBinaryStageSeal{Intent: intent}).DirectoryName()
	binName := pin.ReleaseName()
	marker := intent.Marker()
	if !singleComponent(dirName) || !singleComponent(binName) || !singleComponent(domain.ProbeBinaryStageMarkerName) || marker == "" || len(marker) > domain.MaxProbeBinaryStageMarkerSize || dirName == binName || binName == domain.ProbeBinaryStageMarkerName {
		return domain.ProbeBinaryPin{}, "", "", "", domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return domain.ProbeBinaryPin{}, "", "", "", err
	}
	return pin, dirName, binName, marker, nil
}

func singleComponent(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00")
}

func (w *LocalBinaryStageWriter) runHook(fn func() error) error {
	if fn == nil {
		return nil
	}
	return fn()
}

func dupParentDir(parent *os.File) (*os.File, error) {
	if parent == nil {
		return nil, errLocalStageInternal
	}
	raw, err := parent.SyscallConn()
	if err != nil {
		return nil, err
	}
	var dup int
	var opErr error
	err = raw.Control(func(fd uintptr) {
		dup, opErr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0)
	})
	if err != nil {
		return nil, err
	}
	if opErr != nil {
		return nil, opErr
	}
	f := os.NewFile(uintptr(dup), "local-stage-parent")
	if f == nil {
		_ = unix.Close(dup)
		return nil, errLocalStageInternal
	}
	st, err := fstatFile(f)
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
		_ = f.Close()
		return nil, errLocalStageInternal
	}
	return f, nil
}

func withFD(f *os.File, fn func(fd int) error) error {
	if f == nil {
		return errLocalStageInternal
	}
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}
	var opErr error
	err = raw.Control(func(fd uintptr) {
		opErr = fn(int(fd))
	})
	if err != nil {
		return err
	}
	return opErr
}

func mkdirAt(dir *os.File, name string, mode uint32) error {
	if !singleComponent(name) {
		return errLocalStageInternal
	}
	return withFD(dir, func(fd int) error { return unix.Mkdirat(fd, name, mode) })
}

func openAt(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	if !singleComponent(name) {
		return nil, errLocalStageInternal
	}
	var fd int
	var opErr error
	err := withFD(dir, func(raw int) error {
		fd, opErr = unix.Openat(raw, name, flags, mode)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if opErr != nil {
		return nil, opErr
	}
	f := os.NewFile(uintptr(fd), "local-stage")
	if f == nil {
		_ = unix.Close(fd)
		return nil, errLocalStageInternal
	}
	return f, nil
}

func fstatAt(dir *os.File, name string, st *unix.Stat_t) error {
	if !singleComponent(name) {
		return errLocalStageInternal
	}
	return withFD(dir, func(fd int) error { return unix.Fstatat(fd, name, st, unix.AT_SYMLINK_NOFOLLOW) })
}

func fstatFile(f *os.File) (unix.Stat_t, error) {
	var st unix.Stat_t
	err := withFD(f, func(fd int) error { return unix.Fstat(fd, &st) })
	return st, err
}

func fchmod(f *os.File, mode uint32) error {
	return withFD(f, func(fd int) error { return unix.Fchmod(fd, mode) })
}

func (w *LocalBinaryStageWriter) syncOwned(f *os.File, target string) error {
	err := withFD(f, func(fd int) error { return unix.Fsync(fd) })
	if w != nil && w.syncFault != nil {
		if herr := w.syncFault(target); herr != nil {
			return herr
		}
	}
	return err
}

func (w *LocalBinaryStageWriter) closeOwned(f *os.File, target string) error {
	if f == nil {
		return nil
	}
	err := f.Close()
	if w != nil && w.closeFault != nil {
		if herr := w.closeFault(target); herr != nil {
			return herr
		}
	}
	return err
}

func (w *LocalBinaryStageWriter) writeFull(f *os.File, p []byte) error {
	for len(p) > 0 {
		var n int
		var err error
		if w != nil && w.writeChunk != nil {
			n, err = w.writeChunk(f, p)
		} else {
			n, err = f.Write(p)
		}
		if n < 0 || n > len(p) {
			return errLocalStageInternal
		}
		p = p[n:]
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func sameStat(a, b unix.Stat_t) bool { return a.Dev == b.Dev && a.Ino == b.Ino }

func freshRegular(st unix.Stat_t) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink == 1 && st.Size == 0
}

func regularExact(st unix.Stat_t, perm uint32, size int64) bool {
	return st.Mode&unix.S_IFMT == unix.S_IFREG && st.Nlink == 1 && st.Size == size && uint32(uint64(st.Mode&0o7777)) == perm
}

func dirMode(st unix.Stat_t, exact bool) bool {
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		return false
	}
	if !exact {
		return true
	}
	return uint32(uint64(st.Mode&0o7777)) == localStageDirPerm
}

func (w *LocalBinaryStageWriter) writeSource(ctx context.Context, dst *os.File, src io.Reader, pin domain.ProbeBinaryPin) error {
	buf := make([]byte, localStageCopyBuf)
	hash := sha256.New()
	var written int64
	emptyReads := 0
	for written < pin.Size {
		if err := ctx.Err(); err != nil {
			return err
		}
		want := pin.Size - written
		if want > int64(len(buf)) {
			want = int64(len(buf))
		}
		n, err := src.Read(buf[:want])
		if n < 0 || int64(n) > want {
			return errLocalStageInternal
		}
		if n == 0 && err == nil {
			emptyReads++
			if emptyReads >= 100 {
				return io.ErrNoProgress
			}
			continue
		}
		emptyReads = 0
		if n > 0 {
			if werr := w.writeFull(dst, buf[:n]); werr != nil {
				return werr
			}
			_, _ = hash.Write(buf[:n])
			written += int64(n)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
	}
	if written != pin.Size {
		return errLocalStageInternal
	}
	var extra [1]byte
	confirmedEOF := false
	for range 100 {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := src.Read(extra[:])
		if n != 0 || (err != nil && !errors.Is(err, io.EOF)) {
			return errLocalStageInternal
		}
		if errors.Is(err, io.EOF) {
			confirmedEOF = true
			break
		}
	}
	if !confirmedEOF {
		return io.ErrNoProgress
	}
	if hex.EncodeToString(hash.Sum(nil)) != pin.SHA256 {
		return errLocalStageInternal
	}
	if !validProbeBinary(ctx, dst, pin) {
		return errLocalStageInternal
	}
	return nil
}

func (w *LocalBinaryStageWriter) syncChmodStat(f *os.File, target string, perm uint32, size int64) (unix.Stat_t, error) {
	if err := w.syncOwned(f, target); err != nil {
		return unix.Stat_t{}, err
	}
	if err := fchmod(f, perm); err != nil {
		return unix.Stat_t{}, err
	}
	if err := w.syncOwned(f, target); err != nil {
		return unix.Stat_t{}, err
	}
	st, err := fstatFile(f)
	if err != nil {
		return unix.Stat_t{}, err
	}
	if !regularExact(st, perm, size) {
		return unix.Stat_t{}, errLocalStageInternal
	}
	return st, nil
}

func (w *LocalBinaryStageWriter) readback(ctx context.Context, dir *os.File, name string, want unix.Stat_t, perm uint32, size int64, sum string, pin *domain.ProbeBinaryPin, exact []byte, target string) (*os.File, error) {
	f, err := openAt(dir, name, localStageReadFlags, 0)
	if err != nil {
		return nil, err
	}
	verified := false
	defer func() {
		if !verified {
			_ = w.closeOwned(f, target)
		}
	}()
	st, err := fstatFile(f)
	if err != nil || !sameStat(st, want) || !regularExact(st, perm, size) {
		return nil, errLocalStageInternal
	}
	if pin != nil && !validProbeBinary(ctx, f, *pin) {
		return nil, errLocalStageInternal
	}
	if exact != nil {
		got, rerr := readLimited(ctx, f, size)
		if rerr != nil || !bytesEqual(got, exact) {
			return nil, errLocalStageInternal
		}
		summed := sha256.Sum256(got)
		if hex.EncodeToString(summed[:]) != sum {
			return nil, errLocalStageInternal
		}
	} else {
		got, rerr := hashFile(ctx, f, size)
		if rerr != nil || got != sum {
			return nil, errLocalStageInternal
		}
	}
	verified = true
	return f, nil
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func hashFile(ctx context.Context, f *os.File, size int64) (string, error) {
	if size < 0 || size > domain.MaxProbeBinarySize {
		return "", errLocalStageInternal
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	buf := make([]byte, localStageCopyBuf)
	var got int64
	for got < size {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		want := size - got
		if want > int64(len(buf)) {
			want = int64(len(buf))
		}
		n, err := f.Read(buf[:want])
		if n > 0 {
			_, _ = hash.Write(buf[:n])
			got += int64(n)
		}
		if err != nil {
			return "", err
		}
	}
	var extra [1]byte
	n, err := f.Read(extra[:])
	if n > 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return "", errLocalStageInternal
	}
	if got != size {
		return "", errLocalStageInternal
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func readLimited(ctx context.Context, f *os.File, size int64) ([]byte, error) {
	if size < 0 || size > domain.MaxProbeBinaryStageMarkerSize {
		return nil, errLocalStageInternal
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, size+1)
	var got int
	for int64(got) < size+1 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := f.Read(buf[got:])
		got += n
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, io.ErrNoProgress
		}
	}
	if int64(got) != size {
		return nil, errLocalStageInternal
	}
	return buf[:size], nil
}

func directoryFinal(parent, child *os.File, dirName, binName string, childSt, binSt, markSt unix.Stat_t, binSize, markerSize int64) error {
	now, err := fstatFile(child)
	if err != nil || !sameStat(now, childSt) || !dirMode(now, true) {
		return errLocalStageInternal
	}
	var named unix.Stat_t
	if err := fstatAt(parent, dirName, &named); err != nil || !sameStat(named, childSt) || !dirMode(named, true) {
		return errLocalStageInternal
	}
	entries, err := child.ReadDir(3)
	if err != nil || len(entries) != 2 {
		return errLocalStageInternal
	}
	extra, err := child.ReadDir(1)
	if len(extra) != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return errLocalStageInternal
	}
	found := make(map[string]unix.Stat_t, 2)
	for _, entry := range entries {
		name := entry.Name()
		if !singleComponent(name) {
			return errLocalStageInternal
		}
		var st unix.Stat_t
		if err := fstatAt(child, name, &st); err != nil {
			return errLocalStageInternal
		}
		found[name] = st
	}
	gotBin, okBin := found[binName]
	gotMark, okMark := found[domain.ProbeBinaryStageMarkerName]
	if !okBin || !okMark || len(found) != 2 || sameStat(gotBin, gotMark) {
		return errLocalStageInternal
	}
	if !sameStat(gotBin, binSt) || !regularExact(gotBin, localStageBinPerm, binSize) {
		return errLocalStageInternal
	}
	if !sameStat(gotMark, markSt) || !regularExact(gotMark, localStageMarkPerm, markerSize) {
		return errLocalStageInternal
	}
	return nil
}

func sanitizeLocalStageError(attempted bool, err error) error {
	if err == nil {
		return nil
	}
	if !attempted {
		switch {
		case errors.Is(err, context.Canceled):
			return context.Canceled
		case errors.Is(err, context.DeadlineExceeded):
			return context.DeadlineExceeded
		case errors.Is(err, domain.ErrProbeBinaryStageWriteInput):
			return domain.ErrProbeBinaryStageWriteInput
		default:
			return domain.ErrProbeBinaryStageWriteUnavailable
		}
	}
	if errors.Is(err, domain.ErrProbeBinaryStageWriteConflict) {
		return domain.ErrProbeBinaryStageWriteConflict
	}
	return domain.ErrProbeBinaryStageWriteUnknown
}
