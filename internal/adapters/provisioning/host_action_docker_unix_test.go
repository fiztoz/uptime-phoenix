//go:build linux || darwin

package provisioning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// These tests drive the Docker appliers against a stateful fake of the docker
// CLI (dockerSim) that records every exact argument vector, with t.TempDir()
// directories standing in for the borrowed /var/lib descriptor. No real
// docker, daemon, root-owned path or network is involved, and ownership is
// checked against the test process's own euid.

const dockerTestStateFormat = `{{.Name}}|{{.State.Status}}|{{index .Config.Labels "io.uptime-phoenix.operation"}}`

type dockerSimContainer struct{ status, label string }

// dockerSim models just the verbs the appliers may run. override lets a test
// answer a specific call itself (handled=true) to inject failures.
type dockerSim struct {
	mu          sync.Mutex
	calls       [][]string
	images      map[string]string
	containers  map[string]*dockerSimContainer
	loadIDs     []string
	initOut     string
	createState string
	neverRuns   bool
	runningAt   int // the n-th running poll (1-based) is the first to answer true
	polls       int
	override    func(args []string) (stdout string, code int, err error, handled bool)
}

func newDockerSim() *dockerSim {
	return &dockerSim{images: map[string]string{}, containers: map[string]*dockerSimContainer{}, createState: "created", runningAt: 1}
}

func (d *dockerSim) Run(_ context.Context, args ...string) (string, int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, slices.Clone(args))
	if d.override != nil {
		if out, code, err, ok := d.override(args); ok {
			return out, code, err
		}
	}
	if len(args) == 0 {
		return "", 2, nil
	}
	switch args[0] {
	case "load":
		for _, id := range d.loadIDs {
			d.images[id] = id
		}
		return "Loaded image ID: x\n", 0, nil
	case "image":
		if len(args) == 5 && args[1] == "inspect" && args[2] == "--format" && args[3] == "{{.Id}}" {
			if id, ok := d.images[args[4]]; ok {
				return id + "\n", 0, nil
			}
		}
		return "", 1, nil
	case "tag":
		if id, ok := d.images[args[1]]; ok && len(args) == 3 {
			d.images[args[2]] = id
			return "", 0, nil
		}
		return "", 1, nil
	case "container":
		out, code := d.container(args)
		return out, code, nil
	case "create":
		name, label := "", ""
		for i, a := range args {
			if a == "--name" {
				name = args[i+1]
			}
			if a == "--label" {
				_, label, _ = strings.Cut(args[i+1], "=")
			}
		}
		d.containers[name] = &dockerSimContainer{status: d.createState, label: label}
		return "cid\n", 0, nil
	case "start":
		if c, ok := d.containers[args[1]]; ok {
			if !d.neverRuns {
				c.status = "running"
			}
			return args[1] + "\n", 0, nil
		}
		return "", 1, nil
	case "rm":
		delete(d.containers, args[len(args)-1])
		return "", 0, nil
	case "run":
		return d.initOut, 0, nil
	}
	return "", 2, nil
}

func (d *dockerSim) container(args []string) (string, int) {
	switch {
	case len(args) == 5 && args[1] == "inspect" && args[2] == "--format":
		c, ok := d.containers[args[4]]
		if !ok {
			return "", 1
		}
		if args[3] == "{{.State.Running}}" {
			d.polls++
			if c.status == "running" && d.polls >= d.runningAt {
				return "true\n", 0
			}
			return "false\n", 0
		}
		if args[3] == dockerTestStateFormat {
			return "/" + args[4] + "|" + c.status + "|" + c.label + "\n", 0
		}
	case len(args) == 6 && args[1] == "ls":
		name := strings.TrimSuffix(strings.TrimPrefix(args[5], "name=^/"), "$")
		if _, ok := d.containers[name]; ok {
			return "cid\n", 0
		}
		return "", 0
	}
	return "", 2
}

func (d *dockerSim) recorded() [][]string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.calls)
}

func (d *dockerSim) wantCalls(t *testing.T, want ...[]string) {
	t.Helper()
	got := d.recorded()
	if len(got) != len(want) {
		t.Fatalf("docker calls:\n got %q\nwant %q", got, want)
	}
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Fatalf("docker call %d:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

func (d *dockerSim) wantNoCall(t *testing.T, verb string) {
	t.Helper()
	for _, c := range d.recorded() {
		if len(c) > 0 && c[0] == verb {
			t.Fatalf("docker %s ran: %q", verb, c)
		}
	}
}

var _ ProbeDocker = (*dockerSim)(nil)

func dockerCall(args ...string) []string { return args }

const (
	dockerTestConfigID = "sha256:" + "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	dockerTestIndexID  = "sha256:" + "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	// dockerTestManifestID is the selected manifest digest of dockerFrameIntent.
	dockerTestManifestID = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	dockerTestOtherID    = "sha256:" + "0123456789012345678901234567890123456789012345678901234567890123"
	dockerTestStaged     = "/var/lib/uptime-phoenix-image-stage/stage.tar"
)

func dockerTestTag(intent domain.ProbeHostActionIntent) string {
	return domain.ProbeDockerImageTag(intent.Identity)
}

func dockerTestName(intent domain.ProbeHostActionIntent) string {
	return "uptime-phoenix-probe-" + intent.Identity.OperationID
}

func dockerInspectImage(ref string) []string {
	return dockerCall("image", "inspect", "--format", "{{.Id}}", ref)
}

func dockerInspectState(name string) []string {
	return dockerCall("container", "inspect", "--format", dockerTestStateFormat, name)
}

func dockerListName(name string) []string {
	return dockerCall("container", "ls", "--all", "--quiet", "--filter", "name=^/"+name+"$")
}

func dockerInspectRunning(name string) []string {
	return dockerCall("container", "inspect", "--format", "{{.State.Running}}", name)
}

func wantDockerErr(t *testing.T, what string, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("%s: got %v, want %v", what, got, want)
	}
}

// ---- image_cache ------------------------------------------------------------

func TestApplyProbeImageCacheClassicStoreResolvesByConfigDigest(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeHostActionImageCache)
	sim := newDockerSim()
	sim.loadIDs = []string{dockerTestConfigID}
	if err := ApplyProbeImageCache(t.Context(), dockerTestStaged, intent, sim); err != nil {
		t.Fatal(err)
	}
	tag := dockerTestTag(intent)
	sim.wantCalls(t,
		dockerCall("load", "--input", dockerTestStaged),
		dockerInspectImage(dockerTestConfigID),
		dockerCall("tag", dockerTestConfigID, tag),
		dockerInspectImage(tag),
	)
	if sim.images[tag] != dockerTestConfigID {
		t.Fatalf("tag resolves to %q", sim.images[tag])
	}
}

// Docker 29's containerd image store reports the selected manifest digest as
// the loaded image ID (observed with a real `docker load` of the archive).
func TestApplyProbeImageCacheContainerdStoreResolvesByManifestDigest(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeHostActionImageCache)
	sim := newDockerSim()
	sim.loadIDs = []string{dockerTestManifestID}
	if err := ApplyProbeImageCache(t.Context(), dockerTestStaged, intent, sim); err != nil {
		t.Fatal(err)
	}
	tag := dockerTestTag(intent)
	sim.wantCalls(t,
		dockerCall("load", "--input", dockerTestStaged),
		dockerInspectImage(dockerTestConfigID),
		dockerInspectImage(dockerTestManifestID),
		dockerCall("tag", dockerTestManifestID, tag),
		dockerInspectImage(tag),
	)
	if sim.images[tag] != dockerTestManifestID {
		t.Fatalf("tag resolves to %q", sim.images[tag])
	}
}

func TestApplyProbeImageCacheContainerdStoreResolvesByLayoutIndexDigest(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeHostActionImageCache)
	sim := newDockerSim()
	sim.loadIDs = []string{dockerTestIndexID}
	if err := ApplyProbeImageCache(t.Context(), dockerTestStaged, intent, sim); err != nil {
		t.Fatal(err)
	}
	tag := dockerTestTag(intent)
	sim.wantCalls(t,
		dockerCall("load", "--input", dockerTestStaged),
		dockerInspectImage(dockerTestConfigID),
		dockerInspectImage(dockerTestManifestID),
		dockerInspectImage(dockerTestIndexID),
		dockerCall("tag", dockerTestIndexID, tag),
		dockerInspectImage(tag),
	)
	if sim.images[tag] != dockerTestIndexID {
		t.Fatalf("tag resolves to %q", sim.images[tag])
	}
}

func TestApplyProbeImageCacheEveryFailureAfterLoadIsUnknown(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeHostActionImageCache)
	tag := dockerTestTag(intent)
	cases := map[string]struct {
		prep      func(*dockerSim)
		wantCalls int
		wantNoTag bool
	}{
		"load exits nonzero": {func(s *dockerSim) {
			s.override = func(a []string) (string, int, error, bool) {
				return "", 1, nil, a[0] == "load"
			}
		}, 1, true},
		"load runner error": {func(s *dockerSim) {
			s.override = func(a []string) (string, int, error, bool) {
				return "", -1, errors.New("boom"), a[0] == "load"
			}
		}, 1, true},
		"neither digest resolves": {func(s *dockerSim) {}, 4, true},
		"config digest resolves to another id": {func(s *dockerSim) {
			s.override = func(a []string) (string, int, error, bool) {
				return dockerTestOtherID + "\n", 0, nil, a[0] == "image" && a[4] == dockerTestConfigID
			}
		}, 2, true},
		"index digest resolves to another id": {func(s *dockerSim) {
			s.override = func(a []string) (string, int, error, bool) {
				if a[0] == "image" && a[4] == dockerTestConfigID {
					return "", 1, nil, true
				}
				return dockerTestOtherID + "\n", 0, nil, a[0] == "image" && a[4] == dockerTestIndexID
			}
		}, 4, true},
		"inspect runner error": {func(s *dockerSim) {
			s.override = func(a []string) (string, int, error, bool) {
				return "", -1, errors.New("boom"), a[0] == "image"
			}
		}, 2, true},
		"tag exits nonzero": {func(s *dockerSim) {
			s.loadIDs = []string{dockerTestConfigID}
			s.override = func(a []string) (string, int, error, bool) { return "", 1, nil, a[0] == "tag" }
		}, 3, false},
		"tag resolves to another id": {func(s *dockerSim) {
			s.loadIDs = []string{dockerTestConfigID}
			s.override = func(a []string) (string, int, error, bool) {
				return dockerTestOtherID + "\n", 0, nil, a[0] == "image" && a[4] == tag
			}
		}, 4, false},
		"tag does not resolve": {func(s *dockerSim) {
			s.loadIDs = []string{dockerTestConfigID}
			s.override = func(a []string) (string, int, error, bool) {
				return "", 1, nil, a[0] == "image" && a[4] == tag
			}
		}, 4, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sim := newDockerSim()
			tc.prep(sim)
			err := ApplyProbeImageCache(t.Context(), dockerTestStaged, intent, sim)
			wantDockerErr(t, name, err, domain.ErrProbeBinaryStageWriteUnknown)
			if got := len(sim.recorded()); got != tc.wantCalls {
				t.Fatalf("%d docker calls, want %d: %q", got, tc.wantCalls, sim.recorded())
			}
			if tc.wantNoTag {
				sim.wantNoCall(t, "tag")
			}
		})
	}
}

func TestApplyProbeImageCacheRefusesBadInputBeforeAnyDockerCall(t *testing.T) {
	good := dockerFrameIntent(t, domain.ProbeHostActionImageCache)
	systemd := hostActFrameIntent(domain.ProbeHostActionImageCache)
	for name, tc := range map[string]struct {
		staged string
		intent domain.ProbeHostActionIntent
		docker ProbeDocker
	}{
		"empty path":          {"", good, newDockerSim()},
		"relative path":       {"stage.tar", good, newDockerSim()},
		"unclean path":        {"/var/lib/x/../stage.tar", good, newDockerSim()},
		"newline in path":     {"/var/lib/x\nstage.tar", good, newDockerSim()},
		"container kind":      {dockerTestStaged, dockerFrameIntent(t, domain.ProbeHostActionContainer), newDockerSim()},
		"systemd identity":    {dockerTestStaged, systemd, newDockerSim()},
		"payload-less intent": {dockerTestStaged, domain.ProbeHostActionIntent{}, newDockerSim()},
		"nil runner":          {dockerTestStaged, good, nil},
	} {
		t.Run(name, func(t *testing.T) {
			err := ApplyProbeImageCache(t.Context(), tc.staged, tc.intent, tc.docker)
			wantDockerErr(t, name, err, domain.ErrProbeBinaryStageWriteInput)
			if sim, ok := tc.docker.(*dockerSim); ok && len(sim.recorded()) != 0 {
				t.Fatalf("docker ran: %q", sim.recorded())
			}
		})
	}
	var typedNil *dockerSim
	wantDockerErr(t, "typed nil runner", ApplyProbeImageCache(t.Context(), dockerTestStaged, good, typedNil), domain.ErrProbeBinaryStageWriteInput)
}

// ---- container --------------------------------------------------------------

func dockerContainerSim(t *testing.T, intent domain.ProbeHostActionIntent) *dockerSim {
	t.Helper()
	sim := newDockerSim()
	sim.images[dockerTestTag(intent)] = dockerTestConfigID
	return sim
}

func TestApplyProbeContainerCreatesExactlyTheSignedArguments(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeHostActionContainer)
	sim := dockerContainerSim(t, intent)
	if err := ApplyProbeContainer(t.Context(), intent, sim); err != nil {
		t.Fatal(err)
	}
	name := dockerTestName(intent)
	create := domain.ProbeDockerCreateArgs(intent.Identity)
	sim.wantCalls(t,
		dockerInspectState(name),
		dockerListName(name),
		dockerInspectImage(dockerTestTag(intent)),
		create,
		dockerInspectState(name),
	)
	// The marker the grant signs binds the hash of exactly the vector that ran.
	sum := sha256.Sum256([]byte(strings.Join(sim.recorded()[3], "\x00")))
	if !strings.Contains(intent.Marker(), "\ncontainer_sha256="+hex.EncodeToString(sum[:])+"\n") {
		t.Fatal("the signed marker does not bind the executed create arguments")
	}
	if create[0] != "create" || create[1] != "--name" || create[2] != name {
		t.Fatalf("create vector %q", create)
	}
	if c := sim.containers[name]; c == nil || c.status != "created" || c.label != intent.Identity.OperationID {
		t.Fatalf("container %+v", c)
	}
	sim.wantNoCall(t, "start")
}

func TestApplyProbeContainerRefusalsBeforeCreate(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeHostActionContainer)
	name := dockerTestName(intent)
	t.Run("an existing container is Conflict and nothing is created", func(t *testing.T) {
		for label, op := range map[string]string{"ours": intent.Identity.OperationID, "foreign": "someone-else"} {
			sim := dockerContainerSim(t, intent)
			sim.containers[name] = &dockerSimContainer{status: "running", label: op}
			wantDockerErr(t, label, ApplyProbeContainer(t.Context(), intent, sim), domain.ErrProbeBinaryStageWriteConflict)
			sim.wantCalls(t, dockerInspectState(name))
		}
	})
	t.Run("a missing image tag is Unavailable", func(t *testing.T) {
		sim := newDockerSim()
		wantDockerErr(t, "no image", ApplyProbeContainer(t.Context(), intent, sim), domain.ErrProbeBinaryStageWriteUnavailable)
		sim.wantNoCall(t, "create")
	})
	t.Run("a malformed image id is Unavailable", func(t *testing.T) {
		sim := newDockerSim()
		sim.images[dockerTestTag(intent)] = "not-an-id"
		wantDockerErr(t, "bad id", ApplyProbeContainer(t.Context(), intent, sim), domain.ErrProbeBinaryStageWriteUnavailable)
		sim.wantNoCall(t, "create")
	})
	t.Run("a daemon that cannot answer is Unavailable, never read as absent", func(t *testing.T) {
		sim := dockerContainerSim(t, intent)
		sim.override = func([]string) (string, int, error, bool) { return "", 1, nil, true }
		wantDockerErr(t, "daemon down", ApplyProbeContainer(t.Context(), intent, sim), domain.ErrProbeBinaryStageWriteUnavailable)
		sim.wantNoCall(t, "create")
	})
	t.Run("a runner error on the existence check is Unavailable", func(t *testing.T) {
		sim := dockerContainerSim(t, intent)
		sim.override = func([]string) (string, int, error, bool) { return "", -1, errors.New("boom"), true }
		wantDockerErr(t, "runner error", ApplyProbeContainer(t.Context(), intent, sim), domain.ErrProbeBinaryStageWriteUnavailable)
		sim.wantNoCall(t, "create")
	})
	t.Run("a systemd identity or other kind is Input", func(t *testing.T) {
		for name, in := range map[string]domain.ProbeHostActionIntent{
			"systemd": hostActFrameIntent(domain.ProbeHostActionService),
			"kind":    dockerFrameIntent(t, domain.ProbeHostActionDataDirectory),
		} {
			sim := newDockerSim()
			wantDockerErr(t, name, ApplyProbeContainer(t.Context(), in, sim), domain.ErrProbeBinaryStageWriteInput)
			if len(sim.recorded()) != 0 {
				t.Fatalf("%s: docker ran", name)
			}
		}
	})
}

func TestApplyProbeContainerFailuresAfterCreateAreUnknown(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeHostActionContainer)
	name := dockerTestName(intent)
	for name2, prep := range map[string]func(*dockerSim){
		"create exits nonzero": func(s *dockerSim) {
			s.override = func(a []string) (string, int, error, bool) { return "", 1, nil, a[0] == "create" }
		},
		"create runner error": func(s *dockerSim) {
			s.override = func(a []string) (string, int, error, bool) { return "", -1, errors.New("boom"), a[0] == "create" }
		},
		"container is not in the created state": func(s *dockerSim) { s.createState = "running" },
		"container carries another operation label": func(s *dockerSim) {
			s.override = func(a []string) (string, int, error, bool) {
				if a[0] == "container" && a[1] == "inspect" && len(s.calls) > 4 {
					return "/" + name + "|created|other\n", 0, nil, true
				}
				return "", 0, nil, false
			}
		},
		"container carries another name": func(s *dockerSim) {
			s.override = func(a []string) (string, int, error, bool) {
				if a[0] == "container" && a[1] == "inspect" && len(s.calls) > 4 {
					return "/other|created|" + intent.Identity.OperationID + "\n", 0, nil, true
				}
				return "", 0, nil, false
			}
		},
		"container cannot be inspected": func(s *dockerSim) {
			s.override = func(a []string) (string, int, error, bool) {
				if a[0] == "container" && len(s.calls) > 4 {
					return "", 1, nil, true
				}
				return "", 0, nil, false
			}
		},
	} {
		t.Run(name2, func(t *testing.T) {
			sim := dockerContainerSim(t, intent)
			prep(sim)
			wantDockerErr(t, name2, ApplyProbeContainer(t.Context(), intent, sim), domain.ErrProbeBinaryStageWriteUnknown)
		})
	}
}

// ---- probe_init -------------------------------------------------------------

type dockerInitEnv struct {
	intent  domain.ProbeHostActionIntent
	sim     *dockerSim
	root    string
	parent  *os.File
	dataDir string
}

func newDockerInitEnv(t *testing.T) *dockerInitEnv {
	t.Helper()
	e := &dockerInitEnv{intent: dockerFrameIntent(t, domain.ProbeActivationProbeInit), sim: newDockerSim()}
	e.sim.images[dockerTestTag(e.intent)] = dockerTestConfigID
	e.sim.initOut = string(probeInitRealBytes(t, nil))
	e.root, e.parent = hostActApplyParent(t)
	e.dataDir = filepath.Join(e.root, dockerTestName(e.intent))
	if err := os.Mkdir(e.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(e.dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestApplyProbeDockerInitRunsExactlyTheSignedArguments(t *testing.T) {
	e := newDockerInitEnv(t)
	result, err := ApplyProbeDockerInit(t.Context(), e.parent, e.intent, e.sim)
	if err != nil {
		t.Fatal(err)
	}
	if result != probeInitTestResult() {
		t.Fatalf("result %+v", result)
	}
	e.sim.wantCalls(t,
		dockerInspectImage(dockerTestTag(e.intent)),
		domain.ProbeDockerInitArgs(e.intent.Identity),
	)
	args := e.sim.recorded()[1]
	if args[0] != "run" || !slices.Contains(args, "--rm") || !slices.Contains(args, "none") || args[len(args)-3] != "init" {
		t.Fatalf("init vector %q", args)
	}
}

func TestApplyProbeDockerInitRefusalsBeforeRunning(t *testing.T) {
	t.Run("a non-empty data directory is Conflict", func(t *testing.T) {
		e := newDockerInitEnv(t)
		if err := os.WriteFile(filepath.Join(e.dataDir, "identity"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyProbeDockerInit(t.Context(), e.parent, e.intent, e.sim)
		wantDockerErr(t, "non-empty", err, domain.ErrProbeBinaryStageWriteConflict)
		if len(e.sim.recorded()) != 0 {
			t.Fatalf("docker ran: %q", e.sim.recorded())
		}
	})
	t.Run("a data directory with another mode is Unavailable", func(t *testing.T) {
		e := newDockerInitEnv(t)
		if err := os.Chmod(e.dataDir, 0o750); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyProbeDockerInit(t.Context(), e.parent, e.intent, e.sim)
		wantDockerErr(t, "mode", err, domain.ErrProbeBinaryStageWriteUnavailable)
		if len(e.sim.recorded()) != 0 {
			t.Fatal("docker ran")
		}
	})
	t.Run("a missing data directory is Unavailable", func(t *testing.T) {
		e := newDockerInitEnv(t)
		if err := os.Remove(e.dataDir); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyProbeDockerInit(t.Context(), e.parent, e.intent, e.sim)
		wantDockerErr(t, "missing", err, domain.ErrProbeBinaryStageWriteUnavailable)
		if len(e.sim.recorded()) != 0 {
			t.Fatal("docker ran")
		}
	})
	t.Run("a data directory that is a link is Unavailable", func(t *testing.T) {
		e := newDockerInitEnv(t)
		other := t.TempDir()
		if err := os.Remove(e.dataDir); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(other, e.dataDir); err != nil {
			t.Fatal(err)
		}
		_, err := ApplyProbeDockerInit(t.Context(), e.parent, e.intent, e.sim)
		wantDockerErr(t, "link", err, domain.ErrProbeBinaryStageWriteUnavailable)
		if len(e.sim.recorded()) != 0 {
			t.Fatal("docker ran")
		}
	})
	t.Run("an image tag that does not resolve is Unavailable and nothing runs", func(t *testing.T) {
		e := newDockerInitEnv(t)
		e.sim.images = map[string]string{}
		_, err := ApplyProbeDockerInit(t.Context(), e.parent, e.intent, e.sim)
		wantDockerErr(t, "no image", err, domain.ErrProbeBinaryStageWriteUnavailable)
		e.sim.wantNoCall(t, "run")
	})
	t.Run("a systemd identity or other kind is Input", func(t *testing.T) {
		e := newDockerInitEnv(t)
		for name, in := range map[string]domain.ProbeHostActionIntent{
			"systemd": hostActFrameIntent(domain.ProbeActivationProbeInit),
			"kind":    dockerFrameIntent(t, domain.ProbeActivationServiceStart),
		} {
			_, err := ApplyProbeDockerInit(t.Context(), e.parent, in, e.sim)
			wantDockerErr(t, name, err, domain.ErrProbeBinaryStageWriteInput)
		}
		_, err := ApplyProbeDockerInit(t.Context(), nil, e.intent, e.sim)
		wantDockerErr(t, "nil parent", err, domain.ErrProbeBinaryStageWriteInput)
		if len(e.sim.recorded()) != 0 {
			t.Fatal("docker ran")
		}
	})
}

func TestApplyProbeDockerInitFailuresAfterTheRunStartsAreUnknown(t *testing.T) {
	for name, prep := range map[string]func(*dockerInitEnv){
		"run exits nonzero": func(e *dockerInitEnv) {
			e.sim.override = func(a []string) (string, int, error, bool) { return "", 1, nil, a[0] == "run" }
		},
		"run runner error": func(e *dockerInitEnv) {
			e.sim.override = func(a []string) (string, int, error, bool) { return "", -1, errors.New("boom"), a[0] == "run" }
		},
		"output is not the init document": func(e *dockerInitEnv) { e.sim.initOut = "not json\n" },
		"output is empty":                 func(e *dockerInitEnv) { e.sim.initOut = "" },
	} {
		t.Run(name, func(t *testing.T) {
			e := newDockerInitEnv(t)
			prep(e)
			result, err := ApplyProbeDockerInit(t.Context(), e.parent, e.intent, e.sim)
			wantDockerErr(t, name, err, domain.ErrProbeBinaryStageWriteUnknown)
			if result != (ProbeInitResult{}) {
				t.Fatalf("result leaked on failure: %+v", result)
			}
		})
	}
}

// ---- service_start ----------------------------------------------------------

func dockerStartSim(intent domain.ProbeHostActionIntent, status string) *dockerSim {
	sim := newDockerSim()
	sim.containers[dockerTestName(intent)] = &dockerSimContainer{status: status, label: intent.Identity.OperationID}
	return sim
}

func fastDockerStart(t *testing.T) {
	t.Helper()
	wait, poll := probeDockerStartWait, probeDockerStartPoll
	probeDockerStartWait, probeDockerStartPoll = 150*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() { probeDockerStartWait, probeDockerStartPoll = wait, poll })
}

func TestApplyProbeDockerStartStartsACreatedOrExitedContainerAndWaitsForRunning(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeActivationServiceStart)
	name := dockerTestName(intent)
	for _, status := range []string{"created", "exited"} {
		t.Run(status, func(t *testing.T) {
			sim := dockerStartSim(intent, status)
			if err := ApplyProbeDockerStart(t.Context(), intent, sim); err != nil {
				t.Fatal(err)
			}
			sim.wantCalls(t,
				dockerInspectState(name),
				dockerCall("start", name),
				dockerInspectRunning(name),
			)
		})
	}
	t.Run("running only after a few polls", func(t *testing.T) {
		fastDockerStart(t)
		sim := dockerStartSim(intent, "created")
		sim.runningAt = 3
		if err := ApplyProbeDockerStart(t.Context(), intent, sim); err != nil {
			t.Fatal(err)
		}
		if sim.polls != 3 {
			t.Fatalf("polled %d times, want 3", sim.polls)
		}
	})
}

func TestApplyProbeDockerStartRefusalsBeforeStart(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeActivationServiceStart)
	name := dockerTestName(intent)
	for status, want := range map[string]error{
		"running":    domain.ErrProbeBinaryStageWriteConflict,
		"restarting": domain.ErrProbeBinaryStageWriteConflict,
		"paused":     domain.ErrProbeBinaryStageWriteConflict,
		"dead":       domain.ErrProbeBinaryStageWriteUnavailable,
		"removing":   domain.ErrProbeBinaryStageWriteUnavailable,
	} {
		t.Run(status, func(t *testing.T) {
			sim := dockerStartSim(intent, status)
			wantDockerErr(t, status, ApplyProbeDockerStart(t.Context(), intent, sim), want)
			sim.wantCalls(t, dockerInspectState(name))
		})
	}
	t.Run("a foreign label is Conflict", func(t *testing.T) {
		sim := dockerStartSim(intent, "created")
		sim.containers[name].label = "someone-else"
		wantDockerErr(t, "label", ApplyProbeDockerStart(t.Context(), intent, sim), domain.ErrProbeBinaryStageWriteConflict)
		sim.wantNoCall(t, "start")
	})
	t.Run("an absent container is Unavailable", func(t *testing.T) {
		sim := newDockerSim()
		wantDockerErr(t, "absent", ApplyProbeDockerStart(t.Context(), intent, sim), domain.ErrProbeBinaryStageWriteUnavailable)
		sim.wantCalls(t, dockerInspectState(name), dockerListName(name))
	})
	t.Run("a daemon that cannot answer is Unavailable", func(t *testing.T) {
		sim := newDockerSim()
		sim.override = func([]string) (string, int, error, bool) { return "", 1, nil, true }
		wantDockerErr(t, "down", ApplyProbeDockerStart(t.Context(), intent, sim), domain.ErrProbeBinaryStageWriteUnavailable)
		sim.wantNoCall(t, "start")
	})
	t.Run("a systemd identity or other kind is Input", func(t *testing.T) {
		sim := dockerStartSim(intent, "created")
		for label, in := range map[string]domain.ProbeHostActionIntent{
			"systemd": hostActFrameIntent(domain.ProbeActivationServiceStart),
			"kind":    dockerFrameIntent(t, domain.ProbeActivationProbeInit),
		} {
			wantDockerErr(t, label, ApplyProbeDockerStart(t.Context(), in, sim), domain.ErrProbeBinaryStageWriteInput)
		}
		if len(sim.recorded()) != 0 {
			t.Fatal("docker ran")
		}
	})
}

func TestApplyProbeDockerStartFailuresAfterStartAreUnknown(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeActivationServiceStart)
	t.Run("start exits nonzero", func(t *testing.T) {
		sim := dockerStartSim(intent, "created")
		sim.override = func(a []string) (string, int, error, bool) { return "", 1, nil, a[0] == "start" }
		wantDockerErr(t, "start", ApplyProbeDockerStart(t.Context(), intent, sim), domain.ErrProbeBinaryStageWriteUnknown)
	})
	t.Run("never running within the wait", func(t *testing.T) {
		fastDockerStart(t)
		sim := dockerStartSim(intent, "created")
		sim.neverRuns = true
		began := time.Now()
		wantDockerErr(t, "never", ApplyProbeDockerStart(t.Context(), intent, sim), domain.ErrProbeBinaryStageWriteUnknown)
		if elapsed := time.Since(began); elapsed < probeDockerStartWait {
			t.Fatalf("gave up after %v, before the %v wait", elapsed, probeDockerStartWait)
		}
		if sim.polls < 5 {
			t.Fatalf("only %d polls", sim.polls)
		}
	})
	t.Run("poll runner error until the wait ends", func(t *testing.T) {
		fastDockerStart(t)
		sim := dockerStartSim(intent, "created")
		sim.override = func(a []string) (string, int, error, bool) {
			return "", -1, errors.New("boom"), a[0] == "container" && a[3] == "{{.State.Running}}"
		}
		wantDockerErr(t, "poll error", ApplyProbeDockerStart(t.Context(), intent, sim), domain.ErrProbeBinaryStageWriteUnknown)
	})
	t.Run("canceled while waiting", func(t *testing.T) {
		wait, poll := probeDockerStartWait, probeDockerStartPoll
		probeDockerStartWait, probeDockerStartPoll = time.Minute, time.Minute
		t.Cleanup(func() { probeDockerStartWait, probeDockerStartPoll = wait, poll })
		sim := dockerStartSim(intent, "created")
		sim.neverRuns = true
		ctx, cancel := context.WithCancel(t.Context())
		time.AfterFunc(50*time.Millisecond, cancel)
		wantDockerErr(t, "canceled", ApplyProbeDockerStart(ctx, intent, sim), domain.ErrProbeBinaryStageWriteUnknown)
	})
}

// ---- cleanup ----------------------------------------------------------------

type dockerCleanupEnv struct {
	intent  domain.ProbeHostActionIntent
	sim     *dockerSim
	root    string
	parent  *os.File
	dataDir string
	name    string
}

func newDockerCleanupEnv(t *testing.T, container, data bool) *dockerCleanupEnv {
	t.Helper()
	e := &dockerCleanupEnv{intent: dockerFrameIntent(t, domain.ProbeHostActionCleanup), sim: newDockerSim()}
	e.name = dockerTestName(e.intent)
	e.sim.images[dockerTestTag(e.intent)] = dockerTestConfigID
	e.root, e.parent = hostActApplyParent(t)
	e.dataDir = filepath.Join(e.root, e.name)
	if container {
		e.sim.containers[e.name] = &dockerSimContainer{status: "running", label: e.intent.Identity.OperationID}
	}
	if data {
		if err := os.MkdirAll(filepath.Join(e.dataDir, "keys", "nested"), 0o700); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"identity", "keys/probe.key", "keys/nested/leaf"} {
			if err := os.WriteFile(filepath.Join(e.dataDir, f), []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Symlink("/nonexistent-target", filepath.Join(e.dataDir, "dangling")); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *dockerCleanupEnv) wantDataIntact(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(e.dataDir, "keys", "nested", "leaf")); err != nil {
		t.Fatalf("data directory was touched: %v", err)
	}
}

func (e *dockerCleanupEnv) wantDataGone(t *testing.T) {
	t.Helper()
	if _, err := os.Lstat(e.dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("data directory remains: %v", err)
	}
}

func TestApplyProbeDockerCleanupRemovesTheContainerThenTheDataTree(t *testing.T) {
	e := newDockerCleanupEnv(t, true, true)
	if err := ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, e.sim); err != nil {
		t.Fatal(err)
	}
	e.sim.wantCalls(t,
		dockerInspectState(e.name),
		dockerCall("rm", "--force", e.name),
		dockerInspectState(e.name),
		dockerListName(e.name),
	)
	e.wantDataGone(t)
	if len(e.sim.containers) != 0 {
		t.Fatal("container remains")
	}
	// The image is shared: cleanup never removes it.
	e.sim.wantNoCall(t, "rmi")
	e.sim.wantNoCall(t, "image")
	if _, ok := e.sim.images[dockerTestTag(e.intent)]; !ok {
		t.Fatal("image tag was removed")
	}
	// Idempotent: a second cleanup finds nothing and runs only the checks.
	again := newDockerSim()
	if err := ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, again); err != nil {
		t.Fatal(err)
	}
	again.wantCalls(t, dockerInspectState(e.name), dockerListName(e.name))
}

func TestApplyProbeDockerCleanupPartialInstalls(t *testing.T) {
	t.Run("nothing present is a no-op beyond the existence check", func(t *testing.T) {
		e := newDockerCleanupEnv(t, false, false)
		if err := ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, e.sim); err != nil {
			t.Fatal(err)
		}
		e.sim.wantCalls(t, dockerInspectState(e.name), dockerListName(e.name))
	})
	t.Run("only the data directory", func(t *testing.T) {
		e := newDockerCleanupEnv(t, false, true)
		if err := ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, e.sim); err != nil {
			t.Fatal(err)
		}
		e.sim.wantCalls(t,
			dockerInspectState(e.name), dockerListName(e.name),
			dockerInspectState(e.name), dockerListName(e.name),
		)
		e.sim.wantNoCall(t, "rm")
		e.wantDataGone(t)
	})
	t.Run("only the container", func(t *testing.T) {
		e := newDockerCleanupEnv(t, true, false)
		if err := ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, e.sim); err != nil {
			t.Fatal(err)
		}
		e.sim.wantCalls(t,
			dockerInspectState(e.name),
			dockerCall("rm", "--force", e.name),
			dockerInspectState(e.name), dockerListName(e.name),
		)
	})
}

func TestApplyProbeDockerCleanupRefusesBeforeAnyEffect(t *testing.T) {
	t.Run("a foreign operation label is Conflict and nothing is removed", func(t *testing.T) {
		e := newDockerCleanupEnv(t, true, true)
		e.sim.containers[e.name].label = "someone-else"
		wantDockerErr(t, "label", ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, e.sim), domain.ErrProbeBinaryStageWriteConflict)
		e.sim.wantCalls(t, dockerInspectState(e.name))
		e.wantDataIntact(t)
		if len(e.sim.containers) != 1 {
			t.Fatal("container was removed")
		}
	})
	t.Run("an unlabeled container is Conflict", func(t *testing.T) {
		e := newDockerCleanupEnv(t, true, true)
		e.sim.containers[e.name].label = ""
		wantDockerErr(t, "no label", ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, e.sim), domain.ErrProbeBinaryStageWriteConflict)
		e.sim.wantNoCall(t, "rm")
		e.wantDataIntact(t)
	})
	t.Run("a daemon that cannot answer is Unavailable, never read as nothing to clean", func(t *testing.T) {
		e := newDockerCleanupEnv(t, true, true)
		e.sim.override = func([]string) (string, int, error, bool) { return "", 1, nil, true }
		wantDockerErr(t, "down", ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, e.sim), domain.ErrProbeBinaryStageWriteUnavailable)
		e.sim.wantNoCall(t, "rm")
		e.wantDataIntact(t)
	})
	t.Run("a runner error is Unavailable", func(t *testing.T) {
		e := newDockerCleanupEnv(t, true, true)
		e.sim.override = func([]string) (string, int, error, bool) { return "", -1, errors.New("boom"), true }
		wantDockerErr(t, "error", ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, e.sim), domain.ErrProbeBinaryStageWriteUnavailable)
		e.wantDataIntact(t)
	})
	t.Run("a data directory that is a link is Conflict and the container stays", func(t *testing.T) {
		e := newDockerCleanupEnv(t, true, false)
		target := t.TempDir()
		sentinel := filepath.Join(target, "sentinel")
		if err := os.WriteFile(sentinel, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, e.dataDir); err != nil {
			t.Fatal(err)
		}
		wantDockerErr(t, "link", ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, e.sim), domain.ErrProbeBinaryStageWriteConflict)
		e.sim.wantNoCall(t, "rm")
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("link target was touched: %v", err)
		}
		if len(e.sim.containers) != 1 {
			t.Fatal("container was removed")
		}
	})
	t.Run("a systemd identity or other kind is Input", func(t *testing.T) {
		e := newDockerCleanupEnv(t, true, true)
		for label, in := range map[string]domain.ProbeHostActionIntent{
			"systemd": hostActFrameIntent(domain.ProbeHostActionCleanup),
			"kind":    dockerFrameIntent(t, domain.ProbeActivationServiceStart),
		} {
			wantDockerErr(t, label, ApplyProbeDockerCleanup(t.Context(), e.parent, in, e.sim), domain.ErrProbeBinaryStageWriteInput)
		}
		wantDockerErr(t, "nil parent", ApplyProbeDockerCleanup(t.Context(), nil, e.intent, e.sim), domain.ErrProbeBinaryStageWriteInput)
		if len(e.sim.recorded()) != 0 {
			t.Fatal("docker ran")
		}
		e.wantDataIntact(t)
	})
}

func TestApplyProbeDockerCleanupFailuresAfterAnEffectAreUnknown(t *testing.T) {
	t.Run("rm exits nonzero and the data directory is kept", func(t *testing.T) {
		e := newDockerCleanupEnv(t, true, true)
		e.sim.override = func(a []string) (string, int, error, bool) { return "", 1, nil, a[0] == "rm" }
		wantDockerErr(t, "rm", ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, e.sim), domain.ErrProbeBinaryStageWriteUnknown)
		e.wantDataIntact(t)
	})
	t.Run("the container is still there afterwards", func(t *testing.T) {
		e := newDockerCleanupEnv(t, true, true)
		e.sim.override = func(a []string) (string, int, error, bool) { return "", 0, nil, a[0] == "rm" }
		wantDockerErr(t, "survivor", ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, e.sim), domain.ErrProbeBinaryStageWriteUnknown)
	})
	t.Run("the final check cannot be answered", func(t *testing.T) {
		e := newDockerCleanupEnv(t, true, true)
		e.sim.override = func(a []string) (string, int, error, bool) {
			if a[0] == "container" && a[1] == "ls" {
				return "", 1, nil, true
			}
			return "", 0, nil, false
		}
		// The first existence check finds the container (inspect succeeds, no
		// listing needed); after removal the listing fails, so absence is unproven.
		wantDockerErr(t, "unproven", ApplyProbeDockerCleanup(t.Context(), e.parent, e.intent, e.sim), domain.ErrProbeBinaryStageWriteUnknown)
		e.wantDataGone(t)
	})
}

// ---- staging file -----------------------------------------------------------

func stageFileName(intent domain.ProbeHostActionIntent) string {
	return intent.Identity.OperationID + "-" + intent.AttemptID + ".tar"
}

func TestOpenProbeImageStageFileCreatesProtectedDirectoryAndExclusiveFile(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeHostActionImageCache)
	root, parent := hostActApplyParent(t)
	staged, err := OpenProbeImageStageFile(t.Context(), parent, root, intent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = staged.Remove() })
	dir := filepath.Join(root, ProbeImageStageDirName)
	want := filepath.Join(dir, stageFileName(intent))
	if staged.Path() != want {
		t.Fatalf("path %q, want %q", staged.Path(), want)
	}
	if ProbeImageStageDirName != "uptime-phoenix-image-stage" {
		t.Fatalf("stage directory name drifted: %q", ProbeImageStageDirName)
	}
	dinfo, err := os.Lstat(dir)
	if err != nil || !dinfo.IsDir() || dinfo.Mode().Perm() != 0o700 {
		t.Fatalf("stage dir %v %v", dinfo, err)
	}
	finfo, err := os.Lstat(want)
	if err != nil || !finfo.Mode().IsRegular() || finfo.Mode().Perm() != 0o600 {
		t.Fatalf("stage file %v %v", finfo, err)
	}
	if _, err := staged.Write([]byte("archive")); err != nil {
		t.Fatal(err)
	}
	if err := staged.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(want); err != nil || string(got) != "archive" {
		t.Fatalf("staged bytes %q %v", got, err)
	}
	if _, err := staged.Write([]byte("more")); err == nil {
		t.Fatal("write after Close succeeded")
	}
	// A second open of the same attempt is refused and leaves the file alone.
	if _, err := OpenProbeImageStageFile(t.Context(), parent, root, intent); !errors.Is(err, domain.ErrProbeBinaryStageWriteConflict) {
		t.Fatalf("second open: %v", err)
	}
	if got, _ := os.ReadFile(want); string(got) != "archive" {
		t.Fatal("second open disturbed the staged file")
	}
	if err := staged.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(want); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged file remains: %v", err)
	}
	if err := staged.Remove(); err != nil {
		t.Fatalf("Remove is not idempotent: %v", err)
	}
	if _, err := os.Lstat(dir); err != nil {
		t.Fatal("the shared staging directory must remain")
	}
	// A later attempt reuses the existing 0700 directory.
	next := intent
	next.AttemptID = "44444444-4444-4444-8444-444444444444"
	again, err := OpenProbeImageStageFile(t.Context(), parent, root, next)
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Remove(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenProbeImageStageFileRefusesAnUnprotectedStageDirectory(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeHostActionImageCache)
	t.Run("group-accessible directory", func(t *testing.T) {
		root, parent := hostActApplyParent(t)
		dir := filepath.Join(root, ProbeImageStageDirName)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := OpenProbeImageStageFile(t.Context(), parent, root, intent)
		wantDockerErr(t, "mode", err, domain.ErrProbeBinaryStageWriteUnavailable)
		hostActApplyRequireEntries(t, dir)
	})
	t.Run("symlink to another directory", func(t *testing.T) {
		root, parent := hostActApplyParent(t)
		target := t.TempDir()
		if err := os.Symlink(target, filepath.Join(root, ProbeImageStageDirName)); err != nil {
			t.Fatal(err)
		}
		_, err := OpenProbeImageStageFile(t.Context(), parent, root, intent)
		wantDockerErr(t, "link", err, domain.ErrProbeBinaryStageWriteUnavailable)
		hostActApplyRequireEntries(t, target)
	})
	t.Run("regular file in its place", func(t *testing.T) {
		root, parent := hostActApplyParent(t)
		if err := os.WriteFile(filepath.Join(root, ProbeImageStageDirName), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := OpenProbeImageStageFile(t.Context(), parent, root, intent)
		wantDockerErr(t, "file", err, domain.ErrProbeBinaryStageWriteUnavailable)
	})
}

func TestOpenProbeImageStageFileNeverFollowsOrAdoptsAnExistingEntry(t *testing.T) {
	intent := dockerFrameIntent(t, domain.ProbeHostActionImageCache)
	root, parent := hostActApplyParent(t)
	dir := filepath.Join(root, ProbeImageStageDirName)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, filepath.Join(dir, stageFileName(intent))); err != nil {
		t.Fatal(err)
	}
	_, err := OpenProbeImageStageFile(t.Context(), parent, root, intent)
	wantDockerErr(t, "planted link", err, domain.ErrProbeBinaryStageWriteConflict)
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatal("the planted link target was written")
	}
}

func TestOpenProbeImageStageFileRefusesBadInput(t *testing.T) {
	root, parent := hostActApplyParent(t)
	good := dockerFrameIntent(t, domain.ProbeHostActionImageCache)
	for name, tc := range map[string]struct {
		parent *os.File
		path   string
		intent domain.ProbeHostActionIntent
	}{
		"nil parent":       {nil, root, good},
		"relative path":    {parent, "var/lib", good},
		"other kind":       {parent, root, dockerFrameIntent(t, domain.ProbeHostActionContainer)},
		"systemd identity": {parent, root, hostActFrameIntent(domain.ProbeHostActionDataDirectory)},
		"zero intent":      {parent, root, domain.ProbeHostActionIntent{}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := OpenProbeImageStageFile(t.Context(), tc.parent, tc.path, tc.intent)
			wantDockerErr(t, name, err, domain.ErrProbeBinaryStageWriteInput)
		})
	}
	hostActApplyRequireEntries(t, root)
}
