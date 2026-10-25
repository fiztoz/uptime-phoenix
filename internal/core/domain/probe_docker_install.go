package domain

import (
	"slices"
	"strings"
)

// Docker install resource kinds. image_cache loads the pinned image (shared,
// never owned or removed); container creates, but does not start, the probe
// container. data_directory is shared with the systemd layout.
const (
	ProbeHostActionImageCache = "image_cache"
	ProbeHostActionContainer  = "container"
)

// ProbeDockerDataMount is the probe's data directory inside the container.
const ProbeDockerDataMount = "/var/lib/uptime-phoenix/probe"

// ProbeDockerImageRepository is the local repository the image_cache action
// tags the verified image under; the tag is the selected manifest digest hex.
const ProbeDockerImageRepository = "uptime-phoenix-probe"

// ProbeDockerOperationLabel labels the container with its install operation,
// so cleanup removes only a container this operation created.
const ProbeDockerOperationLabel = "io.uptime-phoenix.operation"

// ProbeHostActionPayload describes the bytes an image_cache action streams
// after its grant: the closed image archive's SHA-256 and size, and the
// digest of its generated OCI layout index (the image ID Docker reports for
// it under the containerd image store). Every other kind carries none.
type ProbeHostActionPayload struct {
	SHA256            string
	Size              int64
	LayoutIndexSHA256 string
}

// Valid requires lowercase SHA-256 values and a bounded archive size.
func (p ProbeHostActionPayload) Valid() bool {
	return probeLowerHex64(p.SHA256) && probeLowerHex64(p.LayoutIndexSHA256) && p.Size > 0 && p.Size <= MaxProbeImageArchiveBytes
}

func probeLowerHex64(v string) bool {
	if len(v) != 64 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if c := v[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ProbeInstallLayout returns an install's data directory and service
// resources (the systemd service or the Docker container), whatever the mode.
// Activation and cleanup address resources through it, never by index.
func ProbeInstallLayout(r ProbeInstallIdentity) (data, service ProbeInstallResourceSpec, ok bool) {
	specs := r.Resources()
	switch {
	case r.Mode == ProbeInstallSystemd && len(specs) == 4:
		return specs[0], specs[3], true
	case r.Mode == ProbeInstallDocker && len(specs) == 3:
		return specs[1], specs[2], true
	}
	return ProbeInstallResourceSpec{}, ProbeInstallResourceSpec{}, false
}

// ProbeDockerImageTag is the local tag image_cache applies to the verified
// image and the container and probe_init run, or "" for a non-Docker identity.
func ProbeDockerImageTag(r ProbeInstallIdentity) string {
	if !r.Valid() || r.Mode != ProbeInstallDocker {
		return ""
	}
	return ProbeDockerImageRepository + ":" + r.Image.ManifestSHA256
}

// probeDockerCommonArgs are the hardening flags shared by the container and
// the one-shot probe_init run: root with only CAP_NET_RAW (as the approved
// systemd unit), no privilege escalation, a read-only root filesystem and
// write access only to the operation's data directory.
func probeDockerCommonArgs(r ProbeInstallIdentity) []string {
	data, _, ok := ProbeInstallLayout(r)
	if !ok {
		return nil
	}
	return []string{
		"--user", "0:0",
		"--cap-drop", "ALL",
		"--cap-add", "NET_RAW",
		"--security-opt", "no-new-privileges",
		"--read-only",
		"--tmpfs", "/tmp",
		"--mount", "type=bind,src=" + data.Reference + ",dst=" + ProbeDockerDataMount,
		"--label", ProbeDockerOperationLabel + "=" + r.OperationID,
	}
}

// ProbeDockerCreateArgs derives the exact `docker` argument vector that
// creates (does not start) the probe container, or nil for a non-Docker
// identity. It is pure data: the hub signs its hash and the helper runs
// exactly these arguments.
func ProbeDockerCreateArgs(r ProbeInstallIdentity) []string {
	tag := ProbeDockerImageTag(r)
	_, service, ok := ProbeInstallLayout(r)
	common := probeDockerCommonArgs(r)
	if tag == "" || !ok || common == nil {
		return nil
	}
	head := []string{"create", "--name", service.Reference, "--restart", "unless-stopped"}
	tail := []string{"--publish", "8443:8443", tag, "run", "--data-dir", ProbeDockerDataMount}
	return slices.Concat(head, common, tail)
}

// ProbeDockerInitArgs derives the exact `docker` argument vector for the
// one-shot `probe init` run: no network, removed on exit, or nil.
func ProbeDockerInitArgs(r ProbeInstallIdentity) []string {
	tag := ProbeDockerImageTag(r)
	common := probeDockerCommonArgs(r)
	if tag == "" || common == nil {
		return nil
	}
	head := []string{"run", "--rm", "--network", "none"}
	tail := []string{tag, "init", "--data-dir", ProbeDockerDataMount}
	return slices.Concat(head, common, tail)
}

// probeDockerArgsDigestInput joins an argument vector unambiguously for the
// marker hash (arguments never contain NUL).
func probeDockerArgsDigestInput(args []string) string {
	return strings.Join(args, "\x00")
}

// ProbeImageLayoutDirName is the operator artifact-directory entry holding
// the OCI image layout for a pin. It is not an input pathname.
func ProbeImageLayoutDirName(p ProbeImagePin) string {
	if !p.Valid() {
		return ""
	}
	return "uptime-phoenix-probe_" + p.Version + "_oci"
}
