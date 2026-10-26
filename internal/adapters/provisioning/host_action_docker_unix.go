//go:build linux || darwin

package provisioning

import (
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// ProbeDocker runs one fixed-argument docker invocation from a fixed absolute
// path with a bounded stdout. Implementations pass an empty (or minimal)
// environment, never PATH, and report a nonzero exit as an exit code, not an
// error.
type ProbeDocker interface {
	Run(ctx context.Context, args ...string) (stdout string, exitCode int, err error)
}

// ProbeImageStageDirName is the single directory, beneath the host's protected
// /var/lib, that image_cache payloads are staged in. It never carries the
// install entry prefix, so preflight does not count it as an install.
const ProbeImageStageDirName = "uptime-phoenix-image-stage"

const (
	probeDockerIDFormat        = "{{.Id}}"
	probeDockerRunningFormat   = "{{.State.Running}}"
	probeDockerContainerFormat = `{{.Name}}|{{.State.Status}}|{{index .Config.Labels "` + domain.ProbeDockerOperationLabel + `"}}`
	probeDockerImageIDPrefix   = "sha256:"
	probeDockerMaxPathSize     = 4096

	probeDockerStatusCreated    = "created"
	probeDockerStatusExited     = "exited"
	probeDockerStatusRunning    = "running"
	probeDockerStatusRestarting = "restarting"
	probeDockerStatusPaused     = "paused"
)

// probeDockerStartWait and probeDockerStartPoll bound waiting for a started
// container to report running. They mirror the systemd start wait and are
// variables only so a test can shorten them; nothing else assigns them.
var (
	probeDockerStartWait = probeServiceStartWait
	probeDockerStartPoll = probeServicePoll
)

// errProbeDockerUnexpected marks an answer from docker that cannot be trusted
// (a malformed inspect, or a failed inspect the daemon cannot confirm).
var errProbeDockerUnexpected = errors.New("unexpected docker answer")

// probeDockerContainer is the identity-relevant part of one inspected container.
type probeDockerContainer struct {
	Name      string
	Status    string
	Operation string
}

// ApplyProbeImageCache loads the staged closed image archive into Docker and
// tags the verified image for this install. Nothing is run before the load
// (loading is idempotent); from the load on every failure is Unknown. The
// image must resolve, by the config digest (classic store), the selected
// manifest digest (containerd store) or the archive's generated layout index
// digest, to an ID equal to the one probed; the tag is then re-inspected to the same ID. Applied means the tag
// resolves to that verified ID. The image is shared and never removed.
func ApplyProbeImageCache(ctx context.Context, stagedArchive string, intent domain.ProbeHostActionIntent, docker ProbeDocker) error {
	if err := probeDockerIntent(ctx, docker, intent, domain.ProbeHostActionImageCache); err != nil {
		return err
	}
	tag := domain.ProbeDockerImageTag(intent.Identity)
	if tag == "" || !probeDockerAbsolutePath(stagedArchive) {
		return domain.ErrProbeBinaryStageWriteInput
	}
	// The classic image store identifies the image by its config digest; the
	// containerd image store by the manifest digest (observed with Docker 29),
	// and some versions by the loaded layout index digest.
	candidates := []string{
		probeDockerImageIDPrefix + intent.Identity.Image.ConfigSHA256,
		probeDockerImageIDPrefix + intent.Identity.Image.ManifestSHA256,
		probeDockerImageIDPrefix + intent.Payload.LayoutIndexSHA256,
	}
	// From here the Docker image store may change.
	if _, code, err := docker.Run(ctx, "load", "--input", stagedArchive); err != nil || code != 0 {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	id := ""
	for _, want := range candidates {
		got, found, err := probeDockerImageID(ctx, docker, want)
		if err != nil {
			return domain.ErrProbeBinaryStageWriteUnknown
		}
		if !found {
			continue
		}
		if got != want {
			return domain.ErrProbeBinaryStageWriteUnknown
		}
		id = got
		break
	}
	if id == "" {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	if _, code, err := docker.Run(ctx, "tag", id, tag); err != nil || code != 0 {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	if got, found, err := probeDockerImageID(ctx, docker, tag); err != nil || !found || got != id || ctx.Err() != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	return nil
}

// ApplyProbeContainer creates, but does not start, the probe container with
// exactly domain.ProbeDockerCreateArgs(identity). A container already holding
// the name is Conflict and an image tag that does not resolve is Unavailable;
// both are definite refusals before any effect. Once create runs every
// failure is Unknown. Applied means the container exists under its name with
// this operation's label in the created state.
func ApplyProbeContainer(ctx context.Context, intent domain.ProbeHostActionIntent, docker ProbeDocker) error {
	if err := probeDockerIntent(ctx, docker, intent, domain.ProbeHostActionContainer); err != nil {
		return err
	}
	args := domain.ProbeDockerCreateArgs(intent.Identity)
	_, service, ok := domain.ProbeInstallLayout(intent.Identity)
	if args == nil || !ok || !probeDockerContainerName(service.Reference) {
		return domain.ErrProbeBinaryStageWriteInput
	}
	name := service.Reference
	if _, present, err := probeDockerInspectContainer(ctx, docker, name); err != nil {
		return probeDockerUnavailable(ctx)
	} else if present {
		return domain.ErrProbeBinaryStageWriteConflict
	}
	if err := probeDockerRequireImage(ctx, docker, domain.ProbeDockerImageTag(intent.Identity)); err != nil {
		return err
	}
	// From here the container may exist.
	if _, code, err := docker.Run(ctx, args...); err != nil || code != 0 {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	c, present, err := probeDockerInspectContainer(ctx, docker, name)
	if err != nil || !present || c.Name != "/"+name || c.Operation != intent.Identity.OperationID ||
		c.Status != probeDockerStatusCreated || ctx.Err() != nil {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	return nil
}

// ApplyProbeDockerInit runs `probe init` once in a one-shot container with
// exactly domain.ProbeDockerInitArgs(identity). Before running anything it
// requires the data directory (beneath dataParent, the host's protected
// /var/lib) to be an empty 0700 directory owned by the effective uid and the
// install's image tag to resolve; those refusals are definite. Once the run
// starts every failure is Unknown: identity material may already exist.
func ApplyProbeDockerInit(ctx context.Context, dataParent *os.File, intent domain.ProbeHostActionIntent, docker ProbeDocker) (ProbeInitResult, error) {
	var zero ProbeInitResult
	if err := probeDockerIntent(ctx, docker, intent, domain.ProbeActivationProbeInit); err != nil {
		return zero, err
	}
	name, err := hostActionName(ctx, dataParent, intent, domain.ProbeActivationProbeInit)
	if err != nil {
		return zero, err
	}
	args := domain.ProbeDockerInitArgs(intent.Identity)
	if args == nil {
		return zero, domain.ErrProbeBinaryStageWriteInput
	}
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
	if err := probeDockerRequireImage(ctx, docker, domain.ProbeDockerImageTag(intent.Identity)); err != nil {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	// From here the probe may have created identity material.
	stdout, code, err := docker.Run(ctx, args...)
	if err != nil || code != 0 || ctx.Err() != nil {
		return zero, domain.ErrProbeBinaryStageWriteUnknown
	}
	result, ok := parseProbeInitOutput([]byte(stdout))
	if !ok {
		return zero, domain.ErrProbeBinaryStageWriteUnknown
	}
	return result, nil
}

// ApplyProbeDockerStart starts the intent's created container and waits for
// it to report running. The container must exist, carry this operation's
// label and be created or exited; one that is already running (or restarting
// or paused) is Conflict and a foreign label is Conflict. After `docker start`
// every failure, including not running in time, is Unknown.
func ApplyProbeDockerStart(ctx context.Context, intent domain.ProbeHostActionIntent, docker ProbeDocker) error {
	if err := probeDockerIntent(ctx, docker, intent, domain.ProbeActivationServiceStart); err != nil {
		return err
	}
	_, service, ok := domain.ProbeInstallLayout(intent.Identity)
	if !ok || !probeDockerContainerName(service.Reference) {
		return domain.ErrProbeBinaryStageWriteInput
	}
	name := service.Reference
	c, present, err := probeDockerInspectContainer(ctx, docker, name)
	if err != nil || !present {
		return probeDockerUnavailable(ctx)
	}
	if c.Name != "/"+name || c.Operation != intent.Identity.OperationID {
		return domain.ErrProbeBinaryStageWriteConflict
	}
	switch c.Status {
	case probeDockerStatusCreated, probeDockerStatusExited:
	case probeDockerStatusRunning, probeDockerStatusRestarting, probeDockerStatusPaused:
		return domain.ErrProbeBinaryStageWriteConflict
	default:
		return domain.ErrProbeBinaryStageWriteUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, code, err := docker.Run(ctx, "start", name); err != nil || code != 0 {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	deadline := time.Now().Add(probeDockerStartWait)
	for {
		out, code, err := docker.Run(ctx, "container", "inspect", "--format", probeDockerRunningFormat, name)
		if err == nil && code == 0 && out == "true\n" {
			return nil
		}
		if ctx.Err() != nil || !time.Now().Before(deadline) {
			return domain.ErrProbeBinaryStageWriteUnknown
		}
		select {
		case <-ctx.Done():
			return domain.ErrProbeBinaryStageWriteUnknown
		case <-time.After(probeDockerStartPoll):
		}
	}
}

// ApplyProbeDockerCleanup removes what one Docker install operation created:
// the container, then the data directory tree beneath dataParent (the host's
// /var/lib). The image is shared and is never removed. An absent resource
// counts as removed, so a partial install is cleaned and a clean host is a
// no-op that runs nothing but the existence checks.
//
// Everything is inspected before anything changes: a container whose
// operation label is not this operation's, or a data directory that is a link
// or foreign-owned, is a definite refusal, and so is a Docker daemon that
// cannot say whether the container exists. Once an effect may have happened
// every failure is Unknown, including a final check that the container and
// the data directory are both gone.
func ApplyProbeDockerCleanup(ctx context.Context, dataParent *os.File, intent domain.ProbeHostActionIntent, docker ProbeDocker) error {
	if err := probeDockerIntent(ctx, docker, intent, domain.ProbeHostActionCleanup); err != nil {
		return err
	}
	name, err := hostActionName(ctx, dataParent, intent, domain.ProbeHostActionCleanup)
	if err != nil {
		return err
	}
	_, service, ok := domain.ProbeInstallLayout(intent.Identity)
	if !ok || service.Reference != name || !probeDockerContainerName(name) {
		return domain.ErrProbeBinaryStageWriteInput
	}

	// Phase 1: inspect. No effects.
	c, present, err := probeDockerInspectContainer(ctx, docker, name)
	if err != nil {
		return probeDockerUnavailable(ctx)
	}
	if present && (c.Name != "/"+name || c.Operation != intent.Identity.OperationID) {
		return domain.ErrProbeBinaryStageWriteConflict
	}
	data, dataSt, err := cleanupOpenOwnedDir(dataParent, name)
	if err != nil {
		return err
	}
	insp := &cleanupInspection{data: data, dataSt: dataSt}
	defer insp.close()
	if !present && insp.data == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Phase 2: effects. Every failure from here is Unknown.
	if present {
		if _, code, err := docker.Run(ctx, "rm", "--force", name); err != nil || code != 0 {
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

	// Phase 3: verify both names are gone.
	if _, stillThere, err := probeDockerInspectContainer(ctx, docker, name); err != nil || stillThere || !cleanupAbsent(dataParent, name) {
		return domain.ErrProbeBinaryStageWriteUnknown
	}
	return nil
}

// probeDockerIntent validates a Docker applier call: a live context, a
// runner, and a valid Docker-mode intent of exactly this kind. A systemd
// intent is never applied by a Docker applier.
func probeDockerIntent(ctx context.Context, docker ProbeDocker, intent domain.ProbeHostActionIntent, kind string) error {
	if imageSnapshotNil(ctx) || imageSnapshotNil(docker) || !intent.Valid() ||
		intent.Kind != kind || intent.Identity.Mode != domain.ProbeInstallDocker {
		return domain.ErrProbeBinaryStageWriteInput
	}
	return ctx.Err()
}

// probeDockerUnavailable is the definite refusal for a pre-effect docker
// failure, or the context's own error when the call was canceled.
func probeDockerUnavailable(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return domain.ErrProbeBinaryStageWriteUnavailable
}

// probeDockerRequireImage refuses (Unavailable) unless the tag resolves to a
// well-formed image ID.
func probeDockerRequireImage(ctx context.Context, docker ProbeDocker, tag string) error {
	if tag == "" {
		return domain.ErrProbeBinaryStageWriteInput
	}
	id, found, err := probeDockerImageID(ctx, docker, tag)
	if err != nil || !found || !probeDockerValidImageID(id) {
		return probeDockerUnavailable(ctx)
	}
	return nil
}

// probeDockerImageID resolves one image reference to its ID. found is false
// when docker reports no such image (a nonzero exit); a runner failure is an
// error.
func probeDockerImageID(ctx context.Context, docker ProbeDocker, ref string) (id string, found bool, err error) {
	out, code, err := docker.Run(ctx, "image", "inspect", "--format", probeDockerIDFormat, ref)
	if err != nil {
		return "", false, err
	}
	if code != 0 {
		return "", false, nil
	}
	return strings.TrimSuffix(out, "\n"), true, nil
}

// probeDockerInspectContainer reports whether the named container exists and,
// when it does, its name, state and operation label. A failed inspect only
// means absent when the daemon also answers an empty listing for the exact
// name: a daemon that is down must never read as a container that is gone.
func probeDockerInspectContainer(ctx context.Context, docker ProbeDocker, name string) (probeDockerContainer, bool, error) {
	var zero probeDockerContainer
	out, code, err := docker.Run(ctx, "container", "inspect", "--format", probeDockerContainerFormat, name)
	if err != nil {
		return zero, false, err
	}
	if code == 0 {
		parts := strings.SplitN(strings.TrimSuffix(out, "\n"), "|", 3)
		if len(parts) != 3 {
			return zero, false, errProbeDockerUnexpected
		}
		return probeDockerContainer{Name: parts[0], Status: parts[1], Operation: parts[2]}, true, nil
	}
	listed, code, err := docker.Run(ctx, "container", "ls", "--all", "--quiet", "--filter", "name=^/"+name+"$")
	if err != nil {
		return zero, false, err
	}
	if code != 0 || strings.TrimSpace(listed) != "" {
		return zero, false, errProbeDockerUnexpected
	}
	return zero, false, nil
}

func probeDockerValidImageID(id string) bool {
	hex, ok := strings.CutPrefix(id, probeDockerImageIDPrefix)
	if !ok || len(hex) != 64 {
		return false
	}
	for i := 0; i < len(hex); i++ {
		if c := hex[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// probeDockerContainerName accepts only a single safe name component, which
// also keeps it a literal inside the listing filter's regular expression.
func probeDockerContainerName(name string) bool {
	if name == "" || len(name) > 128 {
		return false
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

// probeDockerAbsolutePath accepts a clean absolute path without control
// characters, so it can never be read as a docker flag.
func probeDockerAbsolutePath(p string) bool {
	if p == "" || len(p) > probeDockerMaxPathSize || !path.IsAbs(p) || path.Clean(p) != p {
		return false
	}
	return !strings.ContainsAny(p, "\x00\n\r")
}

// ProbeImageStageFile is one exclusive, root-only staged image archive. The
// helper streams the verified payload into it and hands Path to docker load;
// Remove deletes it and releases every descriptor.
type ProbeImageStageFile struct {
	dir  *os.File
	file *os.File
	name string
	path string
}

// OpenProbeImageStageFile creates the intent's staging file
// <operation_id>-<attempt_id>.tar (O_EXCL, O_NOFOLLOW, 0600) inside
// ProbeImageStageDirName beneath a borrowed parent (the host's protected
// /var/lib; parentPath is that directory's absolute path, used only to name
// the file for docker). The staging directory is created 0700 when absent;
// one that exists must be a real directory owned by the effective uid with
// mode 0700, otherwise it is refused (Unavailable). An existing file of that
// name is Conflict.
func OpenProbeImageStageFile(ctx context.Context, parent *os.File, parentPath string, intent domain.ProbeHostActionIntent) (*ProbeImageStageFile, error) {
	if imageSnapshotNil(ctx) || parent == nil || !probeDockerAbsolutePath(parentPath) || !intent.Valid() ||
		intent.Kind != domain.ProbeHostActionImageCache || intent.Identity.Mode != domain.ProbeInstallDocker {
		return nil, domain.ErrProbeBinaryStageWriteInput
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := intent.Identity.OperationID + "-" + intent.AttemptID + ".tar"
	dir, err := probeImageStageDir(parent)
	if err != nil {
		return nil, err
	}
	file, err := openAt(dir, name, localStageWriteFlags, localStageCreatePerm)
	if err != nil {
		_ = dir.Close()
		if errors.Is(err, unix.EEXIST) {
			return nil, domain.ErrProbeBinaryStageWriteConflict
		}
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	fail := func(err error) (*ProbeImageStageFile, error) {
		_ = file.Close()
		_ = cleanupUnlinkAt(dir, name, 0)
		_ = dir.Close()
		return nil, err
	}
	// Set the exact mode on the descriptor, independent of umask.
	if err := fchmod(file, localStageCreatePerm); err != nil {
		return fail(domain.ErrProbeBinaryStageWriteUnavailable)
	}
	st, err := fstatFile(file)
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Nlink != 1 || int(st.Uid) != os.Geteuid() ||
		uint32(uint64(st.Mode&0o7777)) != localStageCreatePerm {
		return fail(domain.ErrProbeBinaryStageWriteUnavailable)
	}
	return &ProbeImageStageFile{dir: dir, file: file, name: name, path: path.Join(parentPath, ProbeImageStageDirName, name)}, nil
}

// probeImageStageDir opens the staging directory beneath parent, creating it
// 0700 when absent, and requires it to be a real directory owned by the
// effective uid with exactly mode 0700.
func probeImageStageDir(parent *os.File) (*os.File, error) {
	created := false
	if err := mkdirAt(parent, ProbeImageStageDirName, hostActionDataDirPerm); err == nil {
		created = true
	} else if !errors.Is(err, unix.EEXIST) {
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	dir, err := openAt(parent, ProbeImageStageDirName, localStageDirFlags, 0)
	if err != nil {
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	if created {
		if err := fchmod(dir, hostActionDataDirPerm); err != nil {
			_ = dir.Close()
			return nil, domain.ErrProbeBinaryStageWriteUnavailable
		}
	}
	st, err := fstatFile(dir)
	if err != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR || int(st.Uid) != os.Geteuid() || st.Mode&0o7777 != hostActionDataDirPerm {
		_ = dir.Close()
		return nil, domain.ErrProbeBinaryStageWriteUnavailable
	}
	return dir, nil
}

// Write appends archive bytes to the staged file.
func (f *ProbeImageStageFile) Write(p []byte) (int, error) {
	if f == nil || f.file == nil {
		return 0, os.ErrClosed
	}
	return f.file.Write(p)
}

// Sync flushes the staged file to stable storage.
func (f *ProbeImageStageFile) Sync() error {
	if f == nil || f.file == nil {
		return os.ErrClosed
	}
	return f.file.Sync()
}

// Close closes the staged file's descriptor so docker can read a complete
// archive; the file stays on disk until Remove.
func (f *ProbeImageStageFile) Close() error {
	if f == nil || f.file == nil {
		return nil
	}
	err := f.file.Close()
	f.file = nil
	return err
}

// Path is the absolute path docker load reads.
func (f *ProbeImageStageFile) Path() string {
	if f == nil {
		return ""
	}
	return f.path
}

// Remove closes anything still open and unlinks the staged file by name
// beneath the staging directory. It is idempotent: an already removed file is
// success.
func (f *ProbeImageStageFile) Remove() error {
	if f == nil {
		return nil
	}
	err := f.Close()
	if f.dir != nil {
		if unlinkErr := cleanupUnlinkAt(f.dir, f.name, 0); unlinkErr != nil && err == nil {
			err = unlinkErr
		}
		if closeErr := f.dir.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
		f.dir = nil
	}
	return err
}
